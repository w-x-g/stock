package chain

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// PancakeSwapV2Factory 是 BSC 主网 PancakeSwap V2 的工厂合约。
//
// 每一个新交易对都由它创建并发出 PairCreated——这是"新币上 DEX"的唯一权威来源。
const PancakeSwapV2Factory = "0xcA143Ce32Fe78f1f7019d7d551a6402fC5350c73"

// PairCreatedTopic 是 keccak256("PairCreated(address,address,address,uint256)")。
//
// 这是 Uniswap V2 系工厂的标准事件签名,PancakeSwap 沿用。值取自实测的
// 链上日志,不是算出来的(项目已不再依赖 go-ethereum,没有 keccak 可用)。
const PairCreatedTopic = "0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9"

// NewPair 是一条新建交易对记录。
//
// 刻意保留**两侧**地址而不在这里挑"哪个是新币":交易对本身不告诉你哪一侧是
// 新币,而计价币的集合是开放的。
//
// 这里踩过一次坑:早先版本硬编码一份基础币名单(WBNB/USDT/...),两侧都不在
// 名单里时取 token0。结果 flap 平台的池子是 新币/wPOPMTx(代币化股票当计价币),
// wPOPMTx 不在名单里,于是它被当成了新币,**真正的新币被丢掉**。
// 现在改为在调用方按"地址出现频率"统一判定计价币——见 FilterQuoteTokens。
type NewPair struct {
	Token0 string
	Token1 string
	// Pair 是交易对合约地址。
	Pair string
	// Block 是建池所在区块。
	Block uint64
	// TxHash 是建池交易哈希。
	TxHash string
}

// DecodePairCreated 从一条日志里解出新建的交易对。
//
// 返回 ok=false 表示这条日志结构不对(不是 PairCreated,或字段残缺)。
func DecodePairCreated(lg Log) (NewPair, bool) {
	if len(lg.Topics) < 3 {
		return NewPair{}, false
	}
	t0, err1 := topicToAddress(lg.Topics[1])
	t1, err2 := topicToAddress(lg.Topics[2])
	if err1 != nil || err2 != nil {
		return NewPair{}, false
	}

	// data 的第一个 32 字节 word 是 pair 地址
	pair, err := firstDataWordToAddress(lg.Data)
	if err != nil {
		return NewPair{}, false
	}

	block, err := ParseHexUint(lg.BlockNumber)
	if err != nil {
		return NewPair{}, false
	}

	return NewPair{Token0: t0, Token1: t1, Pair: pair, Block: block, TxHash: lg.TxHash}, true
}

// QuoteTokenThreshold 是"出现多少次算计价币"的默认阈值。
//
// 计价币(WBNB、USDT、wPOPMTx 之类)会出现在成百上千个池子里,而一个普通
// meme 币最多建几个池。取 50 能干净地把两者分开,同时给热门 meme 留足余量。
const QuoteTokenThreshold = 50

// FilterQuoteTokens 从一批交易对里剔除以计价币为对手方的那一侧,
// 返回"疑似新币"的地址集合。
//
// 判据是**出现频率**,不是硬编码名单:计价币的集合是开放的(每个发射台
// 都可能拿自己的一套代币化股票当计价币),名单永远追不全。
//
// 两侧都很罕见时两个都返回 —— 宁可多问一次 GMGN,不能漏币。
func FilterQuoteTokens(pairs []NewPair, threshold int) []string {
	if threshold <= 0 {
		threshold = QuoteTokenThreshold
	}

	freq := make(map[string]int, len(pairs)*2)
	bump := func(a string) {
		if a != "" {
			freq[a]++
		}
	}
	for _, p := range pairs {
		bump(p.Token0)
		bump(p.Token1)
	}

	seen := make(map[string]struct{}, len(pairs))
	var out []string
	add := func(a string) {
		if a == "" || freq[a] > threshold {
			return
		}
		if _, dup := seen[a]; dup {
			return
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	for _, p := range pairs {
		add(p.Token0)
		add(p.Token1)
	}
	return out
}

// PairIndex 按代币地址索引交易对,便于在建候选时回填 pair 地址。
func PairIndex(pairs []NewPair) map[string]NewPair {
	idx := make(map[string]NewPair, len(pairs))
	for _, p := range pairs {
		if _, ok := idx[p.Token0]; !ok {
			idx[p.Token0] = p
		}
		if _, ok := idx[p.Token1]; !ok {
			idx[p.Token1] = p
		}
	}
	return idx
}

// topicToAddress 从 32 字节的 topic 里取出低 20 字节的地址。
func topicToAddress(topic string) (string, error) {
	h := trimHex(topic)
	if len(h) != 64 {
		return "", fmt.Errorf("topic 长度应为 64,实际 %d", len(h))
	}
	addr := "0x" + strings.ToLower(h[24:])
	if !isHex(addr[2:]) {
		return "", fmt.Errorf("topic 不是合法十六进制")
	}
	return addr, nil
}

// firstDataWordToAddress 取 data 的第一个 32 字节 word 并当作地址解析。
func firstDataWordToAddress(data string) (string, error) {
	h := trimHex(data)
	if len(h) < 64 {
		return "", fmt.Errorf("data 长度不足")
	}
	return topicToAddress("0x" + h[:64])
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// ScanPairsResult 是一次扫描的汇总。
type ScanPairsResult struct {
	Logs  int
	Pairs int
}

// ScanPairs 扫描一段区块区间内的所有 PairCreated 事件。
//
// 区块区间超限时(节点普遍有上限,通常是几千到几万块)会自动对半拆分重试——
// 不同节点、不同负载下这个上限是浮动的,写死一个值不可靠。
func (c *RPC) ScanPairs(ctx context.Context, from, to uint64, out func(NewPair)) (ScanPairsResult, error) {
	var res ScanPairsResult
	err := c.scanRange(ctx, from, to, out, &res, 0)
	return res, err
}

// maxSplitDepth 兜住无限拆分:对半拆 16 次相当于把区间切成 6.5 万份,
// 早已细过任何节点的上限。
const maxSplitDepth = 16

func (c *RPC) scanRange(ctx context.Context, from, to uint64, out func(NewPair), res *ScanPairsResult, depth int) error {
	logs, err := c.GetLogs(ctx, PancakeSwapV2Factory, PairCreatedTopic, from, to)
	if err != nil {
		// 区间过大是常态,对半拆开重试
		if depth < maxSplitDepth && to > from {
			mid := from + (to-from)/2
			if err := c.scanRange(ctx, from, mid, out, res, depth+1); err != nil {
				return err
			}
			return c.scanRange(ctx, mid+1, to, out, res, depth+1)
		}
		return fmt.Errorf("区块 %d-%d 扫描失败: %w", from, to, err)
	}

	res.Logs += len(logs)
	for _, lg := range logs {
		if np, ok := DecodePairCreated(lg); ok {
			res.Pairs++
			if out != nil {
				out(np)
			}
		}
	}
	return nil
}

// BlockByTime 估算某个时间点对应的区块号。
//
// 用"当前区块 + 当前时间 + 平均出块间隔"线性外推,再二分校正一次。
// 目的只是定出扫描起点,不需要精确到单块。
func (c *RPC) BlockByTime(ctx context.Context, targetUnix int64) (uint64, error) {
	head, err := c.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}
	headTime, err := c.BlockTime(ctx, head)
	if err != nil {
		return 0, err
	}

	delta := headTime - targetUnix
	if delta <= 0 {
		return head, nil
	}

	// BSC 出块约 0.45 秒,但这是浮动的,所以先粗算再校正
	const avgBlockSec = 0.45
	est := uint64(float64(delta) / avgBlockSec)
	if est > head {
		est = head
	}
	guess := head - est

	// 校正一次:拿到猜测区块的真实时间,按误差再修
	guessTime, err := c.BlockTime(ctx, guess)
	if err != nil {
		// 校正失败就用粗估值,误差以区块计但扫描起点偏早不影响正确性
		return guess, nil
	}
	drift := guessTime - targetUnix
	if drift > 0 {
		// 猜测点太晚,往前推
		back := uint64(float64(drift) / avgBlockSec)
		if back > guess {
			return 0, nil
		}
		guess -= back
	}
	return guess, nil
}
