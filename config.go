package main

import (
	"fmt"
	"os"
	"strings"
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
	AIBin        string // MARS_AI_BIN：底层 agent 二进制路径（默认 opencode，可替换其他内核）
	ListenAddr   string // MARS_LISTEN_ADDR：opencode serve 监听地址
	Home         string // HOME：配置与 skills 的落盘根
}

// getenvDefault 读取环境变量，为空时返回默认值。
func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// LoadEnv 读取并校验环境变量。三个必填项缺失时一次性汇总报错，避免逐个试错。
// 其中 MARS_MODEL 由镜像入口脚本（scripts/entrypoint.sh）先从 mars 配置接口拉取再注入。
func LoadEnv() (Env, error) {
	e := Env{
		GatewayURL:   os.Getenv("MARS_GATEWAY_URL"),
		LLMBaseURL:   os.Getenv("MARS_LLM_BASE_URL"),
		MCPURL:       os.Getenv("MARS_MCP_URL"),
		SessionToken: os.Getenv("MARS_SESSION_TOKEN"),
		Gray:         os.Getenv("MARS_GRAY"),
		Model:        os.Getenv("MARS_MODEL"),
		AIBin:        getenvDefault("MARS_AI_BIN", "/usr/local/bin/opencode"),
		ListenAddr:   getenvDefault("MARS_LISTEN_ADDR", "0.0.0.0:4096"),
		Home:         getenvDefault("HOME", "/workspace"),
	}

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

// Config 是 opencode.json 的结构化表示，字段形态对应 opencode v1 配置规范。
type Config struct {
	Schema   string               `json:"$schema"`
	Provider map[string]Provider  `json:"provider"`
	Model    string               `json:"model"`
	MCP      map[string]MCPServer `json:"mcp"`
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
		Schema: schemaURL,
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
			"mars-tools": {
				Type:    "remote",
				URL:     mcpServerURL(env),
				Headers: mcpHeaders(env.Gray),
			},
		},
	}
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
