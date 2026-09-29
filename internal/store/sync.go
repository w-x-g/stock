package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"stock/internal/model"
)

// timeFromUnix 把导出时读到的 Unix 秒还原成时间。
//
// 零值与非法值都返回零值时间:调用方据此判断"这一行没有时间",而不是把
// 1970 年当成真实时刻写回库。
func timeFromUnix(ts sql.NullInt64) time.Time {
	if !ts.Valid || ts.Int64 <= 0 {
		return time.Time{}
	}
	return time.Unix(ts.Int64, 0)
}

// 本文件实现跨机器的数据同步:把库里的内容导出成文本文件(提交进 git),另
// 一台电脑拉取后合并回自己的库。
//
// # 为什么是文本文件而不是数据库文件
//
// chain_candidates 每轮筛选都会被批量 UPDATE(15 万行的 checked_at/verdict),
// 若把 SQLite 那样的单文件数据库放进 git,等于每次提交一份完整的几十 MB 二进制
// blob——二进制几乎无法做 delta 压缩,跑十几轮仓库就上 GB。而文本文件 git 会做
// delta,增量提交很小,冲突也是行级的、可以人工合并。
//
// # 为什么只同步这两张表的一部分
//
// 候选池里"未核验"和 nolp 那部分是可以免费重建的(重扫链、重查 DexScreener,
// 都不消耗 GMGN 配额),体积却是全部;真正不可重建的是**花 GMGN 配额换来的
// 核验结论**与**筛选结果**。同步只带这两样。
//
// # 两个文件的分工
//
//	verified.tsv   负结论缓存(rejected / nodata),用来避免另一台重复花配额
//	results.jsonl  筛选通过的完整快照,是交付物本体
//
// passed 刻意**只出现在 results.jsonl** 里:一个"候选表说通过、结果表却没有明细"
// 的地址会让两边的数据自相矛盾,而把它限定在结果文件里就从格式上消灭了这类不一致。

// syncBatchSize 是导入/导出时单批处理的行数,与其余批量读写保持一致。
const syncBatchSize = 500

// VerifiedRow 是一条已核验的结论。
//
// CheckedAt 用 Unix 秒而不是 time.Time:这个值要经 git 往返于两台可能处于不同
// 时区的机器,Unix 秒是唯一与本地时区无关的表示。入库时用 FROM_UNIXTIME 交给
// MySQL 自己换算,Go 侧全程不碰时区。
type VerifiedRow struct {
	ContractAddress string
	Verdict         CandidateVerdict
	CheckedAt       int64
}

// importableVerdicts 是允许从文件导入的结论集合。
//
// 刻意**不含 passed**:passed 的明细在 results.jsonl 里,单有一条 tsv 记录会让
// 候选表与结果表对不上。也不含 error——那是本机"调用失败、可重试"的临时标记,
// 不是结论,导出时也不会写出去。
var importableVerdicts = map[CandidateVerdict]bool{
	VerdictRejected:    true,
	VerdictNoData:      true,
	VerdictNoLiquidity: true,
}

// ---------------------------------------------------------------------------
// 地址与结论的校验
// ---------------------------------------------------------------------------

// normalizeAddress 归一化并校验合约地址。
//
// 必须归一:链上写入时统一小写(chain.pair.go 的 topicToAddress 已 ToLower),
// 而 utf8mb4 的默认排序规则大小写不敏感,`0xAB` 与 `0xab` 在唯一键上会撞车,
// 存储形态却保留原样——不归一就会在库里留下两种写法。
func normalizeAddress(s string) (string, bool) {
	a := strings.ToLower(strings.TrimSpace(s))
	if len(a) != 42 || !strings.HasPrefix(a, "0x") {
		return "", false
	}
	for i := 2; i < len(a); i++ {
		c := a[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return a, true
}

// ParseStats 记录解析过程中被丢弃的内容。
//
// 一定要有:文件被截断、手工编辑出错、或另一边版本更旧时,静默丢弃会让人以为
// "导入了但没生效"。宁可多打几行日志。
type ParseStats struct {
	Lines      int // 文件里的总行数(不含注释与空行)
	Kept       int
	Malformed  int // 列数不对 / 数字解析失败
	BadAddress int
	BadVerdict int
	Duplicates int
}

// String 把统计压成一行,供日志使用。
func (s ParseStats) String() string {
	return fmt.Sprintf("行 %d,采纳 %d,格式错 %d,地址非法 %d,结论非法 %d,重复 %d",
		s.Lines, s.Kept, s.Malformed, s.BadAddress, s.BadVerdict, s.Duplicates)
}

// ---------------------------------------------------------------------------
// verified.tsv 的编解码(纯函数,便于离线测试)
// ---------------------------------------------------------------------------

// verifiedHeader 是文件首行的说明。解析时以 '#' 开头的行会被整行跳过。
const verifiedHeader = "# stock 已核验结论	contract_address<TAB>verdict<TAB>checked_at(Unix 秒)\n"

// FormatVerified 把结论渲染成 TSV 字节。
//
// 按地址升序输出:导出结果必须是确定的,否则每跑一次文件的行序都在变,git 会
// 产生毫无意义的全量 diff。绝不能依赖 map 迭代顺序。
func FormatVerified(rows []VerifiedRow) []byte {
	sorted := make([]VerifiedRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ContractAddress < sorted[j].ContractAddress
	})

	var b strings.Builder
	b.WriteString(verifiedHeader)
	for _, r := range sorted {
		b.WriteString(r.ContractAddress)
		b.WriteByte('\t')
		b.WriteString(string(r.Verdict))
		b.WriteByte('\t')
		b.WriteString(strconv.FormatInt(r.CheckedAt, 10))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// ParseVerified 解析 TSV。
//
// 容错刻意做得宽:单行有问题就跳过并计数,**绝不因为一行坏数据放弃整个文件**
// ——这与仓库其它长任务的失败处理哲学一致(见 store.SavePairs 的单批失败跳过)。
//
// 两个 Windows 上必踩的坑在这里一并处理:
//   - CRLF:git 的 core.autocrlf 会把 LF 换成 CRLF,不 TrimRight 的话结论会变成
//     "rejected\r",既通不过白名单校验,又看不出哪里错了
//   - UTF-8 BOM:首行地址会变成 "0x…",唯一键查不中,于是凭空插一行
func ParseVerified(data []byte) ([]VerifiedRow, ParseStats, error) {
	var stats ParseStats
	data = stripBOM(data)

	seen := make(map[string]struct{})
	var out []VerifiedRow

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		stats.Lines++

		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			stats.Malformed++
			continue
		}

		addr, ok := normalizeAddress(fields[0])
		if !ok {
			stats.BadAddress++
			continue
		}
		verdict := CandidateVerdict(strings.TrimSpace(fields[1]))
		if !importableVerdicts[verdict] {
			stats.BadVerdict++
			continue
		}
		ts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil || ts <= 0 {
			stats.Malformed++
			continue
		}
		if _, dup := seen[addr]; dup {
			stats.Duplicates++
			continue
		}
		seen[addr] = struct{}{}

		out = append(out, VerifiedRow{ContractAddress: addr, Verdict: verdict, CheckedAt: ts})
		stats.Kept++
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ContractAddress < out[j].ContractAddress
	})
	return out, stats, nil
}

// stripBOM 去掉 UTF-8 BOM。
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// ---------------------------------------------------------------------------
// results.jsonl 的编解码(纯函数,便于离线测试)
// ---------------------------------------------------------------------------

// FormatResults 把筛选结果渲染成 JSONL。
//
// 时间统一转 UTC 再序列化:DSN 里是 loc=Local,两台机器若时区不同,同一个时刻
// 会序列化成不同的字符串,文件永远无法收敛。转成 UTC 后字节是确定的。
func FormatResults(recs []model.ScreenRecord) ([]byte, error) {
	sorted := make([]model.ScreenRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ContractAddress < sorted[j].ContractAddress
	})

	var b strings.Builder
	for i := range sorted {
		sorted[i].LaunchedAt = sorted[i].LaunchedAt.UTC()
		if !sorted[i].CheckedAt.IsZero() {
			sorted[i].CheckedAt = sorted[i].CheckedAt.UTC()
		}
		line, err := json.Marshal(sorted[i])
		if err != nil {
			return nil, fmt.Errorf("序列化 %s 失败: %w", sorted[i].ContractAddress, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// ParseResults 解析 JSONL。单行有问题就跳过并计数,不放弃整个文件。
func ParseResults(data []byte) ([]model.ScreenRecord, ParseStats, error) {
	var stats ParseStats
	data = stripBOM(data)

	seen := make(map[string]struct{})
	var out []model.ScreenRecord

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		stats.Lines++

		var rec model.ScreenRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			stats.Malformed++
			continue
		}
		addr, ok := normalizeAddress(rec.ContractAddress)
		if !ok {
			stats.BadAddress++
			continue
		}
		if _, dup := seen[addr]; dup {
			stats.Duplicates++
			continue
		}
		seen[addr] = struct{}{}
		rec.ContractAddress = addr

		out = append(out, rec)
		stats.Kept++
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ContractAddress < out[j].ContractAddress
	})
	return out, stats, nil
}

// ---------------------------------------------------------------------------
// 合并计划的推导(纯函数,便于离线测试)
// ---------------------------------------------------------------------------

// VerifiedPlan 是导入结论的处置计划。
type VerifiedPlan struct {
	// Apply 是需要写入的条目(本机没有,或本机尚未得出结论)。
	Apply []VerifiedRow
	// SkippedCount 是本机已有明确结论、必须原样保留的条目数。
	//
	// 保留本机结论而不是让对方的覆盖过来,是"宁漏勿错"的直接体现:两台机器对
	// 同一个币得出不同结论是可能的(快照时刻不同),静默覆盖会凭空丢掉一台的
	// 判定结果,而且事后无从追溯。
	SkippedCount int
}

// planVerified 决定每条导入结论该怎么落地。
//
// local 是本机已有的结论(verdict 为空表示该地址本机没有,或还没有结论)。
func planVerified(rows []VerifiedRow, local map[string]CandidateVerdict) VerifiedPlan {
	var plan VerifiedPlan
	for _, r := range rows {
		switch v := local[r.ContractAddress]; {
		case v == "" || v == VerdictError:
			// 本机没有,或只有"调用失败"这个临时标记 —— 都该被填补
			plan.Apply = append(plan.Apply, r)
		default:
			plan.SkippedCount++
		}
	}
	return plan
}

// ResultsPlan 是导入筛选结果的处置计划。
type ResultsPlan struct {
	Apply []model.ScreenRecord
	// ConflictCount 是本机候选表已判为不合格、却又出现在对方结果文件里的条数。
	//
	// 这类必须拦掉:放进去会让本机出现"候选表说 rejected、结果表里却有它"的
	// 自相矛盾。之所以会出现,是因为 tsv 与 jsonl 两份文件各自看到的对方状态
	// 不同步(对方先导出 tsv、之后才通过筛选),属于正常的中间态,不是错误。
	ConflictCount int
	// ExistingCount 是本机已有结果记录的条数,按"先写入者优先"保留。
	//
	// 不用时间戳比较新旧:token_screen_metrics.checked_at 是
	// DEFAULT CURRENT_TIMESTAMP 且没有 ON UPDATE,又不在 upsert 子句里,
	// 也就是说**本机重新核验时它不会前进**(screen.go 里"由 DB 自动更新"的
	// 注释是错的)。拿一个不会前进的值比新旧,结论必然是错的,不如不比。
	ExistingCount int
}

// planResults 决定每条导入结果该怎么落地。
//
// localVerdict 是本机候选表的结论,existing 是本机已有的结果记录地址。
func planResults(recs []model.ScreenRecord,
	localVerdict map[string]CandidateVerdict, existing map[string]struct{}) ResultsPlan {

	var plan ResultsPlan
	for _, r := range recs {
		addr := r.ContractAddress
		if _, ok := existing[addr]; ok {
			plan.ExistingCount++
			continue
		}
		switch localVerdict[addr] {
		case VerdictRejected, VerdictNoData, VerdictNoLiquidity:
			plan.ConflictCount++
			continue
		}
		plan.Apply = append(plan.Apply, r)
	}
	return plan
}

// ---------------------------------------------------------------------------
// 导出
// ---------------------------------------------------------------------------

// ExportVerified 读出可以同步的核验结论。
//
// 排除两类:nolp(占候选八成,却在另一台可以免费重建——DexScreener 不消耗
// 配额,带上只会让文件白白膨胀)与 error(临时状态,不是结论)。
func (s *Store) ExportVerified(ctx context.Context, includeNoLiquidity bool) ([]VerifiedRow, error) {
	query := `
		SELECT contract_address, verdict, UNIX_TIMESTAMP(checked_at)
		FROM chain_candidates
		WHERE checked_at IS NOT NULL
		  AND verdict IN ('rejected', 'nodata')`
	if includeNoLiquidity {
		query = `
		SELECT contract_address, verdict, UNIX_TIMESTAMP(checked_at)
		FROM chain_candidates
		WHERE checked_at IS NOT NULL
		  AND verdict IN ('rejected', 'nodata', 'nolp')`
	}
	query += "\n\t\tORDER BY contract_address"

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VerifiedRow
	for rows.Next() {
		var (
			r       VerifiedRow
			verdict string
			ts      sql.NullInt64
		)
		if err := rows.Scan(&r.ContractAddress, &verdict, &ts); err != nil {
			return nil, err
		}
		r.Verdict = CandidateVerdict(verdict)
		r.CheckedAt = ts.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// exportScreenColumns 是导出用的列清单,直接复用写入侧的 screenColumns,
// 保证"写进去的"与"导出来的"永远是同一组列、同一顺序。
//
// 现成的 ListScreenResults 只 select 了 14 列,照它导出会**静默丢掉 21 列**
// (criteria_json、holders_json 与全部安全字段),所以这里必须自己列全。
const exportScreenColumns = screenColumns + ", UNIX_TIMESTAMP(checked_at)"

// ExportResults 读出全部筛选结果,字段与 token_screen_metrics 一一对齐。
func (s *Store) ExportResults(ctx context.Context) ([]model.ScreenRecord, error) {
	query := "SELECT " + exportScreenColumns + " FROM token_screen_metrics ORDER BY contract_address"

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ScreenRecord
	for rows.Next() {
		r, err := scanScreenRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanScreenRecord 按 screenColumns 的顺序把一行读成 ScreenRecord。
//
// 全程用 sql.Null* 接:列都可空,而 NULL 与零值在这里必须折叠成同一种表示
// ——写入侧的 nullString / nullableJSON 已经把空串折叠成 NULL,所以导出时把
// NULL 折回零值、再导入时又折成 NULL,往返是稳定的(幂等的)。
func scanScreenRecord(rows *sql.Rows) (model.ScreenRecord, error) {
	var (
		r           model.ScreenRecord
		name        sql.NullString
		symbol      sql.NullString
		source      sql.NullString
		holders     sql.NullInt64
		snipers     sql.NullInt64
		mcap        sql.NullFloat64
		liq         sql.NullFloat64
		launchpad   sql.NullInt64
		progress    sql.NullFloat64
		migMcap     sql.NullFloat64
		creator     sql.NullString
		creatorSt   sql.NullString
		sniperRt    sql.NullFloat64
		sniperBasis sql.NullString
		sniperCntRt sql.NullFloat64
		sniperHold  sql.NullFloat64
		bundler     sql.NullFloat64
		top10       sql.NullFloat64
		top10NoLP   sql.NullFloat64
		top10Max    sql.NullFloat64
		top10Count  sql.NullInt64
		lpCount     sql.NullInt64
		lpRate      sql.NullFloat64
		lpBasis     sql.NullString
		coverage    sql.NullFloat64
		creatorRt   sql.NullFloat64
		honeypot    sql.NullBool
		buyTax      sql.NullFloat64
		sellTax     sql.NullFloat64
		openSource  sql.NullBool
		renounced   sql.NullBool
		criteria    sql.NullString
		holdersJSON sql.NullString
		checkedAt   sql.NullInt64
	)

	if err := rows.Scan(
		&r.ContractAddress, &name, &symbol, &source, &r.LaunchedAt,
		&holders, &snipers, &mcap, &liq,
		&launchpad, &progress, &migMcap, &creator, &creatorSt,
		&sniperRt, &sniperBasis, &sniperCntRt, &sniperHold, &bundler,
		&top10, &top10NoLP, &top10Max, &top10Count,
		&lpCount, &lpRate, &lpBasis, &coverage,
		&creatorRt, &honeypot, &buyTax, &sellTax, &openSource, &renounced,
		&criteria, &holdersJSON, &checkedAt,
	); err != nil {
		return r, err
	}

	r.Name = name.String
	r.Symbol = symbol.String
	r.Source = model.Source(source.String)
	r.HolderCount = holders.Int64
	r.SniperCount = snipers.Int64
	r.MarketCap = mcap.Float64
	r.Liquidity = liq.Float64
	r.LaunchpadStatus = int(launchpad.Int64)
	r.LaunchpadProgress = progress.Float64
	r.MigrationMarketCap = migMcap.Float64
	r.CreatorAddress = creator.String
	r.CreatorTokenStatus = creatorSt.String
	r.SniperRate = sniperRt.Float64
	r.SniperRateBasis = sniperBasis.String
	r.SniperCountRate = sniperCntRt.Float64
	r.SniperHoldRate = sniperHold.Float64
	r.BundlerRate = bundler.Float64
	r.Top10Rate = top10.Float64
	r.Top10RateNoLP = top10NoLP.Float64
	r.Top10MaxRate = top10Max.Float64
	r.Top10Count = int(top10Count.Int64)
	r.LPExcludedCount = int(lpCount.Int64)
	r.LPExcludedRate = lpRate.Float64
	r.LPBasis = lpBasis.String
	r.HolderCoverage = coverage.Float64
	r.CreatorRate = creatorRt.Float64
	r.IsHoneypot = honeypot.Bool
	r.BuyTax = buyTax.Float64
	r.SellTax = sellTax.Float64
	r.IsOpenSource = openSource.Bool
	r.IsRenounced = renounced.Bool
	r.CriteriaJSON = criteria.String
	r.HoldersJSON = holdersJSON.String

	// CheckedAt 是导出时唯一不在 35 列里的字段,必须带上:导入端要靠它把时间
	// 原样写回,否则每次导入都会把时间刷成"现在",文件永远收敛不了。
	r.CheckedAt = timeFromUnix(checkedAt)

	return r, nil
}

// ---------------------------------------------------------------------------
// 导入
// ---------------------------------------------------------------------------

// ImportStats 汇总一次导入的处置结果。
//
// 计数一律取自**落库之前**算出的计划,而不是 MySQL 的 affected rows:ODKU 下
// 每行返回 1(插入)或 2(更新),单批里两种混在一起时既算不出行数、也拆不出
// 新增与更新。计划是确定的,而且 -dry-run 能给出与真实执行完全一致的计数。
type ImportStats struct {
	// Applied 是实际写入的行数(新增 + 更新)。
	Applied int64
	// Skipped 是本机已有结论/记录、按规则原样保留的行数。
	Skipped int64
	// Blocked 只对结果表有意义:被本机候选表结论否决、没有写入的行数。
	Blocked int64
}

// String 把统计压成一行,供日志使用。
func (s ImportStats) String() string {
	return fmt.Sprintf("写入 %d,跳过 %d,拦下 %d", s.Applied, s.Skipped, s.Blocked)
}

// localCandidateVerdicts 批量读本机对这些地址已有的结论。
//
// 返回值里不存在的键表示"本机没有这个地址";值为空串表示"有,但还没结论"。
func (s *Store) localCandidateVerdicts(ctx context.Context, addrs []string) (map[string]CandidateVerdict, error) {
	out := make(map[string]CandidateVerdict, len(addrs))
	if len(addrs) == 0 {
		return out, nil
	}

	const query = `SELECT contract_address, COALESCE(verdict, '') FROM chain_candidates WHERE contract_address IN (`

	for start := 0; start < len(addrs); start += syncBatchSize {
		end := start + syncBatchSize
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[start:end]

		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, a := range chunk {
			ph[i] = "?"
			args = append(args, a)
		}
		rows, err := s.db.QueryContext(ctx, query+strings.Join(ph, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var addr, verdict string
			if err := rows.Scan(&addr, &verdict); err != nil {
				rows.Close()
				return nil, err
			}
			out[addr] = CandidateVerdict(verdict)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// existingScreenAddresses 批量查本机已有的筛选结果地址。
func (s *Store) existingScreenAddresses(ctx context.Context, addrs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(addrs))
	if len(addrs) == 0 {
		return out, nil
	}

	const query = `SELECT contract_address FROM token_screen_metrics WHERE contract_address IN (`

	for start := 0; start < len(addrs); start += syncBatchSize {
		end := start + syncBatchSize
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[start:end]

		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, a := range chunk {
			ph[i] = "?"
			args = append(args, a)
		}
		rows, err := s.db.QueryContext(ctx, query+strings.Join(ph, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var addr string
			if err := rows.Scan(&addr); err != nil {
				rows.Close()
				return nil, err
			}
			out[addr] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// verifiedUpsertSQL 一条语句同时完成"补上本机没有的"和"填补本机还没有结论的"。
//
// 判断条件**只读 verdict**,并且把 checked_at 放在前面赋值、verdict 放在最后
// ——这不是随手排的。MySQL 的 ON DUPLICATE KEY UPDATE 从左到右求值,后面的
// 赋值能看到前面刚写入的值,所以只要两个判断条件都碰 checked_at,就必然有一
// 种行状态会写坏:
//
//	先赋 verdict 再赋 checked_at → 本机 error 的行 checked_at 不会被刷新
//	先赋 checked_at 再赋 verdict → 全新行(两列皆 NULL)会写出
//	                               checked_at 有值而 verdict 仍为 NULL 的脏行
//
// 现在两个判断都只读 verdict,而 verdict 是最后才写的,于是求值顺序不再影响
// 结果:无论哪条赋值先算,读到的都是原始的 verdict。前提是"verdict 为 NULL
// 时 checked_at 必为 NULL"这个不变式成立——它由 SavePairs(两列都留 NULL)与
// MarkCandidates(两列一起写)保证。
const verifiedUpsertSQL = `
INSERT INTO chain_candidates (contract_address, verdict, checked_at)
VALUES %s
ON DUPLICATE KEY UPDATE
  checked_at = IF(verdict IS NULL OR verdict = 'error', VALUES(checked_at), checked_at),
  verdict    = IF(verdict IS NULL OR verdict = 'error', VALUES(verdict), verdict)`

// ImportVerified 合并核验结论。
//
// 先读一遍本机已有的结论、算出处置计划,再按计划落库。读一遍的代价是一次批量
// SELECT,换来的是确定的计数与可用的 -dry-run;而写入本身即使不做这个计划也
// 是安全的——SQL 里的判断条件会兜住重复导入。
//
// dryRun 为真时只算不写,返回的统计与真实执行完全一致。
func (s *Store) ImportVerified(ctx context.Context, rows []VerifiedRow, dryRun bool) (ImportStats, error) {
	var stats ImportStats
	if len(rows) == 0 {
		return stats, nil
	}

	addrs := make([]string, len(rows))
	for i, r := range rows {
		addrs[i] = r.ContractAddress
	}
	local, err := s.localCandidateVerdicts(ctx, addrs)
	if err != nil {
		return stats, fmt.Errorf("读取本机候选结论失败: %w", err)
	}

	plan := planVerified(rows, local)
	stats.Applied = int64(len(plan.Apply))
	stats.Skipped = int64(plan.SkippedCount)
	if dryRun {
		return stats, nil
	}

	for start := 0; start < len(plan.Apply); start += syncBatchSize {
		end := start + syncBatchSize
		if end > len(plan.Apply) {
			end = len(plan.Apply)
		}
		chunk := plan.Apply[start:end]

		ph := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*3)
		for _, r := range chunk {
			ph = append(ph, "(?, ?, FROM_UNIXTIME(?))")
			args = append(args, r.ContractAddress, string(r.Verdict), r.CheckedAt)
		}

		query := fmt.Sprintf(verifiedUpsertSQL, strings.Join(ph, ","))
		if _, err := execWithRetry(ctx, s.db, query, args); err != nil {
			return stats, fmt.Errorf("导入第 %d~%d 条结论失败: %w", start+1, end, err)
		}
	}
	return stats, nil
}

// screenInsertIgnoreSQL 只插入本机没有的记录("先写入者优先")。
//
// 用 INSERT IGNORE 而不是覆盖:覆盖会让一份较旧的文件把本机较新的核验结果
// 冲掉,而 checked_at 不会随本机重新核验前进,拿它判新旧并不可靠。既然判不
// 准,就不覆盖——通过在导入前后各跑一次 sync,两边最终会收敛到同一份内容。
const screenInsertIgnoreSQL = "INSERT IGNORE INTO token_screen_metrics (" +
	screenColumns + ", checked_at) VALUES %s"

// ImportResults 合并筛选结果。
//
// 写入前先做两件事:拦掉与本机候选表结论矛盾的行,跳过本机已有的行。两者都
// 先算清楚再落库,所以 -dry-run 能给出与真实执行完全一致的处置计数。
//
// dryRun 为真时只算不写。
func (s *Store) ImportResults(ctx context.Context, recs []model.ScreenRecord, dryRun bool) (ImportStats, error) {
	var stats ImportStats
	if len(recs) == 0 {
		return stats, nil
	}

	addrs := make([]string, len(recs))
	for i, r := range recs {
		addrs[i] = r.ContractAddress
	}

	localVerdict, err := s.localCandidateVerdicts(ctx, addrs)
	if err != nil {
		return stats, fmt.Errorf("读取本机候选结论失败: %w", err)
	}
	existing, err := s.existingScreenAddresses(ctx, addrs)
	if err != nil {
		return stats, fmt.Errorf("读取本机结果记录失败: %w", err)
	}

	plan := planResults(recs, localVerdict, existing)
	stats.Applied = int64(len(plan.Apply))
	stats.Skipped = int64(plan.ExistingCount)
	stats.Blocked = int64(plan.ConflictCount)
	if dryRun {
		return stats, nil
	}

	// checked_at 在表上是 NOT NULL,而 FROM_UNIXTIME 对非法输入返回 NULL,
	// 会直接撞上非空约束报错。手工编辑过的文件里可能缺这个字段,所以在这里
	// 兜底成当前时刻,而不是让整批导入失败。
	fallback := time.Now()

	for start := 0; start < len(plan.Apply); start += syncBatchSize {
		end := start + syncBatchSize
		if end > len(plan.Apply) {
			end = len(plan.Apply)
		}
		chunk := plan.Apply[start:end]

		row := "(" + strings.TrimSuffix(strings.Repeat("?,", screenColumnCount), ",") +
			",FROM_UNIXTIME(?))"
		ph := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*(screenColumnCount+1))
		for _, r := range chunk {
			ph = append(ph, row)
			args = append(args, screenArgs(r)...)
			ts := r.CheckedAt
			if ts.IsZero() {
				ts = fallback
			}
			args = append(args, ts.Unix())
		}

		query := fmt.Sprintf(screenInsertIgnoreSQL, strings.Join(ph, ","))
		if _, err := execWithRetry(ctx, s.db, query, args); err != nil {
			return stats, fmt.Errorf("导入第 %d~%d 条筛选结果失败: %w", start+1, end, err)
		}
	}
	return stats, nil
}
