package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// DexScreener 是免费行情接口,这里只用它做一件事:
// 在消耗 GMGN 配额之前,把"链上建了池子、实际上没有流动性"的币淘汰掉。
//
// 为什么值得为此单独接一个数据源(实测 2026-09-26,已毕业 20 个 / 未毕业 28 个样本):
//
//	                在 DexScreener 有交易对      流动性
//	已毕业           20/20 (100%)               最低 3.48,其余 ≥ 4718
//	未毕业           10/28 (36%)                0 ~ 0.5 的有 5 个(空池)
//
// 未毕业的 28 个里 18 个**完全查不到交易对**、5 个流动性是 0~0.5 美元,
// 合计 82% 不必问 GMGN 就能判掉;而已毕业的一个都不会被误杀。
// GMGN 每次调用至少 1 个权重,8.5 万个候选按这个比例能省掉八成。
//
// 它**取代不了** GMGN:launchpad_status 只有 GMGN 有,所以这一步是减负不是替代。
const (
	dexScreenerDefaultBaseURL = "https://api.dexscreener.com"

	// DexScreenerMaxBatch 是单次批量查询的地址数上限(官方文档值)。
	// 批量是这个方案能成立的前提:逐个查要 8.5 万次请求,批量只要 2800 次。
	DexScreenerMaxBatch = 30

	dexScreenerTimeout = 30 * time.Second
)

// DexScreener 是行情接口客户端。
//
// 节流与 GMGN 同款(原子 CAS 推后 nextAllowed,全仓库零 Mutex 的约定),
// 但间隔可以小得多:这个接口免费且宽松,官方给的是每分钟 300 次。
type DexScreener struct {
	baseURL     string
	batch       int
	client      *http.Client
	minInterval time.Duration
	nextAllowed atomic.Int64
}

// NewDexScreener 构造客户端。baseURL 为空时用官方地址,batch 超限时收敛到上限。
func NewDexScreener(baseURL string, batch, minIntervalMS int) *DexScreener {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = dexScreenerDefaultBaseURL
	}
	if batch <= 0 || batch > DexScreenerMaxBatch {
		batch = DexScreenerMaxBatch
	}
	if minIntervalMS <= 0 {
		minIntervalMS = 250
	}
	return &DexScreener{
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		batch:       batch,
		client:      &http.Client{Timeout: dexScreenerTimeout},
		minInterval: time.Duration(minIntervalMS) * time.Millisecond,
	}
}

// Batch 返回单批地址数,供调用方切分候选。
func (d *DexScreener) Batch() int { return d.batch }

// DexPair 是一个交易对,只保留判定需要的字段。
type DexPair struct {
	ChainID      string
	PairAddress  string
	BaseAddress  string
	QuoteAddress string
	LiquidityUSD float64
	MarketCap    float64
	PriceUSD     float64
}

// Tokens 批量查询若干代币地址,返回按地址索引的"最佳交易对"。
//
// 同一个币可能有多个池子(不同 DEX、不同计价币),取**流动性最大**的那个——
// 只看"有没有池子"会被一个空池骗过去,流动性才代表真实交易深度。
//
// 地址在返回里不存在,表示 DexScreener 完全不认识它,调用方据此淘汰。
func (d *DexScreener) Tokens(ctx context.Context, addrs []string) (map[string]DexPair, error) {
	out := make(map[string]DexPair, len(addrs))
	if len(addrs) == 0 {
		return out, nil
	}

	// 响应里每个 pair 同时带 baseToken 和 quoteToken,只有出现在请求集合里的
	// 那个才是我们要的币——否则会把"以它为计价币"的别人的池子算到自己头上。
	want := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		want[strings.ToLower(a)] = struct{}{}
	}

	for start := 0; start < len(addrs); start += d.batch {
		end := start + d.batch
		if end > len(addrs) {
			end = len(addrs)
		}
		pairs, err := d.fetch(ctx, addrs[start:end])
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			// 两侧都要看:一个币既可能是池子的 base 侧,也可能是 quote 侧。
			//
			// 这里踩过一次坑(2026-09-26):原先只认 baseToken,结果 flap 平台
			// 那种 "新币/wPOPMTx" 的池子——新币在 **quote** 侧——被整体判成
			// "查不到交易对"而淘汰。实测漏掉了 0x9803b9e4...7777(`无用`,
			// launchpad_status=1、持币 416,四项全合格)。
			//
			// 与链上"选边"踩的是同一个坑:wPOPMTx 这类代币化股票永远当计价币,
			// 所以新币只能出现在 quote 侧。凡是"按某一侧认币"的写法都会漏。
			for _, addr := range [2]string{p.BaseAddress, p.QuoteAddress} {
				key := strings.ToLower(addr)
				if _, ok := want[key]; !ok {
					continue
				}
				if cur, exists := out[key]; !exists || p.LiquidityUSD > cur.LiquidityUSD {
					out[key] = p
				}
			}
		}
	}
	return out, nil
}

// fetch 查一批地址。调用方负责保证 len(addrs) <= d.batch。
func (d *DexScreener) fetch(ctx context.Context, addrs []string) ([]DexPair, error) {
	if wait := d.reserve(); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	fullURL := d.baseURL + "/latest/dex/tokens/" + strings.Join(addrs, ",")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 dexscreener 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dexscreener 返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	// 注意:查不到任何交易对时 pairs 是**空数组**且 HTTP 200,不是错误。
	// 这正是我们要的信号,不能当失败处理。
	var parsed dexTokensResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w: %s", err, truncate(string(body), 200))
	}

	out := make([]DexPair, 0, len(parsed.Pairs))
	for _, p := range parsed.Pairs {
		out = append(out, DexPair{
			ChainID:      p.ChainID,
			PairAddress:  p.PairAddress,
			BaseAddress:  p.BaseToken.Address,
			QuoteAddress: p.QuoteToken.Address,
			LiquidityUSD: p.Liquidity.USD,
			MarketCap:    p.MarketCap,
			PriceUSD:     parsePriceUSD(p.PriceUSD),
		})
	}
	return out, nil
}

// reserve 原子地把下一次允许请求的时刻推后一个间隔,返回需要等待的时长。
//
// 与 GMGN.reserve 同一套写法:全局 CAS 而不是各 worker 各睡一会——
// 后者在并发下等于没有限流。
func (d *DexScreener) reserve() time.Duration {
	for {
		now := time.Now().UnixNano()
		prev := d.nextAllowed.Load()
		next := now
		if prev > now {
			next = prev
		}
		if d.nextAllowed.CompareAndSwap(prev, next+d.minInterval.Nanoseconds()) {
			return time.Duration(next - now)
		}
	}
}

// dexTokensResponse 对应 /latest/dex/tokens 的响应体。
type dexTokensResponse struct {
	Pairs []dexPairRaw `json:"pairs"`
}

// dexPairRaw 是响应里的单个交易对。
//
// 字段类型按实测写死(2026-09-26):liquidity.usd 与 marketCap 是数字,
// 而 priceUsd 是**字符串**——DexScreener 自己就不统一,所以价格单独容错解析。
type dexPairRaw struct {
	ChainID     string `json:"chainId"`
	PairAddress string `json:"pairAddress"`
	BaseToken   struct {
		Address string `json:"address"`
	} `json:"baseToken"`
	QuoteToken struct {
		Address string `json:"address"`
	} `json:"quoteToken"`
	Liquidity struct {
		USD float64 `json:"usd"`
	} `json:"liquidity"`
	MarketCap float64 `json:"marketCap"`
	PriceUSD  string  `json:"priceUsd"`
}

// parsePriceUSD 把字符串价格转成浮点。
//
// 解析失败返回 0 而不报错:价格只用于日志展示与事后分析,不参与任何判定,
// 没必要因为它把一个本来可用的候选判成失败。
func parsePriceUSD(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// LiquidityFilter 是流动性预筛的判定(纯函数,便于测试)。
//
// 返回应该保留和应该淘汰的地址。规则只有一条:
// **查到交易对,且最大流动性不低于门槛**。
//
// 门槛默认取 1 美元而不是更大值:样本里已毕业的最低流动性是 3.48,
// 留出余量后既不会误杀,又能把流动性 0~0.5 的空池全部拦住。
func LiquidityFilter(addrs []string, pairs map[string]DexPair, minLiquidityUSD float64) (kept, dropped []string) {
	for _, a := range addrs {
		p, ok := pairs[strings.ToLower(a)]
		if !ok || p.LiquidityUSD < minLiquidityUSD {
			dropped = append(dropped, a)
			continue
		}
		kept = append(kept, a)
	}
	return kept, dropped
}
