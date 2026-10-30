package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// captureExec 记录被 exec 的命令，模拟成功。
type captureExec struct {
	cmds []Cmd
	err  error
}

func (c *captureExec) Run(cmd Cmd) error {
	c.cmds = append(c.cmds, cmd)
	return c.err
}

func clearMarsEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MARS_GATEWAY_URL", "MARS_SESSION_TOKEN", "MARS_MODEL", "MARS_AGENT_BACKEND",
		"MARS_AI_BIN", "MARS_LISTEN_ADDR", "MARS_MCP_WAIT_TIMEOUT", "MARS_MCP_PREWARM_TIMEOUT",
		"MARS_LLM_BASE_URL", "MARS_MCP_URL", "MARS_GRAY",
	} {
		t.Setenv(key, "")
	}
}

// TestResolveExecPath 验证 exec 前的路径解析还原 shell 语义。
func TestResolveExecPath(t *testing.T) {
	if got, err := resolveExecPath("/bin/echo"); err != nil || got != "/bin/echo" {
		t.Errorf("含分隔符的路径 = %q, %v; want /bin/echo, nil", got, err)
	}
	got, err := resolveExecPath("echo")
	if err != nil {
		t.Fatalf("裸命令 echo 应能通过 PATH 解析: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("PATH 查找结果应为绝对路径, got %q", got)
	}
	if _, err := resolveExecPath("mars-agent-no-such-binary-xyz"); err == nil {
		t.Error("不存在的裸命令应返回错误")
	}
}

// TestDispatchUnknownSubcommand 验证未知子命令输出用法并返回 64。
func TestDispatchUnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	if got := dispatch([]string{"nope"}, &stderr, nil); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "未知子命令") || !strings.Contains(stderr.String(), "mcp-wait") {
		t.Errorf("stderr = %q, want 未知子命令 + usage", stderr.String())
	}
}

// TestDispatchServeMissingEnv 验证严格校验失败时返回 1 且不 exec。
func TestDispatchServeMissingEnv(t *testing.T) {
	clearMarsEnv(t)
	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch(nil, &stderr, ex.Run); got != exitFailure {
		t.Errorf("exit = %d, want %d", got, exitFailure)
	}
	if len(ex.cmds) != 0 {
		t.Errorf("exec 被调用 %d 次, want 0", len(ex.cmds))
	}
	for _, want := range []string{"MARS_GATEWAY_URL", "MARS_SESSION_TOKEN", "MARS_MODEL"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q 未包含 %s", stderr.String(), want)
		}
	}
}

// TestDispatchServeUnknownBackend 验证未知内核 fail fast，且不 exec。
func TestDispatchServeUnknownBackend(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_GATEWAY_URL", "http://gw")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m")
	t.Setenv("MARS_AGENT_BACKEND", "claude")
	t.Setenv("HOME", t.TempDir())

	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch(nil, &stderr, ex.Run); got != exitFailure {
		t.Errorf("exit = %d, want %d", got, exitFailure)
	}
	if len(ex.cmds) != 0 {
		t.Errorf("exec 被调用 %d 次, want 0", len(ex.cmds))
	}
	if !strings.Contains(stderr.String(), "claude") || !strings.Contains(stderr.String(), "opencode") {
		t.Errorf("stderr = %q, want 含可用内核列表", stderr.String())
	}
}

// TestDispatchServeExec 验证默认走 serve：先落配置再 exec 内核。
func TestDispatchServeExec(t *testing.T) {
	clearMarsEnv(t)
	home := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(home) })
	t.Setenv("MARS_GATEWAY_URL", "http://gw")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m1")
	t.Setenv("MARS_AI_BIN", "/bin/echo")
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:4096")
	t.Setenv("HOME", home)

	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch(nil, &stderr, ex.Run); got != 0 {
		t.Fatalf("exit = %d, want 0, stderr=%s", got, stderr.String())
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("exec 被调用 %d 次, want 1", len(ex.cmds))
	}
	cmd := ex.cmds[0]
	if cmd.Path != "/bin/echo" {
		t.Errorf("Path = %q, want /bin/echo", cmd.Path)
	}
	if got, want := strings.Join(cmd.Args, " "), "serve --hostname 0.0.0.0 --port 4096"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
	if _, err := os.Stat(home + "/.config/opencode/opencode.json"); err != nil {
		t.Errorf("配置未落盘: %v", err)
	}
}

// TestDispatchServeExecError 验证 exec 失败映射为退出码 1。
func TestDispatchServeExecError(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_GATEWAY_URL", "http://gw")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m1")
	t.Setenv("MARS_AI_BIN", "/bin/echo")
	home := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(home) })
	t.Setenv("HOME", home)

	ex := &captureExec{err: errors.New("exec 失败")}
	var stderr strings.Builder
	if got := dispatch(nil, &stderr, ex.Run); got != exitFailure {
		t.Errorf("exit = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "exec 失败") {
		t.Errorf("stderr = %q, want 含 exec 错误", stderr.String())
	}
}

// TestDispatchMaiPassthrough 验证 mai 对不需探测的参数原样透传（不发任何请求）。
func TestDispatchMaiPassthrough(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_AI_BIN", "/bin/echo")

	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch([]string{"mai", "run", "--help"}, &stderr, ex.Run); got != 0 {
		t.Fatalf("exit = %d, want 0, stderr=%s", got, stderr.String())
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("exec 被调用 %d 次, want 1", len(ex.cmds))
	}
	if got, want := strings.Join(ex.cmds[0].Args, " "), "run --help"; got != want {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

// TestDispatchHealth 验证探针退出码 0/1。
func TestDispatchHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != healthPath {
			t.Errorf("路径 = %s, want %s", r.URL.Path, healthPath)
		}
		writeHealthy(w)
	}))
	defer srv.Close()

	clearMarsEnv(t)
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+serverPort(t, srv.URL))
	var stderr strings.Builder
	if got := dispatch([]string{"health"}, &stderr, nil); got != 0 {
		t.Errorf("exit = %d, want 0, stderr=%s", got, stderr.String())
	}

	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+closedPort(t))
	stderr.Reset()
	if got := dispatch([]string{"health"}, &stderr, nil); got != exitFailure {
		t.Errorf("exit = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "health") {
		t.Errorf("stderr = %q, want 含 health", stderr.String())
	}
}

// TestDispatchMCPWaitUsage 验证未知参数返回 64。
func TestDispatchMCPWaitUsage(t *testing.T) {
	clearMarsEnv(t)
	var stderr strings.Builder
	if got := dispatch([]string{"mcp-wait", "--bogus"}, &stderr, nil); got != exitUsage {
		t.Errorf("exit = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "未知参数") {
		t.Errorf("stderr = %q, want 未知参数", stderr.String())
	}
}

// TestDispatchMCPWaitUnreachable 验证内核不可达时返回 2。
func TestDispatchMCPWaitUnreachable(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+closedPort(t))
	var stderr strings.Builder
	if got := dispatch([]string{"mcp-wait", "--dir", "/proj"}, &stderr, nil); got != exitServerUnreachable {
		t.Errorf("exit = %d, want %d, stderr=%s", got, exitServerUnreachable, stderr.String())
	}
}

// TestDispatchPrewarmDisabled 验证超时非法或 ≤0 时直接 0，不触碰内核。
func TestDispatchPrewarmDisabled(t *testing.T) {
	for _, raw := range []string{"0", "-1", "abc"} {
		clearMarsEnv(t)
		t.Setenv("MARS_MCP_PREWARM_TIMEOUT", raw)
		t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+closedPort(t))
		var stderr strings.Builder
		if got := dispatch([]string{"prewarm"}, &stderr, nil); got != 0 {
			t.Errorf("timeout=%q exit = %d, want 0, stderr=%s", raw, got, stderr.String())
		}
	}
}

// stubBackend 是能力可控的内核替身。WaitMCP 一旦被调用即返回错误，
// 用来证明 mcp-wait / prewarm 对不支持 MCP 探测的内核确实跳过，而不是空转等待。
type stubBackend struct {
	caps Capabilities
}

func (stubBackend) Name() string                      { return "stub" }
func (stubBackend) Prepare(Env) error                 { return nil }
func (stubBackend) Launch(Env) (Cmd, error)           { return Cmd{Path: "/bin/echo"}, nil }
func (stubBackend) Health(context.Context, Env) error { return nil }
func (s stubBackend) Capabilities() Capabilities      { return s.caps }
func (stubBackend) Interactive(Env, []string, io.Writer) (Cmd, error) {
	return Cmd{Path: "/bin/echo"}, nil
}
func (stubBackend) WaitMCP(context.Context, Env, MCPWaitOptions) error {
	return errors.New("stub: WaitMCP 不应被调用")
}

// registerBackend 临时注册内核替身，测试结束恢复注册表（测试串行执行）。
func registerBackend(t *testing.T, name string, backend Backend) {
	t.Helper()
	old, had := backendRegistry[name]
	backendRegistry[name] = backend
	t.Cleanup(func() {
		if had {
			backendRegistry[name] = old
			return
		}
		delete(backendRegistry, name)
	})
}

// TestMCPCommandsSkipWhenUnsupported 验证 MCPProbe=false 时两个命令直接成功、不调用 WaitMCP。
func TestMCPCommandsSkipWhenUnsupported(t *testing.T) {
	clearMarsEnv(t)
	registerBackend(t, "no-mcp", stubBackend{})
	t.Setenv("MARS_AGENT_BACKEND", "no-mcp")
	t.Setenv("MARS_MCP_PREWARM_TIMEOUT", "30")
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+closedPort(t))

	var stderr strings.Builder
	if got := dispatch([]string{"mcp-wait", "--timeout", "30"}, &stderr, nil); got != 0 {
		t.Errorf("mcp-wait exit = %d, want 0, stderr=%s", got, stderr.String())
	}
	stderr.Reset()
	if got := dispatch([]string{"prewarm"}, &stderr, nil); got != 0 {
		t.Errorf("prewarm exit = %d, want 0, stderr=%s", got, stderr.String())
	}
}

// TestDispatchMCPWaitConnected 验证 MCP 已 connected 时退出码 0（命令层全链路）。
func TestDispatchMCPWaitConnected(t *testing.T) {
	clearMarsEnv(t)
	srv := mcpStatusServer(t, `{"`+mcpServerName+`":{"status":"connected"}}`)
	defer srv.Close()
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+serverPort(t, srv.URL))

	var stderr strings.Builder
	if got := dispatch([]string{"mcp-wait", "--timeout", "1"}, &stderr, nil); got != 0 {
		t.Errorf("exit = %d, want 0, stderr=%s", got, stderr.String())
	}
}

// TestDispatchMCPWaitTimeout 验证预算耗尽时命令层返回 1 并带状态说明。
func TestDispatchMCPWaitTimeout(t *testing.T) {
	clearMarsEnv(t)
	srv := mcpStatusServer(t, `{"`+mcpServerName+`":{"status":"failed","error":"boom"}}`)
	defer srv.Close()
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+serverPort(t, srv.URL))

	var stderr strings.Builder
	if got := dispatch([]string{"mcp-wait", "--timeout", "1"}, &stderr, nil); got != exitFailure {
		t.Errorf("exit = %d, want %d, stderr=%s", got, exitFailure, stderr.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, "未在 1s 内就绪") || !strings.Contains(msg, "status=failed") {
		t.Errorf("stderr = %q, want 超时状态", msg)
	}
}

// TestDispatchPrewarmReady 验证 prewarm 走通成功分支。
func TestDispatchPrewarmReady(t *testing.T) {
	clearMarsEnv(t)
	srv := mcpStatusServer(t, `{"`+mcpServerName+`":{"status":"connected"}}`)
	defer srv.Close()
	t.Setenv("MARS_LISTEN_ADDR", "0.0.0.0:"+serverPort(t, srv.URL))
	t.Setenv("MARS_MCP_PREWARM_TIMEOUT", "5")
	t.Setenv("HOME", t.TempDir())

	var stderr strings.Builder
	if got := dispatch([]string{"prewarm"}, &stderr, nil); got != 0 {
		t.Errorf("exit = %d, want 0, stderr=%s", got, stderr.String())
	}
}

// TestDispatchPrewarmUnknownBackend 验证 prewarm 不会吞掉内核选择错误。
func TestDispatchPrewarmUnknownBackend(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_AGENT_BACKEND", "nope")
	t.Setenv("MARS_MCP_PREWARM_TIMEOUT", "5")

	var stderr strings.Builder
	if got := dispatch([]string{"prewarm"}, &stderr, nil); got != exitFailure {
		t.Errorf("exit = %d, want %d, stderr=%s", got, exitFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "未知的 agent 内核") {
		t.Errorf("stderr = %q, want 未知内核提示", stderr.String())
	}
}

// TestDispatchHealthUnknownBackend 验证 health 探针同样对未知内核 fail fast。
func TestDispatchHealthUnknownBackend(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_AGENT_BACKEND", "nope")

	var stderr strings.Builder
	if got := dispatch([]string{"health"}, &stderr, nil); got != exitFailure {
		t.Errorf("exit = %d, want %d, stderr=%s", got, exitFailure, stderr.String())
	}
}

// TestDispatchServePrepareError 验证落盘失败时不 exec 内核。
func TestDispatchServePrepareError(t *testing.T) {
	clearMarsEnv(t)
	home := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatalf("写占位文件: %v", err)
	}
	t.Setenv("MARS_GATEWAY_URL", "http://gw")
	t.Setenv("MARS_SESSION_TOKEN", "tok")
	t.Setenv("MARS_MODEL", "m")
	t.Setenv("MARS_AI_BIN", "/bin/echo")
	t.Setenv("HOME", home)

	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch(nil, &stderr, ex.Run); got != exitFailure {
		t.Errorf("exit = %d, want %d, stderr=%s", got, exitFailure, stderr.String())
	}
	if len(ex.cmds) != 0 {
		t.Errorf("落盘失败仍 exec 了 %d 次", len(ex.cmds))
	}
}

// TestDispatchMaiErrors 验证 mai 的未知内核与 exec 失败都映射为 1。
func TestDispatchMaiErrors(t *testing.T) {
	clearMarsEnv(t)
	t.Setenv("MARS_AI_BIN", "/bin/echo")

	t.Setenv("MARS_AGENT_BACKEND", "claude")
	ex := &captureExec{}
	var stderr strings.Builder
	if got := dispatch([]string{"mai"}, &stderr, ex.Run); got != exitFailure {
		t.Errorf("未知内核 exit = %d, want %d, stderr=%s", got, exitFailure, stderr.String())
	}
	if len(ex.cmds) != 0 {
		t.Errorf("未知内核不应 exec, 实际 %d 次", len(ex.cmds))
	}

	t.Setenv("MARS_AGENT_BACKEND", "")
	ex = &captureExec{err: errors.New("exec 失败")}
	stderr.Reset()
	if got := dispatch([]string{"mai", "run", "--help"}, &stderr, ex.Run); got != exitFailure {
		t.Errorf("exec 失败 exit = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "exec 失败") {
		t.Errorf("stderr = %q, want 含 exec 错误", stderr.String())
	}
}

// writeHealthy 输出钉版 opencode /api/health 的最小合法响应体。
func writeHealthy(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"healthy":true}`))
}

// mcpStatusServer 返回固定 MCP 状态的替身内核（/api/health 恒 healthy，/mcp 返回 body）。
func mcpStatusServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/mcp":
			_, _ = w.Write([]byte(body))
		case "/mcp/" + mcpServerName + "/connect":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
}

// TestExecCmdReplacesProcess 在子进程里真正调用 syscall.Exec：
// 既覆盖裸命令的 PATH 解析，也证明成功路径不会回落到 os.Exit。
func TestExecCmdReplacesProcess(t *testing.T) {
	mode := os.Getenv("MARS_TEST_EXEC_MODE")
	if mode == "" {
		for _, bin := range []string{"/bin/echo", "echo"} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestExecCmdReplacesProcess$")
			cmd.Env = append(os.Environ(), "MARS_TEST_EXEC_MODE="+bin)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("子进程失败（bin=%s）: %v, output=%s", bin, err, out)
			}
			if !strings.Contains(string(out), "exec-ok") {
				t.Errorf("bin=%s output = %q, want 含 exec-ok", bin, out)
			}
		}
		return
	}
	if err := execCmd(Cmd{Path: mode, Args: []string{"exec-ok"}}); err != nil {
		t.Fatalf("execCmd(%q): %v", mode, err)
	}
	// execCmd 成功时进程已被替换；走到这里说明没有发生 exec。
	t.Fatalf("execCmd(%q) 未替换进程", mode)
}
