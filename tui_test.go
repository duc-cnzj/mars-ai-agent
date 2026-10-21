package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializeTUI 验证 TUI 配置与品牌插件落盘，且 tui.json 确实引用了插件、
// 保留了鼠标捕获（选区交给 opencode 按面板边界计算，避免 xterm 跨分屏选中）。
func TestMaterializeTUI(t *testing.T) {
	dst := t.TempDir()
	if err := MaterializeTUI(dst); err != nil {
		t.Fatalf("MaterializeTUI() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dst, "tui.json"))
	if err != nil {
		t.Fatalf("读取 tui.json 失败: %v", err)
	}
	var cfg struct {
		Plugin []string `json:"plugin"`
		Mouse  bool     `json:"mouse"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 tui.json 失败: %v", err)
	}
	if len(cfg.Plugin) != 1 || cfg.Plugin[0] != "./plugins/mars-logo.tsx" {
		t.Fatalf("tui.json plugin = %v, want [./plugins/mars-logo.tsx]", cfg.Plugin)
	}
	if !cfg.Mouse {
		t.Errorf("tui.json mouse = false, want true（选区交给 opencode，避免换行时选中右侧分屏）")
	}

	plugin, err := os.ReadFile(filepath.Join(dst, "plugins", "mars-logo.tsx"))
	if err != nil {
		t.Fatalf("读取品牌插件失败: %v", err)
	}
	if !strings.Contains(string(plugin), `home_logo`) {
		t.Errorf("插件未注册 home_logo 插槽")
	}
	if !strings.Contains(string(plugin), "资源总览") {
		t.Errorf("插件缺少启动页能力清单")
	}
}

// TestApplyTUIPrefs 验证 tips_hidden / animations_enabled 落盘，且合并时保留
// kv.json 里已有的其他偏好。
func TestApplyTUIPrefs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")

	home := t.TempDir()
	if err := ApplyTUIPrefs(home); err != nil {
		t.Fatalf("ApplyTUIPrefs() error = %v", err)
	}

	kvPath := filepath.Join(home, ".local", "state", "opencode", "kv.json")
	readKV := func() map[string]any {
		raw, err := os.ReadFile(kvPath)
		if err != nil {
			t.Fatalf("读取 kv.json 失败: %v", err)
		}
		var kv map[string]any
		if err := json.Unmarshal(raw, &kv); err != nil {
			t.Fatalf("解析 kv.json 失败: %v", err)
		}
		return kv
	}

	if hidden, _ := readKV()["tips_hidden"].(bool); !hidden {
		t.Fatalf("tips_hidden = %v, want true", readKV()["tips_hidden"])
	}
	if anim, ok := readKV()["animations_enabled"].(bool); !ok || anim {
		t.Fatalf("animations_enabled = %v, want false", readKV()["animations_enabled"])
	}

	// 已有其他偏好时必须保留，不能被重置；用户显式打开的动画也不能被覆盖。
	if err := os.WriteFile(kvPath, []byte(`{"theme":"system","animations_enabled":true}`), 0o600); err != nil {
		t.Fatalf("预写 kv.json 失败: %v", err)
	}
	if err := ApplyTUIPrefs(home); err != nil {
		t.Fatalf("ApplyTUIPrefs() 合并失败: %v", err)
	}
	kv := readKV()
	if hidden, _ := kv["tips_hidden"].(bool); !hidden {
		t.Errorf("合并后 tips_hidden = %v, want true", kv["tips_hidden"])
	}
	if kv["theme"] != "system" {
		t.Errorf("合并后 theme = %v, want system", kv["theme"])
	}
	if anim, _ := kv["animations_enabled"].(bool); !anim {
		t.Errorf("合并后 animations_enabled = %v, want true（用户显式设置不被覆盖）", kv["animations_enabled"])
	}
}
