package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// skillsFS 内嵌只读的知识型 Skills（SKILL.md），随镜像一起分发。
//
//go:embed skills
var skillsFS embed.FS

// skillsRoot 是内嵌文件系统中的 skills 根目录名。
const skillsRoot = "skills"

// MaterializeSkills 把内嵌 Skills 落盘到 dst，并把文件置为只读（0444）、目录置为只读（0555），
// 以压缩"运行期被诱导写入恶意 skill"的攻击面。
//
// 必须可重复调用：$HOME 挂的是 emptyDir，容器重启（崩溃、OOM、探针失败）不会清空它，
// 而上一轮已把整棵 skills 树锁成只读。因此落盘前先恢复目录/文件的写权限，写完再重新锁紧；
// 否则第二次启动必然 permission denied，容器陷入 CrashLoopBackOff。
func MaterializeSkills(dst string) error {
	var dirs []string
	err := fs.WalkDir(skillsFS, skillsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skillsRoot, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			// 重启场景下目录已是 0555：先恢复可写，才能覆盖其中的只读文件。
			if err := os.Chmod(target, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, target)
			return nil
		}
		content, err := skillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		// 同名文件上次已落成 0444：先恢复可写再覆盖（不存在时 Chmod 返回 ErrNotExist，
		// 由随后的 WriteFile 创建）。写完重新置只读，保证重启后依然是 0444。
		if err := os.Chmod(target, 0o644); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(target, content, 0o444); err != nil {
			return err
		}
		return os.Chmod(target, 0o444)
	})
	if err != nil {
		return fmt.Errorf("落盘 skills 到 %s 失败: %w", dst, err)
	}
	// 目录置只读需自深到浅，否则父目录先只读会导致子目录无法再改。
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return fmt.Errorf("设置 skills 目录只读失败 %s: %w", dirs[i], err)
		}
	}
	return nil
}
