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
