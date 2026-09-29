// Command sync 在本机 MySQL 与仓库里的文本文件之间搬运筛选数据。
//
// 用途是让**两台不互联的电脑**共用成果:一台导出、提交进 git,另一台拉取后
// 导进自己的库。真正要省的是 GMGN 配额——同一批候选币在两边各问一遍,等于
// 把最稀缺的资源烧两遍(实测 15.8 万个候选逐个核验约需 158,000 个权重单位、
// 十几个小时)。
//
// 两个文件:
//
//	data/verified.tsv   rejected / nodata 这类负结论,用来避免重复核验
//	data/results.jsonl  筛选通过的完整快照,是交付物本体
//
// passed 只出现在 results.jsonl 里。若它在 tsv 里也有一份,就可能出现"候选表
// 说通过、结果表却没有明细"的地址,让两边数据自相矛盾。
//
// # 为什么不直接提交数据库文件
//
// chain_candidates 每轮筛选都会被批量 UPDATE(15 万行的 checked_at/verdict)。
// 把 SQLite 那样的单文件数据库放进 git,等于每次提交一整份几十 MB 的二进制
// blob——二进制几乎无法做 delta 压缩,跑十几轮仓库就上 GB;而且二进制无法
// merge,两台机器同时跑就只能二选一,另一份的成果直接丢掉。文本文件两者都没
// 有:git 会做 delta,冲突是行级的。
//
// # 两台机器的操作约定
//
//	git pull                                  先拉,再跑
//	screen ...                                正常筛选
//	sync -mode export -dir ./data             导出
//	git add data/ && git commit && git push
//
// 约定**同一时间只在一台机器上跑 screen**,并在跑之前先 pull。这样 data/ 不会
// 被两台同时修改,不会冲突。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"stock/internal/config"
	"stock/internal/model"
	"stock/internal/store"
)

// 数据文件名与默认目录。
const (
	verifiedFile = "verified.tsv"
	resultsFile  = "results.jsonl"
	defaultDir   = "data"
)

func main() {
	f := parseFlags()
	if err := run(f); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

// flags 是命令行参数。沿用仓库约定:零值表示"用默认值"。
type flags struct {
	mode string
	dir  string
	// includeNoLP 决定导出时是否带上 nolp 结论。
	//
	// 默认不带:nolp 占候选的八成以上(实测 DexScreener 预筛能拦下八成),
	// 而它在另一台是**免费重建**的——重查 DexScreener 不消耗任何 GMGN 配额,
	// 只是慢十几分钟。带上会让文件白白膨胀五倍。
	includeNoLP bool
	dryRun      bool
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.mode, "mode", "", "export=导出到文件 | import=从文件导入(必填)")
	flag.StringVar(&f.dir, "dir", defaultDir, "数据文件所在目录")
	flag.BoolVar(&f.includeNoLP, "include-nolp", false,
		"导出时保留 nolp(无流动性)结论;默认排除,因为它可免费重建")
	flag.BoolVar(&f.dryRun, "dry-run", false, "只报告会怎么处置,不写入数据库")
	flag.Parse()
	return f
}

func run(f flags) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	mode := strings.ToLower(strings.TrimSpace(f.mode))
	if mode != "export" && mode != "import" {
		return fmt.Errorf("-mode 必须是 export 或 import,当前是 %q", f.mode)
	}

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取工作目录失败: %w", err)
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}

	st, err := store.New(cfg.MySQL.DSN())
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.MigrateDir(ctx, filepath.Join(root, "migrations")); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}

	dir := strings.TrimSpace(f.dir)
	if dir == "" {
		dir = defaultDir
	}

	if mode == "export" {
		return runExport(ctx, st, dir, f)
	}
	return runImport(ctx, st, dir, f)
}

// runExport 把库里的内容写成两个文本文件。
func runExport(ctx context.Context, st *store.Store, dir string, f flags) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}

	rows, err := st.ExportVerified(ctx, f.includeNoLP)
	if err != nil {
		return fmt.Errorf("导出核验结论失败: %w", err)
	}
	if !f.includeNoLP {
		log.Printf("ℹ️  已跳过 nolp 结论(它可在另一台免费重建);需要时加 -include-nolp")
	}
	if err := writeFile(filepath.Join(dir, verifiedFile), store.FormatVerified(rows)); err != nil {
		return err
	}
	log.Printf("📤 %s:%d 条核验结论,已写入 %s", verifiedFile, len(rows), humanSize(len(store.FormatVerified(rows))))

	recs, err := st.ExportResults(ctx)
	if err != nil {
		return fmt.Errorf("导出筛选结果失败: %w", err)
	}
	body, err := store.FormatResults(recs)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, resultsFile), body); err != nil {
		return err
	}
	log.Printf("📤 %s:%d 条筛选结果,已写入 %s", resultsFile, len(recs), humanSize(len(body)))

	log.Printf("✅ 导出完成 —— 把 %s 提交进 git,另一台 pull 后跑 sync -mode import", dir)
	return nil
}

// runImport 把两个文件合并进本机数据库。
//
// 顺序是**先结论后结果**,不能颠倒:结果表要用候选表的结论来判断某个地址该不该
// 写入(本机已判它不合格时,就不该凭空给结果表加一条)。
func runImport(ctx context.Context, st *store.Store, dir string, f flags) error {
	if f.dryRun {
		log.Printf("🔎 预演模式:只统计处置结果,不会写入数据库")
	}

	vPath := filepath.Join(dir, verifiedFile)
	rPath := filepath.Join(dir, resultsFile)

	vRows, vMissing, err := readVerified(vPath)
	if err != nil {
		return err
	}
	rRecs, rMissing, err := readResults(rPath)
	if err != nil {
		return err
	}
	// 两个文件都不在,几乎一定是 -dir 写错了。静默什么都不做是最坏的失败模式
	// (与 store.MigrateDir 对空目录的处理同一个理由):用户会以为同步成功了。
	if vMissing && rMissing {
		return fmt.Errorf("目录 %s 下既没有 %s 也没有 %s——请确认 -dir 指向导出时用的目录",
			dir, verifiedFile, resultsFile)
	}
	if vMissing {
		log.Printf("⚠️  %s 不存在,跳过结论导入", verifiedFile)
	}
	if rMissing {
		log.Printf("⚠️  %s 不存在,跳过结果导入", resultsFile)
	}

	if !vMissing {
		vs, err := st.ImportVerified(ctx, vRows, f.dryRun)
		if err != nil {
			return fmt.Errorf("导入核验结论失败: %w", err)
		}
		log.Printf("📥 %s:%s", verifiedFile, vs)
	}
	if !rMissing {
		rs, err := st.ImportResults(ctx, rRecs, f.dryRun)
		if err != nil {
			return fmt.Errorf("导入筛选结果失败: %w", err)
		}
		if rs.Blocked > 0 {
			// 不是错误:tsv 与 jsonl 是两次独立导出,对方"先导出 tsv、之后才
			// 通过筛选"就会产生这种中间态。拦下它是为了不让本机出现
			// "候选表说 rejected、结果表里却有它"的自相矛盾。
			log.Printf("⚠️  %s 里有 %d 条与本机候选结论冲突(本机已判其不合格),已拦下未写入",
				resultsFile, rs.Blocked)
		}
		log.Printf("📥 %s:%s", resultsFile, rs)
	}

	if f.dryRun {
		log.Printf("✅ 预演完成 —— 去掉 -dry-run 即真正写入")
		return nil
	}
	log.Printf("✅ 导入完成")
	return nil
}

// readVerified 读并解析结论文件。返回值 missing 表示文件不存在。
//
// 文件不存在不算错误(对方可能还没导过),但两个文件都不在时会由调用方报错。
func readVerified(path string) (rows []store.VerifiedRow, missing bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	rows, stats, err := store.ParseVerified(raw)
	if err != nil {
		return nil, false, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	log.Printf("📂 %s:%s", filepath.Base(path), stats)
	return rows, false, nil
}

// readResults 读并解析结果文件。
func readResults(path string) (recs []model.ScreenRecord, missing bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	recs, stats, err := store.ParseResults(raw)
	if err != nil {
		return nil, false, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	log.Printf("📂 %s:%s", filepath.Base(path), stats)
	return recs, false, nil
}

// writeFile 落盘。写不成功必须报错:导出的文件是要提交进 git 的,静默失败会
// 让人提交一份残缺的数据。
func writeFile(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}

// humanSize 把字节数转成便于判断"该不该进 git"的形式。
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
