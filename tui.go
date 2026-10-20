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
//go:embed tui
var tuiFS embed.FS

// tuiRoot 是内嵌文件系统中的 TUI 资源根目录名。
const tuiRoot = "tui"

// MaterializeTUI 把内嵌的 TUI 配置与插件落盘到 opencode 配置目录 dst。
//
// tui.json 中插件路径是相对路径 ./plugins/mars-logo.tsx，因此 dst 必须是
// $HOME/.config/opencode（与 tui.json 同级）。与 Skills 不同，这里保持可写：
// 属于用户级 TUI 定制，不涉及执行权限收口。
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
			return os.MkdirAll(target, 0o755)
		}
		content, err := tuiFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		return fmt.Errorf("落盘 TUI 配置到 %s 失败: %w", dst, err)
	}
	return nil
}

// DisableTips 关闭 opencode TUI 首页的内置 tips（如「Use opencode run --attach ...」）。
//
// opencode 的 internal:home-tips 插件把开关存在 TUI 键值存储的 tips_hidden 键上
// （对应命令面板 System → Hide tips），落盘位置按 XDG 约定为
// $XDG_STATE_HOME/opencode/kv.json，未设置 XDG_STATE_HOME 时为
// $HOME/.local/state/opencode/kv.json。这里在启动时统一置位，避免每个沙箱用户
// 首次进入 TUI 都看到与 mars 场景无关的 opencode 通用提示。
//
// 采用读-合并-写，保留用户在 kv.json 里的其他 TUI 偏好。
//
// 调用方应把它当作 best-effort：失败只影响首页提示是否隐藏，不得阻断沙箱启动。
func DisableTips(home string) error {
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
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("读取 %s 失败: %w", path, err)
	}

	if hidden, _ := kv["tips_hidden"].(bool); hidden {
		return nil
	}
	kv["tips_hidden"] = true

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
