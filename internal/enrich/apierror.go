package enrich

import (
	"fmt"
	"strings"
	"time"
)

// APIError 表示外部服务返回了错误状态。
//
// 它带着 HTTP 状态码、原始响应体与限流解除时刻三样东西:状态码用于分类
// (鉴权失败 / 限流 / 无数据),响应体用于给人看,ResetAt 用于告诉用户
// 什么时候可以重试。
type APIError struct {
	StatusCode int
	Body       string
	// ResetAt 是限流解除时刻。为零值表示接口没给这个信息。
	ResetAt time.Time
}

func (e *APIError) Error() string {
	return fmt.Sprintf("接口返回 HTTP %d: %s", e.StatusCode, truncate(e.Body, 200))
}

// truncate 把长文本压成一行并限长,用于日志与错误信息。
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
