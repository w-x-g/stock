// Package chain 通过 JSON-RPC 直接读 BNB Chain 的链上数据。
//
// 为什么需要它:GMGN 的"战壕"列表是**精选榜**,实测会漏掉已毕业的币
// (例如 0x9803b9e4...7777 四个条件全过却不在其中)。而链上的
// PancakeSwap PairCreated 事件是**全量账本**,一个不漏。
package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// UserAgent 是所有 RPC 请求携带的客户端标识。
//
// 必须显式设置:Go 默认的 "Go-http-client/1.1" 会被部分公共节点的 Cloudflare
// 规则直接 403(实测 rpc.48.club:默认 UA 403,换成任意显式 UA 即 200)。
// 这里用自述式标识而不是伪装浏览器——节点运营者有权知道请求来自谁。
const UserAgent = "stock-bsc-indexer/1.0"

// RPC 是一个支持多端点轮询的 JSON-RPC 客户端。
//
// 多端点是为了分摊单个节点的速率限制——扫 5 天要发上百次 getLogs,
// 单端点很容易被打满。
type RPC struct {
	endpoints []string
	client    *http.Client
	rr        atomic.Uint64
}

// New 构造客户端。endpoints 至少一个。
func New(endpoints []string) (*RPC, error) {
	clean := endpoints[:0:0]
	for _, e := range endpoints {
		if s := trimSpace(e); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("至少要配置一个 RPC 端点")
	}
	return &RPC{
		endpoints: clean,
		client:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// Endpoints 返回端点个数,供日志使用。
func (c *RPC) Endpoints() int { return len(c.endpoints) }

// next 轮询取下一个端点。
func (c *RPC) next() string {
	return c.endpoints[c.rr.Add(1)%uint64(len(c.endpoints))]
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpcMaxAttempts 是同一请求的轮次上限。每一轮会把所有端点各试一次。
//
// 需要它是因为长任务里**瞬时网络错误**很常见(实测遇到过节点直接 EOF 断开)。
// 早先只依赖"区间拆分"来容错,结果一个 EOF 被拆成单块仍然失败,整轮扫描中止。
const rpcMaxAttempts = 3

// call 发一次请求,带轮次重试与端点轮询。
//
// 两层容错:内层轮询所有端点(对付单点限流/抽风),外层退避重试(对付瞬时断连)。
func (c *RPC) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt < rpcMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<uint(attempt-1)) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		for i := 0; i < len(c.endpoints); i++ {
			ep := c.next()
			raw, err := c.callOnce(ctx, ep, method, params...)
			if err == nil {
				return raw, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%s 重试 %d 轮 × %d 个端点后仍失败: %w",
		method, rpcMaxAttempts, len(c.endpoints), lastErr)
}

func (c *RPC) callOnce(ctx context.Context, endpoint, method string, params ...any) (json.RawMessage, error) {
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed rpcResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if parsed.Error != nil {
		// 节点的错误文案要原样带出来:getLogs 的超限提示里含有可用的区间上限
		return nil, fmt.Errorf("节点返回错误 %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	return parsed.Result, nil
}

// BlockNumber 取当前链头高度。
func (c *RPC) BlockNumber(ctx context.Context) (uint64, error) {
	raw, err := c.call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	return parseHexUint(raw)
}

// BlockTime 取某个区块的时间戳(Unix 秒)。
func (c *RPC) BlockTime(ctx context.Context, block uint64) (int64, error) {
	raw, err := c.call(ctx, "eth_getBlockByNumber", hexUint(block), false)
	if err != nil {
		return 0, err
	}
	var blk struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &blk); err != nil {
		return 0, fmt.Errorf("解析区块失败: %w", err)
	}
	n, err := strconv.ParseInt(trimHex(blk.Timestamp), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("解析时间戳失败: %w", err)
	}
	return n, nil
}

// Log 是一条链上日志,只保留我们需要的字段。
type Log struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	TxHash      string   `json:"transactionHash"`
}

// GetLogs 拉取一段区块区间内、指定合约与 topic0 的日志。
func (c *RPC) GetLogs(ctx context.Context, address, topic0 string, from, to uint64) ([]Log, error) {
	filter := map[string]any{
		"address":   address,
		"topics":    []any{topic0},
		"fromBlock": hexUint(from),
		"toBlock":   hexUint(to),
	}
	raw, err := c.call(ctx, "eth_getLogs", filter)
	if err != nil {
		return nil, err
	}
	var logs []Log
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, fmt.Errorf("解析日志失败: %w", err)
	}
	return logs, nil
}

// ---------------------------------------------------------------------------
// 十六进制辅助
// ---------------------------------------------------------------------------

func hexUint(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func trimHex(s string) string {
	if len(s) > 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

func parseHexUint(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("解析十六进制失败: %w", err)
	}
	return strconv.ParseUint(trimHex(s), 16, 64)
}

// ParseHexUint 供包外使用(解码日志里的区块号等)。
func ParseHexUint(s string) (uint64, error) { return strconv.ParseUint(trimHex(s), 16, 64) }

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
