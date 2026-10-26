package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// 探测参数。测试会把它们调小以避免真实等待。
var (
	serverProbeAttempts = 3
	serverProbeInterval = time.Second
	serverProbeTimeout  = time.Second
	sessionProbeTimeout = 2 * time.Second
	// getwd 可被测试替换：模拟目录被删除等 Getwd 失败场景。
	getwd = os.Getwd
)

// maiState 是 planMai 的全部决策输入。
type maiState struct {
	Bin              string
	Dir              string
	URL              string
	ServerReady      bool
	ServerHasSession bool // 仅 ServerReady 时为 true 才有意义
	LocalHasSession  bool // 仅 !ServerReady 时有意义
}

// argumentsAllowAttach 判断 `mai run` 的参数是否允许复用常驻 server。
// 用户自带 --attach、文件附件或 help/version 时保持原样执行，
// 因为 attach 模式对 -f 的目录/大文件支持不同。
func argumentsAllowAttach(args []string) bool {
	for _, arg := range args {
		switch {
		case arg == "--attach", strings.HasPrefix(arg, "--attach="),
			arg == "-f", arg == "--file", strings.HasPrefix(arg, "--file="),
			arg == "-h", arg == "--help", arg == "-v", arg == "--version":
			return false
		}
	}
	return true
}

// planMai 是纯函数：按探测结果决定最终 exec 的命令，便于逐分支单测。
func planMai(args []string, st maiState) Cmd {
	if len(args) == 0 {
		if st.ServerReady {
			if st.ServerHasSession {
				return Cmd{Path: st.Bin, Args: []string{"attach", "--dir", st.Dir, "-c", st.URL}}
			}
			return Cmd{Path: st.Bin, Args: []string{"attach", "--dir", st.Dir, st.URL}}
		}
		if st.LocalHasSession {
			return Cmd{Path: st.Bin, Args: []string{"-c", "--auto"}}
		}
		return Cmd{Path: st.Bin, Args: []string{"--auto"}}
	}

	if args[0] == "run" && st.ServerReady && argumentsAllowAttach(args) {
		cmdArgs := []string{"run", "--attach", st.URL, "--dir", st.Dir}
		return Cmd{Path: st.Bin, Args: append(cmdArgs, args[1:]...)}
	}
	return Cmd{Path: st.Bin, Args: append([]string(nil), args...)}
}

// interactiveOpencode 复刻原 scripts/mai.sh 的探测编排：
// 常驻 server 可达时 attach 复用；不可达时退回完整 TUI。
func interactiveOpencode(env Env, args []string, stderr io.Writer) (Cmd, error) {
	// 纯透传分支不依赖当前目录：旧脚本 `exec "$bin" "$@"` 在目录已被删除时也能执行。
	// 先分流，避免为了 `mai run --help` 这类命令无谓地 Getwd 失败。
	if len(args) > 0 && (args[0] != "run" || !argumentsAllowAttach(args)) {
		return Cmd{Path: env.AIBin, Args: append([]string(nil), args...)}, nil
	}

	dir, err := getwd()
	if err != nil {
		return Cmd{}, fmt.Errorf("获取当前目录失败: %w", err)
	}
	// os.Getwd 在 Unix 上会在 $PWD 与 "." 指向同一 inode 时返回 $PWD，
	// 与 shell 的逻辑路径语义一致（符号链接目录不会被解析成物理路径）。
	st := maiState{
		Bin: env.AIBin,
		Dir: dir,
		URL: loopbackURL(env.ListenAddr),
	}
	client := newHTTPClient(0)
	ctx := context.Background()

	if len(args) > 0 {
		// 到这里只剩 `run` 且参数允许复用 attach，需要探测常驻 server。
		st.ServerReady = serverReady(ctx, client, st.URL)
		return planMai(args, st), nil
	}

	st.ServerReady = serverReady(ctx, client, st.URL)
	if !st.ServerReady {
		st.LocalHasSession = localHasSession(env.AIBin, dir)
		return planMai(args, st), nil
	}

	// 进入 TUI 前确保 MCP 已就绪：connect API 只发起连接、不等待完成，
	// 发一次请求就 attach 会让 TUI 在 MCP 未就绪时启动。超时只告警、不阻断。
	if timeout := mcpWaitTimeout(); timeout > 0 {
		_ = waitMCP(ctx, st.URL, MCPWaitOptions{
			Dir:           dir,
			Timeout:       timeout,
			ServerTimeout: 0,
			Log:           stderr,
		})
	}
	st.ServerHasSession = serverHasSession(ctx, client, st.URL, dir)
	return planMai(args, st), nil
}

// serverReady 健康探测加短重试：server 活着但忙时，单次 1s 超时会把它误判为
// 不可达，进而回退完整 TUI、起第二份 server。3 次 × 1s，最坏约 5s。
func serverReady(ctx context.Context, client *http.Client, baseURL string) bool {
	for i := 1; i <= serverProbeAttempts; i++ {
		probeCtx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
		err := healthOK(probeCtx, client, baseURL+"/health")
		cancel()
		if err == nil {
			return true
		}
		if i < serverProbeAttempts {
			realSleep(ctx, serverProbeInterval)
		}
	}
	return false
}

// serverHasSession 通过常驻 server 判断当前目录是否有历史会话。
// 空 body、非法 JSON、null、对象、空数组一律视为无会话（对齐 jq -e 语义）。
func serverHasSession(ctx context.Context, client *http.Client, baseURL, dir string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	code, body, err := doGet(probeCtx, client, baseURL+"/session?"+url.Values{
		"directory": {dir},
		"limit":     {"1"},
	}.Encode())
	if err != nil || code >= http.StatusBadRequest {
		return false
	}
	var sessions []json.RawMessage
	if json.Unmarshal(body, &sessions) != nil {
		return false
	}
	return len(sessions) > 0
}

// localHasSession 在常驻 server 不可达时，用内核自己的会话列表探测当前目录。
// 与旧脚本一致：丢弃 stderr、忽略退出码（非 0 时仍尝试解析 stdout）。
func localHasSession(bin, dir string) bool {
	cmd := exec.Command(bin, "session", "list", "-n", "100", "--format", "json")
	cmd.Stderr = io.Discard
	out, _ := cmd.Output()
	if len(out) == 0 {
		return false
	}
	var sessions []struct {
		Directory string `json:"directory"`
	}
	if json.Unmarshal(out, &sessions) != nil {
		return false
	}
	for _, s := range sessions {
		if s.Directory == dir {
			return true
		}
	}
	return false
}

// mcpWaitTimeout 返回 `mai` 进入 TUI 前的 MCP 等待预算（秒）。
// 未设置时默认 15；非数字或 ≤0 返回 0（跳过等待），对齐旧脚本
// `[ "$mcp_wait" -gt 0 ] 2>/dev/null` 的语义。
func mcpWaitTimeout() int {
	raw := os.Getenv("MARS_MCP_WAIT_TIMEOUT")
	if raw == "" {
		return defaultMCPWaitTimeout
	}
	n, ok := parseDigits(raw)
	if !ok || n <= 0 {
		return 0
	}
	return n
}
