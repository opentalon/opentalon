package bundle

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PluginsLock is the content of plugins.lock (resolved refs for plugins with github+ref).
type PluginsLock struct {
	Plugins map[string]LockEntry `yaml:"plugins"`
}

// ChannelsLock is the content of channels.lock.
type ChannelsLock struct {
	Channels map[string]LockEntry `yaml:"channels"`
}

// LockEntry records the resolved ref and path for a bundled plugin/channel.
type LockEntry struct {
	GitHub   string `yaml:"github"`
	Ref      string `yaml:"ref"`
	Resolved string `yaml:"resolved"` // commit SHA
	Path     string `yaml:"path"`     // path to binary (relative to state dir or absolute)
}

func pluginsLockPath(stateDir string) string {
	return filepath.Join(stateDir, "plugins.lock")
}

func channelsLockPath(stateDir string) string {
	return filepath.Join(stateDir, "channels.lock")
}

// LoadPluginsLock reads plugins.lock from the state directory.
func LoadPluginsLock(stateDir string) (*PluginsLock, error) {
	p := pluginsLockPath(stateDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &PluginsLock{Plugins: make(map[string]LockEntry)}, nil
		}
		return nil, fmt.Errorf("read plugins.lock: %w", err)
	}
	var lock PluginsLock
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse plugins.lock: %w", err)
	}
	if lock.Plugins == nil {
		lock.Plugins = make(map[string]LockEntry)
	}
	return &lock, nil
}

// SavePluginsLock writes plugins.lock to the state directory.
func SavePluginsLock(stateDir string, lock *PluginsLock) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal plugins.lock: %w", err)
	}
	return os.WriteFile(pluginsLockPath(stateDir), data, 0644)
}

// LoadChannelsLock reads channels.lock from the state directory.
func LoadChannelsLock(stateDir string) (*ChannelsLock, error) {
	p := channelsLockPath(stateDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &ChannelsLock{Channels: make(map[string]LockEntry)}, nil
		}
		return nil, fmt.Errorf("read channels.lock: %w", err)
	}
	var lock ChannelsLock
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse channels.lock: %w", err)
	}
	if lock.Channels == nil {
		lock.Channels = make(map[string]LockEntry)
	}
	return &lock, nil
}

// SaveChannelsLock writes channels.lock to the state directory.
func SaveChannelsLock(stateDir string, lock *ChannelsLock) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal channels.lock: %w", err)
	}
	return os.WriteFile(channelsLockPath(stateDir), data, 0644)
}

// SkillsLock is the content of skills.lock (resolved refs for downloaded OpenClaw-style skills).
type SkillsLock struct {
	Repo   *LockEntry            `yaml:"repo,omitempty"`   // default monorepo (one repo, many skill subdirs)
	Skills map[string]LockEntry  `yaml:"skills,omitempty"` // per-skill repos (name -> entry, Path = skill dir)
}

func skillsLockPath(stateDir string) string {
	return filepath.Join(stateDir, "skills.lock")
}

// LoadSkillsLock reads skills.lock from the state directory.
func LoadSkillsLock(stateDir string) (*SkillsLock, error) {
	p := skillsLockPath(stateDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &SkillsLock{Skills: make(map[string]LockEntry)}, nil
		}
		return nil, fmt.Errorf("read skills.lock: %w", err)
	}
	var lock SkillsLock
	if err := yaml.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse skills.lock: %w", err)
	}
	if lock.Skills == nil {
		lock.Skills = make(map[string]LockEntry)
	}
	return &lock, nil
}

// SaveSkillsLock writes skills.lock to the state directory.
func SaveSkillsLock(stateDir string, lock *SkillsLock) error {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := yaml.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal skills.lock: %w", err)
	}
	return os.WriteFile(skillsLockPath(stateDir), data, 0644)
}
