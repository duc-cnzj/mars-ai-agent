package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxProbeBody 限制探测响应体大小：探测接口返回很小，避免异常端点把内存吃满。
const maxProbeBody = 4 << 20

// newHTTPClient 构造探测用客户端：不跟随重定向，对齐 curl 未带 -L 的行为
// （3xx 视为最终响应，由调用方按状态码判断）。
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// doGet 发起 GET，返回状态码与响应体。仅传输错误会返回 err；
// 非 2xx/3xx 仍正常返回，由调用方决定是否视为失败。
func doGet(ctx context.Context, client *http.Client, rawURL string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// doPost 发起 POST 并丢弃响应体；非 2xx 不视为错误（与旧脚本忽略 connect 结果一致）。
func doPost(ctx context.Context, client *http.Client, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProbeBody))
	return nil
}

// healthOK 校验内核确实报告 healthy。
//
// 不能只看状态码：钉版 opencode 的 Web UI 对未知路径做 SPA 兜底并返回 200 HTML，
// 只看 <400 会把「任意 HTTP 服务」（比如一个只会返回 200 的冒名进程）误判成内核
// 就绪，K8s 探针、mai 的 attach 判断与 mcp-wait 的 server 等待都会失真。因此
// 必须同时要求响应体是 {"healthy":true}。内核真正的健康路由见 healthPath。
func healthOK(ctx context.Context, client *http.Client, rawURL string) error {
	code, body, err := doGet(ctx, client, rawURL)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d", code)
	}
	var st struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal(body, &st); err != nil || !st.Healthy {
		return fmt.Errorf("HTTP %d 但响应不是 healthy JSON: %s", code, shortBody(body))
	}
	return nil
}

// shortBody 截断错误信息里的响应体，避免把整页 HTML 灌进探针 stderr。
func shortBody(body []byte) string {
	const max = 80
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}

// realSleep 是可被测试替换的等待实现：持锁等待，不响应上下文取消的调用方自行处理。
func realSleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
