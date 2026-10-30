package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

// TestApplyTUIPrefsNullKV 回归：kv.json 内容为 JSON null 时，反序列化得到 nil map，
// 旧实现写偏好会 panic（assignment to entry in nil map），把 best-effort 变成
// PID 1 崩溃 + CrashLoop。必须当作「无偏好」正常落盘。
func TestApplyTUIPrefsNullKV(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")

	home := t.TempDir()
	kvPath := filepath.Join(home, ".local", "state", "opencode", "kv.json")
	if err := os.MkdirAll(filepath.Dir(kvPath), 0o755); err != nil {
		t.Fatalf("创建状态目录失败: %v", err)
	}
	if err := os.WriteFile(kvPath, []byte("null"), 0o600); err != nil {
		t.Fatalf("预写 kv.json 失败: %v", err)
	}
	if err := ApplyTUIPrefs(home); err != nil {
		t.Fatalf("ApplyTUIPrefs() error = %v", err)
	}

	raw, err := os.ReadFile(kvPath)
	if err != nil {
		t.Fatalf("读取 kv.json 失败: %v", err)
	}
	var kv map[string]any
	if err := json.Unmarshal(raw, &kv); err != nil {
		t.Fatalf("解析 kv.json 失败: %v", err)
	}
	if hidden, _ := kv["tips_hidden"].(bool); !hidden {
		t.Errorf("tips_hidden = %v, want true", kv["tips_hidden"])
	}
	if anim, ok := kv["animations_enabled"].(bool); !ok || anim {
		t.Errorf("animations_enabled = %v, want false", kv["animations_enabled"])
	}
}

// TestMaterializeTUIOverwritesReadOnly 回归：$HOME 是 emptyDir，重启不清空。
// 用户或 agent 在沙箱里把 tui.json / 插件目录改成只读后，第二次启动必须仍能落盘——
// 旧实现直接 WriteFile，EACCES 会让 Prepare 失败、serve 起不来、容器 CrashLoop。
// 纯装饰性的品牌插件不该把沙箱搞挂，这里镜像 MaterializeSkills 的重启安全口径。
func TestMaterializeTUIOverwritesReadOnly(t *testing.T) {
	dst := t.TempDir()
	if err := MaterializeTUI(dst); err != nil {
		t.Fatalf("首次 MaterializeTUI() error = %v", err)
	}

	// 模拟上一轮运行后被改成只读的文件与目录。
	files := []string{
		filepath.Join(dst, "tui.json"),
		filepath.Join(dst, "plugins", "mars-logo.tsx"),
	}
	for _, f := range files {
		if err := os.Chmod(f, 0o444); err != nil {
			t.Fatalf("预置只读文件 %s 失败: %v", f, err)
		}
	}
	if err := os.Chmod(filepath.Join(dst, "plugins"), 0o555); err != nil {
		t.Fatalf("预置只读目录失败: %v", err)
	}

	if err := MaterializeTUI(dst); err != nil {
		t.Fatalf("只读目标上再次落盘失败（重启会 CrashLoop）: %v", err)
	}

	plugin, err := os.ReadFile(files[1])
	if err != nil {
		t.Fatalf("读取插件失败: %v", err)
	}
	if !strings.Contains(string(plugin), "home_logo") {
		t.Errorf("覆盖后插件内容不完整")
	}
}

// TestMarsLogoArtWidth 回归：横幅最宽行是 47 列，而首行只有 46 列。
// 宽度护栏若用 art[0].length 推导会低估 1 列；更危险的是改图后最宽行不再落在首行时
// 护栏整体失效（box 比终端宽），窄终端上会退回 opentui/Bun 段错误。
// 这里锁住两件事：真实最宽行宽度，以及常量必须由全部行的最大值推导。
func TestMarsLogoArtWidth(t *testing.T) {
	src, err := tuiFS.ReadFile(tuiRoot + "/plugins/mars-logo.tsx")
	if err != nil {
		t.Fatalf("读取内嵌插件失败: %v", err)
	}
	text := string(src)

	start := strings.Index(text, "const art = [")
	if start < 0 {
		t.Fatalf("未找到 art 横幅定义")
	}
	end := strings.Index(text[start:], "]")
	if end < 0 {
		t.Fatalf("art 横幅定义未闭合")
	}
	lines := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(text[start:start+end], -1)
	if len(lines) == 0 {
		t.Fatalf("art 横幅没有字符串行")
	}
	firstWidth, maxWidth := 0, 0
	for i, m := range lines {
		w := len(m[1])
		if i == 0 {
			firstWidth = w
		}
		if w > maxWidth {
			maxWidth = w
		}
	}
	if maxWidth != 47 {
		t.Errorf("横幅最宽行 = %d 列, want 47", maxWidth)
	}
	if firstWidth == maxWidth {
		t.Fatalf("首行恰好是最宽行（%d 列），该回归用例失去区分力", maxWidth)
	}

	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "const artWidth") {
			continue
		}
		if !strings.Contains(line, "Math.max") {
			t.Errorf("artWidth 必须对全部行取最大值，实际: %s", strings.TrimSpace(line))
		}
		return
	}
	t.Fatalf("未找到 artWidth 定义")
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

	// 回归：用户从命令面板打开 tips（tips_hidden=false）后，每次容器启动都不能
	// 再被强制改回 true，否则「Show tips」只在当前会话生效，重启即失效。
	if err := os.WriteFile(kvPath, []byte(`{"tips_hidden":false}`), 0o600); err != nil {
		t.Fatalf("预写 kv.json 失败: %v", err)
	}
	if err := ApplyTUIPrefs(home); err != nil {
		t.Fatalf("ApplyTUIPrefs() 合并失败: %v", err)
	}
	if hidden, _ := readKV()["tips_hidden"].(bool); hidden {
		t.Errorf("tips_hidden 被覆盖为 true，用户显式 false 应保留")
	}
}
