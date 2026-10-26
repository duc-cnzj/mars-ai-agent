package main

import (
	"context"
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

// healthOK 实现 curl -f 的语义：状态码 < 400 视为成功。
func healthOK(ctx context.Context, client *http.Client, rawURL string) error {
	code, _, err := doGet(ctx, client, rawURL)
	if err != nil {
		return err
	}
	if code >= http.StatusBadRequest {
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
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
