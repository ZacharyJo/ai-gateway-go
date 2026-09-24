package proxy

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// imagegenSkillFS 内嵌 imagegen skill 资产（SKILL.md + scripts/image_gen.py + agents/openai.yaml），
// 供 `proxy skill-imagegen <dir>` 安装到客户端 skill 目录。
//
//go:embed skills/imagegen
var imagegenSkillFS embed.FS

// installImagegenSkill 把内嵌的 imagegen skill 递归写入 targetDir 下。
// 已存在同名文件时不覆盖（幂等）。返回安装的文件列表。
func installImagegenSkill(targetDir string) ([]string, error) {
	var installed []string
	src := "skills/imagegen"
	err := fs.WalkDir(imagegenSkillFS, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := imagegenSkillFS.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dest := filepath.Join(targetDir, rel)
		if _, err := os.Stat(dest); err == nil {
			// 已存在：不覆盖用户改动
			installed = append(installed, dest)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
		installed = append(installed, dest)
		return nil
	})
	return installed, err
}
