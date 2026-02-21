package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

type PluginStateStore struct {
	mu   sync.RWMutex
	data map[string]map[string]string // pluginID -> key -> value
	dir  string
}

func NewPluginStateStore(dir string) *PluginStateStore {
	return &PluginStateStore{
		data: make(map[string]map[string]string),
		dir:  dir,
	}
}

func (s *PluginStateStore) Get(pluginID, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ns, ok := s.data[pluginID]
	if !ok {
		return "", fmt.Errorf("no state for plugin %q", pluginID)
	}
	val, ok := ns[key]
	if !ok {
		return "", fmt.Errorf("key %q not found for plugin %q", key, pluginID)
	}
	return val, nil
}

func (s *PluginStateStore) Set(pluginID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.data[pluginID] == nil {
		s.data[pluginID] = make(map[string]string)
	}
	s.data[pluginID][key] = value
	return nil
}

func (s *PluginStateStore) Delete(pluginID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ns, ok := s.data[pluginID]
	if !ok {
		return fmt.Errorf("no state for plugin %q", pluginID)
	}
	if _, ok := ns[key]; !ok {
		return fmt.Errorf("key %q not found for plugin %q", key, pluginID)
	}
	delete(ns, key)
	if len(ns) == 0 {
		delete(s.data, pluginID)
	}
	return nil
}

func (s *PluginStateStore) Keys(pluginID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ns, ok := s.data[pluginID]
	if !ok {
		return nil, nil
	}
	keys := make([]string, 0, len(ns))
	for k := range ns {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *PluginStateStore) Save(pluginID string) error {
	if s.dir == "" {
		return nil
	}

	s.mu.RLock()
	ns := s.data[pluginID]
	s.mu.RUnlock()

	if ns == nil {
		return nil
	}

	pluginDir := filepath.Join(s.dir, pluginID)
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		return fmt.Errorf("creating plugin state dir: %w", err)
	}

	data, err := yaml.Marshal(ns)
	if err != nil {
		return fmt.Errorf("marshaling plugin state: %w", err)
	}

	path := filepath.Join(pluginDir, "state.yaml")
	return os.WriteFile(path, data, 0600)
}

func (s *PluginStateStore) Load(pluginID string) error {
	if s.dir == "" {
		return nil
	}

	path := filepath.Join(s.dir, pluginID, "state.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading plugin state: %w", err)
	}

	var ns map[string]string
	if err := yaml.Unmarshal(data, &ns); err != nil {
		return fmt.Errorf("parsing plugin state: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[pluginID] = ns
	return nil
}
