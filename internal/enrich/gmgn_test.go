package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 最小的合法响应体,三个端点各一份。字段名与真实接口一致,
// 但值刻意取"能过判定"的极端简单值——这里测的是传输层,不是判定。
const (
	fakeInfoBody     = `{"code":0,"data":{"address":"0xabc","holder_count":500,"circulating_supply":1000000000,"price":{"price":0.00001},"launchpad_status":2,"stat":{"top_10_holder_rate":0.1},"wallet_tags_stat":{"sniper_wallets":5}}}`
	fakeSecurityBody = `{"code":0,"data":{"is_honeypot":"no","buy_tax":0,"sell_tax":0,"is_open_source":true,"is_renounced":true}}`
	fakeHoldersBody  = `{"code":0,"data":{"holders":[{"address":"0xaaa","amount_percentage":0.01,"addr_type":0}]}}`
)

// newTestClient 起一个假 GMGN 服务并把客户端指向它,同时关掉节流与退避。
//
// 直接改字段而不是传参:NewGMGN 把 0 解释为"用默认值"(节流 1200ms),
// 测试里必须显式归零,否则每个用例都要白等。
func newTestClient(t *testing.T, h http.HandlerFunc) *GMGN {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	g := NewGMGN(srv.URL, "test-key", "bsc", 100, 1)
	if g == nil {
		t.Fatal("构造客户端失败")
	}
	g.minInterval = 0
	g.backoffBase = 0
	return g
}

// serveAll 对三个端点都返回正常响应。
func serveAll(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case gmgnInfoPath:
		w.Write([]byte(fakeInfoBody))
	case gmgnSecurityPath:
		w.Write([]byte(fakeSecurityBody))
	case gmgnHoldersPath:
		w.Write([]byte(fakeHoldersBody))
	default:
		http.NotFound(w, r)
	}
}

// TestNewGMGNReturnsNilWithoutKey 保证缺 key 时返回 nil 而不是一个会 401 的客户端。
//
// 调用方据此跳过整个筛选,与 NewSerper 的行为一致。
func TestNewGMGNReturnsNilWithoutKey(t *testing.T) {
	if g := NewGMGN("http://x", "", "bsc", 100, 0); g != nil {
		t.Errorf("空 key 应返回 nil,实际 %+v", g)
	}
	if g := NewGMGN("http://x", "   ", "bsc", 100, 0); g != nil {
		t.Errorf("纯空白 key 应返回 nil,实际 %+v", g)
	}
}

// TestDefaultBaseURLIsOpenAPI 锁死默认域名。
//
// 这里踩过一次坑:早期版本的默认值是 api.gmgn.ai,而那是个**不存在的域名**
// (DNS 解析返回的是污染地址),症状是所有请求都超时,看起来像被墙。
// 真实域名是 openapi.gmgn.ai——取自官方 CLI 源码的常量,已用真实请求验证过
// (三个端点都返回 401 AUTH_INVALID,说明路径正确、只差鉴权)。
//
// 这条测试让那个错误无法悄悄回来。
func TestDefaultBaseURLIsOpenAPI(t *testing.T) {
	g := NewGMGN("", "k", "bsc", 100, 1000)
	if g == nil {
		t.Fatal("构造失败")
	}

	for name, u := range map[string]string{
		"info":     g.InfoURL("0xabc"),
		"security": g.SecurityURL("0xabc"),
		"holders":  g.HoldersURL("0xabc"),
	} {
		if !strings.HasPrefix(u, "https://openapi.gmgn.ai/") {
			t.Errorf("%s 的默认域名不对: %s", name, u)
		}
		if strings.Contains(u, "//api.gmgn.ai") {
			t.Errorf("%s 用了不存在的域名 api.gmgn.ai: %s", name, u)
		}
	}

	// 三个路径也要锁住
	for path, got := range map[string]string{
		gmgnInfoPath:     g.InfoURL("0xabc"),
		gmgnSecurityPath: g.SecurityURL("0xabc"),
		gmgnHoldersPath:  g.HoldersURL("0xabc"),
	} {
		if !strings.Contains(got, path) {
			t.Errorf("路径 %s 未出现在 %s", path, got)
		}
	}
}

// TestGMGNRequestShape 锁定三个端点的路径、链名、鉴权头与分页参数。
func TestGMGNRequestShape(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]string{} // path -> raw query
	auth := ""

	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path] = r.URL.RawQuery
		// 鉴权头是 X-APIKEY,不是 Authorization: Bearer——用后者会得到
		// AUTH_INVALID: missing api key or client_id
		auth = r.Header.Get("X-APIKEY")
		mu.Unlock()
		serveAll(w, r)
	})

	if _, err := g.Fetch(context.Background(), "0xAbC123"); err != nil {
		t.Fatalf("抓取失败: %v", err)
	}

	for _, p := range []string{gmgnInfoPath, gmgnSecurityPath, gmgnHoldersPath} {
		q, ok := paths[p]
		if !ok {
			t.Errorf("没有请求 %s", p)
			continue
		}
		if !strings.Contains(q, "chain=bsc") {
			t.Errorf("%s 缺少 chain 参数: %q", p, q)
		}
		if !strings.Contains(q, "address=0xAbC123") {
			t.Errorf("%s 缺少 address 参数: %q", p, q)
		}
	}
	// 只有 holders 端点需要 limit
	if q := paths[gmgnHoldersPath]; !strings.Contains(q, "limit=100") {
		t.Errorf("holders 端点应带 limit: %q", q)
	}
	if q := paths[gmgnInfoPath]; strings.Contains(q, "limit=") {
		t.Errorf("info 端点不应带 limit: %q", q)
	}

	if auth != "test-key" {
		t.Errorf("X-APIKEY 头 = %q, 期望 %q", auth, "test-key")
	}
}

// TestGMGNFetchMergesDocuments 验证三个端点的数据被正确合并到一份报告里。
func TestGMGNFetchMergesDocuments(t *testing.T) {
	g := newTestClient(t, serveAll)

	rep, err := g.Fetch(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}

	// info
	if rep.HolderCount != 500 {
		t.Errorf("holder_count = %d, 期望 500", rep.HolderCount)
	}
	if !rep.hasTop10Rate {
		t.Error("top_10_holder_rate 未从 info 解出")
	}
	// security —— 只在这一个端点里有
	if rep.hasMarketCap == false {
		t.Error("market_cap 未算出")
	}
	if !rep.IsOpenSource {
		t.Errorf("security 端点的字段未被合并进来")
	}
	// holders
	if len(rep.Holders) != 1 {
		t.Errorf("持币明细 %d 条, 期望 1", len(rep.Holders))
	}
}

// TestGMGNQuotaBannedNotRetried 是需求的一部分:限流时绝不能重试。
//
// GMGN 在冷却期内重试会**延长封禁**(每次 +5 秒,上限 5 分钟),所以"只发一次"
// 不是优化而是正确性要求,必须被测试钉住。
func TestGMGNQuotaBannedNotRetried(t *testing.T) {
	var calls atomic.Int64
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":429,"error":"RATE_LIMIT_BANNED","msg":"RATE_LIMIT_BANNED"}`))
	})

	rep, err := g.Fetch(context.Background(), "0xabc")
	if err == nil {
		t.Fatalf("限流应当报错,实际返回 %+v", rep)
	}
	if !IsGMGNQuotaBanned(err) {
		t.Errorf("应被识别为限流: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("限流时绝不能重试: 实际发出 %d 次请求", n)
	}
	if got := ResetAt(err); got.Unix() != 1800000000 {
		t.Errorf("应从响应头取出 reset_at: %v", got)
	}
}

// TestGMGNQuotaBannedIn200Envelope 覆盖"HTTP 200 但业务码是限流"的形态。
//
// GMGN 会这样返回限流,只看 HTTP 状态码会漏掉,然后把它当成一份正常响应。
func TestGMGNQuotaBannedIn200Envelope(t *testing.T) {
	var calls atomic.Int64
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"code":429,"error":"RATE_LIMIT_EXCEEDED","message":"slow down","reset_at":1800000001}`))
	})

	_, err := g.Fetch(context.Background(), "0xabc")
	if err == nil {
		t.Fatal("业务码 429 应当报错")
	}
	if !IsGMGNQuotaBanned(err) {
		t.Errorf("应被识别为限流: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("限流时绝不能重试: 实际发出 %d 次请求", n)
	}
	if got := ResetAt(err); got.Unix() != 1800000001 {
		t.Errorf("应从响应体取出 reset_at: %v", got)
	}
}

// TestGMGNUnauthorized 验证 401/403 被识别,且文案能直接指导操作。
func TestGMGNUnauthorized(t *testing.T) {
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":401,"msg":"invalid api key"}`))
	})

	_, err := g.Fetch(context.Background(), "0xabc")
	if err == nil {
		t.Fatal("401 应当报错")
	}
	if !IsGMGNUnauthorized(err) {
		t.Errorf("应被识别为鉴权失败: %v", err)
	}
	if !strings.Contains(err.Error(), "GMGN_API_KEY") {
		t.Errorf("报错文案应指明是 API key 的问题: %v", err)
	}
}

// TestGMGNRetriesOn5xx 验证瞬时故障会退避重试,次数符合预期。
func TestGMGNRetriesOn5xx(t *testing.T) {
	var calls atomic.Int64
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`upstream exploded`))
	})

	if _, err := g.Fetch(context.Background(), "0xabc"); err == nil {
		t.Fatal("持续 5xx 最终应当报错")
	}
	// 首次 + gmgnMaxRetries 次重试,全发生在第一个端点(info)上
	if want := int64(gmgnMaxRetries + 1); calls.Load() != want {
		t.Errorf("5xx 应共发出 %d 次请求,实际 %d 次", want, calls.Load())
	}
}

// TestGMGN4xxNotRetried 验证请求本身的问题不做无谓重试。
func TestGMGN4xxNotRetried(t *testing.T) {
	var calls atomic.Int64
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`bad token address`))
	})

	if _, err := g.Fetch(context.Background(), "0xabc"); err == nil {
		t.Fatal("400 应当报错")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("400 不应重试: 实际发出 %d 次请求", n)
	}
}

// TestGMGNNotFoundIsNoData 验证 404 被区分成"该币无数据"而不是"调用失败"。
//
// 前者只说明这个币查不到,继续处理下一个即可;后者可能需要人工介入。
func TestGMGNNotFoundIsNoData(t *testing.T) {
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":404,"msg":"token not found"}`))
	})

	_, err := g.Fetch(context.Background(), "0xabc")
	if err == nil {
		t.Fatal("404 应当报错")
	}
	if !IsNoTokenData(err) {
		t.Errorf("404 应被识别为无数据: %v", err)
	}
	if IsGMGNQuotaBanned(err) {
		t.Errorf("404 不应被误判为限流")
	}
}

// TestGMGNNonJSONBodyErrors 保证上游返回 HTML 之类时**报错而非零值报告**。
func TestGMGNNonJSONBodyErrors(t *testing.T) {
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body>502 Bad Gateway</body></html>`))
	})

	rep, err := g.Fetch(context.Background(), "0xabc")
	if err == nil {
		t.Fatalf("非 JSON 响应应报错,实际返回 %+v", rep)
	}
	if rep != nil {
		t.Errorf("失败时必须返回 nil 报告,实际 %+v", rep)
	}
}

// TestGMGNHoldersRespectsContextCancel 验证取消后立即返回,不等退避。
func TestGMGNHoldersRespectsContextCancel(t *testing.T) {
	g := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 跟随请求的 ctx 退出,否则 httptest.Server.Close() 会一直等到这里睡完
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.Write([]byte(fakeInfoBody))
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := g.Fetch(ctx, "0xabc"); err == nil {
		t.Fatal("ctx 取消后应当报错")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ctx 取消后应立即返回,实际耗时 %v", elapsed)
	}
}

// TestIntervalForScalesByWeight 验证节流间隔按权重成比例。
//
// 所有端点共用一个间隔会让第一段(info,权重 1)白白慢五倍——而第一段恰恰
// 是要扫最多候选的那一段,它的速度直接决定整轮要跑多久。
func TestIntervalForScalesByWeight(t *testing.T) {
	// minInterval 的语义是"权重 5 的 holders 的间隔"
	g := NewGMGN("http://x", "k", "bsc", 100, 1000)
	if g == nil {
		t.Fatal("构造失败")
	}

	holders := g.intervalFor(gmgnWeightHolders)
	info := g.intervalFor(gmgnWeightInfo)

	if holders != time.Second {
		t.Errorf("holders 间隔 = %v, 期望 1s", holders)
	}
	if info != 200*time.Millisecond {
		t.Errorf("info 间隔 = %v, 期望 200ms(1s 的五分之一)", info)
	}
	if info >= holders {
		t.Errorf("权重低的端点间隔应更短: info=%v holders=%v", info, holders)
	}

	// 配成极小值时要被下界兜住,不能把限流完全关掉
	g2 := NewGMGN("http://x", "k", "bsc", 100, 1)
	if d := g2.intervalFor(gmgnWeightInfo); d < 50*time.Millisecond {
		t.Errorf("间隔应被下界兜住,实际 %v", d)
	}
}

// TestReserveEnforcesGlobalInterval 验证节流是**全局**的,与 worker 数无关。
//
// 这是最容易写错、又最难在生产里发现的地方:写成"每个 worker 各睡一会"时
// 程序不会报错,只是实际 QPS 悄悄翻了几倍,直到被封禁才暴露。
func TestReserveEnforcesGlobalInterval(t *testing.T) {
	const (
		workers   = 8
		perWorker = 10
		interval  = 10 * time.Millisecond
	)
	g := NewGMGN("http://example.invalid", "k", "bsc", 100, 10)

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				time.Sleep(g.reserve(interval))
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := workers * perWorker
	// 第 1 次不等待,其余每次各占一个间隔
	want := time.Duration(total-1) * interval
	if elapsed < want {
		t.Errorf("%d 次请求(8 并发)耗时 %v,至少应为 %v——全局节流失效",
			total, elapsed, want)
	}
}
