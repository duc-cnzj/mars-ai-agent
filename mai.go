package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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

// opencodeSubcommands 是 opencode 的已知子命令（含 auth/plug 两个别名）。
// `mai` 的首位参数命中这些词时保持原有语义透传，其余非选项参数视为要自动
// 发送的首条问题——这样 `mai "查看 uat/x 的日志"` 不必依赖终端注入。
var opencodeSubcommands = map[string]struct{}{
	"completion": {}, "acp": {}, "mcp": {}, "run": {}, "debug": {},
	"providers": {}, "auth": {}, "agent": {}, "upgrade": {}, "uninstall": {},
	"serve": {}, "web": {}, "models": {}, "stats": {}, "export": {},
	"import": {}, "github": {}, "pr": {}, "session": {}, "plugin": {},
	"plug": {}, "db": {}, "attach": {},
}

// takeBindArgs 从 mai 参数中拆出所有 `--bind` / `--unbind` 绑定参数，返回剩余参数、
// 规范化后的绑定范围列表（保持出现顺序、相同项去重）以及是否要求清空绑定。
//
// 绑定有两种粒度：`空间`（整个空间，含其下全部项目）与 `空间/项目`。支持
// `--bind <范围>` 与 `--bind=<范围>` 两种写法，可重复传入以绑定多个范围；
// 未提供时列表为空。`--unbind` 是开关，用于把「当前没有绑定任何范围」写回会话
// （之前的绑定消息仍在历史里，必须显式覆盖）。两者同时出现时以 `--bind` 为准——
// 本函数只负责原样返回两个信号，优先级由调用方按「先看 bindings 再看 unbind」分流。
//
// 只解析第一个位置参数之前的绑定：`mai --bind uat/x "解释 --bind"` 的正文不会被误判，
// `mai run --bind ...` 也保持 run 子命令语义。缺值或格式非法时返回错误，调用方告警后
// 忽略全部绑定——绑定只是默认上下文，不能因此让用户进不去 TUI；错误路径仍会保留
// 全部非绑定参数，避免把后面的问题一并吞掉。
func takeBindArgs(args []string) (rest []string, bindings []string, unbind bool, err error) {
	rest = make([]string, 0, len(args))
	parsingBind := true
	var invalid error
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !parsingBind {
			rest = append(rest, arg)
			continue
		}
		var raw string
		switch {
		case arg == "--unbind":
			unbind = true
			continue
		case arg == "--bind":
			// 值不可能以 "-" 开头（K8s 空间/项目名不允许），下一个 token 是选项时
			// 说明用户漏写了范围：报错但不吞掉该选项，`mai --bind --auto` 仍能带 --auto。
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				if invalid == nil {
					invalid = errors.New("--bind 缺少值（格式：空间 或 空间/项目）")
				}
				continue
			}
			raw = args[i+1]
			i++
		case strings.HasPrefix(arg, "--bind="):
			raw = strings.TrimPrefix(arg, "--bind=")
		default:
			// 第一个位置参数之后全部视为正文/子命令参数，不再解析绑定。
			if !strings.HasPrefix(arg, "-") {
				parsingBind = false
			}
			rest = append(rest, arg)
			continue
		}
		scope, ok := normalizeBindScope(raw)
		if !ok {
			if invalid == nil {
				invalid = fmt.Errorf("--bind 格式应为 空间 或 空间/项目，实际 %q", raw)
			}
			continue
		}
		bindings = appendUnique(bindings, scope)
	}
	if invalid != nil {
		return rest, nil, false, invalid
	}
	return rest, bindings, unbind, nil
}

// normalizeBindScope 规范化一个绑定范围：只接受 `空间` 或 `空间/项目`，
// 去掉各段首尾空白；空段、超过两级的路径、以 "-" 开头的段（疑似漏写范围时
// 被选项顶替，如 `--bind=--auto`）一律视为非法（返回 false）。
func normalizeBindScope(raw string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	if len(parts) > 2 {
		return "", false
	}
	ns := strings.TrimSpace(parts[0])
	if ns == "" || strings.HasPrefix(ns, "-") {
		return "", false
	}
	if len(parts) == 1 {
		return ns, true
	}
	project := strings.TrimSpace(parts[1])
	if project == "" || strings.HasPrefix(project, "-") {
		return "", false
	}
	return ns + "/" + project, true
}

// appendUnique 按顺序追加字符串，已存在的项直接跳过（绑定项少量，线性扫描足够）。
func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

// initialPromptFromArgs 判断这次 `mai` 调用是否携带首条问题。
//
// 只有「首位参数不是选项、也不是已知子命令」才算问题：`mai run`、`mai attach`、
// `mai -c` 仍按原样透传/复用。`mai 查看 uat 的日志` 这类没加引号的写法会被 shell
// 拆成多个参数，这里重新拼回一段文本，用户不必记得加引号。
func initialPromptFromArgs(args []string) (string, bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	if _, ok := opencodeSubcommands[args[0]]; ok {
		return "", false
	}
	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt == "" {
		return "", false
	}
	return prompt, true
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

// planPromptFallback 是常驻 server 不可达时携带首条问题的启动命令：
// `--prompt` 由 opencode 自己填进 TUI 输入行并提交（实测 1.19.2 会自动发送），
// 有本地会话时加 -c 续接上下文，与无问题的 `mai` 口径一致。
func planPromptFallback(bin, prompt string, hasSession bool) Cmd {
	args := []string{"--auto", "--prompt", prompt}
	if hasSession {
		args = append([]string{"-c"}, args...)
	}
	return Cmd{Path: bin, Args: args}
}

// latestSessionID 返回当前目录最近更新的根会话 id。
//
// 必须带 roots=true：内核的会话列表包含 subagent 的 child session，只取 limit=1
// 可能选中子会话，把用户问题投进错误的对话。老内核不认识 roots 时退回不带该参数的
// 查询（行为与改造前一致），避免因参数校验差异直接失败。
//
// 返回 error 表示查询本身失败（传输/HTTP/解析）。调用方应据此降级，而不是当成
// 「没有会话」去新建——否则列表超时但创建成功时会产生重复会话。
func latestSessionID(ctx context.Context, client *http.Client, baseURL, dir string) (string, error) {
	sessions, err := rootSessions(ctx, client, baseURL, dir)
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "", nil
	}
	return sessions[0].ID, nil
}

// rootSessions 返回当前目录最近的根会话列表。与 latestSessionID 分开是为了保留
// serverHasSession 的旧语义：只要列表非空就算有会话，不要求首项一定带 id。
func rootSessions(ctx context.Context, client *http.Client, baseURL, dir string) ([]struct {
	ID string `json:"id"`
}, error) {
	code, body, err := probeSessionList(ctx, client, baseURL, dir, true)
	if err != nil {
		return nil, err
	}
	if code == http.StatusBadRequest || code == http.StatusNotFound {
		code, body, err = probeSessionList(ctx, client, baseURL, dir, false)
		if err != nil {
			return nil, err
		}
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("查询会话失败: HTTP %d: %s", code, shortBody(body))
	}
	var sessions []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &sessions) != nil {
		return nil, fmt.Errorf("解析会话列表失败: %s", shortBody(body))
	}
	return sessions, nil
}

// probeSessionList 查询当前目录最近的会话列表，roots 控制是否只返回根会话。
func probeSessionList(ctx context.Context, client *http.Client, baseURL, dir string, roots bool) (int, []byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	values := url.Values{
		"directory": {dir},
		"limit":     {"1"},
	}
	if roots {
		values.Set("roots", "true")
	}
	return doGet(probeCtx, client, baseURL+"/session?"+values.Encode())
}

// sessionLockPath 返回按目录区分的会话创建锁文件路径（文件名取目录哈希，
// 避免路径分层与超长文件名问题）。
func sessionLockPath(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return filepath.Join(os.TempDir(), fmt.Sprintf("mars-agent-session-%x.lock", sum[:8]))
}

// lockSessionCreation 用文件锁串行化同一目录的「查询→创建会话」，以及随后的隐藏
// 上下文注入（见 ensurePromptSessionLease / withLatestSession）。
//
// 内核不保证会话唯一：UI 并发触发（例如双击项目卡片）时，两个 mai 会先同时查到
// 「无会话」，再各建一个空会话，用户随后在两个 TUI 里看到分裂的历史。同样地，
// 注入的「查重→写入」若不串行，两处会各写一条重复绑定。锁覆盖这两段，持有时间受
// 会话探测超时约束；锁文件打不开或文件系统不支持 flock 时直接放行——这只是优化，
// 不能因此阻断用户进入 TUI。
func lockSessionCreation(dir string) (release func()) {
	f, err := os.OpenFile(sessionLockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// ensurePromptSession 复用当前目录最近更新的根会话；没有可用会话时新建一个。
// 复用是刻意行为：用户点「问 AI / 让 AI 诊断」应接着刚才那段对话，而不是每次新开。
func ensurePromptSession(ctx context.Context, client *http.Client, baseURL, dir string) (string, error) {
	return ensurePromptSessionLease(ctx, client, baseURL, dir, nil)
}

// ensurePromptSessionLease 在目录锁内解析（复用或创建）会话，并在同一把锁内执行 setup
// 注入隐藏上下文。
//
// 只锁「查询+创建」还不够：注入本身是「先查最近一条 synthetic，再写入」的
// check-then-act，若放在锁外，并发双击卡片时两个 mai 会同时读到空历史、各写一条同样
// 的绑定，把重复上下文堆进会话。setup 为 nil 时退化为单纯解析会话。
//
// 错误语义：返回 ("", err) 表示会话解析失败，调用方应降级为普通 TUI；返回 (id, err)
// 表示会话已就绪但 setup 注入失败，调用方仍可 attach 该会话。
func ensurePromptSessionLease(ctx context.Context, client *http.Client, baseURL, dir string, setup func(context.Context, string) error) (string, error) {
	release := lockSessionCreation(dir)
	defer release()
	id, err := ensurePromptSessionLocked(ctx, client, baseURL, dir)
	if err != nil {
		return "", err
	}
	if setup != nil {
		if err := setup(ctx, id); err != nil {
			return id, err
		}
	}
	return id, nil
}

// ensurePromptSessionLocked 是 ensurePromptSession 的锁内版本：调用方必须已持有
// lockSessionCreation(dir)。
func ensurePromptSessionLocked(ctx context.Context, client *http.Client, baseURL, dir string) (string, error) {
	id, err := latestSessionID(ctx, client, baseURL, dir)
	if err != nil {
		return "", fmt.Errorf("查询最近会话失败: %w", err)
	}
	if id != "" {
		return id, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	code, body, err := doJSON(probeCtx, client, baseURL+"/session?"+url.Values{
		"directory": {dir},
	}.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}
	if code < 200 || code >= 300 {
		return "", fmt.Errorf("创建会话失败: HTTP %d: %s", code, shortBody(body))
	}
	var session struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &session) != nil || session.ID == "" {
		return "", fmt.Errorf("创建会话响应缺少 id: %s", shortBody(body))
	}
	return session.ID, nil
}

// withLatestSession 在目录锁内解析当前目录最近根会话并执行 setup 注入。
// 没有根会话时返回 ("", nil) 且不执行 setup（没有历史就没有旧绑定可覆盖）。
// 错误语义与 ensurePromptSessionLease 相同：("", err) 表示查询失败，(id, err) 表示
// 会话已就绪但注入失败。
func withLatestSession(ctx context.Context, client *http.Client, baseURL, dir string, setup func(context.Context, string) error) (string, error) {
	release := lockSessionCreation(dir)
	defer release()
	id, err := latestSessionID(ctx, client, baseURL, dir)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", nil
	}
	if setup != nil {
		if err := setup(ctx, id); err != nil {
			return id, err
		}
	}
	return id, nil
}

// sendInitialPrompt 把首条问题作为异步消息投递给会话：prompt_async 立即返回，
// 模型在后台开始响应；随后 attach 的 TUI 会直接看到问题与流式回复。
// 文本走 JSON 请求体而不是终端按键，不存在被 shell 当命令执行的风险。
func sendInitialPrompt(ctx context.Context, client *http.Client, baseURL, dir, sessionID, prompt string) error {
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	payload := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	code, body, err := doJSON(probeCtx, client,
		baseURL+"/session/"+url.PathEscape(sessionID)+"/prompt_async?"+url.Values{
			"directory": {dir},
		}.Encode(), payload)
	if err != nil {
		return fmt.Errorf("提交首条问题失败: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("提交首条问题失败: HTTP %d: %s", code, shortBody(body))
	}
	return nil
}

// unbindContextText 是「当前没有任何绑定」的隐藏上下文文本。
const unbindContextText = "当前没有绑定任何范围。用户提问未明确指定范围时，不要沿用之前的绑定，请先确认要针对哪个命名空间或项目。"

// bindingContextText 生成「当前绑定范围」的隐藏上下文文本。
// 绑定项有两种粒度：`空间`（整个空间，含其下全部项目）与 `空间/项目`。
//
// 绑定是集合，输出前按范围名排序：UI 以不同顺序传入同一组范围时应生成完全相同的文本，
// 否则 sendBindingContext 的「最近一条相同则跳过」会失效，反复注入堆满历史。
func bindingContextText(bindings []string) string {
	sorted := append([]string(nil), bindings...)
	sort.Strings(sorted)
	scopes := make([]string, 0, len(sorted))
	for _, binding := range sorted {
		if strings.Contains(binding, "/") {
			scopes = append(scopes, "项目 "+binding)
			continue
		}
		scopes = append(scopes, "命名空间 "+binding+"（含该空间下全部项目）")
	}
	return fmt.Sprintf("当前绑定范围：%s。用户提问未明确指定范围时，默认针对上述范围作答；需要具体项目或 Pod 时先用 MCP 工具查询。", strings.Join(scopes, "、"))
}

// sendBindingContext 把「当前绑定范围」作为隐藏上下文注入会话。
//
// bindings 可含多个范围（`空间` 或 `空间/项目`）。noReply 让内核只存储消息、不触发
// 模型回复；synthetic 标记为非用户输入的合成内容，TUI 里不会多出一条提问。模型在后续
// 对话中据此把未明确指定范围的问题默认落到这些范围上——这正是「点项目卡片上的问 AI，
// 只绑定范围、不代发问题」的实现基础。
//
// 会话里最近一条 synthetic 上下文已经是同样的绑定时不重复写入：UI 每次点项目卡片
// 都会调用 mai --bind，重复注入会把同一段绑定堆满历史、挤占模型上下文。
func sendBindingContext(ctx context.Context, client *http.Client, baseURL, dir, sessionID string, bindings []string) error {
	return sendSyntheticContextOnce(ctx, client, baseURL, dir, sessionID,
		bindingContextText(bindings), "注入绑定上下文失败")
}

// sendUnbindContext 把「当前没有绑定任何范围」作为隐藏上下文注入会话。
//
// 用户清空全部绑定后，之前 sendBindingContext 写入的消息仍留在同一会话历史里；
// 只重启一个不带的 mai 不会清除它，模型会继续按旧绑定作答。这里显式覆盖一次；
// 最近一条 synthetic 上下文已经是「无绑定」时同样跳过，避免重复写入。
func sendUnbindContext(ctx context.Context, client *http.Client, baseURL, dir, sessionID string) error {
	return sendSyntheticContextOnce(ctx, client, baseURL, dir, sessionID,
		unbindContextText, "清除绑定上下文失败")
}

// sendSyntheticContextOnce 把 text 作为隐藏上下文写入会话；最近一条 synthetic 文本
// 已经是 text 时直接跳过，避免重复注入把同一段上下文堆满历史。
//
// failMsg 只用于失败告警前缀（绑定/清空两类语义各自保留中文表述），写入规则只有
// 这一份，避免两个调用方各改一半。
func sendSyntheticContextOnce(ctx context.Context, client *http.Client, baseURL, dir, sessionID, text, failMsg string) error {
	if lastSyntheticText(ctx, client, baseURL, dir, sessionID) == text {
		return nil
	}
	if err := postSyntheticContext(ctx, client, baseURL, dir, sessionID, text); err != nil {
		return fmt.Errorf("%s: %w", failMsg, err)
	}
	return nil
}

// messageScanLimit 是读取会话消息用于去重时的扫描上限。
//
// 内核按 limit 返回最近 N 条（实测 1.19.1/1.19.2 支持，按时间正序）。必须设限：
// 长会话整量拉取会触到 maxProbeBody（4MB）截断，JSON 解析失败后去重永远失效，
// 每次绑定都重复注入。绑定上下文只需在最近若干条里找，50 条足够覆盖连续点击
// （中间插入了提问与工具调用也不会立刻超出）；真超出时重新注入只是多一条上下文，
// 不影响正确性。老内核若不认识该参数（400）则回退为整量查询，与改造前一致。
const messageScanLimit = 50

// lastSyntheticText 返回会话中最近一条 synthetic 文本部件（绑定/清空上下文）。
// 查询失败、响应异常或没有命中时返回空串——调用方按「没有相同上下文」处理，
// 宁可多写一条也不阻断主流程。
func lastSyntheticText(ctx context.Context, client *http.Client, baseURL, dir, sessionID string) string {
	code, body, err := probeSessionMessages(ctx, client, baseURL, dir, sessionID, true)
	if err == nil && code == http.StatusBadRequest {
		code, body, err = probeSessionMessages(ctx, client, baseURL, dir, sessionID, false)
	}
	if err != nil || code >= http.StatusBadRequest {
		return ""
	}
	var messages []struct {
		Parts []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Synthetic bool   `json:"synthetic"`
		} `json:"parts"`
	}
	if json.Unmarshal(body, &messages) != nil {
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		for _, part := range messages[i].Parts {
			if part.Synthetic && part.Type == "text" && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}

// probeSessionMessages 查询会话消息，limited 时只取最近 messageScanLimit 条。
func probeSessionMessages(ctx context.Context, client *http.Client, baseURL, dir, sessionID string, limited bool) (int, []byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	values := url.Values{"directory": {dir}}
	if limited {
		values.Set("limit", strconv.Itoa(messageScanLimit))
	}
	return doGet(probeCtx, client,
		baseURL+"/session/"+url.PathEscape(sessionID)+"/message?"+values.Encode())
}

// postSyntheticContext 把一段隐藏上下文消息写入会话：noReply 只存储不触发模型回复，
// synthetic 标记为非用户输入（TUI 不展示成提问）。
func postSyntheticContext(ctx context.Context, client *http.Client, baseURL, dir, sessionID, text string) error {
	probeCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	payload := map[string]any{
		"noReply": true,
		"parts": []map[string]any{{
			"type":      "text",
			"text":      text,
			"synthetic": true,
		}},
	}
	code, body, err := doJSON(probeCtx, client,
		baseURL+"/session/"+url.PathEscape(sessionID)+"/message?"+url.Values{
			"directory": {dir},
		}.Encode(), payload)
	if err != nil {
		return fmt.Errorf("提交会话消息失败: %w", err)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("提交会话消息失败: HTTP %d: %s", code, shortBody(body))
	}
	return nil
}

// interactiveOpencode 复刻原 scripts/mai.sh 的探测编排：
// 常驻 server 可达时 attach 复用；不可达时退回完整 TUI。
func interactiveOpencode(env Env, args []string, stderr io.Writer) (Cmd, error) {
	// --bind / --unbind 在本地消化：它们是 mars-agent 的绑定语义，不能透传给 opencode。
	rest, bindings, unbind, bindErr := takeBindArgs(args)
	if bindErr != nil {
		fmt.Fprintf(stderr, "mars-agent: %v；忽略绑定继续进入 TUI\n", bindErr)
	}
	args = rest
	prompt, hasPrompt := initialPromptFromArgs(args)
	// 纯透传分支不依赖当前目录：旧脚本 `exec "$bin" "$@"` 在目录已被删除时也能执行。
	// 先分流，避免为了 `mai run --help` 这类命令无谓地 Getwd 失败。
	if len(args) > 0 && !hasPrompt && (args[0] != "run" || !argumentsAllowAttach(args)) {
		if len(bindings) > 0 || unbind {
			fmt.Fprintf(stderr, "mars-agent: 绑定参数需要不带其他参数的交互式 mai，已忽略\n")
		}
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

	if hasPrompt {
		return planPromptCommand(ctx, env, client, st, prompt, bindings, unbind, stderr), nil
	}
	// 绑定语义只服务于交互式 mai。带子命令/选项时（如 `mai --bind a/b run hi`）
	// 没有可注入的交互会话，绑定无法交给 opencode，忽略并提示，保持原命令语义。
	if len(args) > 0 {
		if len(bindings) > 0 || unbind {
			fmt.Fprintf(stderr, "mars-agent: 绑定参数需要不带其他参数的交互式 mai，已忽略\n")
		}
		// 到这里只剩 `run` 且参数允许复用 attach，需要探测常驻 server。
		st.ServerReady = serverReady(ctx, client, st.URL)
		return planMai(args, st), nil
	}
	// 只绑定、不代发问题：注入上下文后 attach 该会话，用户自己输入第一条消息。
	if len(bindings) > 0 {
		return planBindCommand(ctx, env, client, st, bindings, stderr), nil
	}
	// 用户清空全部绑定：显式写回「无绑定」，覆盖会话历史里的旧绑定消息。
	if unbind {
		return planUnbindCommand(ctx, env, client, st, stderr), nil
	}

	st.ServerReady = serverReady(ctx, client, st.URL)
	if !st.ServerReady {
		st.LocalHasSession = localHasSession(env.AIBin, dir)
		return planMai(args, st), nil
	}

	// 进入 TUI 前确保 MCP 已就绪：connect API 只发起连接、不等待完成，
	// 发一次请求就 attach 会让 TUI 在 MCP 未就绪时启动。超时只告警、不阻断。
	waitMCPBeforeTUI(ctx, st.URL, st.Dir, stderr)
	st.ServerHasSession = serverHasSession(ctx, client, st.URL, dir)
	return planMai(args, st), nil
}

// plainAttachCmd 构造常驻 server 可达时的普通 attach 命令（不指定会话）。
// 建会话/注入绑定失败时统一走这里：仍然进入 TUI，不让用户卡在报错。
func plainAttachCmd(ctx context.Context, client *http.Client, st maiState) Cmd {
	return planMai(nil, maiState{
		Bin: st.Bin, Dir: st.Dir, URL: st.URL,
		ServerReady:      true,
		ServerHasSession: serverHasSession(ctx, client, st.URL, st.Dir),
	})
}

// attachCmd 构造复用指定会话的 attach 命令。
func attachCmd(bin, dir, sessionID, baseURL string) Cmd {
	return Cmd{Path: bin, Args: []string{"attach", "--dir", dir, "-s", sessionID, baseURL}}
}

// planPromptCommand 为「mai + 首条问题」准备启动命令。
//
// 常驻 server 可达时先把问题投递到当前目录最近会话（没有就新建），再 attach 该会话：
// 用户进 TUI 就能看到问题已发送、回复在流式输出，全程不依赖终端按键时序。
// 投递失败只降级为普通 attach（问题需手动输入），不让用户进不去沙箱。
func planPromptCommand(ctx context.Context, env Env, client *http.Client, st maiState, prompt string, bindings []string, unbind bool, stderr io.Writer) Cmd {
	st.ServerReady = serverReady(ctx, client, st.URL)
	if !st.ServerReady {
		switch {
		case len(bindings) > 0:
			fmt.Fprintf(stderr, "mars-agent: 常驻服务未就绪，无法绑定 %s；问题照常发送\n", strings.Join(bindings, "、"))
		case unbind:
			fmt.Fprintf(stderr, "mars-agent: 常驻服务未就绪，无法清除绑定；问题照常发送\n")
		}
		return planPromptFallback(env.AIBin, prompt, localHasSession(env.AIBin, st.Dir))
	}
	waitMCPBeforeTUI(ctx, st.URL, st.Dir, stderr)
	// 先绑定、后提问：同一段对话里，问题默认落到未明确指定时使用的范围。
	// 绑定注入与「查重」放在与建会话同一把目录锁内，避免并发重复写入。
	sessionID, err := ensurePromptSessionLease(ctx, client, st.URL, st.Dir, func(ctx context.Context, sessionID string) error {
		switch {
		case len(bindings) > 0:
			return sendBindingContext(ctx, client, st.URL, st.Dir, sessionID, bindings)
		case unbind:
			// 没有绑定但显式要求清空：覆盖会话历史里的旧绑定，避免问题仍落到旧范围。
			return sendUnbindContext(ctx, client, st.URL, st.Dir, sessionID)
		}
		return nil
	})
	if sessionID == "" {
		fmt.Fprintf(stderr, "mars-agent: %v；进入普通 TUI，请手动输入问题\n", err)
		return plainAttachCmd(ctx, client, st)
	}
	if err != nil {
		if len(bindings) > 0 {
			fmt.Fprintf(stderr, "mars-agent: %v；绑定未生效\n", err)
		} else {
			fmt.Fprintf(stderr, "mars-agent: %v；请手动确认要针对的空间或项目\n", err)
		}
	}
	if err := sendInitialPrompt(ctx, client, st.URL, st.Dir, sessionID, prompt); err != nil {
		fmt.Fprintf(stderr, "mars-agent: %v；进入 TUI 后请手动输入问题\n", err)
	}
	return attachCmd(st.Bin, st.Dir, sessionID, st.URL)
}

// planBindCommand 为「只绑定范围、不带问题」的 `mai --bind 空间` / `mai --bind 空间/项目`
// （可重复传入以绑定多个范围）准备启动命令：复用当前目录最近会话（没有则新建），
// 注入隐藏绑定上下文后 attach 该会话。与 planPromptCommand 相同，任何一步失败都只
// 降级为普通 attach，不让用户进不去沙箱。
func planBindCommand(ctx context.Context, env Env, client *http.Client, st maiState, bindings []string, stderr io.Writer) Cmd {
	joined := strings.Join(bindings, "、")
	st.ServerReady = serverReady(ctx, client, st.URL)
	if !st.ServerReady {
		fmt.Fprintf(stderr, "mars-agent: 常驻服务未就绪，无法绑定 %s；进入普通 TUI\n", joined)
		st.LocalHasSession = localHasSession(env.AIBin, st.Dir)
		return planMai(nil, st)
	}
	waitMCPBeforeTUI(ctx, st.URL, st.Dir, stderr)
	sessionID, err := ensurePromptSessionLease(ctx, client, st.URL, st.Dir, func(ctx context.Context, sessionID string) error {
		return sendBindingContext(ctx, client, st.URL, st.Dir, sessionID, bindings)
	})
	if sessionID == "" {
		fmt.Fprintf(stderr, "mars-agent: %v；无法绑定 %s，进入普通 TUI\n", err, joined)
		return plainAttachCmd(ctx, client, st)
	}
	if err != nil {
		fmt.Fprintf(stderr, "mars-agent: %v；绑定未生效\n", err)
	}
	return attachCmd(st.Bin, st.Dir, sessionID, st.URL)
}

// planUnbindCommand 为「mai --unbind」（用户清空全部绑定）准备启动命令：复用当前目录
// 最近会话，写入「当前没有绑定任何范围」的隐藏上下文后 attach。之前的绑定消息仍在历史里，
// 不显式覆盖模型会继续沿用。
//
// 没有根会话时不需要新建：没有历史就没有旧绑定可清，直接进入普通 TUI 即可，避免
// 为了写一条「无绑定」消息凭空制造一个空会话。任何一步失败都只降级为普通 attach，
// 不让用户进不去沙箱。
func planUnbindCommand(ctx context.Context, env Env, client *http.Client, st maiState, stderr io.Writer) Cmd {
	st.ServerReady = serverReady(ctx, client, st.URL)
	if !st.ServerReady {
		fmt.Fprintf(stderr, "mars-agent: 常驻服务未就绪，无法清除绑定；进入普通 TUI\n")
		st.LocalHasSession = localHasSession(env.AIBin, st.Dir)
		return planMai(nil, st)
	}
	waitMCPBeforeTUI(ctx, st.URL, st.Dir, stderr)
	sessionID, err := withLatestSession(ctx, client, st.URL, st.Dir, func(ctx context.Context, sessionID string) error {
		return sendUnbindContext(ctx, client, st.URL, st.Dir, sessionID)
	})
	if sessionID == "" {
		if err != nil {
			fmt.Fprintf(stderr, "mars-agent: %v；无法清除绑定，进入普通 TUI\n", err)
			return plainAttachCmd(ctx, client, st)
		}
		// 没有根会话 = 没有历史绑定可清，直接普通 attach。
		return planMai(nil, maiState{Bin: st.Bin, Dir: st.Dir, URL: st.URL, ServerReady: true})
	}
	if err != nil {
		fmt.Fprintf(stderr, "mars-agent: %v；请手动确认要针对的空间或项目\n", err)
	}
	return attachCmd(st.Bin, st.Dir, sessionID, st.URL)
}

// waitMCPBeforeTUI 在 attach/投递问题前等待 MCP 就绪；超时只告警、不阻断。
func waitMCPBeforeTUI(ctx context.Context, baseURL, dir string, stderr io.Writer) {
	if timeout := mcpWaitTimeout(); timeout > 0 {
		_ = waitMCP(ctx, baseURL, MCPWaitOptions{
			Dir:           dir,
			Timeout:       timeout,
			ServerTimeout: 0,
			Log:           stderr,
		})
	}
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

// serverHasSession 通过常驻 server 判断当前目录是否有历史根会话。
// 查询失败、非法 JSON、空数组一律视为无会话（对齐 jq -e 语义）。
func serverHasSession(ctx context.Context, client *http.Client, baseURL, dir string) bool {
	sessions, err := rootSessions(ctx, client, baseURL, dir)
	return err == nil && len(sessions) > 0
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
