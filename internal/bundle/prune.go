package bundle

import (
	"fmt"
	"os"
	"path/filepath"
)

// CleanPlugins removes cached plugins and their lock file.
func CleanPlugins(stateDir string) error {
	return cleanCategory(stateDir, "plugins", pluginsLockPath(stateDir))
}

// CleanChannels removes cached channels and their lock file.
func CleanChannels(stateDir string) error {
	return cleanCategory(stateDir, "channels", channelsLockPath(stateDir))
}

// CleanSkills removes cached skills and their lock file.
func CleanSkills(stateDir string) error {
	return cleanCategory(stateDir, "skills", skillsLockPath(stateDir))
}

// CleanLuaPlugins removes cached Lua plugins and their lock file.
func CleanLuaPlugins(stateDir string) error {
	return cleanCategory(stateDir, "lua_plugins", luaPluginsLockPath(stateDir))
}

// CleanAll removes all cached plugins, channels, skills, Lua plugins, and their lock files.
func CleanAll(stateDir string) error {
	var errs []error
	for _, fn := range []func(string) error{CleanPlugins, CleanChannels, CleanSkills, CleanLuaPlugins} {
		if err := fn(stateDir); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("clean errors: %v", errs)
	}
	return nil
}

func cleanCategory(stateDir, subdir, lockPath string) error {
	dir := filepath.Join(stateDir, subdir)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", lockPath, err)
	}
	return nil
}
