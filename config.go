package main

import (
	"fmt"
	"os"
	"strings"
)

// 运行期默认值。集中在此，供严格入口（LoadEnv）与运行期子命令（loadRuntimeEnv）共用，
// 避免两处默认值漂移。
const (
	defaultBackend    = "opencode"
	defaultAIBin      = "/usr/local/bin/opencode"
	defaultListenAddr = "0.0.0.0:4096"
	defaultHome       = "/workspace"
)

// providerID 是写入 opencode 配置的 LLM 供应商标识，固定为 mars（指向 mars 的 LLM 代理）。
const providerID = "mars"

// providerNPM 是 opencode 侧用于对接 OpenAI 兼容接口的适配包。
//
// ⚠️ opencode v2 起该字段改名为 package，取值也随之变化；升级钉死版本时必须同步核对。
const providerNPM = "@ai-sdk/openai-compatible"

// schemaURL 是 opencode 配置文件的官方 JSON Schema 地址（仅供编辑器补全，运行时不生效）。
const schemaURL = "https://opencode.ai/config.json"

// tokenEnvPlaceholder 是 opencode 支持的 {env:...} 占位符。密钥一律用它引用，
// 避免把会话令牌明文写进落盘的配置文件。
const tokenEnvPlaceholder = "{env:MARS_SESSION_TOKEN}"

// llmProxyPath / mcpPath 是 mars 网关暴露 LLM 代理与 MCP 工具服务的约定路径。
// 二者可分别由 MARS_LLM_BASE_URL / MARS_MCP_URL 整体覆盖，用于本地联调或对接非 mars 端点。
const (
	llmProxyPath = "/llm/v1"
	mcpPath      = "/mcp"
)

// mcpServerName 是写入 opencode 配置的 MCP 服务名，也是工具权限键的前缀。
// opencode 把 MCP 工具注册为 sanitize(服务名) + "_" + sanitize(工具名)。
const mcpServerName = "mars-tools"

// writeTools 是 mars MCP 的写操作工具。
// 钉版 opencode（1.19.2）的 attach 子命令不支持 --auto，而 mai 默认走 attach
// 复用常驻 server，因此这里在配置层显式 allow，等价于完整 TUI 的 --auto。
// 注意：写工具不再弹客户端确认，人工确认由 agent 在对话中先行完成。
// 服务端用 MCP readOnlyHint 标注读写语义，但钉版 opencode 不消费该注解，
// 未配置权限规则时一律默认 allow——显式写 allow 是为了锁定行为，避免升级后默认值变化。
var writeTools = []string{"apply_project", "toggle_hibernate", "restart_pod", "exec_command"}

// grayHeaderName 是 nginx-ingress canary 读取的灰度路由头；值为 always 时固定路由到灰度通道。
const grayHeaderName = "mars_gray"

// Env 是 mars-agent 启动所需的全部环境变量（由沙箱 Pod manifest 注入）。
type Env struct {
	GatewayURL   string // MARS_GATEWAY_URL：mars 网关地址（提供 MCP 工具 + LLM 代理）
	LLMBaseURL   string // MARS_LLM_BASE_URL：可选，LLM 完整 baseURL 覆盖（绕过 <gateway>/llm/v1 约定）
	MCPURL       string // MARS_MCP_URL：可选，MCP 工具服务完整 URL 覆盖（绕过 <gateway>/mcp 约定）
	SessionToken string // MARS_SESSION_TOKEN：短期 scoped JWT，MCP、LLM 与配置接口共用
	Gray         string // MARS_GRAY：灰度通道标记（always=灰度用户，空=非灰度）；由入口脚本探测后注入
	Model        string // MARS_MODEL：模型 id（裸 id）；由入口脚本从 mars /api/ai/config 拉取后注入
	Backend      string // MARS_AGENT_BACKEND：agent 内核名（默认 opencode）
	AIBin        string // MARS_AI_BIN：底层 agent 二进制路径（内核的路径覆盖）
	ListenAddr   string // MARS_LISTEN_ADDR：内核服务监听地址
	Home         string // HOME：配置与 skills 的落盘根
}

// getenvDefault 读取环境变量，为空时返回默认值。
func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envFromOS 是环境变量到 Env 的唯一映射入口：严格与宽松入口共用，
// 避免新增变量时只改一处导致默认值漂移。
func envFromOS() Env {
	return Env{
		GatewayURL:   os.Getenv("MARS_GATEWAY_URL"),
		LLMBaseURL:   os.Getenv("MARS_LLM_BASE_URL"),
		MCPURL:       os.Getenv("MARS_MCP_URL"),
		SessionToken: os.Getenv("MARS_SESSION_TOKEN"),
		Gray:         os.Getenv("MARS_GRAY"),
		Model:        os.Getenv("MARS_MODEL"),
		Backend:      getenvDefault("MARS_AGENT_BACKEND", defaultBackend),
		AIBin:        getenvDefault("MARS_AI_BIN", defaultAIBin),
		ListenAddr:   getenvDefault("MARS_LISTEN_ADDR", defaultListenAddr),
		Home:         getenvDefault("HOME", defaultHome),
	}
}

// LoadEnv 读取并校验环境变量。三个必填项缺失时一次性汇总报错，避免逐个试错。
// 其中 MARS_MODEL 由镜像入口脚本（scripts/entrypoint.sh）先从 mars 配置接口拉取再注入。
func LoadEnv() (Env, error) {
	e := envFromOS()

	var missing []string
	if e.GatewayURL == "" {
		missing = append(missing, "MARS_GATEWAY_URL")
	}
	if e.SessionToken == "" {
		missing = append(missing, "MARS_SESSION_TOKEN")
	}
	if e.Model == "" {
		missing = append(missing, "MARS_MODEL")
	}
	if len(missing) > 0 {
		return Env{}, fmt.Errorf("缺少必填环境变量: %s", strings.Join(missing, ", "))
	}
	return e, nil
}

// loadRuntimeEnv 读取运行期子命令（mai/health/mcp-wait/prewarm）所需的环境变量：
// 只填默认值、不做必填校验。这些命令可能运行在容器启动早期或用户终端里，
// 不应因为 mars 配置尚未注入而拒绝工作（与迁移前的 shell 脚本口径一致）。
func loadRuntimeEnv() Env {
	return envFromOS()
}

// Config 是 opencode.json 的结构化表示，字段形态对应 opencode v1 配置规范。
type Config struct {
	Schema string `json:"$schema"`
	// Autoupdate 固定 false：沙箱内 opencode 是钉版二进制，自更新会破坏版本与
	// 校验和假设；同时 TUI 每 10 分钟一次的升级检查（cli/upgrade.ts）也会被跳过。
	Autoupdate bool                 `json:"autoupdate"`
	Provider   map[string]Provider  `json:"provider"`
	Model      string               `json:"model"`
	MCP        map[string]MCPServer `json:"mcp"`
	Permission map[string]string    `json:"permission"`
}

// Provider 描述一个 LLM 供应商（此处恒为指向 mars 代理的 mars 供应商）。
type Provider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options ProviderOptions          `json:"options"`
	Models  map[string]ProviderModel `json:"models"`
}

// ProviderOptions 是供应商的连接参数。
type ProviderOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
	// Headers 会由 opencode 原样附加到每次 LLM 请求上（灰度用户在这里带 mars_gray）。
	// 非灰度时为 nil，omitempty 保证不落盘空对象。
	Headers map[string]string `json:"headers,omitempty"`
}

// ProviderModel 描述一个可选模型。
type ProviderModel struct {
	Name string `json:"name"`
}

// MCPServer 描述一个 MCP 工具服务（此处为 mars 提供的远程工具服务）。
type MCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// BuildConfig 依据环境变量构造 opencode 配置：LLM 指向 mars 代理、工具指向 mars MCP 服务。
// 二者地址可由 MARS_LLM_BASE_URL / MARS_MCP_URL 整体覆盖（见 llmBaseURL / mcpServerURL）。
//
// 安全要点：密钥一律写成 opencode 的 {env:...} 占位符，绝不把令牌明文落盘。
//
// ⚠️ 整个配置的字段形态（provider/npm/options/mcp）必须与镜像钉死的 opencode 版本核对：
// v1 用 provider/npm/options，v2 用 providers/package/settings。版本升级只需改动此函数一处。
func BuildConfig(env Env) Config {
	return Config{
		Schema:     schemaURL,
		Autoupdate: false,
		Provider: map[string]Provider{
			providerID: {
				NPM:  providerNPM,
				Name: "Mars LLM",
				Options: ProviderOptions{
					BaseURL: llmBaseURL(env),
					APIKey:  tokenEnvPlaceholder,
					Headers: grayHeaders(env.Gray),
				},
				Models: map[string]ProviderModel{
					env.Model: {Name: "Mars Pinned"},
				},
			},
		},
		Model: providerID + "/" + env.Model,
		MCP: map[string]MCPServer{
			mcpServerName: {
				Type:    "remote",
				URL:     mcpServerURL(env),
				Headers: mcpHeaders(env.Gray),
			},
		},
		Permission: writeToolPermissions(),
	}
}

// writeToolPermissions 把 mars MCP 的写工具显式配置成 opencode 的 allow 权限：
// 等价于完整 TUI 的 --auto（自动批准未显式 deny 的请求），调用不再弹客户端确认。
//
// 键名必须与 opencode 的 MCP 工具注册名一致（sanitize(服务名)+"_"+sanitize(工具名)）。
// 不要改用通配符兜底——permission 规则的合并顺序不保证，精确键最稳。
func writeToolPermissions() map[string]string {
	perms := make(map[string]string, len(writeTools))
	for _, tool := range writeTools {
		perms[mcpServerName+"_"+tool] = "allow"
	}
	return perms
}

// llmBaseURL 返回 LLM provider 的 baseURL：MARS_LLM_BASE_URL 有值即整体覆盖
// （本地联调 / 对接非 mars 的 OpenAI 兼容端点），否则回退为 mars 网关的代理约定路径 <gateway>/llm/v1。
func llmBaseURL(env Env) string {
	if env.LLMBaseURL != "" {
		return strings.TrimRight(env.LLMBaseURL, "/")
	}
	return strings.TrimRight(env.GatewayURL, "/") + llmProxyPath
}

// mcpServerURL 返回 MCP 工具服务 URL：MARS_MCP_URL 有值即整体覆盖，否则回退为 <gateway>/mcp。
func mcpServerURL(env Env) string {
	if env.MCPURL != "" {
		return strings.TrimRight(env.MCPURL, "/")
	}
	return strings.TrimRight(env.GatewayURL, "/") + mcpPath
}

// grayHeaders 返回访问 mars 时需要附加的灰度路由头：灰度用户带 mars_gray: always，
// 非灰度返回 nil（ProviderOptions.Headers 的 omitempty 会让它不落盘）。
// 取值直接沿用入口脚本从 /api/auth/info 读到的 MARS_GRAY，本包不再二次判断。
func grayHeaders(gray string) map[string]string {
	if gray == "" {
		return nil
	}
	return map[string]string{grayHeaderName: gray}
}

// mcpHeaders 组装 MCP 请求头：Authorization 恒有；灰度用户再叠加灰度路由头。
func mcpHeaders(gray string) map[string]string {
	headers := map[string]string{"Authorization": "Bearer " + tokenEnvPlaceholder}
	for k, v := range grayHeaders(gray) {
		headers[k] = v
	}
	return headers
}
