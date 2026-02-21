package router

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
)

type Signal int

const (
	SignalAccepted    Signal = 1
	SignalRejected    Signal = -1
	SignalRegenerated Signal = -2
)

type affinityRecord struct {
	Model     provider.ModelRef `json:"model"`
	Signal    Signal            `json:"signal"`
	Timestamp time.Time         `json:"timestamp"`
}

type ModelScore struct {
	Model provider.ModelRef
	Score float64
}

type AffinityStore struct {
	mu        sync.RWMutex
	records   map[TaskType][]affinityRecord
	decayDays int
	path      string
}

func NewAffinityStore(path string, decayDays int) *AffinityStore {
	if decayDays <= 0 {
		decayDays = 30
	}
	return &AffinityStore{
		records:   make(map[TaskType][]affinityRecord),
		decayDays: decayDays,
		path:      path,
	}
}

func (s *AffinityStore) Record(taskType TaskType, model provider.ModelRef, signal Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[taskType] = append(s.records[taskType], affinityRecord{
		Model:     model,
		Signal:    signal,
		Timestamp: time.Now(),
	})
}

func (s *AffinityStore) Get(taskType TaskType) []ModelScore {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := s.records[taskType]
	if len(records) == 0 {
		return nil
	}

	now := time.Now()
	scores := make(map[provider.ModelRef]float64)
	counts := make(map[provider.ModelRef]float64)

	for _, r := range records {
		weight := s.decayWeight(r.Timestamp, now)
		scores[r.Model] += float64(r.Signal) * weight
		counts[r.Model] += weight
	}

	result := make([]ModelScore, 0, len(scores))
	for model, score := range scores {
		if counts[model] > 0 {
			result = append(result, ModelScore{
				Model: model,
				Score: score / counts[model],
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})

	return result
}

func (s *AffinityStore) decayWeight(recorded, now time.Time) float64 {
	days := now.Sub(recorded).Hours() / 24
	halfLife := float64(s.decayDays) / 2
	return math.Exp(-0.693 * days / halfLife)
}

func (s *AffinityStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[TaskType][]affinityRecord)
}

func (s *AffinityStore) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Unmarshal(data, &s.records)
}

func (s *AffinityStore) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}
