// Package main 是沙箱侧 mars-agent 的入口。
//
// 它按子命令执行：
//   - serve（默认）：校验 mars 注入的环境变量、生成内核配置与知识型 Skills，
//     然后 exec 成常驻的 agent 服务（用户经项目终端 `mai` 在沙箱内直接使用）。
//   - mai/health/mcp-wait/prewarm：用户入口、K8s 探针与启动预热。
//
// 内核由 MARS_AGENT_BACKEND 选择（P0 只有 opencode），所有内核特化逻辑收敛在
// Backend 实现中。沙箱侧不 import mars 任何代码，与 mars 之间只经 MCP / HTTP 交互。
package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// main 是进程入口：启动失败即打印原因并以非 0 退出（fail fast，起不来就别起）。
func main() {
	os.Exit(dispatch(os.Args[1:], os.Stderr, execCmd))
}

// execCmd 用 exec 替换当前进程：让底层 agent 成为 PID 1，信号直传；
// 探针通过 `mars-agent health` 转调所选内核的健康检查。
func execCmd(c Cmd) error {
	file, err := resolveExecPath(c.Path)
	if err != nil {
		return err
	}
	// argv[0] 保留调用方给的写法，与 shell `exec "$bin"` 一致。
	return syscall.Exec(file, append([]string{c.Path}, c.Args...), os.Environ())
}

// resolveExecPath 还原 shell `exec` 的语义：含路径分隔符时按路径执行，
// 否则走 PATH 查找。旧 mai.sh 直接 `exec "$bin"`，`MARS_AI_BIN=opencode`
// 这类裸名字可用；syscall.Exec 不做 PATH 查找，这里补上以免行为回归。
func resolveExecPath(path string) (string, error) {
	if strings.ContainsRune(path, os.PathSeparator) {
		return path, nil
	}
	return exec.LookPath(path)
}
