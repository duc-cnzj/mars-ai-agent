package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

const (
	exitFailure           = 1
	exitServerUnreachable = 2
	exitUsage             = 64
)

const usageText = `用法: mars-agent [serve|mai|health|mcp-wait|prewarm] [参数...]
  serve      启动 AI 内核服务（默认，容器 PID 1）
  mai        交互式入口（用户命令 mai 的实现）
  health     内核健康检查（K8s 探针）
  mcp-wait   等待 MCP 工具服务就绪
  prewarm    后台预热 MCP（MARS_MCP_PREWARM_TIMEOUT 控制）`

// dispatch 解析子命令并执行，返回进程退出码。execFn 用于替换进程，
// 生产实现是 syscall.Exec；测试注入替身以断言命令而不真正替换进程。
func dispatch(args []string, stderr io.Writer, execFn func(Cmd) error) int {
	name := "serve"
	if len(args) > 0 {
		name = args[0]
		args = args[1:]
	}

	switch name {
	case "serve":
		if err := serve(execFn); err != nil {
			fmt.Fprintf(stderr, "mars-agent: %v\n", err)
			return exitFailure
		}
		return 0
	case "mai":
		return runMai(args, stderr, execFn)
	case "health":
		return runHealth(stderr)
	case "mcp-wait":
		return runMCPWait(args, stderr)
	case "prewarm":
		return runPrewarm(stderr)
	default:
		fmt.Fprintf(stderr, "mars-agent: 未知子命令 %q\n%s\n", name, usageText)
		return exitUsage
	}
}

// serve 是容器主进程：严格校验 mars 注入的环境，落盘内核配置后 exec 内核。
func serve(execFn func(Cmd) error) error {
	env, err := LoadEnv()
	if err != nil {
		return err
	}
	backend, err := SelectBackend(env)
	if err != nil {
		return err
	}
	if err := backend.Prepare(env); err != nil {
		return err
	}
	cmd, err := backend.Launch(env)
	if err != nil {
		return err
	}
	log.Printf("mars-agent: 内核 %s 准备完成，启动 %s", backend.Name(), cmd.Path)
	return execFn(cmd)
}

// fail 统一子命令的失败输出与退出码，避免每个入口重复三行。
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "mars-agent: %v\n", err)
	return exitFailure
}

func runMai(args []string, stderr io.Writer, execFn func(Cmd) error) int {
	env := loadRuntimeEnv()
	backend, err := SelectBackend(env)
	if err != nil {
		return fail(stderr, err)
	}
	cmd, err := backend.Interactive(env, args, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	if err := execFn(cmd); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// healthProbeTimeout 取 chart 三个探针 timeoutSeconds 的最大值（startup 2s /
// readiness 3s / liveness 3s）。旧的 httpGet 探针由 kubelet 计时，真实上限就是各自
// 的 timeoutSeconds；改成 exec 后若这里只给 1s，busy 的内核（/health 1~3s）会被误判
// 为不健康，连续失败还会触发 liveness 重启、丢掉用户会话。统一给满 3s，更短的探针
// 仍由 kubelet 按自身 timeoutSeconds 截断。
const healthProbeTimeout = 3 * time.Second

// runHealth 供 K8s exec 探针调用。只依赖 MARS_LISTEN_ADDR，不要求 mars 配置已注入。
func runHealth(stderr io.Writer) int {
	env := loadRuntimeEnv()
	backend, err := SelectBackend(env)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	if err := backend.Health(ctx, env); err != nil {
		return fail(stderr, fmt.Errorf("health: %w", err))
	}
	return 0
}

// runMCPWait 复刻原 mars-mcp-wait 的退出码语义：0=就绪，1=超时，2=server 不可达，64=用法错误。
func runMCPWait(args []string, stderr io.Writer) int {
	opt, err := parseMCPWaitArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	env := loadRuntimeEnv()
	backend, err := SelectBackend(env)
	if err != nil {
		return fail(stderr, err)
	}
	// 没有 MCP 探测能力的内核没有「就绪」概念，直接视为成功（而非空转等待）。
	if !backend.Capabilities().MCPProbe {
		return 0
	}
	opt.Log = stderr
	return mcpWaitExitCode(backend.WaitMCP(context.Background(), env, opt))
}

// runPrewarm 在容器启动后后台预热 MCP。MARS_MCP_PREWARM_TIMEOUT 非法或 ≤0 时直接跳过。
func runPrewarm(stderr io.Writer) int {
	raw := os.Getenv("MARS_MCP_PREWARM_TIMEOUT")
	if raw == "" {
		raw = "120"
	}
	timeout, ok := parseDigits(raw)
	if !ok || timeout <= 0 {
		return 0
	}
	env := loadRuntimeEnv()
	backend, err := SelectBackend(env)
	if err != nil {
		return fail(stderr, err)
	}
	if !backend.Capabilities().MCPProbe {
		return 0
	}
	return mcpWaitExitCode(backend.WaitMCP(context.Background(), env, MCPWaitOptions{
		Dir:           env.Home,
		Timeout:       timeout,
		ServerTimeout: 60,
		Log:           stderr,
	}))
}

func mcpWaitExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errMCPServerUnreachable):
		return exitServerUnreachable
	default:
		return exitFailure
	}
}
