package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock 让等待逻辑在测试中零耗时推进。
type fakeClock struct {
	t     *testing.T
	now   time.Time
	slept int
}

func newFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	return &fakeClock{t: t, now: time.Unix(0, 0)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
		c.t.Fatalf("等待被取消: %v", ctx.Err())
	default:
	}
	c.slept++
	c.now = c.now.Add(d)
}

// TestParseMCPWaitArgs 验证参数解析与旧脚本一致（只接受空格分隔形式）。
func TestParseMCPWaitArgs(t *testing.T) {
	t.Setenv("MARS_MCP_WAIT_TIMEOUT", "")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	opt, err := parseMCPWaitArgs(nil)
	if err != nil {
		t.Fatalf("parseMCPWaitArgs(nil) error = %v", err)
	}
	if opt.Dir != cwd || opt.Timeout != defaultMCPWaitTimeout || opt.ServerTimeout != 0 {
		t.Errorf("默认值 = %+v, want dir=%s timeout=%d serverTimeout=0", opt, cwd, defaultMCPWaitTimeout)
	}

	opt, err = parseMCPWaitArgs([]string{"--dir", "/x", "--timeout", "5", "--server-timeout", "7"})
	if err != nil {
		t.Fatalf("parseMCPWaitArgs error = %v", err)
	}
	if opt.Dir != "/x" || opt.Timeout != 5 || opt.ServerTimeout != 7 {
		t.Errorf("解析结果 = %+v, want /x 5 7", opt)
	}

	// 非数字回落各自默认值。
	opt, err = parseMCPWaitArgs([]string{"--timeout", "abc", "--server-timeout", "-1"})
	if err != nil {
		t.Fatalf("parseMCPWaitArgs error = %v", err)
	}
	if opt.Timeout != defaultMCPWaitTimeout {
		t.Errorf("非法 --timeout = %d, want %d", opt.Timeout, defaultMCPWaitTimeout)
	}
	if opt.ServerTimeout != 0 {
		t.Errorf("非法 --server-timeout = %d, want 0", opt.ServerTimeout)
	}

	for _, args := range [][]string{
		{"--unknown"},
		{"--dir"},
		{"--timeout"},
		{"--server-timeout"},
		{"--dir=/x"},
		{"--"},
	} {
		if _, err := parseMCPWaitArgs(args); err == nil {
			t.Errorf("parseMCPWaitArgs(%v) error = nil, want error", args)
		}
	}
}

// TestEnvMCPWaitTimeout 验证环境变量缺省、合法与非法值的回落语义。
func TestEnvMCPWaitTimeout(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"", defaultMCPWaitTimeout},
		{"7", 7},
		{"0", 0},
		{"-1", defaultMCPWaitTimeout},
		{"abc", defaultMCPWaitTimeout},
	}
	for _, tt := range tests {
		t.Setenv("MARS_MCP_WAIT_TIMEOUT", tt.raw)
		if got := envMCPWaitTimeout(); got != tt.want {
			t.Errorf("envMCPWaitTimeout() with %q = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

// TestParseDigits 验证只接受非负纯数字，溢出同样视为非法。
func TestParseDigits(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{"", 0, false},
		{"0", 0, true},
		{"12", 12, true},
		{"-1", 0, false},
		{"1a", 0, false},
		{" 1", 0, false},
		{"999999999999999999999999", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseDigits(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseDigits(%q) = %d, %v; want %d, %v", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

// TestWaitMCPImmediateConnected 验证首次探测即 connected 时不发起 connect、不 sleep。
func TestWaitMCPImmediateConnected(t *testing.T) {
	var connects int32
	gotDir := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/mcp":
			gotDir <- r.URL.Query().Get("directory")
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"connected"}}`))
		case "/mcp/" + mcpServerName + "/connect":
			atomic.AddInt32(&connects, 1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	clock := newFakeClock(t)
	err := waitMCPWith(context.Background(), srv.URL, MCPWaitOptions{Dir: "/proj", Timeout: 15}, clock.Now, clock.Sleep)
	if err != nil {
		t.Fatalf("waitMCPWith() error = %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 0 {
		t.Errorf("connect 被调用 %d 次, want 0", got)
	}
	if clock.slept != 0 {
		t.Errorf("sleep 次数 = %d, want 0", clock.slept)
	}
	if dir := <-gotDir; dir != "/proj" {
		t.Errorf("directory = %q, want /proj", dir)
	}
}

// TestWaitMCPBusyHealth 回归：busy 内核的 /api/health 需要 1~3s，而 serverTimeout=0 时只有
// 一次探测机会。旧的 1s 预算（沿用 shell --max-time 1）会把活着的 server 判成不可达，
// mai 于是跳过 MCP 等待、带着 failed 状态的 MCP 进 TUI。这里用真实默认值验证。
func TestWaitMCPBusyHealth(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实等待约 1.5s")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			time.Sleep(1500 * time.Millisecond) // 落在文档所述 1~3s 区间内
			writeHealthy(w)
		case "/mcp":
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"connected"}}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	clock := newFakeClock(t)
	err := waitMCPWith(context.Background(), srv.URL,
		MCPWaitOptions{Dir: "/proj", Timeout: 15, ServerTimeout: 0}, clock.Now, clock.Sleep)
	if err != nil {
		t.Fatalf("waitMCPWith() error = %v, 忙但活着的 server 不应判为不可达", err)
	}
}

// TestWaitMCPConnectThenConnected 验证先 failed 后重连成功，且 connect 请求带 directory。
func TestWaitMCPConnectThenConnected(t *testing.T) {
	var probes, connects int32
	connectDir := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/mcp":
			if atomic.AddInt32(&probes, 1) == 1 {
				_, _ = w.Write([]byte(`{"mars-tools":{"status":"failed","error":"boom"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"connected"}}`))
		case "/mcp/" + mcpServerName + "/connect":
			atomic.AddInt32(&connects, 1)
			connectDir <- r.URL.Query().Get("directory")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	clock := newFakeClock(t)
	var log strings.Builder
	err := waitMCPWith(context.Background(), srv.URL, MCPWaitOptions{Dir: "/proj", Timeout: 15, Log: &log}, clock.Now, clock.Sleep)
	if err != nil {
		t.Fatalf("waitMCPWith() error = %v", err)
	}
	if got := atomic.LoadInt32(&connects); got != 1 {
		t.Errorf("connect 被调用 %d 次, want 1", got)
	}
	if dir := <-connectDir; dir != "/proj" {
		t.Errorf("connect directory = %q, want /proj", dir)
	}
	if !strings.Contains(log.String(), "等待 MCP 工具服务就绪") {
		t.Errorf("缺少等待 banner: %q", log.String())
	}
}

// TestWaitMCPServerUnreachable 验证 server 不可达时返回哨兵错误（退出码 2）。
func TestWaitMCPServerUnreachable(t *testing.T) {
	clock := newFakeClock(t)
	var log strings.Builder
	err := waitMCPWith(context.Background(), "http://127.0.0.1:"+closedPort(t),
		MCPWaitOptions{Dir: "/proj", Timeout: 15, ServerTimeout: 0, Log: &log},
		clock.Now, clock.Sleep)
	if !errors.Is(err, errMCPServerUnreachable) {
		t.Fatalf("waitMCPWith() error = %v, want errMCPServerUnreachable", err)
	}
	if got := mcpWaitExitCode(err); got != exitServerUnreachable {
		t.Errorf("退出码 = %d, want %d", got, exitServerUnreachable)
	}
	if !strings.Contains(log.String(), "opencode server 未在 0s 内就绪") {
		t.Errorf("缺少不可达消息: %q", log.String())
	}
}

// TestWaitMCPTimeout 验证预算耗尽后返回 errMCPNotReady 并打印 status。
func TestWaitMCPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/mcp":
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"failed","error":"boom"}}`))
		case "/mcp/" + mcpServerName + "/connect":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	clock := newFakeClock(t)
	var log strings.Builder
	err := waitMCPWith(context.Background(), srv.URL, MCPWaitOptions{Dir: "/proj", Timeout: 2, Log: &log}, clock.Now, clock.Sleep)
	if !errors.Is(err, errMCPNotReady) {
		t.Fatalf("waitMCPWith() error = %v, want errMCPNotReady", err)
	}
	if got := mcpWaitExitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d, want %d", got, exitFailure)
	}
	msg := log.String()
	if !strings.Contains(msg, "未在 2s 内就绪") || !strings.Contains(msg, "status=failed") || !strings.Contains(msg, "boom") {
		t.Errorf("超时消息不完整: %q", msg)
	}
	// 2 秒预算、每次迭代 1 秒：与旧脚本一致，第 3 次进入循环时才发现预算耗尽。
	if clock.slept != 2 {
		t.Errorf("sleep 次数 = %d, want 2", clock.slept)
	}
}

// TestWaitMCPTimeoutZero 验证 timeout=0 时只探测一次、不打印 banner。
func TestWaitMCPTimeoutZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case healthPath:
			writeHealthy(w)
		case "/mcp":
			_, _ = w.Write([]byte(`{"mars-tools":{"status":"failed"}}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	clock := newFakeClock(t)
	var log strings.Builder
	err := waitMCPWith(context.Background(), srv.URL, MCPWaitOptions{Dir: "/proj", Timeout: 0, Log: &log}, clock.Now, clock.Sleep)
	if !errors.Is(err, errMCPNotReady) {
		t.Fatalf("waitMCPWith() error = %v, want errMCPNotReady", err)
	}
	if log.Len() != 0 {
		t.Errorf("timeout=0 不应打印消息, got %q", log.String())
	}
	if clock.slept != 0 {
		t.Errorf("sleep 次数 = %d, want 0", clock.slept)
	}
}

// TestWaitMCPContextCancelled 验证 ctx 取消后立即返回，不会在预算未耗尽时空转。
func TestWaitMCPContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 预算给足 60s；若实现不检查 ctx，配合 no-op sleep 会原地空转，由 select 超时兜住。
	done := make(chan error, 1)
	go func() {
		done <- waitMCPWith(ctx, srv.URL, MCPWaitOptions{Dir: "/proj", Timeout: 60, ServerTimeout: 60},
			time.Now, func(context.Context, time.Duration) {})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后仍在等待")
	}
}

// TestStatusOrUnknown 验证空状态在超时消息中显示为 unknown。
func TestStatusOrUnknown(t *testing.T) {
	if got := statusOrUnknown(""); got != "unknown" {
		t.Errorf(`statusOrUnknown("") = %q, want "unknown"`, got)
	}
	if got := statusOrUnknown("failed"); got != "failed" {
		t.Errorf(`statusOrUnknown("failed") = %q, want "failed"`, got)
	}
}

// TestProbeMCPMalformed 验证非法 JSON / 非 2xx 都按状态未知处理。
func TestProbeMCPMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`nope`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, errMsg := probeMCP(ctx, newHTTPClient(0), srv.URL, "/proj")
	if status != "" || errMsg != "" {
		t.Errorf("probeMCP() = %q, %q, want 空", status, errMsg)
	}
}
