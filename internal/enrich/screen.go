package enrich

import (
	"fmt"
	"sort"
	"strings"
)

// ScreenCriteria 是筛选条件的阈值。
//
// 与 gmgn.go 里的解析层刻意分开:解析回答"接口说了什么",这里回答"够不够格"。
// 分开的好处是判定逻辑是纯函数,可以在没有 API key 的情况下用 golden 数据完整测试。
type ScreenCriteria struct {
	// MinHolders / MaxHolders 是持币人数区间:**下界含等于(≥ MinHolders),上界不含(< MaxHolders)**,
	// 即 [MinHolders, MaxHolders)。
	//
	// 下界之所以含等于,是因为需求写作"持币人数不少于 N 人";上界写作"低于 N 人"。
	// 改下界语义时别只改这里——PreFilter / TrenchesPreFilter / Evaluate 三处各有一个比较。
	MinHolders int64
	MaxHolders int64

	// MaxSniperRate 是狙击占比上限(百分数,严格小于)。
	MaxSniperRate float64
	// SniperBasis 决定用哪个口径:"count" 或 "amount"。空值按 count 处理。
	SniperBasis string

	// TopN 是参与单个占比判断的持币者个数,通常为 10。
	TopN int
	// MaxHolderRate 是其中**单个**持币者的持仓上限(百分数,含等于)。
	MaxHolderRate float64

	// MaxMarketCap 是市值上限(美元,严格小于)。
	MaxMarketCap float64

	// RequireMigrated 要求代币的联合曲线已完成、已迁移到 DEX(即"已毕业")。
	//
	// 判据是 GMGN 的 launchpad_status==2,而不是链上那份 graduated 标志——
	// 后者对 four.meme 来源几乎从不置位(实测 6/147621),根本不可用。
	RequireMigrated bool

	// StrictLP 决定"一条 LP 都没识别出来"时如何处理。
	// true(推荐)=判为无法确定并淘汰;false=按原始明细照判。
	StrictLP bool
}

// ScreenResult 是对单个代币的判定结论。
type ScreenResult struct {
	// 四条独立结论。即使整体不通过也全部算出,便于事后统计每条条件的淘汰率——
	// 想知道"改成 5% 会多通过多少"时不必再花一次 API 配额。
	PassHolder    bool
	PassSniper    bool
	PassTop10     bool
	PassMarketCap bool
	// PassMigrated 仅在 RequireMigrated 为真时才有意义。
	PassMigrated bool

	// Passed 表示四条全过且判定依据完整。
	Passed bool
	// Inconclusive 表示关键数据缺失导致无法判定。
	//
	// 必须与 Passed=false 区分:前者是"不知道",后者是"明确不合格"。
	// 把前者当后者会让整轮筛选静默产出空结果,是最危险的失败模式。
	Inconclusive bool
	// Reasons 列出未通过的原因,按判定顺序。
	Reasons []string

	// ---- 判定过程中算出的中间量,直接入库 ----
	SniperRate      float64
	Top10Count      int
	Top10RateNoLP   float64
	Top10MaxRate    float64
	LPExcludedCount int
	LPExcludedRate  float64
	// LPBasis 记录是哪些依据剔除了 LP,便于事后统计哪条通道在起作用。
	LPBasis  string
	Coverage float64
}

// ReasonString 把未通过原因拼成一行,供入库与日志使用。
func (r ScreenResult) ReasonString() string {
	if len(r.Reasons) == 0 {
		return ""
	}
	return truncate(strings.Join(r.Reasons, "; "), 240)
}

// PreFilter 用 token info 就能判定的条件做粗筛。
//
// 四个条件里,毕业状态、持币人数、市值都能从 token info 直接读出;只有狙击占比
// 与 Top10 集中度必须看持币明细。而明细那次请求的权重是 info 的五倍,所以先用
// 这一层把绝大多数候选挡掉能省下大头。
//
// 刻意不复用 Evaluate:Evaluate 在缺明细时会判为"无法确定"——那对最终结论是对的,
// 但这里需要的是一个明确的"要不要继续花钱"的答案。两者的判定语义不同。
//
// **字段缺失时一律放行,不否决**:这里只回答"有没有明确的淘汰理由",而"字段没
// 返回"不是淘汰理由。放行还有一层实际收益——第二段会取 holders 文档,而
// mergeReport 能把那边独有的字段补进来,所以第一段缺的字段有可能在第二段被填上。
// 代价是这些币要多花一次配额,换的是不误杀。
//
// 返回的 reason 只用于日志。
func PreFilter(rep *GMGNTokenReport, c ScreenCriteria) (bool, string) {
	if rep == nil {
		return false, "无数据"
	}
	if c.RequireMigrated && rep.hasLaunchpadStatus && rep.LaunchpadStatus < gmgnLaunchpadLive {
		return false, fmt.Sprintf("未毕业(launchpad_status=%d)", rep.LaunchpadStatus)
	}
	// 每个判断都先问"这个字段拿到了吗":拿不到就不构成淘汰理由。
	// 以前直接拿零值比,于是"字段没返回"会被读成"持币 0 人""市值 $0"而被淘汰。
	if rep.hasHolderCount && (rep.HolderCount < c.MinHolders || rep.HolderCount >= c.MaxHolders) {
		return false, fmt.Sprintf("持币人数 %d 不在 [%d, %d) 内",
			rep.HolderCount, c.MinHolders, c.MaxHolders)
	}
	if rep.hasMarketCap && (rep.MarketCap <= 0 || rep.MarketCap >= c.MaxMarketCap) {
		return false, fmt.Sprintf("市值 $%.0f 不低于上限 $%.0f",
			rep.MarketCap, c.MaxMarketCap)
	}
	// 蜜罐是硬性否决:买得进卖不出。
	// 这一项方向相反——security 文档没返回时 IsHoneypot 是 false,等于放行,
	// 与上面的规则一致,不需要额外处理。
	if rep.IsHoneypot {
		return false, "蜜罐(买得进卖不出)"
	}
	return true, ""
}

// TrenchesPreFilter 用战壕列表自带的字段做粗筛。
//
// 目的与 PreFilter 相同:在花掉权重 5 的持币明细之前先把绝大多数候选挡掉。
// 战壕记录里已经带着持币人数、市值、狙击持仓占比、蜜罐标记与毕业状态,
// 所以四个条件里的大部分都能在这里判掉。
//
// 唯一判不了的是条件 3 的"**每个**都不超过上限"——战壕只给前 10 的合计。
// 但合计有单调性可以利用:
//
//	合计 ≤ 上限          ⇒ 每个必然 ≤ 上限,直接放行
//	合计 > N × 上限      ⇒ 平均就超了,必然有一个超,直接否决
//
// 落在中间那段才真的需要去取明细,交给后面的 Evaluate 定论。
//
// minCreatedUnix 是时间窗口下界(Unix 秒),传 0 表示不限。
func TrenchesPreFilter(t TrenchesToken, c ScreenCriteria, minCreatedUnix int64) (bool, string) {
	// 与 PreFilter 同一条规则:**字段缺失放行,不否决**。缺失是"不知道",
	// 不是"不合格",按零值判会把数据缺口变成一条否决结论。
	//
	// 战壕的 completed 分类本身就是"已毕业"的证据,所以下面只做一致性兜底,
	// 不作为主要判据——实测该分类的 launchpad_status 恒为 1。
	if c.RequireMigrated && t.hasLaunchpadStatus && t.LaunchpadStatus < gmgnLaunchpadLive {
		return false, fmt.Sprintf("未毕业(launchpad_status=%d)", t.LaunchpadStatus)
	}
	// 时间窗口只能在客户端过滤:实测接口的 min_created / max_created 不起作用
	if minCreatedUnix > 0 && t.CreatedTimestamp > 0 && t.CreatedTimestamp < minCreatedUnix {
		return false, "超出时间窗口"
	}
	if t.hasHolderCount && (t.HolderCount < c.MinHolders || t.HolderCount >= c.MaxHolders) {
		return false, fmt.Sprintf("持币人数 %d 不在 [%d, %d) 内",
			t.HolderCount, c.MinHolders, c.MaxHolders)
	}
	if t.hasMarketCap && (t.MarketCap <= 0 || t.MarketCap >= c.MaxMarketCap) {
		return false, fmt.Sprintf("市值 $%.0f 不低于上限 $%.0f",
			t.MarketCap, c.MaxMarketCap)
	}
	// 蜜罐是硬性否决:买得进卖不出
	if t.IsHoneypot {
		return false, "蜜罐(买得进卖不出)"
	}
	// 条件 2 先用持仓口径兜一道。数量口径要明细才算得出来,留给 Evaluate。
	if t.SniperHoldRate >= c.MaxSniperRate {
		return false, fmt.Sprintf("狙击持仓 %.2f%% 不低于 %.2f%%",
			t.SniperHoldRate, c.MaxSniperRate)
	}
	// 条件 3 的快速否决:合计已超过 N 倍上限,平均就超了
	if t.Top10HolderRate > float64(c.TopN)*c.MaxHolderRate {
		return false, fmt.Sprintf("Top10 合计 %.2f%% 已超过 %d × %.2f%%",
			t.Top10HolderRate, c.TopN, c.MaxHolderRate)
	}
	return true, ""
}

// Evaluate 按四个条件判定一个代币。
//
// 纯函数:不碰网络也不碰数据库,因此可以在没有 API key 的情况下用 golden 数据
// 完整测试。这是应对"接口结构未实测"的主要防线——判定逻辑若只能在真跑时验证,
// 那么每一次调参都是在花真配额试错。
//
// pairAddr 用于剔除 LP 池地址,可传空(此时仅靠 addr_type 与基础币名单识别)。
func Evaluate(rep *GMGNTokenReport, pairAddr string, c ScreenCriteria) ScreenResult {
	var r ScreenResult

	if rep == nil {
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "无数据")
		return r
	}

	r.Coverage = rep.Coverage
	r.SniperRate = sniperRate(rep, c.SniperBasis)

	// 下面四条判断都遵循同一条规则:**字段缺失判为"无法确定",不判"不合格"**。
	//
	// 直接拿零值去比是错的:那样"接口没返回 holder_count"会变成"持币 0 人,
	// 不在区间内",一条数据缺口被永久固化成一条否决结论。这正是本文件开头
	// 写明要避免的失败模式。缺失统一置 Inconclusive,由调用方决定怎么处置。

	// ---- 毕业状态(可选) ----
	// 放在最前面:它是唯一"看一个字段就知道"的条件。
	switch {
	case !c.RequireMigrated:
		r.PassMigrated = true
	case !rep.hasLaunchpadStatus:
		// launchpad_status 的 0 是合法值("未开盘"),所以缺失必须靠标记识别
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "接口未返回 launchpad_status,无法判断是否已毕业")
	default:
		r.PassMigrated = rep.LaunchpadStatus >= gmgnLaunchpadLive
		if !r.PassMigrated {
			r.Reasons = append(r.Reasons, fmt.Sprintf(
				"未毕业(launchpad_status=%d,需 ≥ %d)", rep.LaunchpadStatus, gmgnLaunchpadLive))
		}
	}

	// ---- 条件 1:持币人数 ----
	switch {
	case !rep.hasHolderCount:
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "接口未返回 holder_count,无法判断持币人数")
	default:
		r.PassHolder = rep.HolderCount >= c.MinHolders && rep.HolderCount < c.MaxHolders
		if !r.PassHolder {
			r.Reasons = append(r.Reasons, fmt.Sprintf("持币人数 %d 不在 [%d, %d) 内",
				rep.HolderCount, c.MinHolders, c.MaxHolders))
		}
	}

	// ---- 条件 4:市值 ----
	// 市值 = 价格 × 供应量,两者缺一就算不出来。算不出来不等于"市值不达标"。
	switch {
	case !rep.hasMarketCap:
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "接口未返回价格或供应量,算不出市值")
	default:
		// 拿到了字段但市值为 0,是明确的"免费的币"之外的异常情形,照判不合格
		r.PassMarketCap = rep.MarketCap > 0 && rep.MarketCap < c.MaxMarketCap
		if !r.PassMarketCap {
			r.Reasons = append(r.Reasons, fmt.Sprintf("市值 $%.0f 不低于上限 $%.0f",
				rep.MarketCap, c.MaxMarketCap))
		}
	}

	// ---- 条件 2:狙击占比 ----
	// count 口径 = sniper_wallets / holder_count,两个字段缺一不可。
	//
	// amount 口径刻意不在这里判缺失:它取的是明细里带 sniper 标签的持仓之和,
	// 而接口本来就不逐条打标(见 SniperAmountRate 的说明),"没有标签"是常态而非
	// 数据缺口,这个值按设计就是**下界**,照判即可。
	if !strings.EqualFold(strings.TrimSpace(c.SniperBasis), "amount") &&
		(!rep.hasSniperWallets || !rep.hasHolderCount) {
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "接口未返回 sniper_wallets 或 holder_count,算不出狙击占比")
	} else {
		r.PassSniper = r.SniperRate < c.MaxSniperRate
		if !r.PassSniper {
			r.Reasons = append(r.Reasons, fmt.Sprintf("狙击占比 %.2f%% 不低于 %.2f%%(%s 口径)",
				r.SniperRate, c.MaxSniperRate, basisName(c.SniperBasis)))
		}
	}

	// ---- 条件 3:Top10 单个持币者占比 ----
	ex := excludeNonWallets(rep.Holders, pairAddr)
	r.LPExcludedCount = ex.count
	r.LPExcludedRate = ex.rate
	r.LPBasis = ex.basis

	// 接口通常已按持仓降序返回,这里显式排序以防万一——判定依赖"前 N 名"这个语义。
	sort.SliceStable(ex.kept, func(i, j int) bool {
		return ex.kept[i].AmountRate > ex.kept[j].AmountRate
	})

	n := c.TopN
	if n <= 0 {
		n = 10
	}
	top := ex.kept
	if len(top) > n {
		top = top[:n]
	}
	r.Top10Count = len(top)
	for _, h := range top {
		r.Top10RateNoLP += h.AmountRate
		if h.AmountRate > r.Top10MaxRate {
			r.Top10MaxRate = h.AmountRate
		}
	}

	switch {
	case len(rep.Holders) == 0:
		// 明细整段缺失,条件 3 无从谈起
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "接口未返回持币明细")
	case ex.count == 0 && c.StrictLP:
		// 一条 LP 都没识别出来。池子地址通常占供应量的 20~40%,若它混在 Top10 里,
		// 这条判定必然误判。宁可漏,不可错。
		r.Inconclusive = true
		r.Reasons = append(r.Reasons, "未识别到 LP 地址,Top10 占比不可信")
	default:
		r.PassTop10 = len(top) > 0 && r.Top10MaxRate <= c.MaxHolderRate
		if !r.PassTop10 {
			r.Reasons = append(r.Reasons, fmt.Sprintf("Top10 中最大单个占比 %.2f%% 超过 %.2f%%",
				r.Top10MaxRate, c.MaxHolderRate))
		}
	}

	// 判定依据不完整时不给出"通过"结论,但保留其它条件的计算结果供参考。
	r.Passed = rep.Complete && !r.Inconclusive && r.PassMigrated &&
		r.PassHolder && r.PassSniper && r.PassTop10 && r.PassMarketCap

	return r
}

// sniperRate 按指定口径取狙击占比。
//
//   - count:狙击地址数 / 持币人数。不受明细覆盖度影响,是默认口径。
//   - amount:明细中 sniper 标的持仓之和。更贴近"占了多少筹码",但接口只返回
//     前 100 条明细,覆盖不足时这个值只是**下界**——真实占比只会更高。
func sniperRate(rep *GMGNTokenReport, basis string) float64 {
	if strings.EqualFold(strings.TrimSpace(basis), "amount") {
		return rep.SniperAmountRate
	}
	return rep.SniperCountRate
}

func basisName(basis string) string {
	if strings.EqualFold(strings.TrimSpace(basis), "amount") {
		return "持仓"
	}
	return "数量"
}

// excluded 是剔除 LP/销毁地址后的结果。
type excluded struct {
	kept  []GMGNHolder
	count int
	rate  float64
	basis string
}

// excludeNonWallets 从持币明细中剔除不是"真人钱包"的条目。
//
// 这是条件 3 的关键:PancakeSwap 池子本身就是一个 holder,通常占供应量的
// 20~40%,不剔除则"Top10 每个不超过 3%"永远不可能通过。
//
// 三路依据按可靠性排序,命中的那一路记进 basis,便于事后统计哪条在起作用:
//
//  1. addr_type —— 接口直接标注的地址类型,最权威
//     (0=普通钱包, 1=销毁地址, 2=交易所/流动性池)
//  2. pair_address —— 调用方传进来的交易对地址,兜底兼交叉校验
//  3. 标签 —— 明细自带的 pool/lp/burn 类标签
func excludeNonWallets(holders []GMGNHolder, pairAddr string) excluded {
	var out excluded
	reasons := map[string]bool{}
	pair := strings.ToLower(strings.TrimSpace(pairAddr))

	for _, h := range holders {
		switch {
		case h.IsPool():
			out.count++
			out.rate += h.AmountRate
			reasons["addr_type_pool"] = true
		case h.IsBurn():
			// 0x...dead 常年霸榜,但它不是任何人的持仓
			out.count++
			out.rate += h.AmountRate
			reasons["addr_type_burn"] = true
		case pair != "" && h.Address == pair:
			out.count++
			out.rate += h.AmountRate
			reasons["pair_address"] = true
		case hasNonWalletTag(h):
			out.count++
			out.rate += h.AmountRate
			reasons["tag"] = true
		default:
			out.kept = append(out.kept, h)
		}
	}
	out.basis = joinReasonKeys(reasons)
	return out
}

// nonWalletTags 是明细标签里表示"不是真人钱包"的词。
var nonWalletTags = map[string]struct{}{
	"pool": {}, "lp": {}, "liquidity": {},
	"dex": {}, "pair": {}, "burn": {},
}

func hasNonWalletTag(h GMGNHolder) bool {
	for _, t := range h.MakerTags {
		if _, ok := nonWalletTags[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	for _, t := range h.Tags {
		if _, ok := nonWalletTags[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// joinReasonKeys 把命中依据拼成稳定的字符串(排序后连接),空集返回 "none"。
func joinReasonKeys(m map[string]bool) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "+")
}
