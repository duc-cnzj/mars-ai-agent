package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializeSkills 验证 Skills 落盘内容来自内嵌 FS，且文件与目录均被置为只读。
func TestMaterializeSkills(t *testing.T) {
	base := t.TempDir()
	// MaterializeSkills 会把目录置为只读，导致 t.TempDir 的清理无法删除；
	// t.Cleanup 为 LIFO，此处的恢复权限会先于 TempDir 的清理执行。
	t.Cleanup(func() { chmodTreeWritable(base) })

	dst := filepath.Join(base, "skills")
	if err := MaterializeSkills(dst); err != nil {
		t.Fatalf("MaterializeSkills() error = %v", err)
	}

	file := filepath.Join(dst, "mars-resource-model", "SKILL.md")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取 skill 失败: %v", err)
	}
	if !strings.Contains(string(content), "name: mars-resource-model") {
		t.Errorf("skill 内容缺少 frontmatter name")
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat skill 失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Errorf("skill 文件权限 = %o, want 444", perm)
	}

	dirInfo, err := os.Stat(filepath.Join(dst, "crashloop-triage"))
	if err != nil {
		t.Fatalf("stat 目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o555 {
		t.Errorf("skill 目录权限 = %o, want 555", perm)
	}
}

// TestMaterializeSkillsRepeated 验证重复落盘（模拟 emptyDir 存活下的容器重启）不失败，
// 且第二轮之后文件/目录仍保持只读、内容完好。
func TestMaterializeSkillsRepeated(t *testing.T) {
	base := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(base) })

	dst := filepath.Join(base, "skills")
	for i := 1; i <= 2; i++ {
		if err := MaterializeSkills(dst); err != nil {
			t.Fatalf("第 %d 次 MaterializeSkills() error = %v", i, err)
		}
	}

	file := filepath.Join(dst, "mars-resource-model", "SKILL.md")
	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取 skill 失败: %v", err)
	}
	if !strings.Contains(string(content), "name: mars-resource-model") {
		t.Errorf("重复落盘后 skill 内容缺少 frontmatter name")
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat skill 失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o444 {
		t.Errorf("重复落盘后 skill 文件权限 = %o, want 444", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(dst, "crashloop-triage"))
	if err != nil {
		t.Fatalf("stat 目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o555 {
		t.Errorf("重复落盘后 skill 目录权限 = %o, want 555", perm)
	}
}

// TestEmbeddedSkillsFrontmatter 校验每个内嵌 skill 的 frontmatter：name 与目录名一致、
// description 非空。opencode 通过两者发现并选择 skill，写错会被静默忽略。
func TestEmbeddedSkillsFrontmatter(t *testing.T) {
	var count int
	err := fs.WalkDir(skillsFS, skillsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		count++

		content, err := skillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		meta := parseFrontmatter(t, p, string(content))
		if want := filepath.Base(filepath.Dir(p)); meta["name"] != want {
			t.Errorf("%s: frontmatter name = %q, want %q（必须与目录名一致）", p, meta["name"], want)
		}
		if strings.TrimSpace(meta["description"]) == "" {
			t.Errorf("%s: frontmatter description 为空", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历内嵌 skills 失败: %v", err)
	}
	if count == 0 {
		t.Fatal("未找到任何 SKILL.md")
	}
}

// parseFrontmatter 提取 SKILL.md 开头 --- 区块中的顶层键值（本仓只依赖 name/description）。
func parseFrontmatter(t *testing.T, path, content string) map[string]string {
	t.Helper()
	const fence = "---\n"
	if !strings.HasPrefix(content, fence) {
		t.Fatalf("%s: 缺少 frontmatter 起始分隔符", path)
	}
	rest := content[len(fence):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		t.Fatalf("%s: 缺少 frontmatter 结束分隔符", path)
	}
	meta := map[string]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.HasPrefix(line, " ") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return meta
}

// chmodTreeWritable 递归恢复目录/文件的写权限，供测试清理只读树时使用。
func chmodTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
			return nil
		}
		_ = os.Chmod(p, 0o644)
		return nil
	})
}
