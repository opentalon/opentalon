package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"gopkg.in/yaml.v3"
)

type Session struct {
	ID          string             `yaml:"id"`
	Messages    []provider.Message `yaml:"messages"`
	Summary     string             `yaml:"summary,omitempty"` // compressed history when summarization is used
	ActiveModel provider.ModelRef  `yaml:"active_model,omitempty"`
	Metadata    map[string]string  `yaml:"metadata,omitempty"`
	CreatedAt   time.Time          `yaml:"created_at"`
	UpdatedAt   time.Time          `yaml:"updated_at"`
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	dir      string
}

func NewSessionStore(dir string) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session),
		dir:      dir,
	}
}

func (s *SessionStore) Create(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	sess := &Session{
		ID:        id,
		Messages:  make([]provider.Message, 0),
		Metadata:  make(map[string]string),
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[id] = sess
	return sess
}

func (s *SessionStore) Get(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return sess, nil
}

func (s *SessionStore) AddMessage(id string, msg provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Messages = append(sess.Messages, msg)
	sess.UpdatedAt = time.Now()
	return nil
}

func (s *SessionStore) SetModel(id string, model provider.ModelRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.ActiveModel = model
	sess.UpdatedAt = time.Now()
	return nil
}

// SetSummary updates the session summary and trims messages to the given slice (for summarization).
func (s *SessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Summary = summary
	sess.Messages = messages
	sess.UpdatedAt = time.Now()
	return nil
}

func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *SessionStore) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		result = append(result, sess)
	}
	return result
}

func (s *SessionStore) Save(id string) error {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session %q not found", id)
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("creating sessions dir: %w", err)
	}

	data, err := yaml.Marshal(sess)
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}

	path := filepath.Join(s.dir, id+".yaml")
	return os.WriteFile(path, data, 0600)
}

func (s *SessionStore) Load(id string) error {
	path := filepath.Join(s.dir, id+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading session file: %w", err)
	}

	var sess Session
	if err := yaml.Unmarshal(data, &sess); err != nil {
		return fmt.Errorf("parsing session: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &sess
	return nil
}
