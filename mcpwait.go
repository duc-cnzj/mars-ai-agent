package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// defaultMCPWaitTimeout 与旧 scripts/mcp-wait.sh 的默认值一致。
const defaultMCPWaitTimeout = 15

var (
	// errMCPServerUnreachable 表示内核 server 尚不可达（对应旧脚本退出码 2）。
	errMCPServerUnreachable = errors.New("mcp: server unreachable")
	// errMCPNotReady 表示 server 可达但 MCP 未在预算内 connected（对应旧脚本退出码 1）。
	errMCPNotReady = errors.New("mcp: not ready")
)

// mcpStatus 是 opencode GET /mcp 返回的单个服务状态。
type mcpStatus struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// parseMCPWaitArgs 解析 mars-mcp-wait 的入参。刻意手写而不用 flag 包：
// 旧脚本只接受 `--dir D` 形式，不接受 `--dir=D` 或 `--`。
func parseMCPWaitArgs(args []string) (MCPWaitOptions, error) {
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	opt := MCPWaitOptions{
		Dir:           dir,
		Timeout:       envMCPWaitTimeout(),
		ServerTimeout: 0,
	}
	for len(args) > 0 {
		switch args[0] {
		case "--dir":
			if len(args) < 2 {
				return opt, fmt.Errorf("mars-mcp-wait: 参数 --dir 缺少取值")
			}
			opt.Dir = args[1]
			args = args[2:]
		case "--timeout":
			if len(args) < 2 {
				return opt, fmt.Errorf("mars-mcp-wait: 参数 --timeout 缺少取值")
			}
			opt.Timeout = parseSecondsOr(args[1], defaultMCPWaitTimeout)
			args = args[2:]
		case "--server-timeout":
			if len(args) < 2 {
				return opt, fmt.Errorf("mars-mcp-wait: 参数 --server-timeout 缺少取值")
			}
			opt.ServerTimeout = parseSecondsOr(args[1], 0)
			args = args[2:]
		default:
			return opt, fmt.Errorf("mars-mcp-wait: 未知参数 %s", args[0])
		}
	}
	return opt, nil
}

// envMCPWaitTimeout 读取 MARS_MCP_WAIT_TIMEOUT，非数字回落默认值。
func envMCPWaitTimeout() int {
	raw := os.Getenv("MARS_MCP_WAIT_TIMEOUT")
	if raw == "" {
		return defaultMCPWaitTimeout
	}
	return parseSecondsOr(raw, defaultMCPWaitTimeout)
}

// parseSecondsOr 只接受纯数字；空串、负数、非数字一律返回 def
// （对齐旧脚本的 `case ” | *[!0-9]*)` 回落语义）。
func parseSecondsOr(raw string, def int) int {
	n, ok := parseDigits(raw)
	if !ok {
		return def
	}
	return n
}

// parseDigits 解析非负整数；负数与空串都返回 false。
func parseDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// waitMCP 等待内核 server 与 MCP 工具服务就绪。
func waitMCP(ctx context.Context, baseURL string, opt MCPWaitOptions) error {
	return waitMCPWith(ctx, baseURL, opt, time.Now, realSleep)
}

// waitMCPWith 是可注入时钟的等待实现，便于测试零等待。
func waitMCPWith(
	ctx context.Context,
	baseURL string,
	opt MCPWaitOptions,
	now func() time.Time,
	sleep func(context.Context, time.Duration),
) error {
	logw := opt.Log
	if logw == nil {
		logw = io.Discard
	}
	client := newHTTPClient(0)

	// 1. 先等 server 可达。即使 serverTimeout=0 也先探测一次（旧脚本语义）。
	// 注意：sleep 在 ctx 取消后会立即返回，必须显式检查 ctx，否则预算未耗尽时
	// 会空转（realSleep 不推进 fake clock，真实时间里则表现为忙等）。
	healthDeadline := now().Add(time.Duration(opt.ServerTimeout) * time.Second)
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := healthOK(probeCtx, client, baseURL+"/health")
		cancel()
		if err == nil {
			break
		}
		if !now().Before(healthDeadline) {
			fmt.Fprintf(logw, "mars-mcp-wait: opencode server 未在 %ds 内就绪: %s\n", opt.ServerTimeout, baseURL)
			return errMCPServerUnreachable
		}
		sleep(ctx, time.Second)
	}

	// 2. 首次探测 MCP 状态。查询 /mcp 本身会按 directory 创建 instance。
	status, errMsg := probeMCP(ctx, client, baseURL, opt.Dir)
	if status == "connected" {
		return nil
	}
	if opt.Timeout == 0 {
		return errMCPNotReady
	}

	fmt.Fprintf(logw, "mars-mcp-wait: 等待 MCP 工具服务就绪（%s，dir=%s）...\n", mcpServerName, opt.Dir)
	deadline := now().Add(time.Duration(opt.Timeout) * time.Second)
	for status != "connected" {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !now().Before(deadline) {
			break
		}
		// connect 可能阻塞到本次尝试结束；给它剩余预算，但至少 1 秒，
		// 避免把尚未完成的重连掐断。
		remaining := deadline.Sub(now())
		if remaining < time.Second {
			remaining = time.Second
		}
		connectCtx, cancel := context.WithTimeout(ctx, remaining)
		_ = doPost(connectCtx, client, baseURL+"/mcp/"+mcpServerName+"/connect?"+url.Values{
			"directory": {opt.Dir},
		}.Encode())
		cancel()

		status, errMsg = probeMCP(ctx, client, baseURL, opt.Dir)
		if status == "connected" {
			return nil
		}
		if !now().Before(deadline) {
			break
		}
		sleep(ctx, time.Second)
	}

	msg := fmt.Sprintf("mars-mcp-wait: MCP 工具服务未在 %ds 内就绪（status=%s", opt.Timeout, statusOrUnknown(status))
	if errMsg != "" {
		msg += "; " + errMsg
	}
	fmt.Fprintf(logw, "%s）\n", msg)
	return errMCPNotReady
}

// probeMCP 查询 MCP 服务状态。与旧脚本一致：不带 curl -f 语义（非 2xx 也解析 body），
// 传输错误或非法 JSON 都按「状态未知」处理。
func probeMCP(ctx context.Context, client *http.Client, baseURL, dir string) (status, errMsg string) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, body, err := doGet(probeCtx, client, baseURL+"/mcp?"+url.Values{
		"directory": {dir},
	}.Encode())
	if err != nil {
		return "", ""
	}
	var servers map[string]mcpStatus
	if json.Unmarshal(body, &servers) != nil {
		return "", ""
	}
	s := servers[mcpServerName]
	return s.Status, s.Error
}

func statusOrUnknown(status string) string {
	if status == "" {
		return "unknown"
	}
	return status
}
