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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// recvString 从测试通道取值，超时直接失败，避免回归时 go test 整体挂死。
func recvString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("等待测试通道超时")
		return ""
	}
}

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

// TestLocalHasSessionBeyondLegacyListCap 回归：opencode `session list` 不带 -n 时
// 默认只返回最近 100 条，且排序是全局的。用户会话超过 100 条、当前目录的会话又较老时，
// 旧实现传 `-n 100` 会把本目录的会话挤出结果集，mai 回退完整 TUI 时误判「无会话」
// 而丢掉 -c。fake bin 按内核语义模拟截断：仅当请求上限覆盖到第 151 条时才吐出 /target。
func TestLocalHasSessionBeyondLegacyListCap(t *testing.T) {
	script := `#!/bin/sh
n=100; prev=""
for a in "$@"; do
  [ "$prev" = "-n" ] && n="$a"
  prev="$a"
done
i=0
printf '['
while [ "$i" -lt 150 ]; do
  [ "$i" -gt 0 ] && printf ','
  printf '{"directory":"/other"}'
  i=$((i+1))
done
[ "$n" -ge 151 ] && printf ',{"directory":"/target"}'
printf ']'
`
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	if !localHasSession(bin, "/target") {
		t.Error("当前目录的会话被全局排序挤出前 100 条时仍应命中，否则会丢掉 -c")
	}
}

// TestLocalHasSessionSymlinkedDir 回归：内核按物理路径记录会话目录（如 macOS 上
// /tmp → /private/tmp），而 os.Getwd 可能返回逻辑路径。cwd 为符号链接时必须仍判定
// 为「有会话」，否则回退完整 TUI 会丢掉 -c，把用户的“继续上次会话”变成新开会话。
func TestLocalHasSessionSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir := filepath.Join(base, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("创建符号链接失败: %v", err)
	}
	realResolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	bin := filepath.Join(base, "fake-opencode")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' '[{\"directory\":%q}]'\n", realResolved)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	if !localHasSession(bin, linkDir) {
		t.Errorf("符号链接 cwd 应命中物理路径相同的会话")
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
		writeHealthy(w)
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

// TestServerReadyBusyKernel 回归：busy 内核的 /api/health 需要 1~3s（cli.healthProbeTimeout
// 的注释即以此为准），单次探测预算若只有旧的 1s，「重试 3 次」也救不回来——server 明明
// 活着却被判死，mai 退回完整 TUI 并丢掉 -c。这里用真实默认值（不 shrink）验证。
func TestServerReadyBusyKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实等待约 1.5s")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond) // 落在文档所述 1~3s 的区间内
		writeHealthy(w)
	}))
	defer srv.Close()
	if !serverReady(context.Background(), newHTTPClient(0), srv.URL) {
		t.Fatal("忙但活着的 server 应判定为 ready")
	}
}

// TestServerHasSessionBusyKernel 回归：/session 与 /api/health 跑在同一个忙碌内核上。
// 预算若短于 /api/health（旧脚本 --max-time 2），server 有历史会话也会被判成「无会话」，
// attach 丢掉 -c、给用户新开一个会话。这里用真实默认值验证 2.5s 的 /session 仍能判出会话。
func TestServerHasSessionBusyKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实等待约 2.5s")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		_, _ = w.Write([]byte(`[{"directory":"/proj"}]`))
	}))
	defer srv.Close()
	if !serverHasSession(context.Background(), newHTTPClient(0), srv.URL, "/proj") {
		t.Fatal("busy server 上的历史会话应判定为存在")
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
		case healthPath:
			writeHealthy(w)
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
		if r.URL.Path == healthPath {
			writeHealthy(w)
			return
		}
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
	for _, args := range [][]string{{"run", "--help"}, {"models"}} {
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
		case healthPath:
			writeHealthy(w)
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

// TestInitialPromptFromArgs 验证只有「非选项、非已知子命令」的首位参数才算首条问题。
func TestInitialPromptFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		ok   bool
	}{
		{"无参数", nil, "", false},
		{"选项透传", []string{"-c"}, "", false},
		{"run 子命令", []string{"run", "hi"}, "", false},
		{"attach 子命令", []string{"attach", "http://x"}, "", false},
		{"单个问题", []string{"查看 uat/x 的日志"}, "查看 uat/x 的日志", true},
		{"未加引号的多段问题", []string{"查看", "uat/x", "的日志"}, "查看 uat/x 的日志", true},
		{"只有空白", []string{"   "}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := initialPromptFromArgs(tt.args)
			if got != tt.want || ok != tt.ok {
				t.Errorf("initialPromptFromArgs(%v) = (%q, %v), want (%q, %v)", tt.args, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestPlanPromptFallback 验证 server 不可达时的 --prompt 命令组装。
func TestPlanPromptFallback(t *testing.T) {
	const bin = "/usr/local/bin/opencode"
	tests := []struct {
		name       string
		prompt     string
		hasSession bool
		want       []string
	}{
		{"无本地会话", "你好", false, []string{"--auto", "--prompt", "你好"}},
		{"有本地会话", "你好", true, []string{"-c", "--auto", "--prompt", "你好"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planPromptFallback(bin, tt.prompt, tt.hasSession)
			if got.Path != bin || strings.Join(got.Args, " ") != strings.Join(tt.want, " ") {
				t.Errorf("planPromptFallback() = %q %v, want %q %v", got.Path, got.Args, bin, tt.want)
			}
		})
	}
}

// TestLatestSessionID 验证会话 id 解析：没有 id 返回空，查询/解析失败返回错误。
func TestLatestSessionID(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		body    string
		want    string
		wantErr bool
	}{
		{"正常", 200, `[{"id":"ses_1"}]`, "ses_1", false},
		{"空数组", 200, `[]`, "", false},
		{"缺 id", 200, `[{"directory":"/x"}]`, "", false},
		{"非法 JSON", 200, `nope`, "", true},
		{"500", 500, `[{"id":"ses_1"}]`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("directory"); got != "/proj" {
					t.Errorf("directory = %q, want /proj", got)
				}
				if got := r.URL.Query().Get("limit"); got != "1" {
					t.Errorf("limit = %q, want 1", got)
				}
				if got := r.URL.Query().Get("roots"); got != "true" {
					t.Errorf("roots = %q, want true", got)
				}
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got, err := latestSessionID(context.Background(), newHTTPClient(0), srv.URL, "/proj")
			if (err != nil) != tt.wantErr {
				t.Fatalf("latestSessionID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("latestSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLatestSessionIDRootsFallback 验证老内核不认 roots 时退回不带 roots 的查询，
// 保持改造前的可用性。
func TestLatestSessionIDRootsFallback(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("roots") == "true" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("unknown query"))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"ses_old"}]`))
	}))
	defer srv.Close()

	got, err := latestSessionID(context.Background(), newHTTPClient(0), srv.URL, "/proj")
	if err != nil || got != "ses_old" {
		t.Fatalf("latestSessionID() = (%q, %v), want (ses_old, nil)", got, err)
	}
	if calls != 2 {
		t.Errorf("请求次数 = %d, want 2（roots 失败后回退一次）", calls)
	}
}

// TestEnsurePromptSession 验证会话复用与新建两条路径，以及失败时的错误语义。
func TestEnsurePromptSession(t *testing.T) {
	tests := []struct {
		name       string
		listCode   int
		listBody   string
		createCode int
		createBody string
		wantID     string
		wantErr    string
		wantCreate bool
	}{
		{name: "复用最近会话", listBody: `[{"id":"ses_old"}]`, wantID: "ses_old"},
		{
			name: "无会话时新建", listBody: `[]`,
			createCode: 200, createBody: `{"id":"ses_new"}`,
			wantID: "ses_new", wantCreate: true,
		},
		{
			name: "新建返回 500", listBody: `[]`,
			createCode: 500, createBody: "boom",
			wantErr: "HTTP 500", wantCreate: true,
		},
		{
			name: "新建响应缺少 id", listBody: `[]`,
			createCode: 200, createBody: "nope",
			wantErr: "缺少 id", wantCreate: true,
		},
		{
			name: "列表查询失败不新建", listCode: 500,
			wantErr: "查询最近会话失败", wantCreate: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var created bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tt.listCode != 0 {
						w.WriteHeader(tt.listCode)
					}
					_, _ = w.Write([]byte(tt.listBody))
					return
				}
				created = true
				if got := r.URL.Query().Get("directory"); got != "/proj" {
					t.Errorf("POST directory = %q, want /proj", got)
				}
				w.WriteHeader(tt.createCode)
				_, _ = w.Write([]byte(tt.createBody))
			}))
			defer srv.Close()

			got, err := ensurePromptSession(context.Background(), newHTTPClient(0), srv.URL, "/proj")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want 包含 %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != tt.wantID {
				t.Errorf("sessionID = %q, want %q", got, tt.wantID)
			}
			if created != tt.wantCreate {
				t.Errorf("created = %v, want %v", created, tt.wantCreate)
			}
		})
	}
}

// TestEnsurePromptSessionConcurrent 验证并发触发（如双击项目卡片）时按目录加锁：
// 两个调用只创建一个会话。假内核在 POST 记录状态前故意延迟，没有文件锁时两个调用
// 都会先查到空列表并各自建会话；加锁后第二个调用必须复用第一个的会话。
func TestEnsurePromptSessionConcurrent(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			n := created
			mu.Unlock()
			if n == 0 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"ses_1"}]`))
			return
		}
		// 拉宽竞态窗口：先延迟再记录，未加锁时并发调用都会走到这里。
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		created++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"ses_1"}`))
	}))
	defer srv.Close()

	start := make(chan struct{})
	ids := make([]string, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, err := ensurePromptSession(context.Background(), newHTTPClient(0), srv.URL, dir)
			if err != nil {
				t.Errorf("ensurePromptSession() error = %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if created != 1 {
		t.Errorf("并发创建了 %d 个会话, want 1（目录级锁失效）", created)
	}
	if ids[0] != "ses_1" || ids[1] != "ses_1" {
		t.Errorf("并发返回的会话 ID = %v, want 都是 ses_1", ids)
	}
}

// runConcurrentInject 验证并发注入隐藏上下文只写入一条。
//
// 去重是「先查最近一条 synthetic，再写」的 check-then-act：查询与写入若不在同一把
// 目录锁内，两个 mai 会同时读到空历史、各写一条，把同一段绑定堆进会话。假内核把
// 消息查询先快照再阻塞，等两个查询都进入竞态窗口（或锁把第二个挡住直到超时）才放行：
// 有锁时第二次查询能看到第一次的写入并跳过；没有锁时两次都基于空快照写入。
func runConcurrentInject(t *testing.T, invoke func(st maiState)) {
	t.Helper()
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	oldTimeout := sessionProbeTimeout
	sessionProbeTimeout = 2 * time.Second
	t.Cleanup(func() { sessionProbeTimeout = oldTimeout })

	var arrivals, posts int32
	release := make(chan struct{})
	var mu sync.Mutex
	var messages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_1"}]`))
		case r.URL.Path == "/session/ses_1/message" && r.Method == http.MethodGet:
			atomic.AddInt32(&arrivals, 1)
			mu.Lock()
			snapshot := append([]string(nil), messages...)
			mu.Unlock()
			<-release
			out := make([]map[string]any, 0, len(snapshot))
			for _, text := range snapshot {
				out = append(out, map[string]any{
					"parts": []map[string]any{{"type": "text", "text": text, "synthetic": true}},
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/session/ses_1/message" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			if len(payload.Parts) > 0 {
				messages = append(messages, payload.Parts[0].Text)
			}
			mu.Unlock()
			atomic.AddInt32(&posts, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st := maiState{Bin: "/bin/echo", Dir: t.TempDir(), URL: srv.URL}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			invoke(st)
		}()
	}
	close(start)

	// 给两个查询留出进入竞态窗口的时间；只有锁生效时才会只有一个查询到达。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && atomic.LoadInt32(&arrivals) < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("并发写入 %d 条隐藏上下文, want 1（去重与写入必须在同一把目录锁内）", got)
	}
}

// TestConcurrentBindInjectsOnce 验证双击卡片式的并发绑定只写一条上下文。
func TestConcurrentBindInjectsOnce(t *testing.T) {
	runConcurrentInject(t, func(st maiState) {
		_ = planBindCommand(context.Background(), Env{AIBin: "/bin/echo"}, newHTTPClient(0), st, []string{"uat/demo"}, io.Discard)
	})
}

// TestConcurrentUnbindInjectsOnce 验证并发清空绑定只写一条「无绑定」上下文。
func TestConcurrentUnbindInjectsOnce(t *testing.T) {
	runConcurrentInject(t, func(st maiState) {
		_ = planUnbindCommand(context.Background(), Env{AIBin: "/bin/echo"}, newHTTPClient(0), st, io.Discard)
	})
}

// TestSendInitialPrompt 验证问题以 JSON 文本部件投递到指定会话，失败时带上下文返回。
func TestSendInitialPrompt(t *testing.T) {
	dir := "/proj"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/session/ses_1/prompt_async" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("directory"); got != dir {
			t.Errorf("directory = %q, want %q", got, dir)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var payload struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析请求体: %v", err)
		}
		if len(payload.Parts) != 1 || payload.Parts[0].Type != "text" || payload.Parts[0].Text != "你好" {
			t.Errorf("parts = %+v", payload.Parts)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := sendInitialPrompt(context.Background(), newHTTPClient(0), srv.URL, dir, "ses_1", "你好"); err != nil {
		t.Fatalf("sendInitialPrompt() error = %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	closedURL := bad.URL
	bad.Close()
	if err := sendInitialPrompt(context.Background(), newHTTPClient(0), closedURL, dir, "ses_1", "你好"); err == nil ||
		!strings.Contains(err.Error(), "提交首条问题失败") {
		t.Errorf("连接失败应返回带上下文的错误，got %v", err)
	}
}

// TestInteractiveOpencodeInitialPrompt 验证 server 可达时先把问题投递到最近会话再 attach。
func TestInteractiveOpencodeInitialPrompt(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	gotPrompt := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/prompt_async" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) == 0 {
				t.Error("prompt_async 缺少 parts")
			} else {
				gotPrompt <- payload.Parts[0].Text
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"查看", "uat/x", "的日志"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := recvString(t, gotPrompt); got != "查看 uat/x 的日志" {
		t.Errorf("prompt = %q, want %q", got, "查看 uat/x 的日志")
	}
}

// TestInteractiveOpencodeInitialPromptCreatesSession 验证无历史会话时先建会话再投递。
func TestInteractiveOpencodeInitialPromptCreatesSession(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			created = true
			_, _ = w.Write([]byte(`{"id":"ses_new"}`))
		case r.URL.Path == "/session/ses_new/prompt_async" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"你好"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if !created {
		t.Error("没有会话时应创建新会话")
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_new", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestInteractiveOpencodeInitialPromptDegrade 验证投递失败只降级、仍然进入 TUI。
func TestInteractiveOpencodeInitialPromptDegrade(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()

	t.Run("建会话失败降级为普通 attach", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == healthPath:
				writeHealthy(w)
			case r.URL.Path == "/session" && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`[]`))
			case r.URL.Path == "/session" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			default:
				t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()
		port := serverPort(t, srv.URL)
		var stderr strings.Builder
		cmd, err := (opencodeBackend{}).Interactive(Env{
			AIBin:      "/bin/echo",
			ListenAddr: "0.0.0.0:" + port,
		}, []string{"你好"}, &stderr)
		if err != nil {
			t.Fatalf("Interactive() error = %v", err)
		}
		want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
		if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
			t.Errorf("Args = %v, want %v", cmd.Args, want)
		}
		if !strings.Contains(stderr.String(), "创建会话失败") {
			t.Errorf("stderr = %q, want 包含创建会话失败", stderr.String())
		}
	})

	t.Run("投递失败仍 attach 已有会话", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == healthPath:
				writeHealthy(w)
			case r.URL.Path == "/session" && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
			case r.URL.Path == "/session/ses_existing/prompt_async":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("bad"))
			default:
				t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()
		port := serverPort(t, srv.URL)
		var stderr strings.Builder
		cmd, err := (opencodeBackend{}).Interactive(Env{
			AIBin:      "/bin/echo",
			ListenAddr: "0.0.0.0:" + port,
		}, []string{"你好"}, &stderr)
		if err != nil {
			t.Fatalf("Interactive() error = %v", err)
		}
		want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
		if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
			t.Errorf("Args = %v, want %v", cmd.Args, want)
		}
		if !strings.Contains(stderr.String(), "手动输入问题") {
			t.Errorf("stderr = %q, want 包含手动输入提示", stderr.String())
		}
	})
}

// TestInteractiveOpencodeInitialPromptFallback 验证 server 不可达时用 --prompt 回退完整 TUI。
func TestInteractiveOpencodeInitialPromptFallback(t *testing.T) {
	shrinkMaiProbes(t)
	dir, _ := os.Getwd()
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort(t)}

	t.Setenv("MARS_FAKE_SESSIONS", fmt.Sprintf(`[{"directory":%q}]`, dir))
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"你好"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "-c --auto --prompt 你好"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}

	t.Setenv("MARS_FAKE_SESSIONS", `[]`)
	cmd, err = (opencodeBackend{}).Interactive(env, []string{"你好"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--auto --prompt 你好"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

// TestTakeBindArgs 验证 --bind 的两种写法、多选、去重、--unbind 与非法格式处理。
func TestTakeBindArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantRest     []string
		wantBindings []string
		wantUnbind   bool
		wantErr      bool
	}{
		{"未提供", []string{"你好"}, []string{"你好"}, nil, false, false},
		{"空格分隔", []string{"--bind", "uat/demo"}, nil, []string{"uat/demo"}, false, false},
		{"等号写法", []string{"--bind=uat/demo", "你好"}, []string{"你好"}, []string{"uat/demo"}, false, false},
		{"与问题共存", []string{"--bind", "uat/demo", "查看", "日志"}, []string{"查看", "日志"}, []string{"uat/demo"}, false, false},
		{"多选", []string{"--bind", "uat/demo", "--bind=prod/api"}, nil, []string{"uat/demo", "prod/api"}, false, false},
		{"多选去重", []string{"--bind=uat/demo", "--bind", "uat/demo"}, nil, []string{"uat/demo"}, false, false},
		{"首尾空白", []string{"--bind", " uat / demo "}, nil, []string{"uat/demo"}, false, false},
		{"仅命名空间", []string{"--bind", "uat"}, nil, []string{"uat"}, false, false},
		{"命名空间首尾空白", []string{"--bind", " uat "}, nil, []string{"uat"}, false, false},
		{"命名空间与项目混选", []string{"--bind", "uat", "--bind=prod/api"}, nil, []string{"uat", "prod/api"}, false, false},
		{"命名空间去重", []string{"--bind=uat", "--bind", "uat"}, nil, []string{"uat"}, false, false},
		{"显式清空", []string{"--unbind"}, nil, nil, true, false},
		{"清空与问题共存", []string{"--unbind", "你好"}, []string{"你好"}, nil, true, false},
		{"同时给出绑定与清空", []string{"--unbind", "--bind", "uat/demo"}, nil, []string{"uat/demo"}, true, false},
		{"缺值", []string{"--bind"}, nil, nil, false, true},
		{"空命名空间", []string{"--bind", "  "}, nil, nil, false, true},
		{"缺项目", []string{"--bind", "uat/"}, nil, nil, false, true},
		{"多级斜杠", []string{"--bind", "uat/demo/x"}, nil, nil, false, true},
		{"非法 bind 保留后随问题", []string{"--bind", "/demo", "你好"}, []string{"你好"}, nil, false, true},
		{"漏写范围不吞选项", []string{"--bind", "--auto"}, []string{"--auto"}, nil, false, true},
		{"等号后接选项", []string{"--bind=--auto"}, nil, nil, false, true},
		{"问题正文不解析 flag", []string{"请解释", "--unbind", "的作用"}, []string{"请解释", "--unbind", "的作用"}, nil, false, false},
		{"问题后的 bind 不解析", []string{"你好", "--bind", "uat/x"}, []string{"你好", "--bind", "uat/x"}, nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rest, bindings, unbind, err := takeBindArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("takeBindArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if strings.Join(bindings, ",") != strings.Join(tt.wantBindings, ",") {
				t.Errorf("bindings = %v, want %v", bindings, tt.wantBindings)
			}
			if unbind != tt.wantUnbind {
				t.Errorf("unbind = %v, want %v", unbind, tt.wantUnbind)
			}
			if strings.Join(rest, " ") != strings.Join(tt.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

// failingBindServer 构造健康但可注入失败点的假内核：
// sessionListStatus 非 0 时 GET /session 返回该状态（模拟查会话失败）；
// messageStatus 非 0 时 POST /session/ses_existing/message 返回该状态（模拟注入失败）。
// 其余端点按绑定场景正常响应，prompt_async 固定成功。
func failingBindServer(t *testing.T, sessionListStatus, messageStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			if sessionListStatus != 0 {
				w.WriteHeader(sessionListStatus)
				_, _ = w.Write([]byte("boom"))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			if messageStatus != 0 {
				w.WriteHeader(messageStatus)
				_, _ = w.Write([]byte("boom"))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/session/ses_existing/prompt_async" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// bindServer 构造只处理绑定场景的假内核：健康、会话列表、隐藏消息注入。
func bindServer(t *testing.T, gotText chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			var payload struct {
				NoReply bool `json:"noReply"`
				Parts   []struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					Synthetic bool   `json:"synthetic"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if !payload.NoReply {
				t.Error("绑定上下文必须 noReply=true，不能触发模型回复")
			}
			if len(payload.Parts) != 1 || !payload.Parts[0].Synthetic || payload.Parts[0].Type != "text" {
				t.Errorf("绑定上下文 parts 非法: %+v", payload.Parts)
			} else {
				gotText <- payload.Parts[0].Text
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestInteractiveOpencodeBind 验证只绑定不提问：多选绑定一并注入隐藏上下文并 attach 同一会话。
func TestInteractiveOpencodeBind(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	gotText := make(chan string, 1)
	srv := bindServer(t, gotText)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind=uat/demo", "--bind", "prod/api"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	got := recvString(t, gotText)
	for _, wantBinding := range []string{"uat/demo", "prod/api"} {
		if !strings.Contains(got, wantBinding) {
			t.Errorf("绑定上下文 = %q, want 含 %s", got, wantBinding)
		}
	}
}

// TestInteractiveOpencodeBindSkipsDuplicateHealth 验证 mai 链路复用 serverReady 的结果：
// 绑定前只有一次 health 探测，MCP 等待不再重复支付 busy 内核的 /api/health。
func TestInteractiveOpencodeBindSkipsDuplicateHealth(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "5")
	var healths, mcps int32
	gotText := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			atomic.AddInt32(&healths, 1)
			writeHealthy(w)
		case r.URL.Path == "/mcp":
			atomic.AddInt32(&mcps, 1)
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"connected"}}`))
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) == 1 {
				gotText <- payload.Parts[0].Text
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	if _, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, io.Discard); err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got := atomic.LoadInt32(&healths); got != 1 {
		t.Errorf("health 探测 %d 次, want 1（serverReady 一次，waitMCP 应复用）", got)
	}
	if got := atomic.LoadInt32(&mcps); got != 1 {
		t.Errorf("MCP 探测 %d 次, want 1", got)
	}
	_ = recvString(t, gotText)
}

// TestInteractiveOpencodeBindNewSessionSkipsDedupe 验证新建会话时不去读历史消息做去重：
// 新会话历史必为空，直接写入绑定上下文，省一次 GET 往返。
func TestInteractiveOpencodeBindNewSessionSkipsDedupe(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	var posted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"ses_new"}`))
		case r.URL.Path == "/session/ses_new/message" && r.Method == http.MethodGet:
			t.Errorf("新建会话不应查询历史消息（去重可跳过），实际 GET %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/session/ses_new/message" && r.Method == http.MethodPost:
			atomic.AddInt32(&posted, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_new", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := atomic.LoadInt32(&posted); got != 1 {
		t.Errorf("绑定消息写入 %d 次, want 1", got)
	}
}

// TestInteractiveOpencodeBindOverlapsHealthAndResolve 验证健康探测与会话解析并发：
// 服务端在收到 /session 之前不返回 health，串行实现会超时降级，只有并发才能通过。
func TestInteractiveOpencodeBindOverlapsHealthAndResolve(t *testing.T) {
	shrinkMaiProbes(t)
	serverProbeTimeout = 2 * time.Second
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	sessionSeen := make(chan struct{})
	var seenOnce sync.Once
	var healths, overlapped int32
	gotText := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			atomic.AddInt32(&healths, 1)
			select {
			case <-sessionSeen:
				atomic.StoreInt32(&overlapped, 1)
			case <-time.After(time.Second):
			}
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			seenOnce.Do(func() { close(sessionSeen) })
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) == 1 {
				gotText <- payload.Parts[0].Text
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := atomic.LoadInt32(&overlapped); got != 1 {
		t.Error("health 探测未与会话解析并发（串行实现会在这里超时降级）")
	}
	if got := atomic.LoadInt32(&healths); got != 1 {
		t.Errorf("health 探测 %d 次, want 1", got)
	}
	_ = recvString(t, gotText)
}

// TestInteractiveOpencodeBindHealthDownFallsBack 验证健康探测失败时照旧降级，
// 不因并发预热而卡住或误用会话结果。
func TestInteractiveOpencodeBindHealthDownFallsBack(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	var healths int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			atomic.AddInt32(&healths, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--auto"; got != want {
		t.Errorf("Args = %q, want %q（健康探测失败应降级为完整 TUI）", got, want)
	}
	if !strings.Contains(stderr.String(), "常驻服务未就绪") {
		t.Errorf("stderr = %q, want 包含「常驻服务未就绪」", stderr.String())
	}
	if got := atomic.LoadInt32(&healths); got != int32(serverProbeAttempts) {
		t.Errorf("health 探测 %d 次, want %d", got, serverProbeAttempts)
	}
}

// TestSessionPrepRetriesLeaseAfterDialError 验证并发预热的会话请求在 server 尚未
// 监听时拨号失败后，会在健康探测通过后补做一次，不把「还没起好」当成绑定失败。
func TestSessionPrepRetriesLeaseAfterDialError(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			writeHealthy(w)
			return
		}
		t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var calls int32
	prep, ready := startSessionPrep(context.Background(), newHTTPClient(0), srv.URL, "/proj", io.Discard,
		func(context.Context) (string, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return "", &url.Error{Op: "Get", URL: srv.URL + "/session", Err: &net.OpError{
					Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
				}}
			}
			return "ses_ok", nil
		})
	if !ready {
		t.Fatal("serverReady() = false, want true")
	}
	id, err := prep.wait()
	if err != nil || id != "ses_ok" {
		t.Fatalf("wait() = %q, %v, want ses_ok, nil", id, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("会话解析调用 %d 次, want 2（拨号失败后补做一次）", got)
	}
}

// TestSessionPrepHealthDownDoesNotWaitForSessionLock 回归：降级路径不能被目录锁拖住。
// lockSessionCreation 的 Flock 不响应 ctx 取消，而会话解析在健康探测完成前就已并发
// 启动；若另一进程正持有目录锁，等待该 goroutine 会让「常驻服务不可用→降级完整 TUI」
// 这条兜底路径一直卡到对方释放锁（旧实现在降级前根本不会碰这把锁）。
func TestSessionPrepHealthDownDoesNotWaitForSessionLock(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir := t.TempDir()

	// 模拟另一个 mai 长时间持有该目录的创建锁。
	f, err := os.OpenFile(sessionLockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("打开目录锁失败: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("持有目录锁失败: %v", err)
	}
	go func() {
		time.Sleep(5 * time.Second)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}()

	baseURL := "http://127.0.0.1:" + closedPort(t)
	started := time.Now()
	_, ready := startSessionPrep(context.Background(), newHTTPClient(0), baseURL, dir, io.Discard,
		func(rctx context.Context) (string, error) {
			return ensurePromptSessionLease(rctx, newHTTPClient(0), baseURL, dir, nil)
		})
	elapsed := time.Since(started)

	if ready {
		t.Fatal("健康探测失败时 ready 应为 false")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("降级路径被目录锁阻塞了 %v，want <2s", elapsed)
	}
}

// TestIsDialError 验证只把「连接尚未建立」视为可补做错误。
func TestIsDialError(t *testing.T) {
	dial := &url.Error{Op: "Get", URL: "http://127.0.0.1:1/session", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
	}}
	if !isDialError(dial) {
		t.Error("拨号错误未被识别")
	}
	read := &url.Error{Op: "Get", URL: "http://127.0.0.1:1/session", Err: &net.OpError{
		Op: "read", Net: "tcp", Err: errors.New("connection reset"),
	}}
	if isDialError(read) {
		t.Error("读错误不应触发补做")
	}
	if isDialError(context.DeadlineExceeded) {
		t.Error("超时不应触发补做")
	}
}

// TestInteractiveOpencodeBindServerStartsLate 验证 server 晚于 mai 启动时，
// 会话解析与 MCP 等待都会在健康探测通过后补做，最终仍 attach 正确会话。
func TestInteractiveOpencodeBindServerStartsLate(t *testing.T) {
	shrinkMaiProbes(t)
	serverProbeAttempts = 50
	serverProbeInterval = 10 * time.Millisecond
	serverProbeTimeout = 100 * time.Millisecond
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "2")
	dir, _ := os.Getwd()
	port := closedPort(t)
	var posted int32
	gotText := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/mcp":
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"connected"}}`))
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			atomic.AddInt32(&posted, 1)
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) == 1 {
				gotText <- payload.Parts[0].Text
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srvCh := make(chan *http.Server, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			srvCh <- nil
			return
		}
		s := &http.Server{Handler: handler}
		go func() { _ = s.Serve(l) }()
		srvCh <- s
	}()
	defer func() {
		if s := <-srvCh; s != nil {
			_ = s.Close()
		}
	}()

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := atomic.LoadInt32(&posted); got != 1 {
		t.Errorf("绑定消息写入 %d 次, want 1", got)
	}
	_ = recvString(t, gotText)
}

// TestInteractiveOpencodeBindWithPrompt 验证绑定与首条问题共存：先注入绑定，再投递问题。
func TestInteractiveOpencodeBindWithPrompt(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	gotText := make(chan string, 1)
	var gotPrompt atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			var payload struct {
				NoReply bool `json:"noReply"`
				Parts   []struct {
					Text      string `json:"text"`
					Synthetic bool   `json:"synthetic"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if !payload.NoReply || len(payload.Parts) != 1 || !payload.Parts[0].Synthetic {
				t.Errorf("绑定上下文非法: %+v", payload)
				return
			}
			gotText <- payload.Parts[0].Text
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/session/ses_existing/prompt_async" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) > 0 {
				gotPrompt.Store(payload.Parts[0].Text)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo", "查看", "日志"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := recvString(t, gotText); !strings.Contains(got, "uat/demo") {
		t.Errorf("绑定上下文 = %q, want 含 uat/demo", got)
	}
	if got, _ := gotPrompt.Load().(string); got != "查看 日志" {
		t.Errorf("首条问题 = %q, want %q", got, "查看 日志")
	}
}

// TestInteractiveOpencodeUnbind 验证清空全部绑定：显式注入「无绑定」隐藏上下文并 attach。
func TestInteractiveOpencodeUnbind(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	gotText := make(chan string, 1)
	srv := bindServer(t, gotText)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	got := recvString(t, gotText)
	if !strings.Contains(got, "没有绑定") || !strings.Contains(got, "不要沿用之前的绑定") {
		t.Errorf("清除绑定上下文 = %q, want 明确告知无绑定", got)
	}
}

// TestInteractiveOpencodeBindOverridesUnbind 验证 `--unbind` 与 `--bind` 同时出现时以绑定为准：
// 只注入「当前绑定范围」，不写「没有绑定」上下文（优先级由调用方分流保证）。
func TestInteractiveOpencodeBindOverridesUnbind(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	gotText := make(chan string, 1)
	srv := bindServer(t, gotText)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind", "--bind", "uat/demo"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	got := recvString(t, gotText)
	if !strings.Contains(got, "项目 uat/demo") {
		t.Errorf("绑定上下文 = %q, want 含项目 uat/demo", got)
	}
	if strings.Contains(got, "没有绑定") {
		t.Errorf("绑定上下文 = %q, 不应再写「没有绑定」", got)
	}
}

// TestLastSyntheticText 验证隐藏上下文读取器的异常路径与正常命中：
// HTTP 错误、非法 JSON、没有 synthetic 文本都返回空串，命中时取最近一条。
func TestLastSyntheticText(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"HTTP 错误", http.StatusInternalServerError, `[]`, ""},
		{"非法 JSON", http.StatusOK, `nope`, ""},
		{"没有 synthetic 文本", http.StatusOK,
			`[{"parts":[{"type":"text","text":"普通","synthetic":false}]}]`, ""},
		{"取最近一条 synthetic", http.StatusOK,
			`[{"parts":[{"type":"text","text":"旧","synthetic":true}]},{"parts":[{"type":"text","text":"新","synthetic":true}]}]`,
			"新"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			got := lastSyntheticText(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x")
			if got != tt.want {
				t.Errorf("lastSyntheticText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLastSyntheticTextLimit 验证去重查询带 limit 上限（避免长会话整量拉取被截断后
// 去重失效），并在老内核返回 400 时回退为不带 limit 的查询。
func TestLastSyntheticTextLimit(t *testing.T) {
	body := `[{"parts":[{"type":"text","text":"绑定","synthetic":true}]}]`

	t.Run("带 limit", func(t *testing.T) {
		var gotLimit string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotLimit = r.URL.Query().Get("limit")
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		if got := lastSyntheticText(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x"); got != "绑定" {
			t.Errorf("lastSyntheticText() = %q, want 绑定", got)
		}
		if gotLimit != strconv.Itoa(messageScanLimit) {
			t.Errorf("limit = %q, want %d", gotLimit, messageScanLimit)
		}
	})

	t.Run("老内核回退", func(t *testing.T) {
		var limits []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limits = append(limits, r.URL.Query().Get("limit"))
			if r.URL.Query().Get("limit") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		if got := lastSyntheticText(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x"); got != "绑定" {
			t.Errorf("lastSyntheticText() = %q, want 绑定", got)
		}
		if len(limits) != 2 || limits[0] == "" || limits[1] != "" {
			t.Errorf("limit 请求序列 = %v, want 先带 limit 再回退为空", limits)
		}
	})
}

// TestSendBindingContextError 验证注入绑定上下文失败时返回带上下文的错误。
func TestSendBindingContextError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := sendBindingContext(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x", []string{"uat"})
	if err == nil || !strings.Contains(err.Error(), "注入绑定上下文失败") {
		t.Fatalf("err = %v, want 包含「注入绑定上下文失败」", err)
	}
}

// TestSendBindingContextTransportError 验证注入时的传输层失败同样带上下文返回。
func TestSendBindingContextTransportError(t *testing.T) {
	baseURL := "http://127.0.0.1:" + closedPort(t)
	err := sendBindingContext(context.Background(), newHTTPClient(0), baseURL, "/w", "ses_x", []string{"uat"})
	if err == nil || !strings.Contains(err.Error(), "注入绑定上下文失败") {
		t.Fatalf("err = %v, want 包含「注入绑定上下文失败」", err)
	}
}

// TestSendUnbindContextSkipDuplicate 验证最近一条上下文已是「无绑定」时不重复写入。
func TestSendUnbindContextSkipDuplicate(t *testing.T) {
	var posts atomic.Int32
	body, err := json.Marshal([]map[string]any{{
		"parts": []map[string]any{{"type": "text", "text": unbindContextText, "synthetic": true}},
	}})
	if err != nil {
		t.Fatalf("构造消息响应: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := sendUnbindContext(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x"); err != nil {
		t.Fatalf("sendUnbindContext() error = %v", err)
	}
	if posts.Load() != 0 {
		t.Errorf("重复执行注入 = %d 次, want 0", posts.Load())
	}
}

// TestSendUnbindContextError 验证清除绑定上下文失败时返回带上下文的错误。
func TestSendUnbindContextError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := sendUnbindContext(context.Background(), newHTTPClient(0), srv.URL, "/w", "ses_x")
	if err == nil || !strings.Contains(err.Error(), "清除绑定上下文失败") {
		t.Fatalf("err = %v, want 包含「清除绑定上下文失败」", err)
	}
}

// TestInteractiveOpencodeBindSessionCreateFailed 验证建会话失败时绑定降级为普通 attach。
func TestInteractiveOpencodeBindSessionCreateFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, http.StatusInternalServerError, 0)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "无法绑定 uat") {
		t.Errorf("stderr = %q, want 包含「无法绑定 uat」", stderr.String())
	}
}

// TestInteractiveOpencodeBindInjectFailed 验证注入失败时仍 attach 到已有会话并告警。
func TestInteractiveOpencodeBindInjectFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, 0, http.StatusInternalServerError)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "绑定未生效") {
		t.Errorf("stderr = %q, want 包含「绑定未生效」", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindServerDown 验证常驻服务不可达时清除绑定降级为普通 TUI。
func TestInteractiveOpencodeUnbindServerDown(t *testing.T) {
	shrinkMaiProbes(t)
	dir, _ := os.Getwd()
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort(t)}
	t.Setenv("MARS_FAKE_SESSIONS", fmt.Sprintf(`[{"directory":%q}]`, dir))

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"--unbind"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "-c --auto"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "无法清除绑定") {
		t.Errorf("stderr = %q, want 包含「无法清除绑定」", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindSessionQueryFailed 验证查会话失败时清除绑定降级为普通 attach。
func TestInteractiveOpencodeUnbindSessionQueryFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, http.StatusInternalServerError, 0)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "无法清除绑定") {
		t.Errorf("stderr = %q, want 包含「无法清除绑定」", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindInjectFailed 验证清除绑定注入失败时仍 attach 并告警。
func TestInteractiveOpencodeUnbindInjectFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, 0, http.StatusInternalServerError)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "请手动确认要针对的空间或项目") {
		t.Errorf("stderr = %q, want 包含手动确认提示", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindPromptServerDown 验证服务不可达时「清空 + 问题」
// 只告警无法清除绑定，问题仍通过 --prompt 发出。
func TestInteractiveOpencodeUnbindPromptServerDown(t *testing.T) {
	shrinkMaiProbes(t)
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort(t)}
	t.Setenv("MARS_FAKE_SESSIONS", `[]`)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"--unbind", "你好"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--auto --prompt 你好"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "无法清除绑定") {
		t.Errorf("stderr = %q, want 包含「无法清除绑定」", stderr.String())
	}
}

// TestInteractiveOpencodeBindPromptInjectFailed 验证绑定注入失败时问题照常投递。
func TestInteractiveOpencodeBindPromptInjectFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, 0, http.StatusInternalServerError)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat", "你好"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "绑定未生效") {
		t.Errorf("stderr = %q, want 包含「绑定未生效」", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindPromptInjectFailed 验证清空绑定注入失败时问题照常投递。
func TestInteractiveOpencodeUnbindPromptInjectFailed(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := failingBindServer(t, 0, http.StatusInternalServerError)
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind", "你好"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "请手动确认要针对的空间或项目") {
		t.Errorf("stderr = %q, want 包含手动确认提示", stderr.String())
	}
}

// TestInteractiveOpencodeUnbindWithPrompt 验证清空绑定与首条问题共存：先清除旧绑定，再投递问题。
func TestInteractiveOpencodeUnbindWithPrompt(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	gotText := make(chan string, 1)
	var gotPrompt atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodPost:
			var payload struct {
				NoReply bool `json:"noReply"`
				Parts   []struct {
					Text      string `json:"text"`
					Synthetic bool   `json:"synthetic"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if !payload.NoReply || len(payload.Parts) != 1 || !payload.Parts[0].Synthetic {
				t.Errorf("清除绑定上下文非法: %+v", payload)
				return
			}
			gotText <- payload.Parts[0].Text
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/session/ses_existing/prompt_async" && r.Method == http.MethodPost:
			var payload struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.Parts) > 0 {
				gotPrompt.Store(payload.Parts[0].Text)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind", "查看", "日志"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if got := recvString(t, gotText); !strings.Contains(got, "没有绑定") {
		t.Errorf("清除绑定上下文 = %q, want 明确告知无绑定", got)
	}
	if got, _ := gotPrompt.Load().(string); got != "查看 日志" {
		t.Errorf("首条问题 = %q, want %q", got, "查看 日志")
	}
}

// TestBindingContextText 验证两种绑定粒度的上下文文案：空间级明确覆盖其下全部项目。
func TestBindingContextText(t *testing.T) {
	tests := []struct {
		name     string
		bindings []string
		want     []string
	}{
		{"命名空间", []string{"uat"}, []string{"命名空间 uat（含该空间下全部项目）"}},
		{"项目", []string{"uat/demo"}, []string{"项目 uat/demo"}},
		{"混选", []string{"uat", "prod/api"}, []string{"命名空间 uat（含该空间下全部项目）", "项目 prod/api"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bindingContextText(tt.bindings)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("bindingContextText(%v) = %q, 缺少 %q", tt.bindings, got, want)
				}
			}
		})
	}
}

// TestBindingContextTextOrderInsensitive 验证绑定集合与传入顺序无关：
// 同一组范围换序必须生成相同文本，否则去重（最近一条相同则跳过）会被绕过。
func TestBindingContextTextOrderInsensitive(t *testing.T) {
	a := bindingContextText([]string{"uat", "prod/api"})
	b := bindingContextText([]string{"prod/api", "uat"})
	if a != b {
		t.Errorf("绑定顺序影响文案:\n a = %q\n b = %q", a, b)
	}
	// 传入切片不应被就地排序（调用方可能复用）。
	input := []string{"uat", "prod/api"}
	_ = bindingContextText(input)
	if strings.Join(input, ",") != "uat,prod/api" {
		t.Errorf("bindingContextText 修改了入参: %v", input)
	}
}

// TestInteractiveOpencodeBindSkipsDuplicate 验证会话里已是同样的绑定时不重复写入。
func TestInteractiveOpencodeBindSkipsDuplicate(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	text := bindingContextText([]string{"uat/demo"})
	body, err := json.Marshal([]map[string]any{{
		"parts": []map[string]any{{"type": "text", "text": text, "synthetic": true}},
	}})
	if err != nil {
		t.Fatalf("构造消息响应: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"ses_existing"}]`))
		case r.URL.Path == "/session/ses_existing/message" && r.Method == http.MethodGet:
			_, _ = w.Write(body)
		default:
			t.Errorf("重复绑定不应再写消息，意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "uat/demo"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "-s", "ses_existing", "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestInteractiveOpencodeUnbindWithoutSession 验证没有会话时清空绑定不新建空会话。
func TestInteractiveOpencodeUnbindWithoutSession(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == healthPath:
			writeHealthy(w)
		case r.URL.Path == "/session" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("无会话时不应创建会话，意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--unbind"}, io.Discard)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestInteractiveOpencodeInitialPromptFallbackWarnsBinding 验证 server 不可达时
// 带问题的调用会告警绑定未生效，同时照常通过 --prompt 发送问题。
func TestInteractiveOpencodeInitialPromptFallbackWarnsBinding(t *testing.T) {
	shrinkMaiProbes(t)
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort(t)}
	t.Setenv("MARS_FAKE_SESSIONS", `[]`)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"--bind", "uat/demo", "你好"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--auto --prompt 你好"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "无法绑定") {
		t.Errorf("stderr = %q, want 包含无法绑定", stderr.String())
	}
}

// TestInteractiveOpencodeRunWithBindKeepsSubcommand 验证位置参数之后的 --bind
// 不被解析，run 子命令保持原有透传语义。
func TestInteractiveOpencodeRunWithBindKeepsSubcommand(t *testing.T) {
	shrinkMaiProbes(t)
	args := []string{"run", "--bind", "uat/x", "hi"}
	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + closedPort(t),
	}, args, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if strings.Join(cmd.Args, " ") != strings.Join(args, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, args)
	}
	if strings.Contains(stderr.String(), "忽略绑定") {
		t.Errorf("stderr = %q, 不应把 run 之后的 --bind 当绑定解析", stderr.String())
	}
}

// TestInteractiveOpencodeBindBeforeRunIgnored 验证绑定写在子命令之前时也不会劫持
// `mai --bind a/b run hi`：绑定被忽略并告警，run 仍按原语义执行。
func TestInteractiveOpencodeBindBeforeRunIgnored(t *testing.T) {
	shrinkMaiProbes(t)
	args := []string{"--bind", "uat/x", "run", "hi"}
	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + closedPort(t),
	}, args, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if strings.Join(cmd.Args, " ") != strings.Join([]string{"run", "hi"}, " ") {
		t.Errorf("Args = %v, want [run hi]", cmd.Args)
	}
	if !strings.Contains(stderr.String(), "绑定参数需要不带其他参数的交互式 mai") {
		t.Errorf("stderr = %q, want 包含忽略提示", stderr.String())
	}
}

// TestInteractiveOpencodeBindFallback 验证常驻服务不可达时忽略绑定、仍能进入 TUI。
func TestInteractiveOpencodeBindFallback(t *testing.T) {
	shrinkMaiProbes(t)
	dir, _ := os.Getwd()
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nprintf '%s' \"$MARS_FAKE_SESSIONS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake bin: %v", err)
	}
	env := Env{AIBin: bin, ListenAddr: "0.0.0.0:" + closedPort(t)}
	t.Setenv("MARS_FAKE_SESSIONS", fmt.Sprintf(`[{"directory":%q}]`, dir))

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(env, []string{"--bind", "uat/demo"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "-c --auto"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "无法绑定") {
		t.Errorf("stderr = %q, want 包含无法绑定", stderr.String())
	}
}

// TestInteractiveOpencodeBindWithFlagIgnored 验证绑定与其他选项同时出现时告警并原样透传。
func TestInteractiveOpencodeBindWithFlagIgnored(t *testing.T) {
	shrinkMaiProbes(t)
	args := []string{"--bind", "uat", "--help"}
	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + closedPort(t),
	}, args, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if got, want := strings.Join(cmd.Args, " "), "--help"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "绑定参数需要不带其他参数的交互式 mai") {
		t.Errorf("stderr = %q, want 包含忽略提示", stderr.String())
	}
}

// TestInteractiveOpencodeBindInvalid 验证非法绑定只告警、按无绑定进入 TUI。
func TestInteractiveOpencodeBindInvalid(t *testing.T) {
	shrinkMaiProbes(t)
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "0")
	dir, _ := os.Getwd()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/session":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	port := serverPort(t, srv.URL)

	var stderr strings.Builder
	cmd, err := (opencodeBackend{}).Interactive(Env{
		AIBin:      "/bin/echo",
		ListenAddr: "0.0.0.0:" + port,
	}, []string{"--bind", "/demo"}, &stderr)
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	want := []string{"attach", "--dir", dir, "http://127.0.0.1:" + port}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if !strings.Contains(stderr.String(), "忽略绑定") {
		t.Errorf("stderr = %q, want 包含忽略绑定", stderr.String())
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
