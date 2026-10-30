package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// tuiFS 内嵌 opencode TUI 配置与品牌插件（tui.json + plugins/mars-logo.tsx）。
//
// tui.json 里 mouse=true：保留 opencode 的鼠标捕获，让 TUI 自己处理拖选与滚动。
// 这样选区由 opencode 按自身面板边界计算，不会像 xterm.js 那样把左右分屏当成
// 同一行字符、换行时把右侧信息一起选中。复制经 OSC 52 交给浏览器终端处理，
// 前端需注册对应的 OSC 52 handler（见 mars frontend TabShell）。
// 仍可用 Shift+拖（macOS Option+拖）强制走终端原生选区。
//
//go:embed tui
var tuiFS embed.FS

// tuiRoot 是内嵌文件系统中的 TUI 资源根目录名。
const tuiRoot = "tui"

// MaterializeTUI 把内嵌的 TUI 配置与插件落盘到 opencode 配置目录 dst。
//
// tui.json 中插件路径是相对路径 ./plugins/mars-logo.tsx，因此 dst 必须是
// $HOME/.config/opencode（与 tui.json 同级）。与 Skills 不同，这里保持可写：
// 属于用户级 TUI 定制，不涉及执行权限收口。
//
// 必须可重复调用：$HOME 是 emptyDir，容器重启（崩溃、OOM、探针失败）不会清空它；
// 用户或 agent 在沙箱里把 tui.json/插件目录改成只读后（chmod 是普通用户对自己文件的
// 合法操作），第二次启动的 WriteFile 会 EACCES，Prepare 失败直接让 serve 起不来、
// 容器 CrashLoop——一个纯装饰性的品牌插件不该有这种杀伤力。落盘前先恢复写权限，
// 与 MaterializeSkills 的重启安全口径一致。
func MaterializeTUI(dst string) error {
	err := fs.WalkDir(tuiFS, tuiRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tuiRoot, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			// 上次可能被改成只读目录：恢复可写才能覆盖其中的文件。
			return os.Chmod(target, 0o755)
		}
		content, err := tuiFS.ReadFile(p)
		if err != nil {
			return err
		}
		// 同名文件上次可能被改成只读：先恢复可写再覆盖
		// （不存在时 Chmod 返回 ErrNotExist，由随后的 WriteFile 创建）。
		if err := os.Chmod(target, 0o644); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		return fmt.Errorf("落盘 TUI 配置到 %s 失败: %w", dst, err)
	}
	return nil
}

// ApplyTUIPrefs 初始化 opencode TUI 的平台级偏好：
//
//   - 默认关闭首页内置 tips（tips_hidden，对应命令面板 System → Hide tips）；
//   - 默认关闭 TUI 动画（animations_enabled，对应命令面板 Session →
//     Disable animations）。opencode 的 spinner 约 57fps 持续重绘，即使空闲也能
//     吃满一个核，在沙箱里表现为输入/滚动卡顿（opencode #3990 / #42657）。
//
// 两项都只在键不存在时写入默认值：opencode 读缺失键时分别回落 false/true，
// 首次启动仍得到「隐藏 tips、关闭动画」；用户主动改回来（Show tips / Enable
// animations）后必须保留，否则容器每次启动都会悄悄覆盖用户偏好。
//
// 落盘位置按 XDG 约定为 $XDG_STATE_HOME/opencode/kv.json，未设置
// XDG_STATE_HOME 时为 $HOME/.local/state/opencode/kv.json。采用读-合并-写，
// 保留用户在 kv.json 里的其他 TUI 偏好。
//
// 调用方应把它当作 best-effort：失败只影响 TUI 偏好，不得阻断沙箱启动。
func ApplyTUIPrefs(home string) error {
	stateDir := filepath.Join(home, ".local", "state", "opencode")
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		stateDir = filepath.Join(xdg, "opencode")
	}
	path := filepath.Join(stateDir, "kv.json")

	kv := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &kv); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		// JSON null 会把 map 反序列化成 nil，之后任何写入都是
		// "assignment to entry in nil map" panic。本函数是 best-effort 的，
		// panic 会直接带走 PID 1（serve 起不来、容器 CrashLoop），必须挡在这里。
		if kv == nil {
			kv = map[string]any{}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	changed := false
	if _, ok := kv["tips_hidden"]; !ok {
		kv["tips_hidden"] = true
		changed = true
	}
	if _, ok := kv["animations_enabled"]; !ok {
		kv["animations_enabled"] = false
		changed = true
	}
	if !changed {
		return nil
	}

	data, err := json.MarshalIndent(kv, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", path, err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("创建状态目录 %s 失败: %w", stateDir, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}
