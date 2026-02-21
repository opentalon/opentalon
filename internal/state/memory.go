package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Memory struct {
	ID        string    `yaml:"id"`
	Content   string    `yaml:"content"`
	Tags      []string  `yaml:"tags,omitempty"`
	CreatedAt time.Time `yaml:"created_at"`
}

func (m *Memory) HasTag(tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

type MemoryStore struct {
	mu       sync.RWMutex
	memories []*Memory
	dir      string
	nextID   int
}

func NewMemoryStore(dir string) *MemoryStore {
	return &MemoryStore{
		memories: make([]*Memory, 0),
		dir:      dir,
		nextID:   1,
	}
}

func (s *MemoryStore) Add(content string, tags ...string) *Memory {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := &Memory{
		ID:        fmt.Sprintf("mem_%d", s.nextID),
		Content:   content,
		Tags:      tags,
		CreatedAt: time.Now(),
	}
	s.nextID++
	s.memories = append(s.memories, m)
	return m
}

func (s *MemoryStore) Get(id string) (*Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, m := range s.memories {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("memory %q not found", id)
}

func (s *MemoryStore) Search(query string) []*Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	lower := strings.ToLower(query)
	var results []*Memory
	for _, m := range s.memories {
		if strings.Contains(strings.ToLower(m.Content), lower) {
			results = append(results, m)
		}
	}
	return results
}

func (s *MemoryStore) SearchByTag(tag string) []*Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var results []*Memory
	for _, m := range s.memories {
		if m.HasTag(tag) {
			results = append(results, m)
		}
	}
	return results
}

func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, m := range s.memories {
		if m.ID == id {
			s.memories = append(s.memories[:i], s.memories[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("memory %q not found", id)
}

func (s *MemoryStore) List() []*Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Memory, len(s.memories))
	copy(result, s.memories)
	return result
}

func (s *MemoryStore) Save() error {
	if s.dir == "" {
		return nil
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("creating memory dir: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := yaml.Marshal(s.memories)
	if err != nil {
		return fmt.Errorf("marshaling memories: %w", err)
	}

	path := filepath.Join(s.dir, "memories.yaml")
	return os.WriteFile(path, data, 0600)
}

func (s *MemoryStore) Load() error {
	if s.dir == "" {
		return nil
	}

	path := filepath.Join(s.dir, "memories.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading memories: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := yaml.Unmarshal(data, &s.memories); err != nil {
		return fmt.Errorf("parsing memories: %w", err)
	}

	maxID := 0
	for _, m := range s.memories {
		var num int
		if _, err := fmt.Sscanf(m.ID, "mem_%d", &num); err == nil && num > maxID {
			maxID = num
		}
	}
	s.nextID = maxID + 1

	return nil
}
