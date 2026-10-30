package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// opencodeBackend 是当前唯一内核。所有 opencode 特有知识（配置 schema、路径、
// TUI、serve 参数、attach 语义）收敛在本文件与 config.go/tui.go 中。
type opencodeBackend struct{}

func (opencodeBackend) Name() string { return "opencode" }

func (opencodeBackend) Capabilities() Capabilities {
	return Capabilities{
		Server:        true,
		Attach:        true,
		MCPProbe:      true,
		SessionResume: true,
		Branding:      true,
	}
}

// Prepare 复用原有落盘步骤，顺序与迁移前完全一致。
func (opencodeBackend) Prepare(env Env) error {
	data, err := json.MarshalIndent(BuildConfig(env), "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 opencode 配置失败: %w", err)
	}

	cfgDir := filepath.Join(env.Home, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录 %s 失败: %w", cfgDir, err)
	}
	cfgPath := filepath.Join(cfgDir, "opencode.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		return fmt.Errorf("写入 opencode 配置 %s 失败: %w", cfgPath, err)
	}
	if err := MaterializeSkills(filepath.Join(cfgDir, "skills")); err != nil {
		return fmt.Errorf("落盘 skills 失败: %w", err)
	}
	// TUI 品牌定制：替换启动页 logo（tui.json 引用 ./plugins/mars-logo.tsx）。
	if err := MaterializeTUI(cfgDir); err != nil {
		return fmt.Errorf("落盘 TUI 配置失败: %w", err)
	}
	// 初始化 TUI 偏好：关闭首页通用 tips，并默认关闭 spinner 动画（57fps 空转
	// 会吃满一个核，表现为输入/滚动卡顿）。best-effort：kv.json 损坏或 opencode
	// 升级改了键值格式，都不应让用户失去整个沙箱终端。
	if err := ApplyTUIPrefs(env.Home); err != nil {
		log.Printf("mars-agent: 初始化 TUI 偏好失败（忽略，不影响启动）: %v", err)
	}
	return nil
}

// Launch 依据监听地址构造 opencode serve 的命令行参数。
func (opencodeBackend) Launch(env Env) (Cmd, error) {
	args, err := serveArgs(env)
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{Path: env.AIBin, Args: args}, nil
}

// healthPath 是钉版 opencode 的真实健康路由（`GET /doc` 的 OpenAPI 里
// operationId=v2.health.get，返回 {"healthy":true}）。
//
// ⚠️ 不能用 /health：1.19.2 没有该路由，未知路径会被 Web UI 的 SPA 兜底成
// 200 HTML——只看状态码的探针会把任何 HTTP 服务（含冒名进程）当成内核就绪。
// 升级钉版内核后必须重新核对本路径，并确认返回体仍是 {"healthy":true}。
const healthPath = "/api/health"

// Health 直达 opencode serve 的真实健康接口。
func (opencodeBackend) Health(ctx context.Context, env Env) error {
	return healthOK(ctx, newHTTPClient(0), loopbackURL(env.ListenAddr)+healthPath)
}

func (opencodeBackend) WaitMCP(ctx context.Context, env Env, opt MCPWaitOptions) error {
	return waitMCP(ctx, loopbackURL(env.ListenAddr), opt)
}

func (opencodeBackend) Interactive(env Env, args []string, stderr io.Writer) (Cmd, error) {
	return interactiveOpencode(env, args, stderr)
}

// serveArgs 依据监听地址构造 opencode serve 的命令行参数。
//
// ⚠️ 参数名（--hostname/--port）必须与镜像钉死的 opencode 版本核对；
// opencode 升级若改了 CLI，只需改动此函数一处。
func serveArgs(env Env) ([]string, error) {
	host, port, err := net.SplitHostPort(env.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("解析 MARS_LISTEN_ADDR %q 失败: %w", env.ListenAddr, err)
	}
	return []string{"serve", "--hostname", host, "--port", port}, nil
}

// loopbackURL 把监听地址转成容器内回环 URL。
//
// 通配/未指定 host（""、"0.0.0.0"、"::"）回落到 127.0.0.1；其余 host 原样保留
// （localhost、::1 等），IPv6 字面量按 URL 规则加方括号。必须这样做：内核按
// --hostname 绑定的地址决定可达性，只绑 ::1 时探 127.0.0.1 必然连不上，
// 会让 health/mai/mcp-wait/prewarm 全部误判服务不可达。
//
// 非法地址沿用旧脚本语义（取最后一个冒号之后的部分），不做校验：
// 在探测时自然失败，而不是让 mai 直接报错退出。
func loopbackURL(listenAddr string) string {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "http://127.0.0.1:" + listenAddr[strings.LastIndex(listenAddr, ":")+1:]
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + port
}
