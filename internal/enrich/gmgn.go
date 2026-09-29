package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// userAgent 是本包所有对外请求携带的客户端标识。
//
// 必须显式设置:Go 默认的 "Go-http-client/1.1" 会被 Cloudflare 规则拒绝
// (实测同一地址 rpc.48.club:默认 UA 403,任意显式 UA 200)。GMGN 前面同样
// 挂 Cloudflare,所以这里一并带上——用自述式标识,不伪装浏览器。
//
// 与 chain.UserAgent 取值一致,但刻意各自定义:两个包是平级的外部客户端,
// 谁都不该为了一个字符串去依赖对方。
const userAgent = "stock-bsc-indexer/1.0"

// GMGN Agent API 的两个端点。
//
// 端点路径与字段名取自官方 skills 仓库的 SKILL.md(不是文档页——文档页只有
// 概念介绍)。这是本文件里唯一有权威来源的部分,改动前请先核对:
// https://github.com/GMGNAI/gmgn-skills/blob/main/skills/gmgn-token/SKILL.md
// 注意是 openapi 而不是 api:api.gmgn.ai 是**不存在的域名**
// (DNS 解析返回的是污染地址),照它写会一直连不上。
// 这个值取自官方 CLI(gmgn-cli)的源码常量,CLI 里可用 GMGN_API_URL 覆盖。
const (
	gmgnDefaultBaseURL = "https://openapi.gmgn.ai"
	gmgnInfoPath       = "/v1/token/info"
	gmgnSecurityPath   = "/v1/token/security"
	gmgnHoldersPath    = "/v1/market/token_top_holders"
)

// 各端点在漏桶里的权重(官方 SKILL.md)。配额按权重计,所以 holders 一次
// 顶五次 info——决定调用顺序时要以权重为准,不是以次数为准。
const (
	gmgnWeightInfo     = 1
	gmgnWeightSecurity = 1
	gmgnWeightHolders  = 5
)

const (
	// gmgnMaxRetries 是 5xx 与网络错误的退避重试上限。
	// 4xx 一律不重试(见 IsGMGNQuotaBanned 的说明)。
	gmgnMaxRetries = 3
	// gmgnMaxBodyBytes 兜住异常大的响应,避免一次坏响应吃掉内存。
	gmgnMaxBodyBytes = 8 << 20
	// gmgnDefaultTimeout 与其它外部客户端的 30 秒保持一致。
	gmgnDefaultTimeout = 30 * time.Second
	// gmgnMaxHolderLimit 是接口允许的单次条数上限。
	gmgnMaxHolderLimit = 100
	// gmgnLaunchpadLive 是 launchpad_status 里"已上线/已毕业"的取值。
	//
	// 实测确认(2026-09-26):战壕接口的 completed 分类 60 个币**全部是 1**,
	// near_completion 与 new_creation 则全部是 0。老牌在交易的币(CAKE)也是 1。
	// 所以 1 就是"已毕业"。
	//
	// ⚠️ 官方 SKILL.md 写的是 "0=未开盘, 1=live, 2=migrated"——**与实际不符**。
	// 照它判 status==2 会永远筛不出东西:实测我们库里 158,087 个候选,
	// 没有一个 status 是 2。
	gmgnLaunchpadLive = 1
)

// GMGN 是 GMGN Agent API 的客户端。
//
// 定位与 Serper 相同:配额稀缺,必须有闸门控制调用量。差别在于 GMGN 没有批量
// 端点——一个代币两次请求——因此节流必须做在客户端内部,让上层可以放心并发。
// 若把 sleep 放在 worker 里,每个 worker 各睡一会并不等于全局 QPS 受控。
//
// 配额(官方 SKILL.md):漏桶 套餐速率/套餐容量,Free 5/5、Plus 20/20、Pro 50/50。
// holders 端点权重 5、info 端点权重 1,所以 Free 档下 holders 大约每秒一次。
type GMGN struct {
	baseURL     string
	apiKey      string
	chain       string
	holderLimit int
	client      *http.Client
	minInterval time.Duration
	// backoffBase 是退避基数。抽成字段是为了让测试能置零——否则验证"5xx 会重试"
	// 就得白等 3.5 秒(500ms + 1s + 2s)。
	backoffBase time.Duration
	// nextAllowed 是下一个允许发请求的时刻(UnixNano)。用原子 CAS 而非 Mutex,
	// 与全仓库"只用 atomic.*"的约定一致。
	nextAllowed atomic.Int64
}

// NewGMGN 构造客户端。apiKey 为空时返回 nil,调用方据此跳过筛选(与 NewSerper 一致)。
func NewGMGN(baseURL, apiKey, chain string, holderLimit, minIntervalMS int) *GMGN {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = gmgnDefaultBaseURL
	}
	if chain == "" {
		chain = "bsc"
	}
	if holderLimit <= 0 || holderLimit > gmgnMaxHolderLimit {
		holderLimit = gmgnMaxHolderLimit
	}
	if minIntervalMS <= 0 {
		minIntervalMS = 1200
	}
	return &GMGN{
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		apiKey:      apiKey,
		chain:       chain,
		holderLimit: holderLimit,
		client:      &http.Client{Timeout: gmgnDefaultTimeout},
		minInterval: time.Duration(minIntervalMS) * time.Millisecond,
		backoffBase: 500 * time.Millisecond,
	}
}

// InfoURL 返回代币基础信息端点的完整地址。
func (g *GMGN) InfoURL(tokenAddr string) string {
	return g.endpoint(gmgnInfoPath, tokenAddr, 0)
}

// SecurityURL 返回代币安全检测端点的完整地址。
func (g *GMGN) SecurityURL(tokenAddr string) string {
	return g.endpoint(gmgnSecurityPath, tokenAddr, 0)
}

// HoldersURL 返回持币者端点的完整地址。
func (g *GMGN) HoldersURL(tokenAddr string) string {
	return g.endpoint(gmgnHoldersPath, tokenAddr, g.holderLimit)
}

func (g *GMGN) endpoint(path, tokenAddr string, limit int) string {
	q := url.Values{}
	q.Set("chain", g.chain)
	q.Set("address", tokenAddr)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	// 字段名是下划线的 order_by,不是 order-by
	q.Set("order_by", "amount_percentage")
	q.Set("direction", "desc")
	return g.baseURL + path + "?" + q.Encode()
}

// ErrNoTokenData 表示该代币在 GMGN 没有数据。
//
// 与"调用失败"分开:前者只说明这个币查不到,继续处理下一个即可;
// 后者可能是配额或网络问题,需要人工介入。
var ErrNoTokenData = errors.New("gmgn 无该代币数据")

// IsGMGNQuotaBanned 判断错误是否为限流或封禁。
//
// 与 Serper 的关键差别:GMGN 在冷却期内**重试会延长封禁**(每次 +5 秒,上限 5 分钟),
// 因此这里绝不自作主张退避重试。调用方必须立即终止整轮,并打印 ResetAt 供人工决定
// 何时再跑。
func IsGMGNQuotaBanned(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode == http.StatusTooManyRequests {
		return true
	}
	body := strings.ToUpper(ae.Body)
	return strings.Contains(body, "RATE_LIMIT_EXCEEDED") || strings.Contains(body, "RATE_LIMIT_BANNED")
}

// IsGMGNUnauthorized 判断错误是否为密钥缺失或失效。
func IsGMGNUnauthorized(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden
}

// IsNoTokenData 判断错误是否为"该代币在 GMGN 无数据"。
func IsNoTokenData(err error) bool { return errors.Is(err, ErrNoTokenData) }

// ResetAt 返回限流解除时刻。信息缺失时返回零值。
func ResetAt(err error) time.Time {
	var ae *APIError
	if !errors.As(err, &ae) {
		return time.Time{}
	}
	return ae.ResetAt
}

// intervalFor 返回某个端点应使用的最小请求间隔。
//
// 按权重成比例:holders 权重 5、info 与 security 权重 1,所以后者的间隔是
// 前者的五分之一。所有端点共用一个间隔会白白浪费五倍预算——而第一段
// (info)恰恰是要扫最多候选的那一段,它的速度直接决定整轮要跑多久。
//
// minInterval 的语义是"权重 5 的那个端点(holders)的间隔",其余按比例推。
func (g *GMGN) intervalFor(weight int) time.Duration {
	d := time.Duration(int64(g.minInterval) * int64(weight) / gmgnWeightHolders)
	// 下界兜一下,避免配置成极小值时把限流完全关掉
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}

// reserve 返回本次请求还需等待多久,并原子地把下一个可用时刻推后一个间隔。
//
// 用 CAS 而不是 Mutex:抢到令牌的人把 nextAllowed 推后 interval,后来者
// 拿到的是自己被允许的时刻。这样即使 workers=8,全局请求间隔也严格等于
// interval——而在每个 worker 里各睡一会是做不到这一点的。
func (g *GMGN) reserve(interval time.Duration) time.Duration {
	for {
		now := time.Now().UnixNano()
		prev := g.nextAllowed.Load()
		next := now
		if prev > now {
			next = prev
		}
		if g.nextAllowed.CompareAndSwap(prev, next+interval.Nanoseconds()) {
			return time.Duration(next - now)
		}
	}
}

// Info 只取代币基础信息(权重 1)。
//
// 这是两阶段漏斗的第一段:token info 里已经带着 launchpad_status、holder_count、
// 市值与发行价,足以判掉四个条件里的三个。先用它把绝大多数候选筛掉,
// 再对幸存者去花权重 5 的 holders。
//
// 按官方漏桶计,一次 holders 顶五次 info,所以这个分流省下的是大头。
func (g *GMGN) Info(ctx context.Context, tokenAddr string) (*GMGNTokenReport, error) {
	body, err := g.get(ctx, g.InfoURL(tokenAddr), tokenAddr, gmgnWeightInfo)
	if err != nil {
		return nil, err
	}
	return ParseTokenReport(mergeDocuments(body, nil, nil))
}

// Complete 在 Info 的结果上补齐 security 与 holders(权重 6)。
//
// base 必须来自 Info()——它带着已取到的 info 原始响应,这里不再重复请求。
// 白花那一次虽然只值 1 个权重,但配额本就是最稀缺的资源。
//
// 任一请求失败即整体失败:只有一半数据时判定没有意义,而且半份结果无法缓存复用。
func (g *GMGN) Complete(ctx context.Context, tokenAddr string, base *GMGNTokenReport) (*GMGNTokenReport, error) {
	if base == nil {
		return nil, fmt.Errorf("Complete 需要 Info 的结果作为基础")
	}
	secBody, err := g.get(ctx, g.SecurityURL(tokenAddr), tokenAddr, gmgnWeightSecurity)
	if err != nil {
		return nil, err
	}
	holdersBody, err := g.get(ctx, g.HoldersURL(tokenAddr), tokenAddr, gmgnWeightHolders)
	if err != nil {
		return nil, err
	}
	return ParseTokenReport(mergeDocuments(base.RawInfo, secBody, holdersBody))
}

// Fetch 取一个代币的完整快照,等价于 Info + Complete。
//
// 单步用法(测试、临时排查)走这个;批量筛选走两阶段,否则配额吃不消。
func (g *GMGN) Fetch(ctx context.Context, tokenAddr string) (*GMGNTokenReport, error) {
	rep, err := g.Info(ctx, tokenAddr)
	if err != nil {
		return nil, err
	}
	return g.Complete(ctx, tokenAddr, rep)
}

// mergeDocuments 把多次响应拼成 ParseTokenReport 期望的合并形态。
func mergeDocuments(info, security, holders json.RawMessage) []byte {
	out, err := json.Marshal(map[string]json.RawMessage{
		"info":     info,
		"security": security,
		"holders":  holders,
	})
	if err != nil {
		// 只可能是 RawMessage 非法,而它来自 json.Unmarshal,不会失败
		return info
	}
	return out
}

// get 发一次带节流与退避的 GET。
func (g *GMGN) get(ctx context.Context, fullURL, tokenAddr string, weight int) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt <= gmgnMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(g.backoff(attempt)):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// 重试也要重新排队,否则退避会绕过全局节流。
		select {
		case <-time.After(g.reserve(g.intervalFor(weight))):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		body, retryable, err := g.fetchOnce(ctx, fullURL, tokenAddr)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", gmgnMaxRetries, lastErr)
}

// backoff 是指数退避,与 rpc.backoff 同款:从基数起翻倍,封顶 10 秒。
func (g *GMGN) backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * g.backoffBase
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// fetchOnce 发一次请求。第二个返回值表示失败是否值得重试。
func (g *GMGN) fetchOnce(ctx context.Context, fullURL, tokenAddr string) (json.RawMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("构造请求失败: %w", err)
	}
	// 鉴权头是 X-APIKEY,不是 Authorization: Bearer——这是实测确认的
	// (用 Bearer 会得到 AUTH_INVALID: missing api key or client_id)。
	req.Header.Set("X-APIKEY", g.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := g.client.Do(req)
	if err != nil {
		// 网络错误值得重试——但 ctx 被取消时不重试,否则会白等 3 轮退避
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
		ae := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			ResetAt:    parseResetAt(resp.Header.Get("X-RateLimit-Reset"), body),
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// 文案要能直接指导操作——这是最可能遇到的失败
			return nil, false, fmt.Errorf(
				"GMGN_API_KEY 无效或未生效——请确认已在 gmgn.ai/ai 申请并填入 .env: %w", ae)
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, false, ae
		case resp.StatusCode == http.StatusNotFound:
			return nil, false, fmt.Errorf("%w: %s", ErrNoTokenData, tokenAddr)
		default:
			// 5xx 等瞬时故障值得重试;其余 4xx 是请求本身的问题,重试无意义
			return nil, resp.StatusCode >= 500, ae
		}
	}

	// GMGN 对**未收录**的代币返回 200 + 空 body(连 JSON 都没有,不是 404)。
	// 实测:抽样 30 个 four.meme 币,29 个是这种响应。
	//
	// 必须当成"无数据"而不是解析失败:否则整轮会被记成"失败"，
	// 看起来像程序出了问题,实际只是这些币没被索引。
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false, fmt.Errorf("%w: %s(接口返回空响应)", ErrNoTokenData, tokenAddr)
	}

	// 200 也可能是业务错误码(如限流),先解一层信封
	if err := checkEnvelopeCode(body); err != nil {
		// 限流**绝不能**重试——GMGN 在冷却期内重试会延长封禁。
		// 其余业务码里只有 5xx 值得重试。
		var ae *APIError
		retry := errors.As(err, &ae) && ae.StatusCode >= 500 && !IsGMGNQuotaBanned(err)
		return nil, retry, err
	}
	return json.RawMessage(body), false, nil
}

// checkEnvelopeCode 检查响应体里的业务错误码。
//
// GMGN 的限流会以 200 + {"code":429,"error":"RATE_LIMIT_BANNED"} 的形式返回,
// 只看 HTTP 状态码会漏掉。
func checkEnvelopeCode(body []byte) error {
	// 实测形态:{"code":401,"error":"AUTH_INVALID","message":"missing api key or client_id"}
	var env struct {
		Code    flexInt `json:"code"`
		Error   string  `json:"error"`
		Message string  `json:"message"`
		Msg     string  `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil // 不是信封形态,交给后续解析处理
	}
	if env.Code == 0 {
		return nil
	}
	msg := env.Message
	if msg == "" {
		msg = env.Msg
	}
	return &APIError{
		StatusCode: int(env.Code),
		// error 是给程序看的错误码(AUTH_INVALID / RATE_LIMIT_BANNED),
		// message 是给人看的说明,两个都留下
		Body:    fmt.Sprintf("code=%d %s %s", int64(env.Code), env.Error, msg),
		ResetAt: parseResetAt("", body),
	}
}

// parseResetAt 依次从响应头的 unix 秒与响应体的 reset_at 字段取限流解除时刻。
func parseResetAt(header string, body []byte) time.Time {
	if header != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0)
		}
	}

	var probe struct {
		ResetAt flexInt `json:"reset_at"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.ResetAt > 0 {
		return time.Unix(int64(probe.ResetAt), 0)
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 归一化结果
// ---------------------------------------------------------------------------

// GMGNTokenReport 是一个代币的完整快照,由 token info 与 token_top_holders 合并而成。
//
// 所有 rate 字段统一为百分数 0~100。接口原始值是 0~1 小数,换算只在解析层做——
// 把量纲集中到唯一一处,下游就不必每次判断"这个 0.05 是 5% 还是 0.05%"。
type GMGNTokenReport struct {
	ContractAddress string
	Name            string
	Symbol          string

	// ---- 规模指标 ----
	HolderCount int64
	MarketCap   float64 // 美元 = price.price × circulating_supply
	Liquidity   float64 // 美元,取最大池

	// ---- 发行状态 ----
	// LaunchpadStatus: 0=未开盘, 1=进行中, 2=已迁移到 DEX(即"已毕业")。
	LaunchpadStatus   int
	LaunchpadProgress float64 // 联合曲线进度,0~1
	// CreatedTimestamp 是代币创建时间(Unix 秒)。
	CreatedTimestamp   int64
	MigratedPool       string
	MigrationMarketCap float64

	// ---- 聚合指标(全部已归一化为百分数) ----
	Top10Rate       float64
	BundlerRate     float64 // 捆绑机器人的成交量占比
	CreatorRate     float64 // 创建者持仓占比
	RatTraderRate   float64 // 内部人成交量占比
	FreshWalletRate float64

	CreatorAddress     string
	CreatorTokenStatus string // "hold" 仍在持有 / "sell" 已清仓

	// ---- 安全检测(来自 token security 端点) ----
	// IsHoneypot 仅 BSC/Base 可测,其它链恒为 false。
	// 蜜罐意味着买得进卖不出,是硬性否决项。
	IsHoneypot bool
	BuyTax     float64 // 百分数,如 3 = 3%
	SellTax    float64

	// 注意:官方 SKILL.md 里提到的 rug_ratio **在实测的 security 响应里不存在**,
	// 所以这里没有对应字段。需要风险评分时不要凭文档臆造字段名。
	IsOpenSource bool
	IsRenounced  bool

	// ---- 钱包画像计数 ----
	// SniperWallets 是接口直接给出的狙击钱包**数量**,是"狙击占比"的权威来源。
	SniperWallets  int64
	BundlerWallets int64
	SmartWallets   int64

	// ---- 派生指标 ----
	// SniperCountRate = sniper_wallets / holder_count,不受明细覆盖度影响。
	SniperCountRate float64
	// SniperHoldRate 来自 stat.top70_sniper_hold_rate,是接口直接给出的
	// 狙击**持仓**占比(前 70 名里狙击地址的持仓之和),不需要从明细凑。
	SniperHoldRate float64
	// SniperAmountRate 是明细里 sniper 标记地址的持仓之和。
	//
	// 接口实测并不逐条给 sniper 打标签,所以它通常为 0,只能算**下界**。
	// 要持仓口径请优先用 SniperHoldRate;这个字段保留是为了在接口改版
	// 开始逐条打标时能立刻用上。
	SniperAmountRate float64

	// Holders 按 AmountRate 降序,只含接口返回的前 N 条(最多 100)。
	Holders []GMGNHolder
	// Coverage 是明细覆盖的供应占比(%),等于各条 AmountRate 之和。
	// 明显小于 100 说明明细没覆盖全,持仓型口径只能算下界。
	Coverage float64

	// Missing 列出判定所需但响应里找不到的字段。
	// "字段缺失"与"值为 0"必须区分:前者无法判定,后者是明确的判定依据。
	Missing []string
	// Complete 表示判定所需字段齐全。
	Complete bool
	// RateSuspicious 表示聚合值与明细求和互相矛盾(只可能是量纲或字段名错了)。
	RateSuspicious bool

	// Raw 保留原始响应,首次实测时用来核对字段名(-dump-raw)。
	RawInfo     json.RawMessage
	RawSecurity json.RawMessage
	RawHolders  json.RawMessage

	// 存在性标记。三个子文档各只带一部分字段,所以"缺失"必须在合并之后
	// 统一判定——在子文档里判会把别的文档负责的字段也算成缺失。
	// 用标记而不是判零值,是因为 sniper_wallets 这类字段的 0 是合法值
	// (确实没有狙击钱包),必须与"字段没返回"区分开。
	hasHolderCount   bool
	hasMarketCap     bool
	hasSniperWallets bool
	hasTop10Rate     bool
}

// GMGNHolder 是一个持有者条目,只保留判定与排障需要的字段。
type GMGNHolder struct {
	Address string
	// AmountRate 是占总供应量的百分数(接口给 0~1 小数)。
	AmountRate float64
	// AddrType 是接口的地址类型:0=普通钱包,2=交易所/流动性池。
	// 这是剔除 LP 最可靠的依据——比 meme_tokens.pair_address 权威得多,
	// 后者对 four.meme 来源根本是空字符串。
	AddrType int
	// Exchange 在 AddrType==2 时给出池子名称(如 pancake_v2)。
	Exchange string
	// MakerTags 是代币特定行为标签:bundler / paper_hands / top_holder 等。
	MakerTags []string
	// Tags 是平台级钱包标签:kol / smart_degen / axiom 等。
	Tags []string
	// IsOnCurve 为 true 表示还在联合曲线上(未毕业)。
	IsOnCurve bool
	// TransferIn 表示当前持仓是转账得来而非买入。
	TransferIn bool
	// IsSniper 是标签里含 sniper 的便捷投影。
	//
	// 注意:接口未必给每条都打这个标签,所以它只适合用来算**下界**。
	// 要算占比请优先用报告里的 SniperCountRate。
	IsSniper bool
}

// IsPool 表示该条目是 DEX 交易池或交易所地址。
func (h GMGNHolder) IsPool() bool { return h.AddrType == 2 }

// IsBurn 表示该条目是销毁地址。
//
// addr_type==1 就是销毁地址——官方 SKILL.md 只写了 0 与 2,但实测数据里
// 0x...dead 的 addr_type 正是 1(CAKE 的持仓榜首就是它,占 93.75%)。
// 地址形态作为兜底,防止接口不返回 addr_type。
//
// 0x...dead 常年霸榜持仓榜,但它不是任何人的持仓,必须与 LP 一起剔除。
func (h GMGNHolder) IsBurn() bool { return h.AddrType == 1 || isBurnAddress(h.Address) }

// isBurnAddress 识别常见的销毁地址。
func isBurnAddress(addr string) bool {
	a := strings.ToLower(strings.TrimSpace(addr))
	if a == "" {
		return false
	}
	// 0x0000000000000000000000000000000000000000 / ...dead / ...0000
	switch a {
	case "0x0000000000000000000000000000000000000000",
		"0x000000000000000000000000000000000000dead",
		"0x0000000000000000000000000000000000000001":
		return true
	}
	return strings.HasSuffix(a, "dead") || strings.HasSuffix(a, "0000")
}

// ---------------------------------------------------------------------------
// 宽容的标量解析
// ---------------------------------------------------------------------------

// flexFloat 宽容解析数字:接受数字、字符串数字、null 与空串。
//
// 接口未实测的前提下,这一条能挡掉绝大多数改版——Go 原生 float64 遇到 "0.05"
// 会让整份响应解析失败,而实际上各家 API 混用字符串数字是常态。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

// flexInt 同上,用于整数。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(v)
		return nil
	}
	// 有些端点用浮点表示整数
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(int64(v))
		return nil
	}
	*f = 0
	return nil
}

// flexBool 宽容解析布尔:接受 true/false、0/1、"yes"/"no"。
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(string(b)), `"`)) {
	case "true", "1", "yes", "y":
		*f = true
	case "false", "0", "no", "n", "", "null":
		*f = false
	default:
		*f = false
	}
	return nil
}

// ---------------------------------------------------------------------------
// 多键名提取
// ---------------------------------------------------------------------------

// gmgnFields 是 JSON 对象的原始字段表。
//
// 用它而不是固定 struct,是为了让同一个逻辑字段能接受多个候选键名——
// 接口文档未实测,字段名在 snake_case / camelCase 之间摇摆,逐个 if 分叉
// 会散成几十处。表驱动只需在一处列出候选名。
type gmgnFields map[string]json.RawMessage

// pick 返回第一个存在的候选键的原始值与键名。
func (f gmgnFields) pick(keys ...string) (json.RawMessage, string, bool) {
	for _, k := range keys {
		if raw, ok := f[k]; ok && !isJSONNull(raw) {
			return raw, k, true
		}
	}
	return nil, "", false
}

// sub 取出嵌套对象。接口把大部分指标放在 stat / dev / wallet_tags_stat / pool 里。
//
// 接受多个候选键名,理由与 pick 相同:命名风格可能变。
func (f gmgnFields) sub(keys ...string) gmgnFields {
	raw, _, ok := f.pick(keys...)
	if !ok {
		return nil
	}
	var out gmgnFields
	if !decode(raw, &out) {
		return nil
	}
	return out
}

func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// decode 把原始值解进目标类型。失败时保持零值(宽容解析)。
func decode(raw json.RawMessage, dst any) bool {
	if isJSONNull(raw) {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// ParseTokenReport 解析一份合并快照,形如 {"info": {...}, "holders": {...}}。
//
// 导出它的唯一理由是离线验证:-fixtures 模式与 golden 测试都要绕过 HTTP,
// 而它们必须走与线上完全相同的解析路径。
//
// 解析失败**绝不返回零值报告**:零值报告会在下游表现为"各项指标都是 0,
// 判定不通过",让整轮筛选静默产出空结果——这是最危险的失败模式。
func ParseTokenReport(body []byte) (*GMGNTokenReport, error) {
	var wrapper struct {
		Info     json.RawMessage `json:"info"`
		Security json.RawMessage `json:"security"`
		Holders  json.RawMessage `json:"holders"`
	}
	// 合并形态与单文档形态都接受:后者便于手工喂一份裸响应做排查
	if err := json.Unmarshal(body, &wrapper); err != nil ||
		(len(wrapper.Info) == 0 && len(wrapper.Security) == 0 && len(wrapper.Holders) == 0) {
		rep, err := parseTokenDocument(body)
		if err != nil {
			return nil, err
		}
		rep.finish()
		return rep, nil
	}

	rep := &GMGNTokenReport{}
	if len(wrapper.Info) > 0 {
		got, err := parseTokenDocument(wrapper.Info)
		if err != nil {
			return nil, fmt.Errorf("解析 info 失败: %w", err)
		}
		rep = got
		rep.RawInfo = wrapper.Info
	}
	if len(wrapper.Security) > 0 {
		got, err := parseTokenDocument(wrapper.Security)
		if err != nil {
			return nil, fmt.Errorf("解析 security 失败: %w", err)
		}
		mergeReport(rep, got)
		rep.RawSecurity = wrapper.Security
	}
	if len(wrapper.Holders) > 0 {
		got, err := parseTokenDocument(wrapper.Holders)
		if err != nil {
			return nil, fmt.Errorf("解析 holders 失败: %w", err)
		}
		mergeReport(rep, got)
		rep.RawHolders = wrapper.Holders
	}

	rep.finish()
	return rep, nil
}

// mergeReport 把 holders 文档里独有的内容合进以 info 为底的结果。
//
// info 优先:它是聚合值的权威来源,holders 文档里的同名字段是冗余的。
func mergeReport(dst, src *GMGNTokenReport) {
	if len(src.Holders) > 0 {
		dst.Holders = src.Holders
	}
	if !dst.hasHolderCount && src.hasHolderCount {
		dst.HolderCount = src.HolderCount
		dst.hasHolderCount = true
	}
	if !dst.hasMarketCap && src.hasMarketCap {
		dst.MarketCap = src.MarketCap
		dst.hasMarketCap = true
	}
	if !dst.hasSniperWallets && src.hasSniperWallets {
		dst.SniperWallets = src.SniperWallets
		dst.hasSniperWallets = true
	}
	if !dst.hasTop10Rate && src.hasTop10Rate {
		dst.Top10Rate = src.Top10Rate
		dst.hasTop10Rate = true
	}
	if dst.SniperAmountRate == 0 {
		dst.SniperAmountRate = src.SniperAmountRate
	}
	if dst.ContractAddress == "" {
		dst.ContractAddress = src.ContractAddress
	}
	if dst.LaunchpadStatus == 0 {
		dst.LaunchpadStatus = src.LaunchpadStatus
	}
	// 安全字段只可能来自 security 文档,不存在"被覆盖"的问题
	if src.IsHoneypot {
		dst.IsHoneypot = true
	}
	if dst.BuyTax == 0 {
		dst.BuyTax = src.BuyTax
	}
	if dst.SellTax == 0 {
		dst.SellTax = src.SellTax
	}
	if src.IsOpenSource {
		dst.IsOpenSource = true
	}
	if src.IsRenounced {
		dst.IsRenounced = true
	}
}

// parseTokenDocument 解析单个响应文档。
func parseTokenDocument(body []byte) (*GMGNTokenReport, error) {
	data, err := unwrapEnvelope(body)
	if err != nil {
		return nil, err
	}

	var f gmgnFields
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析 gmgn 数据段失败: %w\n响应片段: %s",
			err, truncate(string(body), 300))
	}

	rep := &GMGNTokenReport{}

	// 接口把指标分散在四个嵌套对象里
	stat := f.sub("stat")
	walletTags := f.sub("wallet_tags_stat", "walletTagsStat")
	dev := f.sub("dev")
	pool := f.sub("pool")
	price := f.sub("price")

	// ---- 身份与规模 ----
	if raw, _, ok := f.pick("address"); ok {
		var s string
		if decode(raw, &s) {
			rep.ContractAddress = strings.ToLower(strings.TrimSpace(s))
		}
	}
	if raw, _, ok := f.pick("symbol"); ok {
		decode(raw, &rep.Symbol)
	}
	if raw, _, ok := f.pick("name"); ok {
		decode(raw, &rep.Name)
	}

	// 持币人数:顶层与 stat 里都有,优先顶层
	if n, ok := pickInt64(f, "holder_count", "holderCount"); ok {
		rep.HolderCount = n
		rep.hasHolderCount = true
	} else if n, ok := pickInt64(stat, "holder_count", "holderCount"); ok {
		rep.HolderCount = n
		rep.hasHolderCount = true
	}

	// 市值 = 价格 × 流通供应量。接口不直接给市值。
	priceVal, okPrice := pickFloat(price, "price")
	supply, okSupply := pickFloat(f, "circulating_supply", "circulatingSupply")
	if !okSupply {
		supply, okSupply = pickFloat(f, "total_supply", "totalSupply")
	}
	if okPrice && okSupply {
		rep.MarketCap = priceVal * supply
		rep.hasMarketCap = true
	}

	if v, ok := pickFloat(f, "liquidity"); ok {
		rep.Liquidity = v
	} else if v, ok := pickFloat(pool, "liquidity"); ok {
		rep.Liquidity = v
	}

	// ---- 发行状态 ----
	if v, ok := pickInt64(f, "launchpad_status", "launchpadStatus"); ok {
		rep.LaunchpadStatus = int(v)
	}
	if v, ok := pickFloat(f, "launchpad_progress", "launchpadProgress"); ok {
		rep.LaunchpadProgress = v
	}
	if v, ok := pickInt64(f, "creation_timestamp", "creationTimestamp"); ok {
		rep.CreatedTimestamp = v
	}
	if raw, _, ok := f.pick("migrated_pool", "migratedPool"); ok {
		decode(raw, &rep.MigratedPool)
	}
	if v, ok := pickFloat(f, "migration_market_cap", "migrationMarketCap"); ok {
		rep.MigrationMarketCap = v
	}

	// ---- 聚合比率 ----
	if v, ok := pickRate(stat, "top_10_holder_rate", "top10_holder_rate", "top10HolderRate"); ok {
		rep.Top10Rate = v
		rep.hasTop10Rate = true
	} else if v, ok := pickRate(dev, "top_10_holder_rate", "top10HolderRate"); ok {
		rep.Top10Rate = v
		rep.hasTop10Rate = true
	}

	if v, ok := pickRate(stat, "top_bundler_trader_percentage", "topBundlerTraderPercentage"); ok {
		rep.BundlerRate = v
	}
	if v, ok := pickRate(stat, "creator_hold_rate", "creatorHoldRate"); ok {
		rep.CreatorRate = v
	}
	if v, ok := pickRate(stat, "top_rat_trader_percentage", "topRatTraderPercentage"); ok {
		rep.RatTraderRate = v
	}
	if v, ok := pickRate(stat, "fresh_wallet_rate", "freshWalletRate"); ok {
		rep.FreshWalletRate = v
	}
	// 接口直接给出的狙击持仓占比,不必从明细凑
	if v, ok := pickRate(stat, "top70_sniper_hold_rate", "top70SniperHoldRate"); ok {
		rep.SniperHoldRate = v
	}

	// ---- 钱包画像计数 ----
	if n, ok := pickInt64(walletTags, "sniper_wallets", "sniperWallets"); ok {
		rep.SniperWallets = n
		rep.hasSniperWallets = true
	}
	if n, ok := pickInt64(walletTags, "bundler_wallets", "bundlerWallets"); ok {
		rep.BundlerWallets = n
	}
	if n, ok := pickInt64(walletTags, "smart_wallets", "smartWallets"); ok {
		rep.SmartWallets = n
	}

	// ---- 创建者 ----
	if raw, _, ok := dev.pick("creator_address", "creatorAddress"); ok {
		decode(raw, &rep.CreatorAddress)
	}
	if raw, _, ok := dev.pick("creator_token_status", "creatorTokenStatus"); ok {
		decode(raw, &rep.CreatorTokenStatus)
	}

	// ---- 安全检测 ----
	// is_honeypot 返回的是字符串 "yes"/"no"(非 BSC/Base 链为空串),
	// flexBool 会把 "yes" 解成 true、空串解成 false,正好合用。
	if raw, _, ok := f.pick("is_honeypot", "isHoneypot", "honeypot"); ok {
		var v flexBool
		decode(raw, &v)
		rep.IsHoneypot = bool(v)
	}
	if v, ok := pickRate(f, "buy_tax", "buyTax"); ok {
		rep.BuyTax = v
	}
	if v, ok := pickRate(f, "sell_tax", "sellTax"); ok {
		rep.SellTax = v
	}
	if raw, _, ok := f.pick("is_open_source", "isOpenSource", "open_source"); ok {
		var v flexBool
		decode(raw, &v)
		rep.IsOpenSource = bool(v)
	}
	if raw, _, ok := f.pick("is_renounced", "isRenounced", "renounced"); ok {
		var v flexBool
		decode(raw, &v)
		rep.IsRenounced = bool(v)
	}

	// ---- 持币明细 ----
	rep.Holders = parseHolders(f)

	return rep, nil
}

// MarshalTokenReport 把报告序列化成 ParseTokenReport 能读回的合并形态。
//
// 与 ParseTokenReport 互为逆操作,所以 -dump-raw 落盘的文件可以直接用
// -fixtures 读回,形成"实测 → 校准字段名 → 复现"的闭环,中间不需要任何
// 手工转换。
func MarshalTokenReport(rep *GMGNTokenReport) []byte {
	if rep == nil {
		return nil
	}
	doc := map[string]json.RawMessage{}
	if !isJSONNull(rep.RawInfo) {
		doc["info"] = rep.RawInfo
	}
	if !isJSONNull(rep.RawSecurity) {
		doc["security"] = rep.RawSecurity
	}
	if !isJSONNull(rep.RawHolders) {
		doc["holders"] = rep.RawHolders
	}
	if len(doc) == 0 {
		return nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return out
}

// finish 在合并完成后计算派生指标、汇总缺失项。
//
// 必须在所有子文档合并完之后调用:单个子文档只带一部分字段,
// 在子文档阶段判定"缺什么"会把别的文档负责的字段也算成缺失。
func (rep *GMGNTokenReport) finish() {
	for _, h := range rep.Holders {
		rep.Coverage += h.AmountRate
		if h.IsSniper {
			rep.SniperAmountRate += h.AmountRate
		}
	}

	if rep.hasHolderCount && rep.HolderCount > 0 {
		rep.SniperCountRate = float64(rep.SniperWallets) / float64(rep.HolderCount) * 100
	}

	// 量纲自检:聚合值与明细求和矛盾时,只可能是量纲或字段名错了。
	//
	// 对照的必须是**剔除销毁/LP 之后**的明细——接口的 top_10_holder_rate 就是
	// 这个口径。实测确认:某币的销毁地址独占 93.75%,而接口给的 top10 只有
	// 3.35%,恰好等于"仅普通钱包前 10 名之和"(3.3473%)。拿全部明细去对账
	// 会把正常数据误报成量纲错误。
	//
	// 阈值取 20% 相对偏差,给"明细未覆盖全"留出空间。
	if rep.Top10Rate > 0 && len(rep.Holders) > 0 {
		ex := excludeNonWallets(rep.Holders, "")
		detailSum := 0.0
		for i, h := range ex.kept {
			if i >= 10 {
				break
			}
			detailSum += h.AmountRate
		}
		if relDiff(rep.Top10Rate, detailSum) > 0.2 {
			rep.RateSuspicious = true
		}
	}

	var missing []string
	if !rep.hasHolderCount {
		missing = append(missing, "holder_count")
	}
	if !rep.hasMarketCap {
		missing = append(missing, "market_cap")
	}
	if !rep.hasSniperWallets {
		missing = append(missing, "sniper_wallets")
	}
	if !rep.hasTop10Rate {
		missing = append(missing, "top_10_holder_rate")
	}
	if len(rep.Holders) == 0 {
		// 条件 3 完全依赖明细,没有它就不算"字段齐全"
		missing = append(missing, "holders")
	}

	rep.Missing = missing
	rep.Complete = len(missing) == 0
}

// parseHolders 从字段表里取出持币者列表。
//
// 列表键名同样未实测,按可能性从高到低尝试。
func parseHolders(f gmgnFields) []GMGNHolder {
	listKeys := []string{"holders", "list", "items", "top_holders", "topHolders", "holder_list"}

	raw, _, ok := f.pick(listKeys...)
	if ok {
		return decodeHolders(raw)
	}
	// 明细也可能嵌在某个子对象里
	for _, container := range []string{"data", "holders_data"} {
		sub := f.sub(container)
		if sub == nil {
			continue
		}
		if raw, _, ok := sub.pick(listKeys...); ok {
			return decodeHolders(raw)
		}
	}
	return nil
}

func decodeHolders(raw json.RawMessage) []GMGNHolder {
	var items []gmgnFields
	if !decode(raw, &items) {
		return nil
	}
	out := make([]GMGNHolder, 0, len(items))
	for _, it := range items {
		out = append(out, parseHolder(it))
	}
	return out
}

// parseHolder 解析单个持币者条目。
func parseHolder(f gmgnFields) GMGNHolder {
	var h GMGNHolder

	if raw, _, ok := f.pick("address", "wallet_address", "walletAddress",
		"holder_address", "holderAddress", "addr", "wallet"); ok {
		var s string
		if decode(raw, &s) {
			h.Address = strings.ToLower(strings.TrimSpace(s))
		}
	}

	if raw, _, ok := f.pick("amount_percentage", "amountPercentage", "amount_percent",
		"amountPercent", "percentage", "percent", "rate", "ratio"); ok {
		var v flexFloat
		decode(raw, &v)
		h.AmountRate = normalizeRate(float64(v))
	}

	if raw, _, ok := f.pick("addr_type", "addrType", "address_type", "addressType"); ok {
		var v flexInt
		decode(raw, &v)
		h.AddrType = int(v)
	}

	if raw, _, ok := f.pick("exchange"); ok {
		decode(raw, &h.Exchange)
	}

	if raw, _, ok := f.pick("maker_token_tags", "makerTokenTags", "maker_tags", "makerTags"); ok {
		decode(raw, &h.MakerTags)
	}
	if raw, _, ok := f.pick("tags", "wallet_tags", "walletTags"); ok {
		decode(raw, &h.Tags)
	}

	if raw, _, ok := f.pick("is_on_curve", "isOnCurve"); ok {
		var v flexBool
		decode(raw, &v)
		h.IsOnCurve = bool(v)
	}
	if raw, _, ok := f.pick("transfer_in", "transferIn"); ok {
		var v flexBool
		decode(raw, &v)
		h.TransferIn = bool(v)
	}

	for _, t := range append(append([]string{}, h.MakerTags...), h.Tags...) {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "sniper":
			h.IsSniper = true
		}
	}
	return h
}

// unwrapEnvelope 从响应体中取出承载数据的对象。
//
// 依次尝试 data → result → 裸对象。这是信封形态未实测时的第二道防线:
// 无论接口返回 {code,msg,data}、{code,result} 还是直接把数据放在顶层,都能落地。
func unwrapEnvelope(body []byte) (json.RawMessage, error) {
	var env struct {
		Code    flexInt         `json:"code"`
		Msg     string          `json:"msg"`
		Message string          `json:"message"`
		Error   string          `json:"error"`
		Data    json.RawMessage `json:"data"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("解析 gmgn 响应失败: %w\n响应片段: %s",
			err, truncate(string(body), 300))
	}

	if env.Code != 0 {
		msg := env.Msg
		if msg == "" {
			msg = env.Message
		}
		if msg == "" {
			msg = env.Error
		}
		return nil, &APIError{
			StatusCode: int(env.Code),
			Body:       fmt.Sprintf("code=%d %s", int64(env.Code), msg),
			ResetAt:    parseResetAt("", body),
		}
	}

	if !isJSONNull(env.Data) {
		return env.Data, nil
	}
	if !isJSONNull(env.Result) {
		return env.Result, nil
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// 取值辅助
// ---------------------------------------------------------------------------

func pickFloat(f gmgnFields, keys ...string) (float64, bool) {
	if f == nil {
		return 0, false
	}
	raw, _, ok := f.pick(keys...)
	if !ok {
		return 0, false
	}
	var v flexFloat
	if !decode(raw, &v) {
		return 0, false
	}
	return float64(v), true
}

func pickInt64(f gmgnFields, keys ...string) (int64, bool) {
	if f == nil {
		return 0, false
	}
	raw, _, ok := f.pick(keys...)
	if !ok {
		return 0, false
	}
	var v flexInt
	if !decode(raw, &v) {
		return 0, false
	}
	return int64(v), true
}

// pickRate 取值并归一化为百分数。
func pickRate(f gmgnFields, keys ...string) (float64, bool) {
	v, ok := pickFloat(f, keys...)
	if !ok {
		return 0, false
	}
	return normalizeRate(v), true
}

// normalizeRate 把接口的 0~1 小数统一成百分数。
//
// 接口文档明确这类字段是 0~1(如 top_10_holder_rate 与 amount_percentage,
// "0.05 = 5%"),但不同字段仍可能混用小数与百分数,因此既不能无条件 *100
// 也不能原样不动。
//
// 判据取"严格小于 1 视为小数":top10 占比作为小数不可能等于 1(那意味着
// top10 持有 100%),而作为百分数等于 1 就是 1%,是常见值。所以边界值 1.0
// 按百分数解释更安全。ParseTokenReport 里的 RateSuspicious 交叉校验会兜住
// 这类误判。
func normalizeRate(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v < 1.0 {
		return v * 100
	}
	return v
}

// relDiff 返回两个数的相对差异(以 a 为基准)。
func relDiff(a, b float64) float64 {
	if a == 0 {
		return 0
	}
	d := (a - b) / a
	if d < 0 {
		d = -d
	}
	return d
}
