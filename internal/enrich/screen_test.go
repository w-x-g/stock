package enrich

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 夹具文件名就是代币合约地址,这样同一批文件既能被测试读取,
// 又能直接喂给 cmd/screen 的 -fixtures 模式。
const (
	addrPass      = "0x1111111111111111111111111111111111111111"
	addrLPTop1    = "0x2222222222222222222222222222222222222222"
	addrLPUnknown = "0x7777777777777777777777777777777777777777"
	addrCamel     = "0x8888888888888888888888888888888888888888"
)

// testCriteria 与 config 的默认阈值保持一致。
func testCriteria() ScreenCriteria {
	return ScreenCriteria{
		MinHolders:      300,
		MaxHolders:      2000,
		MaxSniperRate:   5,
		SniperBasis:     "count",
		TopN:            10,
		MaxHolderRate:   3,
		MaxMarketCap:    1_000_000,
		RequireMigrated: true,
		StrictLP:        true,
	}
}

// loadFixture 读取 testdata 下的响应夹具。
//
// 这些夹具的字段名取自官方 SKILL.md,**但不是实测样本**。
// 首次真跑 -dump-raw 之后必须替换为真实响应——在那之前,测试通过只说明
// 判定逻辑自洽,不说明线上字段名取对了。
func loadFixture(t *testing.T, addr string) []byte {
	t.Helper()
	p := filepath.Join("testdata", addr+".json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", p, err)
	}
	return b
}

// almostEqual 用容差比较浮点。
//
// 比率字段会经过 "0~1 小数 × 100" 的归一化,0.346*100 在 IEEE754 下是
// 34.599999999999994,直接 == 比较必然失败。
func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

func mustParse(t *testing.T, addr string) *GMGNTokenReport {
	t.Helper()
	rep, err := ParseTokenReport(loadFixture(t, addr))
	if err != nil {
		t.Fatalf("解析夹具 %s 失败: %v", addr, err)
	}
	return rep
}

// TestParseMergedDocument 验证三个子文档被正确合并。
//
// 单看任何一个子文档都不足以判定:info 带聚合值、security 带蜜罐、holders 带明细。
func TestParseMergedDocument(t *testing.T) {
	rep := mustParse(t, addrPass)

	if !rep.Complete {
		t.Fatalf("夹具字段应当齐全,缺失: %v", rep.Missing)
	}
	if rep.RateSuspicious {
		t.Errorf("聚合值与明细求和应当一致,却被判为矛盾")
	}

	// 来自 info
	if rep.HolderCount != 500 {
		t.Errorf("holder_count = %d, 期望 500", rep.HolderCount)
	}
	if !almostEqual(rep.MarketCap, 30000) {
		t.Errorf("市值 = %v, 期望 30000 (= 0.00003 × 10^9,两个值在接口里都是字符串)",
			rep.MarketCap)
	}
	if rep.LaunchpadStatus != gmgnLaunchpadLive {
		t.Errorf("launchpad_status = %d, 期望 %d", rep.LaunchpadStatus, gmgnLaunchpadLive)
	}
	if rep.CreatorTokenStatus != "sell" {
		t.Errorf("creator_token_status = %q, 期望 sell", rep.CreatorTokenStatus)
	}
	if rep.SniperWallets != 10 {
		t.Errorf("sniper_wallets = %d, 期望 10", rep.SniperWallets)
	}
	if !almostEqual(rep.SniperCountRate, 2) {
		t.Errorf("狙击占比 = %v%%, 期望 2%% (10/500)", rep.SniperCountRate)
	}
	if !almostEqual(rep.Top10Rate, 9.8) {
		t.Errorf("top10 占比 = %v, 期望 9.8 (\"0.098\" 归一化)", rep.Top10Rate)
	}
	if !almostEqual(rep.SniperHoldRate, 1.2) {
		t.Errorf("狙击持仓占比 = %v, 期望 1.2 (top70_sniper_hold_rate)",
			rep.SniperHoldRate)
	}

	// 来自 security —— 只在这一个子文档里
	if rep.IsHoneypot {
		t.Errorf("is_honeypot 应为 false(接口给的是 JSON 布尔,不是字符串)")
	}
	if !rep.IsOpenSource || !rep.IsRenounced {
		t.Errorf("is_open_source / is_renounced 应被解出: %v / %v",
			rep.IsOpenSource, rep.IsRenounced)
	}

	// 来自 holders
	if len(rep.Holders) != 12 {
		t.Fatalf("持币明细 %d 条, 期望 12", len(rep.Holders))
	}
	if !rep.Holders[0].IsBurn() {
		t.Errorf("首条应为销毁地址(addr_type=1),实际 addr_type=%d", rep.Holders[0].AddrType)
	}
	if !rep.Holders[1].IsPool() {
		t.Errorf("第 2 条应为 LP(addr_type=2),实际 addr_type=%d", rep.Holders[1].AddrType)
	}
	if len(rep.Holders[3].MakerTags) != 1 || rep.Holders[3].MakerTags[0] != "bundler" {
		t.Errorf("第 4 条的 maker_token_tags 未解出: %v", rep.Holders[3].MakerTags)
	}
}

// TestEvaluatePass 是最基础的一条:条件全过。
func TestEvaluatePass(t *testing.T) {
	rep := mustParse(t, addrPass)
	got := Evaluate(rep, "", testCriteria())

	if got.Inconclusive {
		t.Fatalf("不应判为无法确定: %v", got.Reasons)
	}
	if !got.Passed {
		t.Fatalf("应当通过,却未通过: %v", got.Reasons)
	}
	for name, ok := range map[string]bool{
		"holder": got.PassHolder, "sniper": got.PassSniper, "top10": got.PassTop10,
		"mcap": got.PassMarketCap, "migrated": got.PassMigrated,
	} {
		if !ok {
			t.Errorf("条件 %s 应当通过", name)
		}
	}
}

// TestEvaluateLPTop1 是条件 3 的核心回归用例。
//
// LP 池排在第 1 名且占 35%。若不剔除,Top10 里必然有一个远超 3%,
// 这条判定就永远不可能通过——那正是需求里最容易做错的地方。
func TestEvaluateLPTop1(t *testing.T) {
	rep := mustParse(t, addrLPTop1)
	got := Evaluate(rep, "", testCriteria())

	if got.LPExcludedCount != 1 {
		t.Errorf("应剔除 1 个 LP 地址,实际剔除 %d 个", got.LPExcludedCount)
	}
	if got.LPBasis != "addr_type_pool" {
		t.Errorf("剔除依据应为 addr_type_pool,实际为 %q", got.LPBasis)
	}
	// 剔除前最大的单个持仓是 LP 的 35%
	if !almostEqual(rep.Holders[0].AmountRate, 35) {
		t.Fatalf("夹具构造有误:首个持有者应为 LP 且占 35%%,实际 %.2f%%", rep.Holders[0].AmountRate)
	}
	if got.Top10MaxRate > 3 {
		t.Errorf("剔除 LP 后最大单个占比应低于 3%%,实际 %.2f%%", got.Top10MaxRate)
	}
	if !got.Passed {
		t.Fatalf("应当通过,却未通过: %v", got.Reasons)
	}
}

// TestEvaluateStrictLPInconclusive 验证"无法识别 LP"与"明确不合格"被区分开。
//
// 把前者当成后者,会让整轮筛选静默产出空结果并显得一切正常——这是最危险的
// 失败模式,必须被测试钉住。
func TestEvaluateStrictLPInconclusive(t *testing.T) {
	rep := mustParse(t, addrLPUnknown)

	strict := Evaluate(rep, "", testCriteria())
	if !strict.Inconclusive {
		t.Fatalf("明细无 addr_type 且无 pair_address 时,StrictLP=true 应判为无法确定")
	}
	if strict.Passed {
		t.Errorf("无法确定时不应给出通过结论")
	}
	if strict.LPBasis != "none" {
		t.Errorf("未识别到 LP 时依据应为 none,实际 %q", strict.LPBasis)
	}

	// 放宽后按原始明细照判,结论应当明确(而不是无法确定)
	lax := testCriteria()
	lax.StrictLP = false
	if got := Evaluate(rep, "", lax); got.Inconclusive {
		t.Errorf("StrictLP=false 时不应判为无法确定")
	}
}

// TestEvaluatePairAddressFallback 验证没有 addr_type 时,
// 靠库里存的 pair_address 也能正确剔除 LP。
func TestEvaluatePairAddressFallback(t *testing.T) {
	rep := mustParse(t, addrLPTop1)
	// 抹掉 addr_type,模拟接口不提供该字段的情况
	for i := range rep.Holders {
		rep.Holders[i].AddrType = 0
	}

	const pair = "0x9999999999999999999999999999999999999999"
	got := Evaluate(rep, pair, testCriteria())

	if got.LPExcludedCount != 1 || got.LPBasis != "pair_address" {
		t.Fatalf("应通过 pair_address 剔除 LP,实际剔除 %d 个,依据 %q",
			got.LPExcludedCount, got.LPBasis)
	}
	if !got.Passed {
		t.Fatalf("应当通过,却未通过: %v", got.Reasons)
	}
}

// TestEvaluateRequireMigrated 验证"已毕业"这一条真的会否决。
//
// 判据是 launchpad_status==2。链上那份 graduated 标志不可用(对 four.meme
// 实测 6/147621),所以这条只能靠 GMGN。
func TestEvaluateRequireMigrated(t *testing.T) {
	rep := mustParse(t, addrPass)

	// 未毕业:联合曲线还没走完
	notYet := *rep
	notYet.LaunchpadStatus = 0
	got := Evaluate(&notYet, "", testCriteria())
	if got.Passed || got.PassMigrated {
		t.Errorf("launchpad_status=0(还在曲线上)不应通过")
	}

	// 关掉该要求后,同一个币应当能通过——证明否决确实来自这一条
	lax := testCriteria()
	lax.RequireMigrated = false
	if got := Evaluate(&notYet, "", lax); !got.Passed {
		t.Errorf("关闭 RequireMigrated 后应当通过: %v", got.Reasons)
	}
}

// TestEvaluateSingleConditionFailures 逐个破坏单一条件。
//
// 关键在于断言**其余条件仍然通过**——否则说明判定被短路了,
// 那条永远不生效的条件形同虚设(与 abi 测试里"截断数据不得静默出空值"
// 是同一类关注点)。
func TestEvaluateSingleConditionFailures(t *testing.T) {
	cases := []struct {
		name   string
		broken string // 期望失败的那一条
		mutate func(*GMGNTokenReport)
	}{
		{"市值超限", "mcap", func(r *GMGNTokenReport) { r.MarketCap = 2_000_000 }},
		{"市值缺失", "mcap", func(r *GMGNTokenReport) { r.MarketCap = 0 }},
		{"持币人数过低", "holder", func(r *GMGNTokenReport) { r.HolderCount = 50 }},
		{"持币人数过高", "holder", func(r *GMGNTokenReport) { r.HolderCount = 5000 }},
		// 下界是闭区间,所以"刚好差一个"才是不合格的那一侧
		{"持币人数差一个到不了下界", "holder", func(r *GMGNTokenReport) { r.HolderCount = 299 }},
		{"狙击占比超限", "sniper", func(r *GMGNTokenReport) {
			r.SniperWallets = 60
			r.SniperCountRate = 12
		}},
		// 改的是第 3 条而不是第 2 条:第 1 条是销毁地址、第 2 条是 LP,
		// 两者都会被剔除,改了也不会影响判定
		{"Top10 单个超限", "top10", func(r *GMGNTokenReport) { r.Holders[2].AmountRate = 8 }},
		{"未毕业", "migrated", func(r *GMGNTokenReport) { r.LaunchpadStatus = 0 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := mustParse(t, addrPass)
			tc.mutate(rep)

			got := Evaluate(rep, "", testCriteria())
			if got.Passed {
				t.Fatalf("应当不通过,却通过了")
			}
			if got.Inconclusive {
				t.Fatalf("这是明确的判定,不应判为无法确定")
			}
			if len(got.Reasons) == 0 {
				t.Errorf("未通过时必须给出原因")
			}

			checks := map[string]bool{
				"holder":   got.PassHolder,
				"sniper":   got.PassSniper,
				"top10":    got.PassTop10,
				"mcap":     got.PassMarketCap,
				"migrated": got.PassMigrated,
			}
			for field, ok := range checks {
				want := field != tc.broken
				if ok != want {
					t.Errorf("条件 %s 通过=%v,期望 %v——判定可能被短路了", field, ok, want)
				}
			}
		})
	}
}

// TestHolderLowerBoundIsInclusive 钉住持币人数下界的闭区间语义。
//
// 下界写的是"不少于 N 人",所以 N 本身**合格**;上界是"低于 N 人",所以 N 本身不合格。
// 两端语义不对称,而同一个区间在 PreFilter / TrenchesPreFilter / Evaluate 三处各判一次
// ——漏改任何一处都会让粗筛和最终判定对同一个币给出相反的答案。
func TestHolderLowerBoundIsInclusive(t *testing.T) {
	c := testCriteria() // MinHolders=300, MaxHolders=2000

	cases := []struct {
		holders int64
		want    bool
	}{
		{c.MinHolders - 1, false}, // 299:差一个
		{c.MinHolders, true},      // 300:等于下界,含等于
		{c.MaxHolders - 1, true},  // 1999:紧贴上界之内
		{c.MaxHolders, false},     // 2000:等于上界,不含等于
	}

	for _, tc := range cases {
		rep := fullReport()
		rep.HolderCount = tc.holders

		if got := Evaluate(rep, "", c); got.PassHolder != tc.want {
			t.Errorf("Evaluate:%d 人 PassHolder=%v,期望 %v(原因:%s)",
				tc.holders, got.PassHolder, tc.want, got.ReasonString())
		}
		if ok, reason := PreFilter(rep, c); ok != tc.want {
			t.Errorf("PreFilter:%d 人放行=%v,期望 %v(原因:%s)",
				tc.holders, ok, tc.want, reason)
		}
	}
}

// TestEvaluateNilReport 保证缺数据时不会 panic,并给出"无法确定"而非"不合格"。
func TestEvaluateNilReport(t *testing.T) {
	got := Evaluate(nil, "", testCriteria())
	if !got.Inconclusive || got.Passed {
		t.Fatalf("nil 报告应判为无法确定,实际 Inconclusive=%v Passed=%v",
			got.Inconclusive, got.Passed)
	}
}

// TestSniperBasisCountVsAmount 说明为什么 count 是默认口径。
//
// count 口径用接口直接给的 sniper_wallets 数量,不受明细覆盖度影响;
// amount 口径要在明细里找带 sniper 标签的地址求和,而接口未必给每条都打标签——
// 于是它只能算**下界**。
func TestSniperBasisCountVsAmount(t *testing.T) {
	rep := mustParse(t, addrPass)

	byCount := Evaluate(rep, "", testCriteria())
	if byCount.SniperRate != 2 { // 10 / 500
		t.Errorf("count 口径应为 2%%,实际 %.2f%%", byCount.SniperRate)
	}
	if rep.SniperCountRate == 0 {
		t.Error("count 口径应被算出并入库")
	}

	c := testCriteria()
	c.SniperBasis = "amount"
	byAmount := Evaluate(rep, "", c)
	// 夹具里没有任何一条带 sniper 标签,所以 amount 口径为 0——
	// 这正是"接口不逐条打标"时该口径退化成下界的样子
	if byAmount.SniperRate != 0 {
		t.Errorf("明细无 sniper 标签时 amount 口径应为 0,实际 %.2f%%", byAmount.SniperRate)
	}
}

// ---------------------------------------------------------------------------
// 解析层
// ---------------------------------------------------------------------------

// TestParseFieldAliases 断言 snake_case 与 camelCase 两种命名解析结果一致。
//
// 这是别名合并逻辑唯一有效的测试方式:单看某一份夹具,无法发现"只认其中
// 一种写法"这个缺陷。顺带也验证了"带信封"与"裸对象"两种形态。
func TestParseFieldAliases(t *testing.T) {
	snake := mustParse(t, addrPass)
	camel := mustParse(t, addrCamel)

	if snake.HolderCount != camel.HolderCount {
		t.Errorf("holder_count: %d != %d", snake.HolderCount, camel.HolderCount)
	}
	if snake.MarketCap != camel.MarketCap {
		t.Errorf("market_cap: %v != %v", snake.MarketCap, camel.MarketCap)
	}
	if snake.SniperWallets != camel.SniperWallets {
		t.Errorf("sniper_wallets: %d != %d", snake.SniperWallets, camel.SniperWallets)
	}
	if snake.Top10Rate != camel.Top10Rate {
		t.Errorf("top_10_holder_rate: %v != %v", snake.Top10Rate, camel.Top10Rate)
	}
	if snake.BundlerRate != camel.BundlerRate {
		t.Errorf("bundler_rate: %v != %v", snake.BundlerRate, camel.BundlerRate)
	}
	if snake.CreatorTokenStatus != camel.CreatorTokenStatus {
		t.Errorf("creator_token_status: %q != %q", snake.CreatorTokenStatus, camel.CreatorTokenStatus)
	}
	if snake.LaunchpadStatus != camel.LaunchpadStatus {
		t.Errorf("launchpad_status: %d != %d", snake.LaunchpadStatus, camel.LaunchpadStatus)
	}
	if snake.SniperHoldRate != camel.SniperHoldRate {
		t.Errorf("sniper_hold_rate: %v != %v", snake.SniperHoldRate, camel.SniperHoldRate)
	}
	if snake.IsOpenSource != camel.IsOpenSource || snake.IsRenounced != camel.IsRenounced {
		t.Errorf("安全字段不一致: open_source %v/%v, renounced %v/%v",
			snake.IsOpenSource, camel.IsOpenSource, snake.IsRenounced, camel.IsRenounced)
	}
	if !snake.Complete || !camel.Complete {
		t.Errorf("两种命名都应判为完整: snake.missing=%v camel.missing=%v",
			snake.Missing, camel.Missing)
	}

	if len(snake.Holders) != len(camel.Holders) {
		t.Fatalf("明细条数不一致: %d != %d", len(snake.Holders), len(camel.Holders))
	}
	for i := range snake.Holders {
		a, b := snake.Holders[i], camel.Holders[i]
		if a.Address != b.Address {
			t.Errorf("第 %d 条地址: %q != %q", i, a.Address, b.Address)
		}
		if a.AmountRate != b.AmountRate {
			t.Errorf("第 %d 条占比: %v != %v", i, a.AmountRate, b.AmountRate)
		}
		if a.AddrType != b.AddrType {
			t.Errorf("第 %d 条 addr_type: %d != %d", i, a.AddrType, b.AddrType)
		}
	}

	// 地址应被归一化为小写,否则与 pair_address 比较会漏。
	// 夹具里这条地址首尾是大小写混排的,正好验证归一化确实发生。
	if snake.Holders[0].Address != "0x000000000000000000000000000000000000dead" {
		t.Errorf("地址未归一化为小写: %q", snake.Holders[0].Address)
	}
}

// TestParseMissingFieldsReported 验证"字段缺失"与"值为 0"被区分开。
//
// 必须用存在性标记而不是判零值:sniper_wallets 的 0 是合法值
// (确实没有狙击钱包),不能当成"字段没返回"。
func TestParseMissingFieldsReported(t *testing.T) {
	body := []byte(`{
		"info": { "code": 0, "data": {
			"address": "0xabc",
			"holder_count": 500,
			"circulating_supply": 1000000000,
			"price": { "price": 0.00001 },
			"wallet_tags_stat": { "sniper_wallets": 0 },
			"stat": { "top_10_holder_rate": 0.1 }
		} }
	}`)

	rep, err := ParseTokenReport(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// sniper_wallets=0 是明确的值,不该被算成缺失
	if rep.SniperWallets != 0 {
		t.Errorf("sniper_wallets 应为 0,实际 %d", rep.SniperWallets)
	}
	if len(rep.Holders) != 0 {
		t.Errorf("没有明细时 Holders 应为空")
	}
	// 这份响应里确实没给 launchpad_status,所以它和 holders 都该被报出来。
	//
	// launchpad_status 必须计入缺失:它的 0 是合法值("未开盘"),若拿零值当
	// "未毕业",一次数据缺口就会变成一条否决结论(实测撞过同类问题)。
	if rep.Complete {
		t.Errorf("缺明细时不应判为完整")
	}
	want := map[string]bool{"holders": true, "launchpad_status": true}
	if len(rep.Missing) != len(want) {
		t.Fatalf("缺失项应为 %v,实际 %v", want, rep.Missing)
	}
	for _, m := range rep.Missing {
		if !want[m] {
			t.Errorf("不该把 %s 算成缺失", m)
		}
	}
}

// TestParseTolerantScalars 验证数字被写成字符串、以及 null 时的行为。
//
// 各家 API 混用字符串数字是常态,而 Go 原生 float64 遇到 "0.05" 会让整份
// 响应解析失败——这一层必须容错。
func TestParseTolerantScalars(t *testing.T) {
	body := []byte(`{
		"info": { "code": 0, "data": {
			"address": "0xabc",
			"holder_count": "500",
			"circulating_supply": "1000000000",
			"price": { "price": "0.00002" },
			"liquidity": null,
			"launchpad_status": "2",
			"wallet_tags_stat": { "sniper_wallets": 12 },
			"stat": { "top_10_holder_rate": "0.12" }
		} },
		"security": { "code": 0, "data": {
			"is_honeypot": "no",
			"sell_tax": "0.03",
			"is_open_source": "true",
			"is_renounced": 1
		} }
	}`)

	rep, err := ParseTokenReport(body)
	if err != nil {
		t.Fatalf("宽容解析不应失败: %v", err)
	}
	if rep.HolderCount != 500 {
		t.Errorf("字符串整数 parse 失败: %d", rep.HolderCount)
	}
	if rep.MarketCap != 20000 {
		t.Errorf("字符串价格 × 字符串供应量算错: %v", rep.MarketCap)
	}
	if rep.Liquidity != 0 {
		t.Errorf("null 应按 0 处理: %v", rep.Liquidity)
	}
	if rep.LaunchpadStatus != 2 {
		t.Errorf("字符串 launchpad_status parse 失败: %d", rep.LaunchpadStatus)
	}
	if rep.Top10Rate != 12 {
		t.Errorf("0~1 小数应被归一化为百分数: %v", rep.Top10Rate)
	}
	if rep.IsHoneypot {
		t.Errorf(`is_honeypot 为 "no" 时应解析为 false`)
	}
	if !almostEqual(rep.SellTax, 3) {
		t.Errorf("sell_tax 归一化失败: %v, 期望 3", rep.SellTax)
	}
	if !rep.IsOpenSource || !rep.IsRenounced {
		t.Errorf("安全布尔字段未解出: open_source=%v renounced=%v",
			rep.IsOpenSource, rep.IsRenounced)
	}
}

// TestNormalizeRate 锁定量纲换算的边界。
//
// 边界值 1.0 是关键:作为小数它意味着 top10 持有 100%(不可能),
// 作为百分数它是 1%(常见值)。故取"严格小于 1 视为小数"。
func TestNormalizeRate(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{-1, 0},
		{0.05, 5},
		{0.5, 50},
		{0.999, 99.9},
		{1.0, 1},   // 边界:按百分数解释
		{3.5, 3.5}, // 已是百分数
		{100, 100},
	}
	for _, tc := range cases {
		if got := normalizeRate(tc.in); got != tc.want {
			t.Errorf("normalizeRate(%v) = %v, 期望 %v", tc.in, got, tc.want)
		}
	}
}

// TestIsBurnAddress 锁定销毁地址的识别。
//
// 接口只定义了 addr_type 0 与 2,销毁地址落在 0 里,只能认地址。
func TestIsBurnAddress(t *testing.T) {
	burn := []string{
		"0x0000000000000000000000000000000000000000",
		"0x000000000000000000000000000000000000dEaD",
		"0x0000000000000000000000000000000000000001",
	}
	for _, a := range burn {
		if !isBurnAddress(a) {
			t.Errorf("%s 应被识别为销毁地址", a)
		}
	}
	keep := []string{
		"",
		"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"0xf000000000000000000000000000000000000005",
	}
	for _, a := range keep {
		if isBurnAddress(a) {
			t.Errorf("%s 不应被识别为销毁地址", a)
		}
	}
}

// TestParseRejectsGarbage 保证解析失败时**报错而不是返回零值报告**。
//
// 零值报告会在下游表现为"各项指标都是 0、判定不通过",让整轮筛选静默产出
// 空结果并显得一切正常。
func TestParseRejectsGarbage(t *testing.T) {
	for _, body := range []string{
		`not json at all`,
		`<html><body>502 Bad Gateway</body></html>`,
		``,
	} {
		rep, err := ParseTokenReport([]byte(body))
		if err == nil {
			t.Errorf("输入 %q 应当报错,却返回了报告 %+v", body, rep)
		}
		if rep != nil {
			t.Errorf("解析失败时必须返回 nil 报告,实际 %+v", rep)
		}
	}
}

// TestParseEnvelopeErrorCode 验证业务错误码被转成 APIError。
//
// GMGN 的限流会以 200 + code:429 的形式返回,只看 HTTP 状态码会漏掉。
func TestParseEnvelopeErrorCode(t *testing.T) {
	body := []byte(`{"code":429,"error":"RATE_LIMIT_BANNED","msg":"too many","reset_at":1800000000}`)

	rep, err := ParseTokenReport(body)
	if err == nil {
		t.Fatalf("code!=0 应当报错,实际返回 %+v", rep)
	}
	if !IsGMGNQuotaBanned(err) {
		t.Errorf("应被识别为限流,实际: %v", err)
	}
	if got := ResetAt(err); got.Unix() != 1800000000 {
		t.Errorf("reset_at 提取失败: %v", got)
	}
}

// TestMarshalTokenReportRoundTrip 保证 dump 出来的文件能被读回去。
//
// 这是 -dump-raw → -fixtures 闭环的基础:实测一轮落盘的真实响应,
// 必须能直接当夹具用,中间不需要任何手工转换。
func TestMarshalTokenReportRoundTrip(t *testing.T) {
	orig := mustParse(t, addrPass)

	body := MarshalTokenReport(orig)
	if len(body) == 0 {
		t.Fatal("序列化结果为空")
	}

	back, err := ParseTokenReport(body)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.HolderCount != orig.HolderCount ||
		back.MarketCap != orig.MarketCap ||
		back.SniperWallets != orig.SniperWallets ||
		back.IsHoneypot != orig.IsHoneypot ||
		len(back.Holders) != len(orig.Holders) {
		t.Errorf("回读结果与原始不一致:\n原始 %+v\n回读 %+v", orig, back)
	}
	if !back.Complete {
		t.Errorf("回读后应仍判为字段齐全,缺失: %v", back.Missing)
	}
}

// ---------------------------------------------------------------------------
// 字段缺失:不得成为淘汰依据
// ---------------------------------------------------------------------------
//
// 这一组用例锁定的是本仓库明确写下的不变式:
//
//	Inconclusive(不知道)必须与 Passed=false(明确不合格)区分开,
//	把前者当后者会让整轮筛选静默产出错误的否决结论。
//
// 曾经的写法是直接拿零值去比,于是"接口没返回 holder_count"被读成"持币 0 人,
// 不在区间内"——一条数据缺口固化成一条否决结论。

// fullReport 造一份字段齐全、判定应当通过的报告,供缺失用例做对照。
func fullReport() *GMGNTokenReport {
	rep := &GMGNTokenReport{
		HolderCount:     500,
		MarketCap:       20000,
		SniperWallets:   10,
		LaunchpadStatus: 1,
		Holders: []GMGNHolder{
			{Address: "0xpool", AmountRate: 30, AddrType: 2}, // LP,判定时会被剔除
			{Address: "0xh1", AmountRate: 1.5},
			{Address: "0xh2", AmountRate: 1.0},
		},
	}
	rep.hasHolderCount = true
	rep.hasMarketCap = true
	rep.hasSniperWallets = true
	rep.hasTop10Rate = true
	rep.hasLaunchpadStatus = true
	rep.finish()
	return rep
}

// TestFullReportPasses 是对照组:先证明这份报告本身能通过,下面清字段才有意义。
func TestFullReportPasses(t *testing.T) {
	rep := fullReport()
	if !rep.Complete {
		t.Fatalf("对照报告应当是字段齐全的,缺失: %v", rep.Missing)
	}
	res := Evaluate(rep, "", testCriteria())
	if !res.Passed {
		t.Fatalf("对照报告应当通过,实际:%s", res.ReasonString())
	}
}

// TestEvaluateMissingFieldsIsInconclusive 逐个清掉判定所需的字段,
// 确认结果是"无法确定"而不是"不合格"。
func TestEvaluateMissingFieldsIsInconclusive(t *testing.T) {
	cases := []struct {
		name  string
		clear func(*GMGNTokenReport)
	}{
		{"缺 holder_count", func(r *GMGNTokenReport) { r.hasHolderCount = false; r.HolderCount = 0 }},
		{"缺 market_cap", func(r *GMGNTokenReport) { r.hasMarketCap = false; r.MarketCap = 0 }},
		{"缺 sniper_wallets", func(r *GMGNTokenReport) { r.hasSniperWallets = false; r.SniperWallets = 0 }},
		{"缺 launchpad_status", func(r *GMGNTokenReport) { r.hasLaunchpadStatus = false; r.LaunchpadStatus = 0 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := fullReport()
			tc.clear(rep)
			rep.finish()

			res := Evaluate(rep, "", testCriteria())
			if res.Passed {
				t.Fatal("字段缺失时不该判为通过")
			}
			if !res.Inconclusive {
				t.Errorf("字段缺失必须判为 Inconclusive(不知道),不能判成明确不合格;原因:%s",
					res.ReasonString())
			}
		})
	}
}

// TestPreFilterLetsMissingFieldsThrough 确认粗筛在字段缺失时**放行**而不是否决。
//
// 放行还有一层实际收益:第二段会取 holders 文档,而 mergeReport 能把那边独有的
// 字段补进来,所以第一段缺的字段有可能在第二段被填上。
func TestPreFilterLetsMissingFieldsThrough(t *testing.T) {
	cases := []struct {
		name  string
		clear func(*GMGNTokenReport)
	}{
		{"缺 holder_count", func(r *GMGNTokenReport) { r.hasHolderCount = false; r.HolderCount = 0 }},
		{"缺 market_cap", func(r *GMGNTokenReport) { r.hasMarketCap = false; r.MarketCap = 0 }},
		{"缺 launchpad_status", func(r *GMGNTokenReport) { r.hasLaunchpadStatus = false; r.LaunchpadStatus = 0 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := fullReport()
			tc.clear(rep)

			ok, reason := PreFilter(rep, testCriteria())
			if !ok {
				t.Errorf("字段缺失时应当放行,不该淘汰(原因:%s)", reason)
			}
		})
	}
}

// TestPreFilterStillRejectsRealFailures 是对照组:放行只针对**字段缺失**,
// 真正的"取到了值但不达标"必须照旧淘汰,否则粗筛就白做了。
func TestPreFilterStillRejectsRealFailures(t *testing.T) {
	cases := []struct {
		name string
		set  func(*GMGNTokenReport)
	}{
		{"持币人数超上限", func(r *GMGNTokenReport) { r.HolderCount = 5000 }},
		{"持币人数低于下限", func(r *GMGNTokenReport) { r.HolderCount = 50 }},
		{"市值超上限", func(r *GMGNTokenReport) { r.MarketCap = 2_000_000 }},
		{"未毕业", func(r *GMGNTokenReport) { r.LaunchpadStatus = 0 }},
		{"蜜罐", func(r *GMGNTokenReport) { r.IsHoneypot = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := fullReport()
			tc.set(rep)

			if ok, reason := PreFilter(rep, testCriteria()); ok {
				t.Errorf("这是明确的淘汰理由,不该放行(市值/持币等:%+v)", repr(rep))
				_ = reason
			}
		})
	}
}

// repr 只是给失败信息一点上下文,避免打印整个报告。
func repr(r *GMGNTokenReport) string {
	return fmt.Sprintf("持币 %d 市值 %.0f 状态 %d 蜜罐 %v",
		r.HolderCount, r.MarketCap, r.LaunchpadStatus, r.IsHoneypot)
}
