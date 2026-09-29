// Package model 定义领域数据结构。
package model

import "time"

// ChainID 是 BNB Chain 主网 id。
const ChainID = 56

// Source 标识代币的发现渠道。
type Source string

const (
	// SourceFourMeme 表示通过 four.meme 发射平台创建。
	SourceFourMeme Source = "four_meme"
	// SourcePancakeV2 表示直接部署合约并新建了 PancakeSwap 交易对。
	SourcePancakeV2 Source = "pancake_v2"
)

// Token 是一个代币的标识信息。
//
// 只保留筛选流程真正用到的字段:身份(地址/名称)、来源、发行时间,
// 以及交易对地址(剔除 LP 时的兜底依据)。行情类指标一律不在这里——
// 它们来自 GMGN 的快照,是分钟级的,不适合塞进领域模型。
type Token struct {
	ChainID         int64
	ContractAddress string
	Name            string
	Symbol          string
	Source          Source
	// PairAddress 是 PancakeSwap 交易对地址,可为空。
	// 战壕接口不返回它,此时剔除 LP 只能靠 addr_type 与标签。
	PairAddress string
	// LaunchedAt 是发行时间。
	LaunchedAt time.Time
}
