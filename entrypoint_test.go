package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEntrypointRetriesTransientFailure 网关先返回 5xx、随后恢复时，入口脚本应重试并成功。
func TestEntrypointRetriesTransientFailure(t *testing.T) {
	requireTooling(t, "sh", "curl", "jq")

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// runEntrypoint 以受控环境执行镜像入口脚本，返回 stdout/stderr 与退出码。
// 继承环境中的 MARS_* 全部剔除，避免宿主机变量干扰断言。
func runEntrypoint(t *testing.T, gateway string) (stdout, stderr string, exitCode int) {
	t.Helper()

	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MARS_") {
			continue
		}
		env = append(env, kv)
	}

	cmd := exec.Command("sh", "scripts/entrypoint.sh")
	cmd.Env = append(env,
		"MARS_GATEWAY_URL="+gateway,
		"MARS_SESSION_TOKEN=test-token",
		"MARS_AGENT_BIN=/bin/echo",
	)
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
