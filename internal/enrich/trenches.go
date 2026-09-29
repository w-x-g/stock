package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// gmgnTrenchesPath 是"战壕"端点——按发射台生命周期分类返回代币。
//
// 这个端点改变了整个筛选的架构:原本要拿十几万个地址逐个去问 GMGN,
// 现在一次请求(权重 2)就能拿到"已毕业 + 市值/持币数已达标"的那几十个。
// 实测对比:158,087 个候选逐个问约需 158,000 权重;走这里只需 2。
const gmgnTrenchesPath = "/v1/trenches"

// gmgnWeightTrenches 是战壕端点的权重(官方 SKILL.md)。
const gmgnWeightTrenches = 2

// 战壕的三个生命周期分类。
const (
	// TrenchesCompleted 是已完成联合曲线、迁移到公开市场的代币——即"已毕业"。
	TrenchesCompleted = "completed"
	// TrenchesNearCompletion 是曲线快满、即将毕业。
	TrenchesNearCompletion = "near_completion"
	// TrenchesNewCreation 是刚创建、还在曲线上。
	TrenchesNewCreation = "new_creation"
)

// TrenchesFilter 是服务端筛选条件。
//
// 服务端筛掉的部分不会消耗后续请求,所以能用服务端就用服务端。
// 实测确认 min_marketcap / max_marketcap / min_holder_count / max_holder_count
// 都生效;min_created / max_created 实测无效(传秒数字符串也不起作用),
// 因此时间窗口改为在客户端按 CreatedTimestamp 过滤。
type TrenchesFilter struct {
	MinMarketCap   float64
	MaxMarketCap   float64
	MinHolderCount int64
	MaxHolderCount int64
	// Limit 是每个分类的返回上限,接口硬上限 80。
	Limit int
}

// TrenchesToken 是战壕列表里的一条。
//
// 字段名与接口一致(下划线 -> 驼峰),类型在这里统一:
// 接口把不少数值以**字符串**返回(如 market_cap),解析层已经转成数值,
// 比例字段统一归一化为百分数 0~100。
type TrenchesToken struct {
	Address  string
	Symbol   string
	Name     string
	Exchange string

	HolderCount int64
	MarketCap   float64
	Liquidity   float64
	Volume24h   float64
	Swaps24h    int64

	// Top10HolderRate 是前 10 名持仓**合计**(已剔除销毁与 LP),百分数。
	// 它只能用来放宽/否决条件 3,判不了"每个都不超过 3%"。
	Top10HolderRate float64
	// SniperHoldRate 来自 top70_sniper_hold_rate,是狙击**持仓**占比,百分数。
	SniperHoldRate     float64
	BundlerTradeRate   float64
	CreatorBalanceRate float64
	DevTeamHoldRate    float64
	FreshWalletRate    float64

	IsHoneypot     bool
	BuyTax         float64
	SellTax        float64
	OpenSource     bool
	OwnerRenounced bool

	Launchpad       string
	LaunchpadStatus int64

	// CreatedTimestamp 是发行时间(Unix 秒),客户端按它过滤时间窗口。
	CreatedTimestamp int64
	// CompleteTimestamp 是毕业时间(Unix 秒)。
	CompleteTimestamp int64

	CreatorTokenStatus string
	SmartDegenCount    int64
	RenownedCount      int64
}

// TrenchesAll 分档取全战壕列表。
//
// 为什么需要它:接口每个分类**硬上限 60 条**,而且 limit 参数被忽略
// (实测传 10 也返回 60)。所以单次 Trenches 调用的结果是"前 60 名样本",
// 不是全集——真实池子可能是它的好几倍。
//
// 绕开的办法是按市值区间递归二分:某个子区间返回不足 60 条,就说明那一段
// 已经取全了。实测按你的条件(市值 <$50K、持币 100-2000):
// 单次查询 55 条,分档后 71 个唯一地址,差别是实打实的漏。
//
// 代价是请求数从 1 涨到十几,但权重只有 2,依然远低于逐个地址去问。
func (g *GMGN) TrenchesAll(ctx context.Context, types []string, f TrenchesFilter) ([]TrenchesToken, error) {
	// 硬上限的保守取值。取 60 是因为实测每次都不超过它。
	const bandCap = 60
	// 递归深度上限。二分 8 次最多 256 个区间,足以把任何一档切到 60 以下,
	// 同时也兜住了"服务端行为变化导致永不收敛"的风险。
	const maxDepth = 8
	// 请求数上限,防止意外的无限细分。
	const maxRequests = 120

	lo := f.MinMarketCap
	if lo <= 0 {
		lo = 0
	}
	hi := f.MaxMarketCap
	if hi <= 0 {
		hi = 1e9
	}

	seen := map[string]TrenchesToken{}
	requests := 0

	var walk func(lo, hi float64, depth int) error
	walk = func(lo, hi float64, depth int) error {
		if requests >= maxRequests {
			return fmt.Errorf("分档查询超过 %d 次仍未收敛,已中止"+
				"(服务端的截断行为可能变了)", maxRequests)
		}
		requests++

		sub := f
		sub.MinMarketCap = lo
		sub.MaxMarketCap = hi

		toks, err := g.Trenches(ctx, types, sub)
		if err != nil {
			return err
		}

		// 不足上限,说明这一档已经取全;或到达深度下限,只能接受可能不完整的结果
		if len(toks) < bandCap || depth >= maxDepth || hi-lo < 1 {
			for _, t := range toks {
				seen[t.Address] = t
			}
			return nil
		}

		mid := lo + (hi-lo)/2
		if err := walk(lo, mid, depth+1); err != nil {
			return err
		}
		return walk(mid, hi, depth+1)
	}

	if err := walk(lo, hi, 0); err != nil {
		return nil, err
	}

	out := make([]TrenchesToken, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	// 按发行时间倒序,让日志与后续处理顺序稳定
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedTimestamp > out[j].CreatedTimestamp
	})
	return out, nil
}

// Trenches 拉取战壕列表。
//
// types 传 TrenchesCompleted 等;留空表示三类都要。
func (g *GMGN) Trenches(ctx context.Context, types []string, f TrenchesFilter) ([]TrenchesToken, error) {
	if len(types) == 0 {
		types = []string{TrenchesNewCreation, TrenchesNearCompletion, TrenchesCompleted}
	}
	limit := f.Limit
	if limit <= 0 || limit > 80 {
		limit = 80
	}

	// 请求体是 {version:"v2", <分类>:{...}} 这种按分类分节的结构,
	// 不是平铺的。这是从官方 CLI 源码里读出来的,光看文档猜不到。
	section := map[string]any{
		// 这两项是接口要求的固定值,不是可选筛选项
		"filters":               []string{"offchain", "onchain"},
		"launchpad_platform_v2": true,
		"limit":                 limit,
	}
	if f.MinMarketCap > 0 {
		section["min_marketcap"] = f.MinMarketCap
	}
	if f.MaxMarketCap > 0 {
		section["max_marketcap"] = f.MaxMarketCap
	}
	if f.MinHolderCount > 0 {
		section["min_holder_count"] = f.MinHolderCount
	}
	if f.MaxHolderCount > 0 {
		section["max_holder_count"] = f.MaxHolderCount
	}

	body := map[string]any{"version": "v2"}
	for _, t := range types {
		body[t] = section
	}

	raw, err := g.post(ctx, gmgnTrenchesPath, "chain="+g.chain, body, gmgnWeightTrenches)
	if err != nil {
		return nil, err
	}

	// 响应形如 {code:0, data:{completed:[...], near_completion:[...], new_creation:[...]}}
	var wrapper struct {
		Data map[string][]gmgnFields `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("解析战壕响应失败: %w\n响应片段: %s",
			err, truncate(string(raw), 300))
	}

	var out []TrenchesToken
	for _, t := range types {
		for _, f := range wrapper.Data[t] {
			out = append(out, parseTrenchesToken(f))
		}
	}
	return out, nil
}

// post 发一次带节流与退避的 POST。
//
// 与 get 分开是因为请求体与查询串的组装方式不同;节流与重试逻辑共用同一套。
func (g *GMGN) post(ctx context.Context, path, rawQuery string, payload any, weight int) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构造请求体失败: %w", err)
	}
	fullURL := g.baseURL + path
	if rawQuery != "" {
		fullURL += "?" + rawQuery
	}

	var lastErr error
	for attempt := 0; attempt <= gmgnMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(g.backoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		select {
		case <-time.After(g.reserve(g.intervalFor(weight))):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		out, retryable, err := g.postOnce(ctx, fullURL, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", gmgnMaxRetries, lastErr)
}

func (g *GMGN) postOnce(ctx context.Context, fullURL string, payload []byte) (json.RawMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("X-APIKEY", g.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("请求 gmgn 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, gmgnMaxBodyBytes))
	if err != nil {
		return nil, true, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		ae := &APIError{StatusCode: resp.StatusCode, Body: string(body),
			ResetAt: parseResetAt(resp.Header.Get("X-RateLimit-Reset"), body)}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, false, ae
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, false, fmt.Errorf(
				"GMGN_API_KEY 无效或未生效——请确认已在 gmgn.ai/ai 申请并填入 .env: %w", ae)
		default:
			return nil, resp.StatusCode >= 500, ae
		}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, fmt.Errorf("%w: 战壕接口返回空响应", ErrNoTokenData)
	}
	if err := checkEnvelopeCode(body); err != nil {
		var ae *APIError
		retry := errors.As(err, &ae) && ae.StatusCode >= 500 && !IsGMGNQuotaBanned(err)
		return nil, retry, err
	}
	return json.RawMessage(body), false, nil
}

// parseTrenchesToken 把一条战壕记录转成领域结构。
func parseTrenchesToken(f gmgnFields) TrenchesToken {
	var t TrenchesToken

	if raw, _, ok := f.pick("address"); ok {
		var s string
		if decode(raw, &s) {
			t.Address = strings.ToLower(strings.TrimSpace(s))
		}
	}
	if raw, _, ok := f.pick("symbol"); ok {
		decode(raw, &t.Symbol)
	}
	if raw, _, ok := f.pick("name"); ok {
		decode(raw, &t.Name)
	}
	if raw, _, ok := f.pick("exchange"); ok {
		decode(raw, &t.Exchange)
	}

	t.HolderCount, _ = pickInt64(f, "holder_count", "holderCount")
	t.MarketCap, _ = pickFloat(f, "market_cap", "marketCap")
	t.Liquidity, _ = pickFloat(f, "liquidity")
	t.Volume24h, _ = pickFloat(f, "volume_24h", "volume24h")
	t.Swaps24h, _ = pickInt64(f, "swaps_24h", "swaps24h")

	t.Top10HolderRate, _ = pickRate(f, "top_10_holder_rate", "top10HolderRate")
	t.SniperHoldRate, _ = pickRate(f, "top70_sniper_hold_rate", "top70SniperHoldRate")
	t.BundlerTradeRate, _ = pickRate(f, "bundler_trader_amount_rate", "bundlerTraderAmountRate")
	t.CreatorBalanceRate, _ = pickRate(f, "creator_balance_rate", "creatorBalanceRate")
	t.DevTeamHoldRate, _ = pickRate(f, "dev_team_hold_rate", "devTeamHoldRate")
	t.FreshWalletRate, _ = pickRate(f, "fresh_wallet_rate", "freshWalletRate")

	if raw, _, ok := f.pick("is_honeypot", "isHoneypot"); ok {
		var v flexBool
		decode(raw, &v)
		t.IsHoneypot = bool(v)
	}
	t.BuyTax, _ = pickRate(f, "buy_tax", "buyTax")
	t.SellTax, _ = pickRate(f, "sell_tax", "sellTax")

	if raw, _, ok := f.pick("open_source", "openSource"); ok {
		var v flexBool
		decode(raw, &v)
		t.OpenSource = bool(v)
	}
	if raw, _, ok := f.pick("owner_renounced", "ownerRenounced"); ok {
		var v flexBool
		decode(raw, &v)
		t.OwnerRenounced = bool(v)
	}

	if raw, _, ok := f.pick("launchpad"); ok {
		decode(raw, &t.Launchpad)
	}
	t.LaunchpadStatus, _ = pickInt64(f, "launchpad_status", "launchpadStatus")
	t.CreatedTimestamp, _ = pickInt64(f, "created_timestamp", "createdTimestamp")
	t.CompleteTimestamp, _ = pickInt64(f, "complete_timestamp", "completeTimestamp")

	if raw, _, ok := f.pick("creator_token_status", "creatorTokenStatus"); ok {
		decode(raw, &t.CreatorTokenStatus)
	}
	t.SmartDegenCount, _ = pickInt64(f, "smart_degen_count", "smartDegenCount")
	t.RenownedCount, _ = pickInt64(f, "renowned_count", "renownedCount")

	// 时间戳可能是字符串
	if t.CreatedTimestamp == 0 {
		if raw, _, ok := f.pick("created_timestamp", "createdTimestamp"); ok {
			if n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(string(raw)), `"`), 10, 64); err == nil {
				t.CreatedTimestamp = n
			}
		}
	}
	return t
}
