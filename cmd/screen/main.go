// Command screen 筛选满足一组硬性条件的 BSC meme 币并入库。
//
// 四个条件:
//
//  1. 持币人数在区间内
//  2. 狙击占比低于上限
//  3. Top10 中每个持币者的持仓不超过上限(已剔除 LP 与销毁地址)
//  4. 市值低于上限
//
// 候选来自本库已有的 meme_tokens 表(近 N 天 + 已毕业 + 持币人数区间),
// 这三个条件全走索引、零 API 消耗;其余指标由 GMGN 补齐。
// 只有四个条件全部通过的才会写入 token_screen_metrics。
//
// 之所以自成一命令而不做成 enrich 的一个 -mode:enrich 的三个模式语义统一
// (把外部信息写回主表,是"补全存量"),而这里是"查询 → 判定 → 写独立结果表",
// 有自己的一整套阈值参数和自己的表;而且 enrich 不建表,这个命令需要自己迁移。
//
// # 首次真跑的顺序
//
// 默认的并发数与节流间隔是**猜的**(GMGN 配额未公开)。所以第一次务必:
//
//	screen -limit 10 -workers 1 -dump-raw ./scratch
//
// 确认字段名与限流行为后,据 ./scratch 下的原始响应修正 internal/enrich/gmgn.go
// 里的候选键名,再把 internal/enrich/testdata 换成真实样本,最后放大 -limit。
//
// # 没有 API key 也能验证全链路
//
//	screen -dry-run                                 只跑候选 SQL,不调 API
//	screen -fixtures ./internal/enrich/testdata     用本地夹具代替真实请求
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"stock/internal/chain"
	"stock/internal/config"
	"stock/internal/enrich"
	"stock/internal/model"
	"stock/internal/store"
)

func main() {
	f := parseFlags()

	if err := run(f); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

// prefilterLogLimit 是最多打印几条粗筛淘汰原因。
//
// 打太多会淹没真正通过的那几条,但一条都不打又会在"全部被拦下"时完全失去线索。
const prefilterLogLimit = 12

// flags 是命令行参数。沿用 backfill 的约定:零值表示"用配置里的值",
// 这样默认行为集中在 config 一处,命令行只用于临时覆盖。
type flags struct {
	days    int
	limit   int
	workers int
	source  string

	minHolders int64
	maxHolders int64
	maxMcap    float64
	maxSniper  float64
	maxTop10   float64

	sniperBasis string
	topN        int
	// requireMigrated 用字符串而非 bool:flag 包的 bool 无法区分"未指定"与
	// "显式设为 false",而这里需要保留"未指定时用配置值"的语义。
	requireMigrated string

	order string
	// discover 是发现路径:空(默认)走链上扫 PairCreated,trenches 走 GMGN 战壕榜。
	discover string
	// scanOnly 表示只做链上发现、不核验。扫链不消耗 GMGN 配额,可以先跑它验证。
	scanOnly bool

	dryRun   bool
	fixtures string
	dumpRaw  string
}

func parseFlags() flags {
	var f flags
	flag.IntVar(&f.days, "days", 0, "候选时间窗口(天),0 表示用配置值")
	flag.IntVar(&f.limit, "limit", 0, "单轮处理的代币数上限,0 表示用配置值")
	flag.IntVar(&f.workers, "workers", 0, "并发数,0 表示用配置值")
	flag.StringVar(&f.source, "source", "", "限定发现渠道: four_meme | pancake_v2 | 空=不限")

	flag.Int64Var(&f.minHolders, "min-holders", 0, "持币人数下界(严格大于),0 表示用配置值")
	flag.Int64Var(&f.maxHolders, "max-holders", 0, "持币人数上界(严格小于),0 表示用配置值")
	flag.Float64Var(&f.maxMcap, "max-mcap", 0, "市值上限(美元),0 表示用配置值")
	flag.Float64Var(&f.maxSniper, "max-sniper", 0, "狙击占比上限(百分数),0 表示用配置值")
	flag.Float64Var(&f.maxTop10, "max-top10", 0, "Top10 中单个持币者占比上限(百分数),0 表示用配置值")

	flag.StringVar(&f.sniperBasis, "sniper-basis", "", "狙击口径: count | amount")
	flag.IntVar(&f.topN, "top-n", 0, "取前 N 名持币者判断,0 表示用配置值")
	flag.StringVar(&f.requireMigrated, "require-migrated", "",
		"是否只保留已毕业的代币: true | false | 空=用配置值")

	flag.StringVar(&f.discover, "discover", "", "发现路径: 空(默认)=链上扫 PairCreated(完整) | trenches=GMGN 战壕榜(有漏)")
	flag.StringVar(&f.order, "order", "", "扫库路径的扫描方向: newest | oldest")

	flag.BoolVar(&f.scanOnly, "scan-only", false, "只做链上发现(不消耗 GMGN 配额),不核验")

	flag.BoolVar(&f.dryRun, "dry-run", false, "只列出候选,不调用 GMGN")
	flag.StringVar(&f.fixtures, "fixtures", "", "离线模式: 目录下 <address>.json 代替真实请求")
	flag.StringVar(&f.dumpRaw, "dump-raw", "", "把原始响应写入该目录(排障用)")

	flag.Parse()
	return f
}

func run(f flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取工作目录失败: %w", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}

	criteria := buildCriteria(cfg, f)

	st, err := store.New(cfg.MySQL.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	// 自给自足:不要求先跑一次 backfill 才建表
	if err := st.MigrateDir(ctx, filepath.Join(root, "migrations")); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}

	if strings.EqualFold(f.discover, "trenches") {
		return runTrenches(ctx, cfg, st, criteria, f)
	}
	return runChain(ctx, cfg, st, criteria, f)
}

// runChain 走链上发现路径——默认路径。
//
// 分两步,而且两步都可断点续跑:
//
//	① 扫链:eth_getLogs 拉近 N 天的 PancakeSwap PairCreated,写入 chain_candidates
//	   (INSERT IGNORE,重复扫不产生重复行)
//	② 核验:取还没问过 GMGN 的候选,逐个调 token info 粗筛,幸存者再取明细
//
// 为什么不用战壕:那是 GMGN 的精选榜,实测会漏币(某个四个条件全过的已毕业币
// 就不在其中)。链上的 PairCreated 是全量账本,一个不漏。
func runChain(ctx context.Context, cfg *config.Config, st *store.Store,
	c enrich.ScreenCriteria, f flags) error {

	days := pickInt(f.days, cfg.Screen.Days)

	// ---- ① 扫链 ----
	if err := scanChain(ctx, cfg, st, days); err != nil {
		return err
	}

	if f.scanOnly {
		return printCandidateProgress(ctx, st)
	}

	// ---- ② 核验 ----
	g := enrich.NewGMGN(cfg.GMGN.BaseURL, cfg.GMGN.APIKey, cfg.GMGN.Chain,
		cfg.GMGN.HolderLimit, cfg.GMGN.MinIntervalMS)
	if g == nil {
		return fmt.Errorf("GMGN_API_KEY 未配置——请在 .env 中填入后重试 " +
			"(申请地址 https://gmgn.ai/ai,查询类只需 API Key 不需要私钥)")
	}

	prog, err := st.CandidateProgress(ctx)
	if err != nil {
		return fmt.Errorf("读取候选进度失败: %w", err)
	}
	log.Printf("📊 候选池 %d 个,待核验 %d 个(已通过 %d / 已筛掉 %d / 无流动性 %d / 无数据 %d)",
		prog.Total, prog.Pending, prog.Passed, prog.Rejected, prog.NoLiquidity, prog.NoData)

	if prog.Pending == 0 {
		log.Printf("✅ 没有待核验的候选")
		return nil
	}

	// limit 为 0 表示不限量(由 store 侧处理,不会拼成 LIMIT 0)
	limit := f.limit
	pending, err := st.PendingCandidates(ctx, limit)
	if err != nil {
		return fmt.Errorf("查询待核验候选失败: %w", err)
	}

	// ---- ② 流动性预筛 ----
	//
	// 排在所有 GMGN 调用之前:这是整条链路上唯一不花配额的核验步骤。
	// 实测(2026-09-26)能拦下八成以上候选——已毕业的币 20/20 都能在 DexScreener
	// 查到交易对,而未毕业的 28 个里 18 个完全查不到、5 个流动性是 0~0.5 美元。
	// GMGN 每次调用至少 1 个权重,这一步省掉的是大头。
	if cfg.DexScreener.Enabled {
		ds := enrich.NewDexScreener(cfg.DexScreener.BaseURL, cfg.DexScreener.Batch,
			cfg.DexScreener.MinIntervalMS)
		before := len(pending)
		pending, err = liquidityPrefilter(ctx, st, ds, pending,
			cfg.DexScreener.MinLiquidityUSD, f.workers)
		if err != nil {
			return fmt.Errorf("流动性预筛失败: %w", err)
		}
		log.Printf("💧 流动性预筛:%d → %d 个(淘汰 %d 个,零 GMGN 配额)",
			before, len(pending), before-len(pending))
		if len(pending) == 0 {
			return printCandidateProgress(ctx, st)
		}
	}

	log.Printf("🔍 本轮核验 %d 个(每个至少消耗 1 个权重)", len(pending))

	s := &screener{st: st, criteria: c, dumpDir: f.dumpRaw}

	// 全体暂停时刻(UnixNano,0 表示不暂停)。
	//
	// 这是长任务能跑完的关键:几万个候选必然撞上限流,而"撞到就中止"
	// 等于永远跑不完。改为全体 worker 一起暂停到接口给的解除时刻,
	// 然后自动继续——任务自己会调节奏,不需要人工续跑。
	var pauseUntil atomic.Int64

	s.fetch = func(ctx context.Context, addr string, full bool) (*enrich.GMGNTokenReport, error) {
		if err := waitIfPaused(ctx, &pauseUntil); err != nil {
			return nil, err
		}

		rep, err := g.Info(ctx, addr)
		if err != nil {
			noteQuotaPause(&pauseUntil, err)
			return nil, err
		}
		if !full {
			return rep, nil
		}
		rep, err = g.Complete(ctx, addr, rep)
		if err != nil {
			noteQuotaPause(&pauseUntil, err)
			return nil, err
		}
		return rep, nil
	}
	if f.dumpRaw != "" {
		if err := os.MkdirAll(f.dumpRaw, 0o755); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", f.dumpRaw, err)
		}
	}

	// 并发核验。
	//
	// 必须并发:节流是客户端的**全局**间隔(原子 CAS),它限制的是"多久发一次",
	// 不是"同时有几个人在等"。单协程时每次请求要等完整的网络往返(约 1 秒),
	// 节流那几百毫秒根本起不了作用——实测串行只有 52 个/分钟,而节流允许 250+。
	// 并发之后网络等待被重叠掉,吞吐才真正由节流决定。
	workers := f.workers
	if workers <= 0 {
		workers = 8
	}
	jobs := make(chan store.Candidate)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cand := range jobs {
				if ctx.Err() != nil || s.stop.Load() {
					return
				}
				s.verifyOne(ctx, st, s.fetch, c, cand)
				if n := s.queried.Load(); n%200 == 0 {
					log.Printf("  … 已核验 %d,通过 %d,筛掉 %d,无数据 %d,失败 %d",
						n, s.passed.Load(), s.prefiltered.Load(), s.noData.Load(), s.failed.Load())
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, cand := range pending {
			select {
			case jobs <- cand:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	return s.report()
}

// liquidityPrefilter 用 DexScreener 的免费批量接口做第一道筛。
//
// 返回应当继续送 GMGN 核验的候选;被淘汰的已经写库标记,不再返回。
//
// 批量是这里的关键:接口一次能查 30 个地址,8.5 万个候选只要约 2800 次请求。
// 若改成逐个查,请求量就和 GMGN 一个量级,省下的时间全没了。
//
// 失败处理刻意**保守**:某批请求出错就整批原样放行,绝不因为第三方接口抖动
// 把候选误判掉。代价是那批要多花 GMGN 配额,这个代价值得。
func liquidityPrefilter(ctx context.Context, st *store.Store, ds *enrich.DexScreener,
	pending []store.Candidate, minLiquidity float64, workers int) ([]store.Candidate, error) {

	if ds == nil || len(pending) == 0 {
		return pending, nil
	}
	if workers <= 0 {
		workers = 4
	}

	batch := ds.Batch()
	var batches [][]store.Candidate
	for start := 0; start < len(pending); start += batch {
		end := start + batch
		if end > len(pending) {
			end = len(pending)
		}
		batches = append(batches, pending[start:end])
	}

	type batchResult struct {
		kept   []store.Candidate
		failed bool
	}

	// 结果用 channel 回传而不是共享切片:全仓库零 Mutex,汇总只由本协程做。
	jobs := make(chan []store.Candidate)
	results := make(chan batchResult, len(batches))

	// 预筛要跑十分钟量级(节流 250ms 一次 x 两千多批),一条不打印会让人以为卡死。
	var processed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				k, f := prefetchLiquidity(ctx, st, ds, b, minLiquidity)
				if n := processed.Add(1); n%200 == 0 {
					log.Printf("  … 流动性预筛已处理 %d/%d 批", n, len(batches))
				}
				results <- batchResult{kept: k, failed: f}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, b := range batches {
			select {
			case jobs <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var kept []store.Candidate
	failed := 0
	for r := range results {
		kept = append(kept, r.kept...)
		if r.failed {
			failed++
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if failed > 0 {
		log.Printf("⚠️  预筛有 %d/%d 批请求失败,这些批次原样送入核验(不会误杀,只多花配额)",
			failed, len(batches))
	}
	return kept, nil
}

// prefetchLiquidity 处理一批候选:批量查行情、分流,并把淘汰的写库。
//
// 第二个返回值表示这一批是否因为请求失败而整批放行。
func prefetchLiquidity(ctx context.Context, st *store.Store, ds *enrich.DexScreener,
	batch []store.Candidate, minLiquidity float64) (kept []store.Candidate, failed bool) {

	addrs := make([]string, len(batch))
	for i, c := range batch {
		addrs[i] = c.ContractAddress
	}

	pairs, err := ds.Tokens(ctx, addrs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true
		}
		return batch, true
	}

	_, dropped := enrich.LiquidityFilter(addrs, pairs, minLiquidity)
	if len(dropped) == 0 {
		return batch, false
	}
	if err := st.MarkCandidates(ctx, dropped, store.VerdictNoLiquidity); err != nil {
		// 标记失败不影响本批的判定:这些候选下轮会被重新拾取,再筛一次而已
		log.Printf("  ⚠️  标记无流动性候选失败(下轮自动重试): %v", err)
	}

	drop := make(map[string]struct{}, len(dropped))
	for _, a := range dropped {
		drop[a] = struct{}{}
	}
	kept = make([]store.Candidate, 0, len(batch)-len(dropped))
	for _, c := range batch {
		if _, ok := drop[c.ContractAddress]; !ok {
			kept = append(kept, c)
		}
	}
	return kept, false
}

// waitIfPaused 在"全体暂停"期间阻塞,直到解除或 ctx 结束。
//
// 暂停是**全局**的:一个 worker 撞上限流,所有 worker 都该停下来等,
// 否则其余 goroutine 会继续把请求打进去,把短暂冷却拖成更长的封禁。
func waitIfPaused(ctx context.Context, until *atomic.Int64) error {
	for {
		ns := until.Load()
		if ns == 0 {
			return nil
		}
		d := time.Until(time.Unix(0, ns))
		if d <= 0 {
			return nil
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// noteQuotaPause 在遇到限流时登记一个全体暂停时刻。
//
// 只登记、不等待:本次调用照常失败(候选不会被标记,下轮自动重试),
// 而后续请求会先过 waitIfPaused 这一关。上限 15 分钟是为了防止
// 接口给一个离谱的 reset 时间把任务卡死。
func noteQuotaPause(until *atomic.Int64, err error) {
	if !enrich.IsGMGNQuotaBanned(err) {
		return
	}
	wait := time.Until(enrich.ResetAt(err))
	if wait < time.Minute {
		wait = time.Minute
	}
	if wait > 15*time.Minute {
		wait = 15 * time.Minute
	}
	next := time.Now().Add(wait).UnixNano()
	// 只往后推,不往前拉
	for {
		cur := until.Load()
		if cur >= next || until.CompareAndSwap(cur, next) {
			break
		}
	}
}

// verifyOne 核验单个候选。
//
// 抽成独立函数是为了能放进 worker pool —— runChain 的核验阶段是并发的。
func (s *screener) verifyOne(ctx context.Context, st *store.Store,
	fetch fetchFunc, c enrich.ScreenCriteria, cand store.Candidate) {

	s.queried.Add(1)

	info, err := fetch(ctx, cand.ContractAddress, false)
	if err != nil {
		s.handleFetchError(err)
		if enrich.IsNoTokenData(err) {
			_ = st.MarkCandidates(ctx, []string{cand.ContractAddress}, store.VerdictNoData)
			s.noData.Add(1)
		}
		// 其它错误不标记:下一轮会重新拾取
		return
	}

	if ok, reason := enrich.PreFilter(info, c); !ok {
		_ = st.MarkCandidates(ctx, []string{cand.ContractAddress}, store.VerdictRejected)
		// 前若干条打印原因:几万个候选全打会淹没输出,但一条不打印就完全看不出
		// 是什么条件在拦——通过率异常时无从判断是筛得对还是筛错了。
		if n := s.prefiltered.Add(1); n <= prefilterLogLimit {
			log.Printf("  ⊘ %-20s 淘汰: %s", cand.ContractAddress[:12], reason)
		}
		return
	}

	s.deep.Add(1)
	rep, err := fetch(ctx, cand.ContractAddress, true)
	if err != nil {
		s.handleFetchError(err)
		return
	}
	s.dumpRaw(cand.ContractAddress, rep)

	tok := model.Token{
		ChainID:         model.ChainID,
		ContractAddress: cand.ContractAddress,
		Name:            rep.Name,
		Symbol:          rep.Symbol,
		PairAddress:     cand.PairAddress,
		LaunchedAt:      time.Unix(rep.CreatedTimestamp, 0),
	}

	res, saved := s.finish(ctx, tok, rep)
	switch {
	case res.Inconclusive:
		// "不知道"不是"不合格"。这里刻意**不标记**:标记要写 checked_at,而
		// checked_at 一置位这条候选就再也不会被拾取,等于把一次数据缺失永久
		// 固化成一条否决结论——正是 enrich/screen.go 里写明要避免的那种
		// "最危险的失败模式"。留空则下轮自动重新核验。
		//
		// 代价要说清楚:若某个币**永远**判不出结果(比如怎么都识别不出 LP),
		// 它每轮都会重花一次配额。等实测确认这类币多到值得处理时,再加一个
		// 尝试计数字段把它挡掉。
	case !res.Passed:
		s.prefiltered.Add(1)
		_ = st.MarkCandidates(ctx, []string{cand.ContractAddress}, store.VerdictRejected)
	default:
		// 先落库、成功了才标记。反过来会让"候选表说通过、结果表却没有"成为
		// 永久状态,而且 checked_at 已置位,永远不会重试。
		if saved {
			_ = st.MarkCandidates(ctx, []string{cand.ContractAddress}, store.VerdictPassed)
		}
	}
}

// printCandidateProgress 打印候选池进度。scan-only 模式用它收尾。
func printCandidateProgress(ctx context.Context, st *store.Store) error {
	p, err := st.CandidateProgress(ctx)
	if err != nil {
		return err
	}
	log.Printf("📊 候选池:共 %d 个,待核验 %d,已通过 %d,已筛掉 %d,无流动性 %d,无数据 %d,失败 %d",
		p.Total, p.Pending, p.Passed, p.Rejected, p.NoLiquidity, p.NoData, p.Errored)
	return nil
}

// scanChain 扫链并把新交易对写入候选池。
func scanChain(ctx context.Context, cfg *config.Config, st *store.Store, days int) error {
	rpc, err := chain.New(cfg.Chain.RPCEndpoints)
	if err != nil {
		return err
	}

	head, err := rpc.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("获取链头高度失败: %w", err)
	}
	since := time.Now().AddDate(0, 0, -days)
	from, err := rpc.BlockByTime(ctx, since.Unix())
	if err != nil {
		return fmt.Errorf("定位起始区块失败: %w", err)
	}

	chunk := cfg.Chain.ChunkSize
	if chunk == 0 {
		chunk = 50_000
	}
	log.Printf("⛓  扫链:区块 %d ~ %d(约 %d 天,共 %d 个分片,每片 %d 块)",
		from, head, days, (head-from)/chunk+1, chunk)

	var (
		totalLogs int64
		failed    int
		allPairs  []chain.NewPair
	)
	for start := from; start <= head; start += chunk {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := start + chunk - 1
		if end > head {
			end = head
		}
		res, err := rpc.ScanPairs(ctx, start, end, func(p chain.NewPair) {
			allPairs = append(allPairs, p)
		})
		totalLogs += int64(res.Logs)
		if err != nil {
			// 单个分片失败**不中止整轮**。这是长任务:一次网络抖动就全盘放弃,
			// 代价远大于漏掉一小段区块。失败的分片记下来,由调用方决定是否补扫。
			failed++
			log.Printf("  ⚠️  区块 %d-%d 失败(跳过): %v", start, end, err)
			continue
		}
		log.Printf("  ✓ 区块 %d-%d:日志 %d 条,累计 %d 个交易对",
			start, end, res.Logs, len(allPairs))
	}
	if failed > 0 {
		log.Printf("⚠️  有 %d 个分片失败被跳过;重跑本命令即可补上(候选表按唯一键去重)", failed)
	}

	// 扫完之后统一判定哪一侧是计价币 —— 判据是"地址出现频率",不是硬编码名单。
	// 计价币的集合是开放的(每个发射台可能拿自己的一套代币化股票当计价币),
	// 名单永远追不全;而频率是数据自己说出来的。
	tokens := chain.FilterQuoteTokens(allPairs, cfg.Chain.QuoteThreshold)
	idx := chain.PairIndex(allPairs)
	log.Printf("📉 交易对 %d 个 → 剔除计价币后剩 %d 个疑似新币",
		len(allPairs), len(tokens))

	inputs := make([]store.CandidateInput, 0, len(tokens))
	for _, t := range tokens {
		in := store.CandidateInput{Token: t}
		if p, ok := idx[t]; ok {
			in.Pair, in.Block, in.TxHash = p.Pair, p.Block, p.TxHash
		}
		inputs = append(inputs, in)
	}

	inserted, err := st.SavePairs(ctx, inputs)
	if err != nil {
		return err
	}
	log.Printf("✅ 扫链完成:日志 %d 条,交易对 %d 个,本轮新入库 %d 个候选",
		totalLogs, len(allPairs), inserted)
	return nil
}

// runTrenches 走"战壕"发现路径。
//
// 与从库里扫地址的差别在于**候选从哪来**:这里让 GMGN 直接给出"已毕业、且市值
// 与持币数已达标"的那几十个,而不是把库里十几万个地址逐个去问。
//
// 实测对比(近 5 天, BSC):
//
//	扫库:158,087 个候选 × 权重 1 起 = 至少 158,000 权重,约 11 小时
//	战壕:一次请求 = 权重 2
//
// 而且战壕记录里已经带着持币人数、市值、狙击持仓占比、蜜罐标记,
// 四个条件里的大部分在客户端就能判掉,只有"Top10 每个都不超上限"这一条
// 需要再去取明细(战壕只给前 10 的合计)。
func runTrenches(ctx context.Context, cfg *config.Config, st *store.Store,
	c enrich.ScreenCriteria, f flags) error {

	g := enrich.NewGMGN(cfg.GMGN.BaseURL, cfg.GMGN.APIKey, cfg.GMGN.Chain,
		cfg.GMGN.HolderLimit, cfg.GMGN.MinIntervalMS)
	if g == nil {
		return fmt.Errorf("GMGN_API_KEY 未配置——请在 .env 中填入后重试 " +
			"(申请地址 https://gmgn.ai/ai,查询类只需 API Key 不需要私钥)")
	}

	days := pickInt(f.days, cfg.Screen.Days)
	sinceUnix := time.Now().AddDate(0, 0, -days).Unix()

	flt := enrich.TrenchesFilter{
		// 市值与持币数交给服务端筛,省得把不达标的也传回来
		MaxMarketCap:   c.MaxMarketCap,
		MinHolderCount: c.MinHolders,
		MaxHolderCount: c.MaxHolders,
		Limit:          80,
	}
	log.Printf("🔭 拉取战壕列表(已毕业;服务端筛:市值 < $%.0f,持币 %d~%d;分档取全)",
		c.MaxMarketCap, c.MinHolders, c.MaxHolders)

	toks, err := g.TrenchesAll(ctx, []string{enrich.TrenchesCompleted}, flt)
	if err != nil {
		return fmt.Errorf("拉取战壕列表失败: %w", err)
	}
	log.Printf("✅ 战壕返回 %d 个已毕业代币", len(toks))

	if len(toks) == 0 {
		log.Printf("✅ 服务端筛选后就没有候选了——说明当前没有同时满足" +
			"「已毕业 + 市值 + 持币人数」的币")
		return nil
	}

	var (
		kept     []enrich.TrenchesToken
		rejected int
	)
	for _, t := range toks {
		ok, reason := enrich.TrenchesPreFilter(t, c, sinceUnix)
		if ok {
			kept = append(kept, t)
			continue
		}
		rejected++
		if rejected <= prefilterLogLimit {
			log.Printf("  ⊘ %-22s 粗筛淘汰: %s", displayName2(t.Symbol, t.Name), reason)
		}
	}
	log.Printf("📋 客户端粗筛淘汰 %d 个,剩 %d 个;开始取持币明细(每个权重 5)",
		rejected, len(kept))

	if len(kept) == 0 {
		return nil
	}

	fetch, err := buildFetcher(cfg, f)
	if err != nil {
		return err
	}
	if f.dumpRaw != "" {
		if err := os.MkdirAll(f.dumpRaw, 0o755); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", f.dumpRaw, err)
		}
	}

	s := &screener{st: st, fetch: fetch, criteria: c, dumpDir: f.dumpRaw}
	for _, t := range kept {
		if ctx.Err() != nil {
			break
		}
		if s.stop.Load() {
			break
		}
		// 战壕记录里没有链上库那套字段,按发射台币补一个最小 Token 供入库用
		tok := model.Token{
			ChainID:         model.ChainID,
			ContractAddress: t.Address,
			Name:            t.Name,
			Symbol:          t.Symbol,
			Source:          model.SourceFourMeme,
			LaunchedAt:      time.Unix(t.CreatedTimestamp, 0),
		}
		s.queried.Add(1)
		s.deep.Add(1)

		rep, err := fetch(ctx, t.Address, true)
		if err != nil {
			s.handleFetchError(err)
			continue
		}
		s.dumpRaw(t.Address, rep)
		// 战壕来源的代币不进候选表(它们不在 chain_candidates 里),
		// 所以只看判定与落库的结果,没有候选需要回填标记
		s.finish(ctx, tok, rep)
	}
	return s.report()
}

// displayName2 在 symbol 与 name 之间挑一个能看的。
func displayName2(symbol, name string) string {
	if s := strings.TrimSpace(symbol); s != "" {
		return s
	}
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return "(无名)"
}

// buildCriteria 把配置与命令行 flag 合并成一套判定阈值。
func buildCriteria(cfg *config.Config, f flags) enrich.ScreenCriteria {
	c := enrich.ScreenCriteria{
		MinHolders:      cfg.Screen.MinHolders,
		MaxHolders:      cfg.Screen.MaxHolders,
		MaxSniperRate:   cfg.Screen.MaxSniperRate,
		SniperBasis:     cfg.Screen.SniperBasis,
		TopN:            cfg.Screen.TopN,
		MaxHolderRate:   cfg.Screen.MaxTop10Rate,
		MaxMarketCap:    cfg.Screen.MaxMarketCap,
		RequireMigrated: cfg.Screen.RequireMigrated,
		StrictLP:        cfg.Screen.StrictLP,
	}
	if f.requireMigrated != "" {
		c.RequireMigrated = strings.EqualFold(f.requireMigrated, "true")
	}
	if f.minHolders > 0 {
		c.MinHolders = f.minHolders
	}
	if f.maxHolders > 0 {
		c.MaxHolders = f.maxHolders
	}
	if f.maxSniper > 0 {
		c.MaxSniperRate = f.maxSniper
	}
	if f.sniperBasis != "" {
		c.SniperBasis = f.sniperBasis
	}
	if f.topN > 0 {
		c.TopN = f.topN
	}
	if f.maxTop10 > 0 {
		c.MaxHolderRate = f.maxTop10
	}
	if f.maxMcap > 0 {
		c.MaxMarketCap = f.maxMcap
	}
	return c
}

// buildFetcher 决定取数方式:离线夹具还是真实客户端。
func buildFetcher(cfg *config.Config, f flags) (fetchFunc, error) {
	if f.fixtures != "" {
		log.Printf("📂 离线模式:从 %s 读取夹具,不会发出任何网络请求", f.fixtures)
		return fixtureFetcher(f.fixtures), nil
	}

	g := enrich.NewGMGN(cfg.GMGN.BaseURL, cfg.GMGN.APIKey, cfg.GMGN.Chain,
		cfg.GMGN.HolderLimit, cfg.GMGN.MinIntervalMS)
	if g == nil {
		return nil, fmt.Errorf("GMGN_API_KEY 未配置——请在 .env 中填入后重试 " +
			"(申请地址 https://gmgn.ai/ai,查询类只需 API Key 不需要私钥)")
	}
	log.Printf("🌐 GMGN %s (chain=%s, 节流 %dms, 两阶段漏斗已启用)",
		cfg.GMGN.BaseURL, cfg.GMGN.Chain, cfg.GMGN.MinIntervalMS)

	// 两阶段:full=false 时只花 1 个权重取 info,粗筛通过后才花另外 6 个。
	return func(ctx context.Context, addr string, full bool) (*enrich.GMGNTokenReport, error) {
		rep, err := g.Info(ctx, addr)
		if err != nil {
			return nil, err
		}
		if !full {
			return rep, nil
		}
		return g.Complete(ctx, addr, rep)
	}, nil
}

// fetchFunc 取一份代币报告。
//
// full=false 时只取 token info(权重 1),true 时再补齐 security 与 holders(权重 6)。
// 离线夹具一次就带齐三份文档,所以它忽略 full——这是有意的:夹具模式验证的是
// 解析与判定,不是配额策略。
type fetchFunc func(ctx context.Context, addr string, full bool) (*enrich.GMGNTokenReport, error)

// fixtureFetcher 从目录读取 <合约地址>.json,走与线上完全相同的解析路径。
//
// 这是没有 API key 时验证全链路的手段——它同时覆盖了迁移、候选 SQL、
// 解析、判定、写库,唯一替换掉的只有 HTTP 那一跳。
func fixtureFetcher(dir string) fetchFunc {
	return func(_ context.Context, addr string, _ bool) (*enrich.GMGNTokenReport, error) {
		p := filepath.Join(dir, strings.ToLower(addr)+".json")
		b, err := os.ReadFile(p)
		if err != nil {
			// 按"该币无数据"处理:夹具目录里没有就跳过,不算失败
			return nil, fmt.Errorf("%w: 读取夹具 %s 失败: %v", enrich.ErrNoTokenData, p, err)
		}
		return enrich.ParseTokenReport(b)
	}
}

// screener 持有本轮筛选的状态。
type screener struct {
	st       *store.Store
	fetch    fetchFunc
	criteria enrich.ScreenCriteria
	dumpDir  string

	queried     atomic.Int64
	prefiltered atomic.Int64
	// deep 是进入第二段的个数(取 security 与 holders)。权重核算要靠它,
	// 不能事后用"查询数 - 淘汰数"倒推——写库失败之类的情况会让倒推失准。
	deep         atomic.Int64
	passed       atomic.Int64
	inconclusive atomic.Int64
	noData       atomic.Int64
	failed       atomic.Int64
	quotaHits    atomic.Int64
	// stop 用于"配额封禁 / key 失效"时让所有 worker 停手。
	// GMGN 在冷却期内重试会延长封禁,所以遇到限流必须立刻收工。
	stop atomic.Bool
}

// finish 对一份完整快照做判定并入库。
//
// 抽出来是因为两条发现路径(扫库 / 战壕)到这里就汇合了:无论候选从哪来,
// 拿到完整快照之后的判定与落库逻辑完全一样。
//
// 两个返回值都不可省:
//
//	res   判定结论。调用方要靠 Inconclusive 与 !Passed 的区别来决定怎么标记候选
//	saved 结果是否**确实落库了**。注意它不是"是否通过"——通过但写库失败同样
//	      是 false。扫库路径必须据此决定要不要标记 passed:标记了就不再重试,
//	      而结果却没进库,这条通过记录就永久消失了。
func (s *screener) finish(ctx context.Context, tok model.Token,
	rep *enrich.GMGNTokenReport) (enrich.ScreenResult, bool) {

	res := enrich.Evaluate(rep, tok.PairAddress, s.criteria)

	switch {
	case res.Inconclusive:
		s.inconclusive.Add(1)
		log.Printf("  ⊘ %-22s 无法确定: %s", displayName(tok), res.ReasonString())
		return res, false
	case !res.Passed:
		// 未通过的币不入库,也不逐条打印——候选数以千计,全打会淹没真正通过的那几条
		return res, false
	}

	if err := s.st.SaveScreenMetrics(ctx, buildRecord(tok, rep, res, s.criteria)); err != nil {
		s.failed.Add(1)
		log.Printf("⚠️  写入 %s 的筛选结果失败(候选不标记,下轮重试): %v",
			tok.ContractAddress, err)
		return res, false
	}
	s.passed.Add(1)
	log.Printf("  ✅ %-22s 市值 $%-8.0f 持币 %-5d 狙击 %.2f%% Top10 最大 %.2f%%",
		displayName(tok), rep.MarketCap, rep.HolderCount, res.SniperRate, res.Top10MaxRate)
	return res, true
}

// handleFetchError 分类处理取数失败。
func (s *screener) handleFetchError(err error) {
	switch {
	case enrich.IsGMGNQuotaBanned(err):
		// 刻意**不**置 stop:长任务撞限流是常态,中止等于永远跑不完。
		// 暂停由 noteQuotaPause 登记,全体 worker 会一起等到解除时刻。
		s.quotaHits.Add(1)
		if n := s.quotaHits.Load(); n == 1 || n%50 == 0 {
			log.Printf("⏸  GMGN 限流(第 %d 次),全体暂停。%s", n, resetHint(err))
		}
	case enrich.IsGMGNUnauthorized(err):
		if s.stop.CompareAndSwap(false, true) {
			log.Printf("❌ %v", err)
		}
	case enrich.IsNoTokenData(err):
		s.noData.Add(1)
	default:
		s.failed.Add(1)
		log.Printf("⚠️  查询失败: %v", err)
	}
}

// dumpRaw 把三次原始响应按合并形态落盘。
//
// 落合并形态而不是三次响应的原始拼接,是为了让产出的文件**可以直接当夹具用**:
//
//	screen -dump-raw ./scratch          # 实测一轮,落盘真实响应
//	screen -fixtures ./scratch          # 用它们离线复现,不花配额
//
// 这样"实测 → 校准字段名 → 复现"的闭环中间不需要任何手工转换。
func (s *screener) dumpRaw(addr string, rep *enrich.GMGNTokenReport) {
	if s.dumpDir == "" {
		return
	}
	body := enrich.MarshalTokenReport(rep)
	if len(body) == 0 {
		return
	}
	p := filepath.Join(s.dumpDir, strings.ToLower(addr)+".json")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		log.Printf("⚠️  写原始响应 %s 失败: %v", p, err)
	}
}

// report 打印本轮小结。
func (s *screener) report() error {
	log.Printf("───────────────────────────────")
	if n := s.quotaHits.Load(); n > 0 {
		log.Printf("⏸  本轮共触发限流 %d 次(已自动等待并继续)", n)
	}
	log.Printf("📊 已查询 %d,粗筛淘汰 %d,通过 %d,无法确定 %d,无数据 %d,失败 %d",
		s.queried.Load(), s.prefiltered.Load(), s.passed.Load(),
		s.inconclusive.Load(), s.noData.Load(), s.failed.Load())

	// "无法确定"与"已写入失败"这两类都**没有标记候选**,下轮会原样重来。
	// 不说清楚的话,看到"每轮都在核验同样多的币"会以为是去重坏了。
	if n := s.inconclusive.Load(); n > 0 {
		log.Printf("   ⚠️  其中 %d 个数据不足无法判定,候选未标记,下轮会重新核验(并再花一次配额)", n)
	}
	if n := s.failed.Load(); n > 0 {
		log.Printf("   ⚠️  其中 %d 个因写入或调用失败未落库,候选未标记,下轮会重新核验", n)
	}

	// 粗筛省下的配额是本功能能不能在免费档跑起来的关键,值得明说。
	// 权重口径:每个候选的第一段 1 个,进入第二段的再各加 6 个
	// (security 权重 1 + holders 权重 5)。
	if d := s.deep.Load(); d > 0 {
		log.Printf("   其中 %d 个进入第二段(取明细),两段合计约 %d 个权重单位",
			d, s.queried.Load()+d*6)
	} else {
		log.Printf("   %d 个全部止步于第一段,合计约 %d 个权重单位",
			s.queried.Load(), s.queried.Load())
	}

	if s.stop.Load() {
		log.Printf("⚠️  本轮因限流或鉴权问题提前结束,已通过的结果照常入库")
	}
	return nil
}

// buildRecord 把判定结果转成入库记录。
func buildRecord(tok model.Token, rep *enrich.GMGNTokenReport,
	res enrich.ScreenResult, c enrich.ScreenCriteria) model.ScreenRecord {

	criteriaJSON, _ := json.Marshal(c)
	holdersJSON, _ := json.Marshal(rep.Holders)

	return model.ScreenRecord{
		ContractAddress: tok.ContractAddress,
		Name:            tok.Name,
		Symbol:          tok.Symbol,
		Source:          tok.Source,
		LaunchedAt:      tok.LaunchedAt,

		HolderCount: rep.HolderCount,
		SniperCount: rep.SniperWallets,
		MarketCap:   rep.MarketCap,
		Liquidity:   rep.Liquidity,

		LaunchpadStatus:    rep.LaunchpadStatus,
		LaunchpadProgress:  rep.LaunchpadProgress,
		MigrationMarketCap: rep.MigrationMarketCap,
		CreatorAddress:     rep.CreatorAddress,
		CreatorTokenStatus: rep.CreatorTokenStatus,

		SniperRate:      res.SniperRate,
		SniperRateBasis: c.SniperBasis,
		SniperCountRate: rep.SniperCountRate,
		SniperHoldRate:  rep.SniperHoldRate,
		BundlerRate:     rep.BundlerRate,

		Top10Rate:     rep.Top10Rate,
		Top10RateNoLP: res.Top10RateNoLP,
		Top10MaxRate:  res.Top10MaxRate,
		Top10Count:    res.Top10Count,

		LPExcludedCount: res.LPExcludedCount,
		LPExcludedRate:  res.LPExcludedRate,
		LPBasis:         res.LPBasis,
		HolderCoverage:  rep.Coverage,

		CreatorRate:  rep.CreatorRate,
		IsHoneypot:   rep.IsHoneypot,
		BuyTax:       rep.BuyTax,
		SellTax:      rep.SellTax,
		IsOpenSource: rep.IsOpenSource,
		IsRenounced:  rep.IsRenounced,

		CriteriaJSON: string(criteriaJSON),
		HoldersJSON:  string(holdersJSON),
	}
}

// resetHint 把限流解除时间转成人话。
func resetHint(err error) string {
	t := enrich.ResetAt(err)
	if t.IsZero() {
		return "请稍后再试"
	}
	return fmt.Sprintf("解除时间 %s(约 %s 后)",
		t.Format("15:04:05"), time.Until(t).Round(time.Second))
}

func pickInt(flagVal, cfgVal int) int {
	if flagVal > 0 {
		return flagVal
	}
	return cfgVal
}

func displayName(t model.Token) string {
	if s := strings.TrimSpace(t.Symbol); s != "" {
		return s
	}
	if s := strings.TrimSpace(t.Name); s != "" {
		return s
	}
	return "(无名)"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
