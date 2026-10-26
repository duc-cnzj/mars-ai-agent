package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shrinkMaiProbes(t *testing.T) {
	t.Helper()
	oldAttempts, oldInterval := serverProbeAttempts, serverProbeInterval
	oldProbeTimeout, oldSessionTimeout := serverProbeTimeout, sessionProbeTimeout
	serverProbeAttempts = 3
	serverProbeInterval = time.Millisecond
	serverProbeTimeout = 50 * time.Millisecond
	sessionProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		serverProbeAttempts = oldAttempts
		serverProbeInterval = oldInterval
		serverProbeTimeout = oldProbeTimeout
		sessionProbeTimeout = oldSessionTimeout
	})
}

// TestPlanMai 覆盖原 mai.sh 的全部分支。
func TestPlanMai(t *testing.T) {
	const (
		bin = "/usr/local/bin/opencode"
		dir = "/workspace/proj"
		loc = "http://127.0.0.1:4096"
	)
	tests := []struct {
		name string
		args []string
		st   maiState
		want []string
	}{
		{
			name: "server 有会话：attach -c",
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true, ServerHasSession: true},
			want: []string{"attach", "--dir", dir, "-c", loc},
		},
		{
			name: "server 无会话：attach 不带 -c",
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true},
			want: []string{"attach", "--dir", dir, loc},
		},
		{
			name: "server 不可达 + 本地有会话：完整 TUI -c --auto",
			st:   maiState{Bin: bin, Dir: dir, URL: loc, LocalHasSession: true},
			want: []string{"-c", "--auto"},
		},
		{
			name: "server 不可达 + 本地无会话：完整 TUI --auto",
			st:   maiState{Bin: bin, Dir: dir, URL: loc},
			want: []string{"--auto"},
		},
		{
			name: "run 复用 server",
			args: []string{"run", "你好"},
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true},
			want: []string{"run", "--attach", loc, "--dir", dir, "你好"},
		},
		{
			name: "run 带特殊参数：原样透传",
			args: []string{"run", "-f", "a.txt"},
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true},
			want: []string{"run", "-f", "a.txt"},
		},
		{
			name: "run 帮助：原样透传",
			args: []string{"run", "--help"},
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true},
			want: []string{"run", "--help"},
		},
		{
			name: "非 run 参数：原样透传",
			args: []string{"foo", "--bar"},
			st:   maiState{Bin: bin, Dir: dir, URL: loc, ServerReady: true},
			want: []string{"foo", "--bar"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planMai(tt.args, tt.st)
			if got.Path != tt.st.Bin {
				t.Errorf("Path = %q, want %q", got.Path, tt.st.Bin)
			}
			if strings.Join(got.Args, " ") != strings.Join(tt.want, " ") {
				t.Errorf("Args = %v, want %v", got.Args, tt.want)
			}
		})
	}
}

// TestArgumentsAllowAttach 验证 run 的特殊参数名单与旧脚本逐字一致。
func TestArgumentsAllowAttach(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"run", "hi"}, true},
		{[]string{"run", "--attach"}, false},
		{[]string{"run", "--attach=http://x"}, false},
		{[]string{"run", "-f"}, false},
		{[]string{"run", "--file"}, false},
		{[]string{"run", "--file=a.txt"}, false},
		{[]string{"run", "-h"}, false},
		{[]string{"run", "--help"}, false},
		{[]string{"run", "-v"}, false},
		{[]string{"run", "--version"}, false},
		{[]string{"run", "--unknown"}, true},
	}
	for _, tt := range tests {
		if got := argumentsAllowAttach(tt.args); got != tt.want {
			t.Errorf("argumentsAllowAttach(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

// TestServerHasSession 验证各种响应形态都按「无会话」处理，500 也视为无会话。
func TestServerHasSession(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want bool
	}{
		{"非空数组", 200, `[{"id":"1"}]`, true},
		{"空数组", 200, `[]`, false},
		{"null", 200, `null`, false},
		{"对象", 200, `{}`, false},
		{"空 body", 200, ``, false},
		{"非法 JSON", 200, `nope`, false},
		{"500", 500, `[{"id":"1"}]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/session" {
					t.Errorf("路径 = %s, want /session", r.URL.Path)
				}
				if r.URL.Query().Get("limit") != "1" {
					t.Errorf("limit = %q, want 1", r.URL.Query().Get("limit"))
				}
				gotDir <- r.URL.Query().Get("directory")
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got := serverHasSession(ctx, newHTTPClient(0), srv.URL, "/workspace/proj")
			if got != tt.want {
				t.Errorf("serverHasSession() = %v, want %v", got, tt.want)
			}
			if dir := <-gotDir; dir != "/workspace/proj" {
				t.Errorf("directory = %q, want /workspace/proj", dir)
			}
		})
	}
}

// TestLocalHasSession 验证本地会话列表按目录过滤，且非 0 退出码仍解析 stdout。
func TestLocalHasSession(t *testing.T) {
	tests := []struct {
		name   string
		script string
		dir    string
		want   bool
	}{
		{
			name:   "命中当前目录",
			script: "#!/bin/sh\nprintf '%s' '[{\"directory\":\"/a\"},{\"directory\":\"/b\"}]'\n",
			dir:    "/b",
			want:   true,
		},
		{
			name:   "未命中当前目录",
			script: "#!/bin/sh\nprintf '%s' '[{\"directory\":\"/a\"}]'\n",
			dir:    "/c",
			want:   false,
		},
		{
			name:   "非 0 退出仍解析 stdout",
			script: "#!/bin/sh\nprintf '%s' '[{\"directory\":\"/b\"}]'\nexit 1\n",
			dir:    "/b",
			want:   true,
		},
		{
			name:   "非法 JSON",
			script: "#!/bin/sh\nprintf '%s' 'nope'\n",
			dir:    "/b",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "fake-opencode")
			if err := os.WriteFile(bin, []byte(tt.script), 0o755); err != nil {
				t.Fatalf("写 fake bin: %v", err)
			}
			if got := localHasSession(bin, tt.dir); got != tt.want {
				t.Errorf("localHasSession() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestServerReady 验证 3 次重试语义。
func TestServerReady(t *testing.T) {
	shrinkMaiProbes(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !serverReady(context.Background(), newHTTPClient(0), srv.URL) {
		t.Error("serverReady() = false, want true（第三次应成功）")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("探测次数 = %d, want 3", got)
	}

	calls = 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	if serverReady(context.Background(), newHTTPClient(0), srv2.URL) {
		t.Error("serverReady() = true, want false")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("探测次数 = %d, want 3", got)
	}
}

// TestMCPWaitTimeout 验证 mai 的 MCP 等待预算解析与旧脚本一致。
func TestMCPWaitTimeout(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", 15},
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"5", 5},
	}
	for _, tt := range tests {
		t.Setenv("MARS_MCP_WAIT_TIMEOUT", tt.raw)
		if got := mcpWaitTimeout(); got != tt.want {
			t.Errorf("mcpWaitTimeout() with %q = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

// TestInteractiveOpencodeServerReady 验证 server 可达时 attach 并复用 URL。
func TestInteractiveOpencodeServerReady(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/session":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestInteractiveOpencodeRunAttach 验证 `mai run` 在 server 可达时复用 attach。
func TestInteractiveOpencodeRunAttach(t *testing.T) {
	shrinkMaiProbes(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"run", "hi"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	dir, _ := os.Getwd()
	want := []string{"run", "--attach", "http://127.0.0.1:" + port, "--dir", dir, "hi"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestInteractiveOpencodeSkipsProbe 验证 run 特殊参数与非 run 参数都不触发 server 探测。
func TestInteractiveOpencodeSkipsProbe(t *testing.T) {
	shrinkMaiProbes(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	env := Env{AIBin: "/bin/echo", ListenAddr: "0.0.0.0:" + port}
	for _, args := range [][]string{{"run", "--help"}, {"foo"}} {
		cmd, err := (opencodeBackend{}).Interactive(env, args, io.Discard)
		if err != nil {
			t.Fatalf("Interactive(%v) error = %v", args, err)
		}
		if strings.Join(cmd.Args, " ") != strings.Join(args, " ") {
			t.Errorf("Interactive(%v) Args = %v, want 原样透传", args, cmd.Args)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server 被探测 %d 次, want 0", got)
	}
}

// TestInteractivePassthroughWithoutCwd 验证纯透传不依赖当前目录（对齐旧脚本 exec 语义）。
func TestInteractivePassthroughWithoutCwd(t *testing.T) {
	old := getwd
	getwd = func() (string, error) { return "", errors.New("cwd 已删除") }
	t.Cleanup(func() { getwd = old })

	env := Env{AIBin: "/bin/echo", ListenAddr: "0.0.0.0:" + closedPort(t)}
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"run", "--help"}, io.Discard)
	if err != nil {
		t.Fatalf("纯透传不应因当前目录不可用而失败: %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "run --help"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}

	if _, err := (opencodeBackend{}).Interactive(env, nil, io.Discard); err == nil {
		t.Error("需要当前目录的分支应返回错误")
	}
}

// TestInteractiveOpencodeFallback 验证 server 不可达时按本地会话列表决定是否带 -c。
func TestInteractiveOpencodeFallback(t *testing.T) {
	shrinkMaiProbes(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	jsonSessions := fmt.Sprintf(`[{"directory":%q}]`, dir)

	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	t.Setenv("MARS_FAKE_SESSIONS", jsonSessions)

	closedPort := closedPort(t)
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort}

	cmd, err := (opencodeBackend{}).Interactive(env, nil, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "-c --auto"; got != want {
		t.Errorf("Args = %q, want %q（当前目录有会话）", got, want)
	}

	// 会话列表不属于当前目录时，不带 -c。
	t.Setenv("MARS_FAKE_SESSIONS", `[{"directory":"/elsewhere"}]`)
	cmd, err = (opencodeBackend{}).Interactive(env, nil, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--auto"; got != want {
		t.Errorf("Args = %q, want %q（无当前目录会话）", got, want)
	}
}

// TestInteractiveOpencodeUsesLogicalPWD 验证 --dir 与会话查询使用逻辑路径（不解析符号链接）。
func TestInteractiveOpencodeUsesLogicalPWD(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")

	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)

	gotDir := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/session":
			gotDir <- r.URL.Query().Get("directory")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if dir := <-gotDir; dir != link {
		t.Errorf("session directory = %q, want 逻辑路径 %q", dir, link)
	}
	if !argsContainPair(cmd.Args, "--dir", link) {
		t.Errorf("Args = %v, want --dir %q", cmd.Args, link)
	}
}

func argsContainPair(args []string, key, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == val {
			return true
		}
	}
	return false
}

func serverPort(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Port()
}

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return strconv.Itoa(port)
}

// ensure JSON import stays used even if future edits remove the literal above.
var _ = json.Valid
