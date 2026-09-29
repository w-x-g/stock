package store

import (
	"strings"
	"testing"
	"time"

	"stock/internal/model"
)

// 本文件测的是同步功能的**纯函数部分**:编解码与合并计划。
// 它们不碰数据库、不碰网络,所以可以完整离线验证——这一点很重要,因为
// 合并规则一旦写错,代价是往用户库里写脏数据。

// ---------------------------------------------------------------------------
// verified.tsv
// ---------------------------------------------------------------------------

func TestFormatVerifiedSortsAndRoundTrips(t *testing.T) {
	rows := []VerifiedRow{
		{ContractAddress: "0xcccccccccccccccccccccccccccccccccccccccc", Verdict: VerdictNoData, CheckedAt: 1758856800},
		{ContractAddress: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Verdict: VerdictRejected, CheckedAt: 1758856000},
		{ContractAddress: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Verdict: VerdictRejected, CheckedAt: 1758856400},
	}

	body := FormatVerified(rows)

	// 必须按地址升序:否则每跑一次导出的行序都在变,git 会产生毫无意义的全量 diff
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "#") {
		t.Fatalf("首行应当是注释,实际 %q", lines[0])
	}
	for i, want := range []string{
		"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"0xcccccccccccccccccccccccccccccccccccccccc",
	} {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Errorf("第 %d 行应以 %s 开头,实际 %q", i+1, want, lines[i+1])
		}
	}

	got, stats, err := ParseVerified(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if stats.Kept != 3 || stats.Malformed != 0 || stats.BadAddress != 0 || stats.BadVerdict != 0 {
		t.Fatalf("统计不符: %s", stats)
	}
	if len(got) != len(rows) {
		t.Fatalf("往返后条数 %d,期望 %d", len(got), len(rows))
	}
	// 按地址查而不是按下标比:上面喂进去的输入是无序的,而导出会排序,
	// 下标天然对不上(排序本身已由上面的行序断言覆盖)。
	byAddr := make(map[string]VerifiedRow, len(got))
	for _, r := range got {
		byAddr[r.ContractAddress] = r
	}
	for _, want := range rows {
		gotRow, ok := byAddr[want.ContractAddress]
		if !ok {
			t.Errorf("往返后缺少 %s", want.ContractAddress)
			continue
		}
		if gotRow != want {
			t.Errorf("%s 往返不一致:\n得到 %+v\n期望 %+v", want.ContractAddress, gotRow, want)
		}
	}
}

// TestParseVerifiedToleratesWindowsLineEndings 覆盖 git 在 Windows 上的默认行为。
//
// core.autocrlf=true 时检出会把 LF 换成 CRLF,不处理的话结论会解析成
// "rejected\r"——既通不过白名单,又看不出哪里错了。
func TestParseVerifiedToleratesWindowsLineEndings(t *testing.T) {
	body := []byte("# 注释\r\n0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\trejected\t1758856000\r\n")

	rows, stats, err := ParseVerified(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应当解析出 1 条,实际 %d 条(统计:%s)", len(rows), stats)
	}
	if rows[0].Verdict != VerdictRejected {
		t.Errorf("结论应为 rejected,实际 %q(CRLF 没被剥掉?)", rows[0].Verdict)
	}
	if rows[0].CheckedAt != 1758856000 {
		t.Errorf("时间戳解析错误: %d", rows[0].CheckedAt)
	}
}

// TestParseVerifiedStripsBOM 覆盖带 BOM 的文件。
//
// BOM 不剥掉的话首行地址会变成"BOM+0x…",唯一键查不中,于是凭空插入一行。
func TestParseVerifiedStripsBOM(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tnodata\t1758856000\n")...)

	rows, stats, err := ParseVerified(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 || stats.BadAddress != 0 {
		t.Fatalf("BOM 应当被剥掉,实际 %d 条 / 地址非法 %d(统计:%s)",
			len(rows), stats.BadAddress, stats)
	}
	if rows[0].ContractAddress != "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("地址不正确: %q", rows[0].ContractAddress)
	}
}

func TestParseVerifiedSkipsBadLines(t *testing.T) {
	body := []byte(strings.Join([]string{
		"# 注释行",
		"",
		"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\trejected\t1758856000", // 好
		"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\trejected",             // 列数不足
		"0xZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ\trejected\t1758856000", // 地址非法
		"0xcccccccccccccccccccccccccccccccccccccccc\tpassed\t1758856000",   // passed 不该走 tsv
		"0xdddddddddddddddddddddddddddddddddddddddd\trejected\tnotanumber", // 时间戳非法
		"0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\tnonsense\t1758856000", // 未知结论
		"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\trejected\t1758856000", // 重复
	}, "\n") + "\n")

	rows, stats, err := ParseVerified(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应当只采纳 1 条,实际 %d 条(统计:%s)", len(rows), stats)
	}
	if stats.Malformed != 2 {
		t.Errorf("格式错应为 2(列数不足 + 时间戳非法),实际 %d", stats.Malformed)
	}
	if stats.BadAddress != 1 {
		t.Errorf("地址非法应为 1,实际 %d", stats.BadAddress)
	}
	// passed 与 nonsense 都该被白名单拦下:前者说明它错文件了,后者是拼写问题
	if stats.BadVerdict != 2 {
		t.Errorf("结论非法应为 2,实际 %d", stats.BadVerdict)
	}
	if stats.Duplicates != 1 {
		t.Errorf("重复应为 1,实际 %d", stats.Duplicates)
	}
}

// TestParseVerifiedNormalizesAddressCase 保证大小写不会在库里留下两种写法。
//
// utf8mb4 的默认排序规则大小写不敏感,`0xAB` 与 `0xab` 在唯一键上会撞车,
// 存储形态却保留原样。
func TestParseVerifiedNormalizesAddressCase(t *testing.T) {
	body := []byte("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\trejected\t1758856000\n")

	rows, _, err := ParseVerified(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应当解析出 1 条,实际 %d 条", len(rows))
	}
	if rows[0].ContractAddress != "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("地址应当被归一为小写,实际 %q", rows[0].ContractAddress)
	}
}

// ---------------------------------------------------------------------------
// results.jsonl
// ---------------------------------------------------------------------------

func TestFormatResultsRoundTrips(t *testing.T) {
	// 刻意用一个非 UTC 时区构造:导出必须归一成 UTC,否则两台时区不同的机器
	// 会把同一个时刻序列化成不同字符串,文件永远无法收敛。
	zone := time.FixedZone("CST", 8*3600)
	launched := time.Date(2026, 9, 26, 20, 0, 0, 0, zone)
	checked := time.Date(2026, 9, 27, 4, 30, 0, 0, zone)

	recs := []model.ScreenRecord{
		{
			ContractAddress: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Name:            "B",
			Symbol:          "BBB",
			Source:          model.SourceFourMeme,
			LaunchedAt:      launched,
			CheckedAt:       checked,
			HolderCount:     500,
			MarketCap:       12345.67,
			SniperRate:      1.25,
			Top10MaxRate:    0.5,
			IsHoneypot:      false,
			IsOpenSource:    true,
			CriteriaJSON:    `{"MaxHolders":2000}`,
			HoldersJSON:     `[{"Address":"0xaaa"}]`,
		},
		{
			ContractAddress: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Name:            "A",
			LaunchedAt:      launched,
			CheckedAt:       checked,
			HolderCount:     300,
		},
	}

	body, err := FormatResults(recs)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(body), "Z\"") {
		t.Errorf("时间应当序列化成 UTC(以 Z 结尾),实际:%s", body)
	}

	got, stats, err := ParseResults(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if stats.Kept != 2 || stats.Malformed != 0 {
		t.Fatalf("统计不符: %s", stats)
	}
	// 解析结果按地址升序
	if got[0].ContractAddress != "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("应当按地址升序,首条是 %s", got[0].ContractAddress)
	}

	// 逐字段核对:往返必须无损,否则另一台会拿到不完整的结果
	b := got[1]
	want := recs[0]
	if b.ContractAddress != want.ContractAddress || b.Name != want.Name ||
		b.Symbol != want.Symbol || b.Source != want.Source ||
		b.HolderCount != want.HolderCount || b.MarketCap != want.MarketCap ||
		b.SniperRate != want.SniperRate || b.Top10MaxRate != want.Top10MaxRate ||
		b.IsOpenSource != want.IsOpenSource ||
		b.CriteriaJSON != want.CriteriaJSON || b.HoldersJSON != want.HoldersJSON {
		t.Errorf("往返丢字段:\n得到 %+v\n期望 %+v", b, want)
	}
	// 时刻本身必须一致(时区表示不同不算差异)
	if !b.LaunchedAt.Equal(want.LaunchedAt) {
		t.Errorf("发行时间不一致: 得到 %v 期望 %v", b.LaunchedAt, want.LaunchedAt)
	}
	if !b.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("判定时间不一致: 得到 %v 期望 %v", b.CheckedAt, want.CheckedAt)
	}
}

func TestParseResultsSkipsBadLines(t *testing.T) {
	body := []byte(strings.Join([]string{
		`{"ContractAddress":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Name":"A"}`,
		`{"ContractAddress":"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Name":"B"`,
		`{"ContractAddress":"not-an-address","Name":"C"}`,
		`{"ContractAddress":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Name":"A2"}`,
		``,
	}, "\n"))

	recs, stats, err := ParseResults(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("应当只采纳 1 条,实际 %d 条(统计:%s)", len(recs), stats)
	}
	if stats.Malformed != 1 {
		t.Errorf("格式错应为 1(截断的 JSON),实际 %d", stats.Malformed)
	}
	if stats.BadAddress != 1 {
		t.Errorf("地址非法应为 1,实际 %d", stats.BadAddress)
	}
	if stats.Duplicates != 1 {
		t.Errorf("重复应为 1,实际 %d", stats.Duplicates)
	}
}

// ---------------------------------------------------------------------------
// 合并计划
// ---------------------------------------------------------------------------

const (
	addrA = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	addrB = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	addrC = "0xcccccccccccccccccccccccccccccccccccccccc"
)

func TestPlanVerified(t *testing.T) {
	rows := []VerifiedRow{
		{ContractAddress: addrA, Verdict: VerdictRejected, CheckedAt: 1},
		{ContractAddress: addrB, Verdict: VerdictRejected, CheckedAt: 2},
		{ContractAddress: addrC, Verdict: VerdictNoData, CheckedAt: 3},
	}
	local := map[string]CandidateVerdict{
		// addrA 本机没有 → 该填
		addrB: "",            // 本机有但还没结论 → 该填
		addrC: VerdictPassed, // 本机已有明确结论 → 保留本机,不能覆盖
	}

	plan := planVerified(rows, local)

	if len(plan.Apply) != 2 {
		t.Fatalf("应当有 2 条待写入,实际 %d 条", len(plan.Apply))
	}
	if plan.SkippedCount != 1 {
		t.Errorf("应当跳过 1 条,实际 %d", plan.SkippedCount)
	}
	for _, r := range plan.Apply {
		if r.ContractAddress == addrC {
			t.Errorf("%s 本机已有结论,不该被覆盖", addrC)
		}
	}
}

// TestPlanVerifiedFillsErrorRows 确认本机的"调用失败"标记会被对方的结论填补。
//
// error 不是结论,只是"这次没问成,下轮重试"。用对方的结论填上能省掉一次
// 重试,正是这个功能要省的东西。
func TestPlanVerifiedFillsErrorRows(t *testing.T) {
	rows := []VerifiedRow{{ContractAddress: addrA, Verdict: VerdictRejected, CheckedAt: 1}}
	local := map[string]CandidateVerdict{addrA: VerdictError}

	plan := planVerified(rows, local)

	if len(plan.Apply) != 1 {
		t.Fatalf("error 行应当被填补,实际待写入 %d 条", len(plan.Apply))
	}
}

// TestPlanVerifiedSelfImportIsNoop 是"export → import 应当幂等"的纯函数证明。
//
// 同一条数据导出再导回来,本机状态与文件内容一致,所以没有任何一行需要改动。
// 这条不成立的话,每次同步都会往 git 里塞一份伪 diff。
func TestPlanVerifiedSelfImportIsNoop(t *testing.T) {
	rows := []VerifiedRow{
		{ContractAddress: addrA, Verdict: VerdictRejected, CheckedAt: 1},
		{ContractAddress: addrB, Verdict: VerdictNoData, CheckedAt: 2},
	}
	local := map[string]CandidateVerdict{
		addrA: VerdictRejected,
		addrB: VerdictNoData,
	}

	plan := planVerified(rows, local)

	if len(plan.Apply) != 0 {
		t.Errorf("自导入应当无改动,实际待写入 %d 条", len(plan.Apply))
	}
	if plan.SkippedCount != 2 {
		t.Errorf("应当全部跳过,实际跳过 %d 条", plan.SkippedCount)
	}
}

func TestPlanResultsBlocksConflicts(t *testing.T) {
	recs := []model.ScreenRecord{
		{ContractAddress: addrA},
		{ContractAddress: addrB},
		{ContractAddress: addrC},
	}
	local := map[string]CandidateVerdict{
		// addrA 本机没有 → 该写入
		addrB: VerdictRejected, // 本机已判不合格 → 必须拦下
		addrC: VerdictPassed,   // 本机也判通过 → 该写入
	}
	existing := map[string]struct{}{}

	plan := planResults(recs, local, existing)

	if len(plan.Apply) != 2 {
		t.Fatalf("应当有 2 条待写入,实际 %d 条", len(plan.Apply))
	}
	if plan.ConflictCount != 1 {
		t.Errorf("应当拦下 1 条冲突,实际 %d", plan.ConflictCount)
	}
	for _, r := range plan.Apply {
		if r.ContractAddress == addrB {
			t.Errorf("%s 本机候选结论是 rejected,不该写进结果表——"+
				"否则本机的候选表与结果表会自相矛盾", addrB)
		}
	}
}

// TestPlanResultsKeepsExisting 确认"先写入者优先":本机已有记录时不覆盖。
func TestPlanResultsKeepsExisting(t *testing.T) {
	recs := []model.ScreenRecord{{ContractAddress: addrA}}
	existing := map[string]struct{}{addrA: {}}

	plan := planResults(recs, nil, existing)

	if len(plan.Apply) != 0 {
		t.Errorf("本机已有该记录,不该覆盖,实际待写入 %d 条", len(plan.Apply))
	}
	if plan.ExistingCount != 1 {
		t.Errorf("应当计 1 条已存在,实际 %d", plan.ExistingCount)
	}
}

// TestPlanResultsSelfImportIsNoop 同样验证幂等:全部已存在时无改动。
func TestPlanResultsSelfImportIsNoop(t *testing.T) {
	recs := []model.ScreenRecord{
		{ContractAddress: addrA},
		{ContractAddress: addrB},
	}
	existing := map[string]struct{}{addrA: {}, addrB: {}}

	plan := planResults(recs, nil, existing)

	if len(plan.Apply) != 0 || plan.ConflictCount != 0 {
		t.Errorf("自导入应当无改动,实际待写入 %d 条 / 拦下 %d 条",
			len(plan.Apply), plan.ConflictCount)
	}
}

// ---------------------------------------------------------------------------
// 列对齐
// ---------------------------------------------------------------------------

// TestExportScreenColumnsArity 保证导出列清单与写入列清单同步。
//
// 导出比写入多一列 checked_at(它不在 screenColumns 里,由 DB 维护,但必须
// 带出去才能原样写回)。两边一旦脱节,导入时的占位符个数就会与参数个数对不上,
// 报错却是驱动的"Column count doesn't match value count"——离真正的原因很远。
func TestExportScreenColumnsArity(t *testing.T) {
	got := len(splitColumns(exportScreenColumns))
	if want := screenColumnCount + 1; got != want {
		t.Fatalf("导出列数 %d,期望 %d(screenColumns %d + checked_at)",
			got, want, screenColumnCount)
	}
	if !strings.HasSuffix(exportScreenColumns, "UNIX_TIMESTAMP(checked_at)") {
		t.Errorf("导出列清单末尾应当是 UNIX_TIMESTAMP(checked_at),实际 %q", exportScreenColumns)
	}
}
