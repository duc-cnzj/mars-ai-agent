package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLoadEnv_Defaults 验证可选环境变量在缺省时回落到约定默认值。
func TestLoadEnv_Defaults(t *testing.T) {
	t.Setenv("MARS_GATEWAY_URL", "http://gw:9000")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m1")
	t.Setenv("MARS_AI_BIN", "")
	t.Setenv("MARS_LISTEN_ADDR", "")
	t.Setenv("HOME", "")
	t.Setenv("MARS_LLM_BASE_URL", "")
	t.Setenv("MARS_MCP_URL", "")
	t.Setenv("MARS_GRAY", "")

	env, err := LoadEnv()
	if err != nil {
		t.Fatalf("LoadEnv() error = %v", err)
	}
	if env.AIBin != "/usr/local/bin/opencode" {
		t.Errorf("AIBin = %q, want 默认值", env.AIBin)
	}
	if env.ListenAddr != "0.0.0.0:4096" {
		t.Errorf("ListenAddr = %q, want 默认值", env.ListenAddr)
	}
	if env.Home != "/workspace" {
		t.Errorf("Home = %q, want /workspace", env.Home)
	}
	// LLM/MCP 地址覆盖项缺省为空，由 BuildConfig 回落到网关约定路径。
	if env.LLMBaseURL != "" || env.MCPURL != "" {
		t.Errorf("覆盖项缺省应为空, got LLMBaseURL=%q MCPURL=%q", env.LLMBaseURL, env.MCPURL)
	}
	if env.Gray != "" {
		t.Errorf("Gray = %q, want 空（非灰度）", env.Gray)
	}
}

// TestLoadEnv_URLOverrides 验证 MARS_LLM_BASE_URL / MARS_MCP_URL 被原样读取。
func TestLoadEnv_URLOverrides(t *testing.T) {
	t.Setenv("MARS_GATEWAY_URL", "http://gw:9000")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m1")
	t.Setenv("MARS_LLM_BASE_URL", "https://api.example.com/v1")
	t.Setenv("MARS_MCP_URL", "https://mcp.example.com/sse")

	env, err := LoadEnv()
	if err != nil {
		t.Fatalf("LoadEnv() error = %v", err)
	}
	if env.LLMBaseURL != "https://api.example.com/v1" {
		t.Errorf("LLMBaseURL = %q", env.LLMBaseURL)
	}
	if env.MCPURL != "https://mcp.example.com/sse" {
		t.Errorf("MCPURL = %q", env.MCPURL)
	}
}

// TestLoadEnv_MissingRequired 验证必填项缺失时报错并点名具体变量。
func TestLoadEnv_MissingRequired(t *testing.T) {
	tests := []struct {
		name           string
		gw, tok, model string
		want           []string
	}{
		{"全部缺失", "", "", "", []string{"MARS_GATEWAY_URL", "MARS_SESSION_TOKEN", "MARS_MODEL"}},
		{"仅令牌缺失", "http://gw", "", "m1", []string{"MARS_SESSION_TOKEN"}},
		{"仅模型缺失", "http://gw", "tok", "", []string{"MARS_MODEL"}},
		{"仅网关缺失", "", "tok", "m1", []string{"MARS_GATEWAY_URL"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MARS_GATEWAY_URL", tt.gw)
			t.Setenv("MARS_SESSION_TOKEN", tt.tok)
			t.Setenv("MARS_MODEL", tt.model)
			_, err := LoadEnv()
			if err == nil {
				t.Fatal("LoadEnv() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息 %q 未点名 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestBuildConfig 验证配置各字段取值正确，且令牌不以明文落盘。
func TestBuildConfig(t *testing.T) {
	env := Env{
		GatewayURL:   "http://mars-server.mars.svc:9000/", // 末尾斜杠应被规整
		SessionToken: "tok",
		Model:        "m1",
		Home:         "/workspace",
	}
	cfg := BuildConfig(env)

	p, ok := cfg.Provider[providerID]
	if !ok {
		t.Fatalf("缺少 provider %q", providerID)
	}
	if p.NPM != providerNPM {
		t.Errorf("npm = %q, want %q", p.NPM, providerNPM)
	}
	if p.Options.BaseURL != "http://mars-server.mars.svc:9000/llm/v1" {
		t.Errorf("baseURL = %q", p.Options.BaseURL)
	}
	if p.Options.APIKey != tokenEnvPlaceholder {
		t.Errorf("apiKey = %q, want 环境占位符（不得明文落盘）", p.Options.APIKey)
	}
	if _, ok := p.Models["m1"]; !ok {
		t.Errorf("models 缺少 m1")
	}
	if cfg.Model != "mars/m1" {
		t.Errorf("model = %q, want mars/m1", cfg.Model)
	}

	mcp, ok := cfg.MCP["mars-tools"]
	if !ok {
		t.Fatal("缺少 mcp mars-tools")
	}
	if mcp.URL != "http://mars-server.mars.svc:9000/mcp" {
		t.Errorf("mcp url = %q", mcp.URL)
	}
	if mcp.Headers["Authorization"] != "Bearer "+tokenEnvPlaceholder {
		t.Errorf("mcp Authorization = %q", mcp.Headers["Authorization"])
	}

	// 序列化产物绝不应包含令牌明文。
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), env.SessionToken) {
		t.Errorf("序列化结果疑似含令牌明文: %s", data)
	}
}

// TestBuildConfig_Overrides 验证 MARS_LLM_BASE_URL / MARS_MCP_URL 能整体覆盖网关派生的约定路径，
// 且末尾斜杠被规整。
func TestBuildConfig_Overrides(t *testing.T) {
	env := Env{
		GatewayURL: "http://gw:9000",
		LLMBaseURL: "https://api.example.com/v1/", // 末尾斜杠应被规整
		MCPURL:     "https://mcp.example.com/sse/",
		Model:      "m1",
	}
	cfg := BuildConfig(env)

	if got := cfg.Provider[providerID].Options.BaseURL; got != "https://api.example.com/v1" {
		t.Errorf("baseURL = %q, want 覆盖值并去尾斜杠", got)
	}
	if got := cfg.MCP["mars-tools"].URL; got != "https://mcp.example.com/sse" {
		t.Errorf("mcp url = %q, want 覆盖值并去尾斜杠", got)
	}
}

// TestBuildConfig_GrayHeader 验证灰度用户的 LLM 与 MCP 请求都带上 mars_gray 路由头，
// 且非灰度时该头完全不出现在落盘配置里。
func TestBuildConfig_GrayHeader(t *testing.T) {
	tests := []struct {
		name string
		gray string
		want bool
	}{
		{"灰度用户", "always", true},
		{"非灰度用户", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := BuildConfig(Env{GatewayURL: "http://gw:9000", Model: "m1", Gray: tt.gray})

			llmHdr := cfg.Provider[providerID].Options.Headers
			mcpHdr := cfg.MCP["mars-tools"].Headers
			if tt.want {
				if llmHdr["mars_gray"] != "always" {
					t.Errorf("provider headers = %v, want mars_gray=always", llmHdr)
				}
				if mcpHdr["mars_gray"] != "always" {
					t.Errorf("mcp headers = %v, want mars_gray=always", mcpHdr)
				}
			} else {
				if llmHdr != nil {
					t.Errorf("非灰度 provider headers = %v, want nil（不落盘）", llmHdr)
				}
				if _, ok := mcpHdr["mars_gray"]; ok {
					t.Errorf("非灰度 mcp headers 不应含 mars_gray: %v", mcpHdr)
				}
			}
			// Authorization 始终保留，灰度头只做叠加。
			if mcpHdr["Authorization"] != "Bearer "+tokenEnvPlaceholder {
				t.Errorf("mcp Authorization = %q", mcpHdr["Authorization"])
			}

			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if got := strings.Contains(string(data), "mars_gray"); got != tt.want {
				t.Errorf("序列化含 mars_gray = %v, want %v: %s", got, tt.want, data)
			}
		})
	}
}

// TestServeArgs 验证监听地址解析为 opencode serve 参数，非法地址报错。
func TestServeArgs(t *testing.T) {
	tests := []struct {
		addr string
		want []string
		ok   bool
	}{
		{"0.0.0.0:4096", []string{"serve", "--hostname", "0.0.0.0", "--port", "4096"}, true},
		{"127.0.0.1:8080", []string{"serve", "--hostname", "127.0.0.1", "--port", "8080"}, true},
		{"没有端口", nil, false},
	}
	for _, tt := range tests {
		got, err := serveArgs(Env{ListenAddr: tt.addr})
		if !tt.ok {
			if err == nil {
				t.Errorf("serveArgs(%q) error = nil, want error", tt.addr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("serveArgs(%q) error = %v", tt.addr, err)
		}
		if strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("serveArgs(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}
