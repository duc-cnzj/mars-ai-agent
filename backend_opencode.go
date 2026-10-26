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

// Health 直达 opencode serve 的 /health。
func (opencodeBackend) Health(ctx context.Context, env Env) error {
	return healthOK(ctx, newHTTPClient(0), loopbackURL(env.ListenAddr)+"/health")
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
// 保留旧脚本 `${addr##*:}` 的语义（取最后一个冒号之后的部分），不做地址合法性校验：
// 非法地址会在探测时自然失败，而不是让 mai 直接报错退出。
func loopbackURL(listenAddr string) string {
	return "http://127.0.0.1:" + listenAddr[strings.LastIndex(listenAddr, ":")+1:]
}
