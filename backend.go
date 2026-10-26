package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Backend 是 agent 内核的唯一契约：新增内核 = 加一个实现并在 backendRegistry 注册。
// 调用方（serve/mai/health/mcp-wait/prewarm）只依赖本接口，不再猜二进制 basename。
type Backend interface {
	// Name 返回内核标识（与 MARS_AGENT_BACKEND 取值一致）。
	Name() string

	// Prepare 幂等落盘内核所需的全部静态资产（原生配置、MCP、skills、品牌/偏好）。
	Prepare(env Env) error

	// Launch 返回容器主进程要 exec 的命令（server 型内核的 serve 命令）。
	Launch(env Env) (Cmd, error)

	// Interactive 返回用户 `mai` 要 exec 的命令。stderr 用于传递非致命的告警
	// （如 MCP 预热超时），不得阻断交互。
	Interactive(env Env, args []string, stderr io.Writer) (Cmd, error)

	// Health 供 K8s exec 探针调用，返回 nil 表示内核已就绪。
	Health(ctx context.Context, env Env) error

	// WaitMCP 等待 MCP 工具服务可用；不支持探测的内核直接返回 nil。
	WaitMCP(ctx context.Context, env Env, opt MCPWaitOptions) error

	// Capabilities 声明能力，脚本/探针据此决策。
	Capabilities() Capabilities
}

// Capabilities 描述一个内核的结构性能力。MCPProbe 已参与运行时决策
// （mcp-wait / prewarm 对不支持的 内核直接跳过等待）；其余能力位目前是
// 自描述，供文档、测试与后续接入多内核时裁剪调用链使用。
type Capabilities struct {
	Server        bool // 有常驻 HTTP server（可 attach / HTTP 探针）
	Attach        bool // 交互式可复用常驻 server
	MCPProbe      bool // 可通过 API 探测 MCP connected 状态
	SessionResume bool // 支持继续历史会话
	Branding      bool // 支持 TUI 品牌定制
}

// Cmd 是待执行命令。Args 不含 argv0，由调用方用 syscall.Exec 拼装，
// 以保留 PID 1、信号直传与探针语义。
type Cmd struct {
	Path string
	Args []string
}

// MCPWaitOptions 是 MCP 就绪等待参数。Log 为 nil 时不输出进度/告警。
type MCPWaitOptions struct {
	Dir           string
	Timeout       int
	ServerTimeout int
	Log           io.Writer
}

// backendRegistry 是 shim/内核注册表。P0 只有 opencode。
var backendRegistry = map[string]Backend{
	"opencode": opencodeBackend{},
}

// SelectBackend 按 env.Backend 选择内核；空值回落到 opencode，未知值 fail fast。
func SelectBackend(env Env) (Backend, error) {
	name := env.Backend
	if name == "" {
		name = defaultBackend
	}
	backend, ok := backendRegistry[name]
	if !ok {
		return nil, fmt.Errorf("未知的 agent 内核 %q（可选：%s）", name, strings.Join(registeredBackends(), ", "))
	}
	return backend, nil
}

// registeredBackends 返回排序后的内核名，用于错误信息与测试。
func registeredBackends() []string {
	names := make([]string, 0, len(backendRegistry))
	for name := range backendRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
