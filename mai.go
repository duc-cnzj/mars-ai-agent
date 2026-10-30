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
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 探测参数。测试会把它们调小以避免真实等待。
var (
	serverProbeAttempts = 3
	serverProbeInterval = time.Second
	// 单次探测预算必须覆盖 busy 内核的 /api/health 耗时（1~3s，见 cli.healthProbeTimeout）：
	// 只有重试次数、不放宽单次超时，忙但活着的 server 仍会被判死，mai 退回完整 TUI 丢掉 -c。
	serverProbeTimeout = healthProbeTimeout
	// /session 也走同一个忙碌内核：预算短于 /api/health 时，server 明明有历史会话却会被
	// 判成「无会话」，attach 丢掉 -c、新开一个会话（旧脚本 --max-time 2 的同款缺陷）。
	sessionProbeTimeout = healthProbeTimeout
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

// serverReady 健康探测加短重试：server 活着但忙时，单次超时过短会把它误判为
// 不可达，进而回退完整 TUI、起第二份 server。3 次 × 3s，最坏约 11s；
// 服务确实没起时 connection refused 立即返回，实际约 2s。
func serverReady(ctx context.Context, client *http.Client, baseURL string) bool {
	for i := 1; i <= serverProbeAttempts; i++ {
		probeCtx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
		err := healthOK(probeCtx, client, baseURL+healthPath)
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

// localSessionScanLimit 是本地会话探测的扫描上限。
//
// opencode `session list` 不带 -n 时默认只返回最近 100 条（-n 100 同值），且排序是
// 全局的、不按目录过滤：会话较多的用户，当前目录的老会话会被其他目录的新会话挤出
// 前 100，mai 便误判「无会话」，回退完整 TUI 时丢掉 -c、给用户新开一个会话。
// 这里显式放大上限，让按目录过滤的判定能看到更久的历史（过滤本身见 localHasSession）。
const localSessionScanLimit = 10000

// localHasSession 在常驻 server 不可达时，用内核自己的会话列表探测当前目录。
// 与旧脚本一致：丢弃 stderr、忽略退出码（非 0 时仍尝试解析 stdout）。
//
// ⚠️ 必须比较物理路径：内核按物理路径记录会话目录（如 macOS 上 /tmp → /private/tmp），
// 而 os.Getwd 在 $PWD 与 "." 同 inode 时返回逻辑路径（符号链接不解析）。直接字符串比较
// 会漏判，导致回退完整 TUI 时丢掉 -c、新开一个会话。
func localHasSession(bin, dir string) bool {
	cmd := exec.Command(bin, "session", "list", "-n", strconv.Itoa(localSessionScanLimit), "--format", "json")
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
	want := physicalPath(dir)
	for _, s := range sessions {
		if physicalPath(s.Directory) == want {
			return true
		}
	}
	return false
}

// physicalPath 把逻辑路径解析成物理路径（等价 shell 的 pwd -P）。
// 路径已不存在等无法解析的情况退回原值：此时两侧都走原值比较，
// 行为与改造前的字符串比较一致，不会更差。
func physicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
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
