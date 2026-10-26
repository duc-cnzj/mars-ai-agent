package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOpencodePrepare 验证落盘路径、权限、内容与可重复执行（模拟 emptyDir 存活下的重启）。
func TestOpencodePrepare(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(home) })

	env := Env{
		GatewayURL:   "http://gw:9000",
		SessionToken: "tok",
		Model:        "m1",
		Backend:      "opencode",
		AIBin:        "/usr/local/bin/opencode",
		ListenAddr:   "0.0.0.0:4096",
		Home:         home,
	}
	for i := 1; i <= 2; i++ {
		if err := (opencodeBackend{}).Prepare(env); err != nil {
			t.Fatalf("第 %d 次 Prepare() error = %v", i, err)
		}
	}

	cfgDir := filepath.Join(home, ".config", "opencode")
	cfgPath := filepath.Join(cfgDir, "opencode.json")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat opencode.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("opencode.json 权限 = %o, want 600", perm)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读取 opencode.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 opencode.json: %v", err)
	}
	if cfg.Model != "mars/m1" {
		t.Errorf("model = %q, want mars/m1", cfg.Model)
	}
	if _, ok := cfg.MCP[mcpServerName]; !ok {
		t.Errorf("缺少 mcp %q", mcpServerName)
	}
	if strings.Contains(string(raw), env.SessionToken) {
		t.Errorf("配置疑似含令牌明文: %s", raw)
	}

	skillPath := filepath.Join(cfgDir, "skills", "mars-resource-model", "SKILL.md")
	skillInfo, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("stat skill: %v", err)
	}
	if perm := skillInfo.Mode().Perm(); perm != 0o444 {
		t.Errorf("skill 文件权限 = %o, want 444", perm)
	}
	skillDir, err := os.Stat(filepath.Join(cfgDir, "skills", "crashloop-triage"))
	if err != nil {
		t.Fatalf("stat skill 目录: %v", err)
	}
	if perm := skillDir.Mode().Perm(); perm != 0o555 {
		t.Errorf("skill 目录权限 = %o, want 555", perm)
	}

	if _, err := os.Stat(filepath.Join(cfgDir, "tui.json")); err != nil {
		t.Errorf("tui.json 未落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "opencode", "kv.json")); err != nil {
		t.Errorf("kv.json 未落盘: %v", err)
	}
}

// TestOpencodePrepareError 验证落盘失败时返回错误，而不是带着坏配置继续启动。
func TestOpencodePrepareError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatalf("写占位文件: %v", err)
	}
	err := (opencodeBackend{}).Prepare(Env{
		GatewayURL:   "http://gw",
		SessionToken: "tok",
		Model:        "m",
		Home:         home,
	})
	if err == nil {
		t.Error("HOME 不是目录时 Prepare() error = nil, want error")
	}
}

// TestOpencodeLaunch 验证 serve 参数拼装与非法监听地址报错。
func TestOpencodeLaunch(t *testing.T) {
	cmd, err := (opencodeBackend{}).Launch(Env{
		AIBin:      "/usr/local/bin/opencode",
		ListenAddr: "0.0.0.0:4096",
	})
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if cmd.Path != "/usr/local/bin/opencode" {
		t.Errorf("Path = %q", cmd.Path)
	}
	if got, want := strings.Join(cmd.Args, " "), "serve --hostname 0.0.0.0 --port 4096"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}

	if _, err := (opencodeBackend{}).Launch(Env{ListenAddr: "没有端口"}); err == nil {
		t.Error("非法 ListenAddr 应返回错误")
	}
}

// TestOpencodeHealth 验证 /health 的 2xx/3xx 视为成功、4xx/5xx 与不可达视为失败，
// 且不跟随重定向（对齐 curl 无 -L 的语义）。
func TestOpencodeHealth(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		wantErr bool
	}{
		{"200", http.StatusOK, false},
		{"302", http.StatusFound, false},
		{"400", http.StatusBadRequest, true},
		{"500", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 302 指向一个必然失败的地址：若客户端跟随重定向，Health 会失败，
				// 从而真正验证 CheckRedirect 禁跟随，而不是只看最终状态码。
				if r.URL.Path == "/redirected" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if tt.code == http.StatusFound {
					w.Header().Set("Location", "/redirected")
				}
				w.WriteHeader(tt.code)
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse url: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = (opencodeBackend{}).Health(ctx, Env{ListenAddr: "0.0.0.0:" + u.Port()})
			if (err != nil) != tt.wantErr {
				t.Errorf("Health() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestOpencodeHealthUnreachable 验证连接被拒绝时返回错误。
func TestOpencodeHealthUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (opencodeBackend{}).Health(ctx, Env{ListenAddr: "0.0.0.0:" + strconv.Itoa(port)}); err == nil {
		t.Error("Health() error = nil, want connection error")
	}
}
