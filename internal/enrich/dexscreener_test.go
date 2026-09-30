package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dexBody 是响应夹具。
//
// 与 gmgn 的夹具不同,这份**是实测结构**:字段名与类型取自 2026-09-26 的
// 真实响应(liquidity.usd 与 marketCap 是数字,priceUsd 是字符串)。
// 但数值是构造的,只用来验证解析与分流逻辑。
const dexBody = `{
  "schemaVersion": "1.0.0",
  "pairs": [
    {
      "chainId": "bsc",
      "dexId": "pancakeswap",
      "pairAddress": "0xAAA",
      "baseToken": {"address": "0xTokenA", "symbol": "AAA"},
      "quoteToken": {"address": "0xWBNB", "symbol": "WBNB"},
      "priceUsd": "0.0000137",
      "liquidity": {"usd": 13730.5, "base": 100, "quote": 200},
      "marketCap": 13730
    },
    {
      "chainId": "bsc",
      "dexId": "pancakeswap",
      "pairAddress": "0xBBB",
      "baseToken": {"address": "0xTokenA", "symbol": "AAA"},
      "quoteToken": {"address": "0xUSDT", "symbol": "USDT"},
      "priceUsd": "0.0000138",
      "liquidity": {"usd": 99000.0, "base": 1, "quote": 1},
      "marketCap": 13800
    },
    {
      "chainId": "bsc",
      "dexId": "pancakeswap",
      "pairAddress": "0xCCC",
      "baseToken": {"address": "0xTokenB", "symbol": "BBB"},
      "quoteToken": {"address": "0xTokenQ", "symbol": "QQQ"},
      "priceUsd": "1.0",
      "liquidity": {"usd": 55555.0, "base": 1, "quote": 1},
      "marketCap": 999
    }
  ]
}`

func newTestDex(t *testing.T, handler http.HandlerFunc) *DexScreener {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewDexScreener(srv.URL, DexScreenerMaxBatch, 1)
}

func TestDexTokensParsesAndPicksDeepestPool(t *testing.T) {
	var gotPath, gotUA string
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(dexBody))
	})

	pairs, err := d.Tokens(context.Background(), []string{"0xTokenA"})
	if err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}

	// 同一个币有两个池子(流动性 13730 与 99000),必须取深的那个
	p, ok := pairs["0xtokena"]
	if !ok {
		t.Fatalf("没有解析出 0xTokenA,得到 %v", pairs)
	}
	if p.LiquidityUSD != 99000.0 {
		t.Errorf("流动性 = %v,期望取最大的 99000", p.LiquidityUSD)
	}
	if p.PairAddress != "0xBBB" {
		t.Errorf("pair = %s,期望 0xBBB(流动性最大的那个)", p.PairAddress)
	}
	if p.PriceUSD != 0.0000138 {
		t.Errorf("价格 = %v,期望 0.0000138(priceUsd 是字符串,要能解析)", p.PriceUSD)
	}

	if !strings.Contains(gotPath, "/latest/dex/tokens/0xTokenA") {
		t.Errorf("请求路径 = %s,期望含 /latest/dex/tokens/0xTokenA", gotPath)
	}
	// UA 必须显式设置:Go 默认 UA 会被 Cloudflare 拦(实测 rpc.48.club 就是这样)
	if gotUA != userAgent {
		t.Errorf("User-Agent = %q,期望 %q", gotUA, userAgent)
	}
}

// TestDexTokensMatchesQuoteSide 守住一个**真实漏币的 bug**(2026-09-26):
// 新币可能出现在池子的 **quote** 侧——flap 平台的池子是 "新币/wPOPMTx"
// 这种形状(代币化股票当计价币),新币在 quote 那一侧。
//
// 原先只认 baseToken,于是这类币全部被判成"查不到交易对"而淘汰。
// 实测漏掉了 0x9803b9e4...7777(`无用`,持币 416、已毕业,四项全合格)。
//
// 这个用例曾经写反过(断言 quote 侧必须被忽略),所以夹具里的 0xTokenQ
// 只出现在 quoteToken 位置,是专门为这条回归准备的。
func TestDexTokensMatchesQuoteSide(t *testing.T) {
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(dexBody))
	})

	pairs, err := d.Tokens(context.Background(), []string{"0xTokenQ"})
	if err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}
	p, ok := pairs["0xtokenq"]
	if !ok {
		t.Fatal("quote 侧的代币没被匹配到——这正是漏掉 flap 平台新币的那个 bug")
	}
	if p.PairAddress != "0xCCC" {
		t.Errorf("pair = %s,期望 0xCCC", p.PairAddress)
	}
	if p.LiquidityUSD != 55555.0 {
		t.Errorf("流动性 = %v,期望 55555", p.LiquidityUSD)
	}

	// base 侧的匹配不能被这次改动破坏
	pairsB, err := d.Tokens(context.Background(), []string{"0xTokenB"})
	if err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}
	if pb, ok := pairsB["0xtokenb"]; !ok || pb.PairAddress != "0xCCC" {
		t.Errorf("base 侧匹配被破坏了: %+v", pairsB)
	}
}

// TestDexTokensBothSidesPickDeepest 验证一个币同时出现在两侧多个池子时,
// 仍然按流动性取最大的那个,而不是被某一侧覆盖。
func TestDexTokensBothSidesPickDeepest(t *testing.T) {
	body := `{"pairs":[
	  {"chainId":"bsc","pairAddress":"0xShallow","baseToken":{"address":"0xMe"},
	   "quoteToken":{"address":"0xWBNB"},"liquidity":{"usd":10}},
	  {"chainId":"bsc","pairAddress":"0xDeep","baseToken":{"address":"0xStock"},
	   "quoteToken":{"address":"0xMe"},"liquidity":{"usd":90000}}
	]}`
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	pairs, err := d.Tokens(context.Background(), []string{"0xMe"})
	if err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}
	p, ok := pairs["0xme"]
	if !ok {
		t.Fatal("没有匹配到 0xMe")
	}
	if p.PairAddress != "0xDeep" {
		t.Errorf("pair = %s,期望 0xDeep(跨两侧也要取最深的)", p.PairAddress)
	}
}

// TestDexTokensEmptyPairsIsNotError 守住一个关键语义:
// 查不到任何交易对时接口返回 HTTP 200 + 空数组——这正是"这个币没有真实流动性"
// 的信号,是我们要的结果,绝不能当成调用失败。
func TestDexTokensEmptyPairsIsNotError(t *testing.T) {
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"schemaVersion":"1.0.0","pairs":[]}`))
	})

	pairs, err := d.Tokens(context.Background(), []string{"0xNothing"})
	if err != nil {
		t.Fatalf("空 pairs 不该报错,得到: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("期望空结果,得到 %v", pairs)
	}
}

func TestDexTokensHTTPErrorIsError(t *testing.T) {
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})

	if _, err := d.Tokens(context.Background(), []string{"0xAny"}); err == nil {
		t.Fatal("HTTP 500 应当报错")
	}
}

// TestDexTokensSplitsIntoBatches 验证超过单批上限时会切分。
func TestDexTokensSplitsIntoBatches(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"pairs":[]}`))
	}))
	defer srv.Close()

	d := NewDexScreener(srv.URL, 2, 1) // 每批 2 个
	addrs := []string{"0xa", "0xb", "0xc", "0xd", "0xe"}
	if _, err := d.Tokens(context.Background(), addrs); err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}
	// 5 个地址按 2 切分应为 3 批
	if n := calls.Load(); n != 3 {
		t.Errorf("请求次数 = %d,期望 3(5 个地址按每批 2 个切)", n)
	}
}

// TestDexTokensRespectsContext 验证 ctx 取消能立刻返回,不会白等节流。
func TestDexTokensRespectsContext(t *testing.T) {
	d := NewDexScreener("http://127.0.0.1:1", DexScreenerMaxBatch, 60_000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := d.Tokens(ctx, []string{"0xa"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("ctx 已取消,应当返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后没有立即返回")
	}
}

// TestLiquidityFilter 覆盖分流规则的边界。
//
// 注意 0xempty 与 0xnodata 的区别,这正是本功能的核心:
//
//	0xempty   对方**明确说**流动性是 0        → 空池,淘汰
//	0xnodata  对方**根本没给**流动性字段      → 数据缺口,放行
//
// 两者在 JSON 反序列化后都可能是 0,靠 HasLiquidity 才能分开。
func TestLiquidityFilter(t *testing.T) {
	pairs := map[string]DexPair{
		"0xdeep":   {LiquidityUSD: 13730, HasLiquidity: true},
		"0xedge":   {LiquidityUSD: 1, HasLiquidity: true},   // 刚好等于门槛:应当保留
		"0xempty":  {LiquidityUSD: 0, HasLiquidity: true},   // 明确是空池:淘汰
		"0xdust":   {LiquidityUSD: 0.5, HasLiquidity: true}, // 有池子但形同空池:淘汰
		"0xnodata": {HasLiquidity: false},                   // 有交易对但对方没给流动性
	}
	addrs := []string{"0xdeep", "0xedge", "0xempty", "0xdust", "0xnodata", "0xmissing"}

	kept, dropped, unknown := LiquidityFilter(addrs, pairs, 1)

	wantKept := map[string]bool{"0xdeep": true, "0xedge": true, "0xnodata": true}
	if len(kept) != len(wantKept) {
		t.Fatalf("保留 %v,期望 %v", kept, wantKept)
	}
	for _, a := range kept {
		if !wantKept[a] {
			t.Errorf("%s 不该被保留", a)
		}
	}
	// 0xmissing 代表"接口完全不认识这个币",这才是可靠的淘汰信号
	wantDropped := map[string]bool{"0xempty": true, "0xdust": true, "0xmissing": true}
	if len(dropped) != len(wantDropped) {
		t.Fatalf("淘汰 %v,期望 %v", dropped, wantDropped)
	}
	for _, a := range dropped {
		if !wantDropped[a] {
			t.Errorf("%s 不该被淘汰", a)
		}
	}
	// 数据缺口必须单独计数,否则观察不到对方缺口的规模
	if len(unknown) != 1 || unknown[0] != "0xnodata" {
		t.Errorf("未知流动性应当只有 0xnodata,实际 %v", unknown)
	}
}

// TestDexTokensMarksMissingLiquidity 覆盖"响应里有交易对、但没有 liquidity 字段"。
//
// 这是实测撞到的真实形态(2026-09-29):0x9803b9e4…7777(`无用`)的交易对在
// DexScreener 上有 24 小时 540 笔成交,却完全没有 liquidity 字段,而 volume
// 全是 0——是对方索引残缺,不是真的没有池子。同一时刻 GMGN 给的流动性是 $13,786。
//
// 不把这种情况与"流动性为 0"分开,就会把一个四项全合格的币当空池淘汰掉。
func TestDexTokensMarksMissingLiquidity(t *testing.T) {
	const addr = "0x9803b9e4536e2ffdfcd745756f25243d82ad7777"
	body := `{"schemaVersion":"1.0.0","pairs":[{
		"chainId":"bsc","dexId":"pancakeswap","pairAddress":"0xpair",
		"baseToken":{"address":"0x4ebf5fd25b02022afad96e2fa25da54a246fded0"},
		"quoteToken":{"address":"` + addr + `"},
		"priceNative":"1039674.1362",
		"txns":{"h24":{"buys":261,"sells":279}},
		"volume":{"h24":0,"h6":0,"h1":0,"m5":0}
	}]}`

	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	pairs, err := d.Tokens(context.Background(), []string{addr})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	p, ok := pairs[addr]
	if !ok {
		t.Fatal("应当能从 quoteToken 侧认出这个币")
	}
	if p.HasLiquidity {
		t.Error("响应里没有 liquidity 字段,HasLiquidity 应当为 false")
	}

	// 分流结果:必须放行,不能当空池
	kept, dropped, unknown := LiquidityFilter([]string{addr}, pairs, 1)
	if len(kept) != 1 || len(dropped) != 0 {
		t.Errorf("缺流动性字段应当放行,实际保留 %v / 淘汰 %v", kept, dropped)
	}
	if len(unknown) != 1 {
		t.Errorf("应当计入未知,实际 %v", unknown)
	}
}

// TestDexTokensZeroLiquidityIsNotUnknown 确认"明确给了 0"仍然会被淘汰。
//
// 与上一条互为对照:放行只针对**字段缺失**,不能把真正的空池也放进来,
// 否则预筛就白做了。
func TestDexTokensZeroLiquidityIsNotUnknown(t *testing.T) {
	const addr = "0x0000000000000000000000000000000000000001"
	body := `{"schemaVersion":"1.0.0","pairs":[{
		"chainId":"bsc","pairAddress":"0xpair",
		"baseToken":{"address":"` + addr + `"},
		"quoteToken":{"address":"0xwbnb"},
		"liquidity":{"usd":0,"base":0,"quote":0}
	}]}`

	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	pairs, err := d.Tokens(context.Background(), []string{addr})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !pairs[addr].HasLiquidity {
		t.Error("响应里有 liquidity 对象,HasLiquidity 应当为 true")
	}
	kept, dropped, unknown := LiquidityFilter([]string{addr}, pairs, 1)
	if len(kept) != 0 || len(dropped) != 1 {
		t.Errorf("流动性为 0 应当淘汰,实际保留 %v / 淘汰 %v", kept, dropped)
	}
	if len(unknown) != 0 {
		t.Errorf("这不是数据缺口,不该计入未知,实际 %v", unknown)
	}
}

// TestDexTokensKeyCaseInsensitive 验证大小写混写的地址能对上。
// 链上地址大小写是校验和形式,而接口返回的可能是小写,不做归一化会全部查不到。
func TestDexTokensKeyCaseInsensitive(t *testing.T) {
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(dexBody))
	})
	pairs, err := d.Tokens(context.Background(), []string{"0xTOKENA"})
	if err != nil {
		t.Fatalf("Tokens 失败: %v", err)
	}
	if _, ok := pairs["0xtokena"]; !ok {
		t.Errorf("大写输入没有归一化到小写键,得到 %v", pairs)
	}
}

// TestDexTokensTruncatedBodyIsError 验证响应被截断时报错而不是给出零值。
func TestDexTokensTruncatedBodyIsError(t *testing.T) {
	d := newTestDex(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pairs":[{"chainId":`))
	})
	if _, err := d.Tokens(context.Background(), []string{"0xa"}); err == nil {
		t.Fatal("截断的 JSON 应当报错")
	}
}
