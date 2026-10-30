package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestEntrypointRetriesTransientFailure 网关先返回 5xx、随后恢复时，入口脚本应重试并成功。
func TestEntrypointRetriesTransientFailure(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/info" {
			_, _ = w.Write([]byte(`{"isGray":false}`))
			return
		}
		if r.URL.Path != "/api/ai/config" {
			t.Errorf("请求路径 = %s, want /api/ai/config", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization = %q", auth)
		}
		if atomic.AddInt32(&calls, 1) < 3 {
			http.Error(w, `{"error":"not ready"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"model":"demo-model"}`))
	}))
	defer srv.Close()

	stdout, stderr, code := runEntrypoint(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("请求次数 = %d, want 3", got)
	}
	if !strings.Contains(stderr, "第 2 次拉取配置失败") {
		t.Errorf("stderr 未体现重试: %s", stderr)
	}
	if !strings.Contains(stdout, "model=demo-model") {
		t.Errorf("stdout 未注入模型: %s", stdout)
	}
}

// TestEntrypointFailsFastOnAuthError 4xx（429 除外）是配置/鉴权问题，不应重试。
func TestEntrypointFailsFastOnAuthError(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/info" {
			_, _ = w.Write([]byte(`{"isGray":false}`))
			return
		}
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":"bad token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, stderr, code := runEntrypoint(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("请求次数 = %d, want 1（4xx 不应重试）", got)
	}
	if !strings.Contains(stderr, "401") {
		t.Errorf("stderr 未带出状态码: %s", stderr)
	}
}

// TestEntrypointFailsFastOnMalformedJSON 200 + 非 JSON（网关被登录页/代理劫持的典型形态）时，
// 应打印原始响应体并以 1 退出，而不是透出 jq 的 parse error 与其退出码 4。
func TestEntrypointFailsFastOnMalformedJSON(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/info":
			_, _ = w.Write([]byte(`{"isGray":false}`))
		case "/api/ai/config":
			_, _ = w.Write([]byte(`<html>login</html>`))
		default:
			t.Errorf("意外请求路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	_, stderr, code := runEntrypoint(t, srv.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "无法解析") || !strings.Contains(stderr, "<html>login</html>") {
		t.Errorf("stderr 未带出原始响应体: %s", stderr)
	}
}

// TestEntrypointGivesUpOnNetworkError 网络错误按上限重试后仍失败时，应退出非 0 并说明尝试次数。
func TestEntrypointGivesUpOnNetworkError(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	// 拿一个刚释放的本地端口，确保连接被立即拒绝。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	_, stderr, code := runEntrypoint(t, "http://"+addr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "已尝试 3 次") {
		t.Errorf("stderr 未说明重试上限: %s", stderr)
	}
}

// TestEntrypointGrayHeader 验证灰度探测结果同时作用于「配置请求头」与「传给 wrapper 的环境」：
// isGray=true → 配置请求带 mars_gray: always，且 MARS_GRAY=always 进入 opencode 环境；
// isGray=false → 不带该头；显式设置 MARS_GRAY 时跳过探测。
func TestEntrypointGrayHeader(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq", "env")

	tests := []struct {
		name       string
		isGray     bool
		extraEnv   []string
		wantHeader string
		wantEnv    bool
		wantProbes int32
	}{
		{"灰度用户", true, nil, "always", true, 1},
		{"非灰度用户", false, nil, "", false, 1},
		{"显式设置跳过探测", false, []string{"MARS_GRAY=always"}, "always", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probes int32
			var gotHeader atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/auth/info":
					atomic.AddInt32(&probes, 1)
					_, _ = fmt.Fprintf(w, `{"isGray":%t}`, tt.isGray)
				case "/api/ai/config":
					gotHeader.Store(r.Header.Get("mars_gray"))
					_, _ = w.Write([]byte(`{"model":"demo-model"}`))
				default:
					t.Errorf("意外请求路径 %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			extra := append([]string{"MARS_AGENT_BIN=/usr/bin/env"}, tt.extraEnv...)
			stdout, stderr, code := runEntrypoint(t, srv.URL, extra...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
			}
			if got, _ := gotHeader.Load().(string); got != tt.wantHeader {
				t.Errorf("配置请求 mars_gray = %q, want %q", got, tt.wantHeader)
			}
			if got := atomic.LoadInt32(&probes); got != tt.wantProbes {
				t.Errorf("灰度探测次数 = %d, want %d", got, tt.wantProbes)
			}
			if got := strings.Contains(stdout, "MARS_GRAY=always"); got != tt.wantEnv {
				t.Errorf("wrapper 环境含 MARS_GRAY=always = %v, want %v", got, tt.wantEnv)
			}
		})
	}
}

// TestEntrypointGrayProbeFailOpen 探测接口不可用/返回非 JSON 时按非灰度继续启动，
// 不能因为拿不到灰度标记就让沙箱 CrashLoop。
func TestEntrypointGrayProbeFailOpen(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq", "env")

	var gotHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/info":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/api/ai/config":
			gotHeader.Store(r.Header.Get("mars_gray"))
			_, _ = w.Write([]byte(`{"model":"demo-model"}`))
		}
	}))
	defer srv.Close()

	_, stderr, code := runEntrypoint(t, srv.URL)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if got, _ := gotHeader.Load().(string); got != "" {
		t.Errorf("探测失败时不应带 mars_gray，got %q", got)
	}
}

// TestEntrypointGrayProbeRetriesTransientFailure 灰度标记只在启动时取一次：
// 一次 5xx 抖动不应把灰度用户在整个 Pod 生命周期钉在非灰度通道，
// 探测应与配置拉取同口径重试（网络错误/5xx/429）。
func TestEntrypointGrayProbeRetriesTransientFailure(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq", "env")

	var probes int32
	var gotHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/info":
			if atomic.AddInt32(&probes, 1) < 3 {
				http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"isGray":true}`))
		case "/api/ai/config":
			gotHeader.Store(r.Header.Get("mars_gray"))
			_, _ = w.Write([]byte(`{"model":"demo-model"}`))
		default:
			t.Errorf("意外请求路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, stderr, code := runEntrypoint(t, srv.URL, "MARS_AGENT_BIN=/usr/bin/env")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if got := atomic.LoadInt32(&probes); got != 3 {
		t.Errorf("灰度探测次数 = %d, want 3（5xx 应重试）", got)
	}
	if got, _ := gotHeader.Load().(string); got != "always" {
		t.Errorf("配置请求 mars_gray = %q, want always", got)
	}
	if !strings.Contains(stdout, "MARS_GRAY=always") {
		t.Errorf("wrapper 环境未注入 MARS_GRAY=always: %s", stdout)
	}
	if !strings.Contains(stderr, "第 2 次探测灰度通道失败") {
		t.Errorf("stderr 未体现探测重试: %s", stderr)
	}
}

// TestEntrypointGrayProbeFailsFastOnAuthError 探测接口返回非 429 4xx（如令牌过期）时，
// 重试无意义：直接按非灰度继续，且仍不阻断启动。
func TestEntrypointGrayProbeFailsFastOnAuthError(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq", "env")

	var probes int32
	var gotHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/info":
			atomic.AddInt32(&probes, 1)
			http.Error(w, `{"error":"token expired"}`, http.StatusUnauthorized)
		case "/api/ai/config":
			gotHeader.Store(r.Header.Get("mars_gray"))
			_, _ = w.Write([]byte(`{"model":"demo-model"}`))
		default:
			t.Errorf("意外请求路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	_, stderr, code := runEntrypoint(t, srv.URL, "MARS_AGENT_BIN=/usr/bin/env")
	if code != 0 {
		t.Fatalf("exit = %d, want 0（探测失败应 fail-open）; stderr=%s", code, stderr)
	}
	if got := atomic.LoadInt32(&probes); got != 1 {
		t.Errorf("灰度探测次数 = %d, want 1（401 不应重试）", got)
	}
	if got, _ := gotHeader.Load().(string); got != "" {
		t.Errorf("探测失败时不应带 mars_gray，got %q", got)
	}
	if !strings.Contains(stderr, "401") {
		t.Errorf("stderr 未带出状态码: %s", stderr)
	}
}

// TestEntrypointSpawnsPrewarm 验证入口脚本在 exec 前台命令前拉起后台 prewarm。
func TestEntrypointSpawnsPrewarm(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/info":
			_, _ = w.Write([]byte(`{"isGray":false}`))
		case "/api/ai/config":
			_, _ = w.Write([]byte(`{"model":"demo-model"}`))
		default:
			t.Errorf("意外请求路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), "calls.log")
	fgPath := filepath.Join(t.TempDir(), "foreground")
	bin := filepath.Join(t.TempDir(), "fake-agent")
	// prewarm 分支会等待前台进程落一个标记文件：只有真的在后台并发执行，
	// 前台才能先跑完；若被写成前台阻塞，它会等满 2s 并记录 prewarm-foreground。
	script := `#!/bin/sh
if [ "$1" = "prewarm" ]; then
  i=0
  while [ ! -f "$MARS_FOREGROUND" ] && [ "$i" -lt 200 ]; do
    sleep 0.01
    i=$((i + 1))
  done
  if [ -f "$MARS_FOREGROUND" ]; then
    printf 'prewarm-background\n' >> "$MARS_LOG"
  else
    printf 'prewarm-foreground\n' >> "$MARS_LOG"
  fi
  exit 0
fi
: > "$MARS_FOREGROUND"
printf 'foreground\n' >> "$MARS_LOG"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("写 fake agent: %v", err)
	}

	_, stderr, code := runEntrypoint(t, srv.URL,
		"MARS_AGENT_BIN="+bin, "MARS_LOG="+logPath, "MARS_FOREGROUND="+fgPath)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}

	// 后台进程与前台 exec 并发，轮询而不是固定 sleep。
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(logPath)
		log := string(raw)
		if strings.Contains(log, "prewarm-foreground") {
			t.Fatalf("prewarm 被前台阻塞执行（应为后台），记录: %q", log)
		}
		if strings.Contains(log, "foreground") && strings.Contains(log, "prewarm-background") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("入口脚本未在后台拉起 prewarm，记录: %q", log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runEntrypoint 以受控环境执行镜像入口脚本，返回 stdout/stderr 与退出码。
// 继承环境中的 MARS_* 全部剔除，避免宿主机变量干扰断言；extraEnv 形如 KEY=VALUE，
// 在默认值之后覆盖设置。
func runEntrypoint(t *testing.T, gateway string, extraEnv ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	envMap := map[string]string{}
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(k, "MARS_") {
			continue
		}
		envMap[k] = kv[len(k)+1:]
	}
	envMap["MARS_GATEWAY_URL"] = gateway
	envMap["MARS_SESSION_TOKEN"] = "test-token"
	envMap["MARS_AGENT_BIN"] = "/bin/echo"
	for _, kv := range extraEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("非法 extraEnv %q", kv)
		}
		envMap[k] = kv[len(k)+1:]
	}
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command("sh", "scripts/entrypoint.sh")
	cmd.Env = env
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err == nil {
		return out.String(), errBuf.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), errBuf.String(), ee.ExitCode()
	}
	t.Fatalf("执行入口脚本失败: %v", err)
	return "", "", -1
}

// requireTooling 在缺少脚本依赖时跳过（入口脚本运行于 debian 镜像，宿主机可能未装全）。
func requireTooling(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("缺少 %s，跳过入口脚本测试", tool)
		}
	}
}
