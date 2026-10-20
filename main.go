// Package main 是沙箱侧 mars-agent 的入口。
//
// 它只做三件事：校验 mars 注入的环境变量（其中模型由入口脚本经配置接口补齐）、
// 生成 opencode 配置与知识型 Skills，
// 然后 exec 成常驻的 opencode serve 服务（用户经项目终端 `mai` 在沙箱内直接使用）。
// 沙箱侧不 import mars 任何代码，与 mars 之间只经 MCP / HTTP 交互。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// main 是进程入口：启动失败即打印原因并以非 0 退出（fail fast，起不来就别起）。
func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mars-agent: %v\n", err)
		os.Exit(1)
	}
}

// run 执行启动流程：加载 env → 落配置 → 落 skills → exec opencode serve。
// 返回非 nil 错误表示启动失败（调用方据此退出）。
func run() error {
	env, err := LoadEnv()
	if err != nil {
		return err
	}

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
	// 关闭 opencode 首页内置 tips（run --attach 等与 mars 场景无关的通用提示）。
	// 属于装饰性优化：失败只告警，绝不阻断启动——kv.json 损坏或 opencode 升级改了
	// 键值格式，都不应让用户失去整个沙箱终端。
	if err := DisableTips(env.Home); err != nil {
		log.Printf("mars-agent: 关闭 TUI tips 失败（忽略，不影响启动）: %v", err)
	}

	args, err := serveArgs(env)
	if err != nil {
		return err
	}
	log.Printf("mars-agent: 配置已写入 %s，启动 %s", cfgPath, env.AIBin)
	// 用 exec 替换当前进程：让底层 agent 成为 PID 1，信号直传、探针直打其 /health。
	return syscall.Exec(env.AIBin, append([]string{env.AIBin}, args...), os.Environ())
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
