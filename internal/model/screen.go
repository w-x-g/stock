package model

import "time"

// ScreenRecord 是一条筛选通过的记录,对应 token_screen_metrics 表。
//
// 只存四个条件全部通过的代币——因此没有"未通过原因"这类字段,
// 也没有 pass_* 布尔标记。
//
// 指标字段一律是**百分数 0~100**(不是 0~1 小数):量纲换算在解析层统一完成,
// 从库到报表不再出现第二种口径。
type ScreenRecord struct {
	ContractAddress string
	Name            string
	Symbol          string
	Source          Source
	LaunchedAt      time.Time

	// ---- 原始指标,全部取自同一次 GMGN 快照 ----
	HolderCount int64
	// SniperCount 是接口直接给出的狙击钱包数量(wallet_tags_stat.sniper_wallets)。
	// 它比"从明细里数带 sniper 标签的地址"可靠:接口未必给每条都打标签。
	SniperCount int64
	MarketCap   float64
	Liquidity   float64

	// ---- 发行状态 ----
	// LaunchpadStatus: 0=未开盘, 1=进行中, 2=已迁移到 DEX(即"已毕业")。
	LaunchpadStatus    int
	LaunchpadProgress  float64
	MigrationMarketCap float64

	CreatorAddress     string
	CreatorTokenStatus string

	// ---- 比率 ----
	// SniperRate 是实际用于判定的那个口径,由 SniperRateBasis 指明。
	SniperRate      float64
	SniperRateBasis string
	// SniperCountRate = 狙击钱包数 / 持币人数。
	SniperCountRate float64
	// SniperHoldRate 是接口直接给出的狙击持仓占比(top70_sniper_hold_rate)。
	SniperHoldRate float64
	BundlerRate    float64

	// Top10Rate 含 LP,用于与接口聚合值交叉校验;后两个是剔除后的结果。
	Top10Rate     float64
	Top10RateNoLP float64
	Top10MaxRate  float64
	Top10Count    int

	// LPExcludedCount / LPBasis 记录剔除了什么、依据是哪一条,
	// 便于事后统计哪条通道在起作用。
	LPExcludedCount int
	LPExcludedRate  float64
	LPBasis         string

	// HolderCoverage 是明细覆盖的供应占比(%);明显低于 100 说明
	// 持仓型口径只能算下界。
	HolderCoverage float64

	CreatorRate float64

	// ---- 安全检测 ----
	// IsHoneypot 仅 BSC/Base 可测。蜜罐意味着买得进卖不出,是硬性否决项。
	IsHoneypot   bool
	BuyTax       float64 // 百分数
	SellTax      float64
	IsOpenSource bool
	IsRenounced  bool

	// CriteriaJSON 是本次判定所用阈值的快照。
	CriteriaJSON string
	// HoldersJSON 是剔除前的持币明细,用于日后调整 LP 规则时离线重算。
	HoldersJSON string

	// CheckedAt 是判定时刻,由数据库写入。只在读取时填充。
	CheckedAt time.Time
}
