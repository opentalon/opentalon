package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const installedSkillsFilename = "installed_skills.yaml"

// installedSkillsFile is the on-disk shape for data_dir/installed_skills.yaml.
type installedSkillsFile struct {
	Skills []SkillEntry `yaml:"skills"`
}

// LoadInstalledSkills reads data_dir/installed_skills.yaml and returns the list of skills.
// Returns nil, nil if the file does not exist.
func LoadInstalledSkills(dataDir string) ([]SkillEntry, error) {
	if dataDir == "" {
		return nil, nil
	}
	path := filepath.Join(dataDir, installedSkillsFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f installedSkillsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Skills, nil
}

// SaveInstalledSkills writes the list of skills to data_dir/installed_skills.yaml.
func SaveInstalledSkills(dataDir string, skills []SkillEntry) error {
	if dataDir == "" {
		return fmt.Errorf("data_dir is required to save installed skills")
	}
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, installedSkillsFilename)
	f := installedSkillsFile{Skills: skills}
	data, err := yaml.Marshal(&f)
	if err != nil {
		return fmt.Errorf("marshal installed skills: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// AppendInstalledSkill loads installed_skills.yaml, appends the new skill if not already present, and saves.
func AppendInstalledSkill(dataDir string, skill SkillEntry) error {
	skills, err := LoadInstalledSkills(dataDir)
	if err != nil {
		return err
	}
	for _, s := range skills {
		if s.Name == skill.Name {
			return nil // already present
		}
	}
	skills = append(skills, skill)
	return SaveInstalledSkills(dataDir, skills)
}
