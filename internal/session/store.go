package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Session holds per-user state that persists across messages.
type Session struct {
	ClaudeSessionID   string    `json:"claude_session_id"`
	LastUsed          time.Time `json:"last_used"`
	ModelOverride     string    `json:"model_override,omitempty"`
	TotalCostUSD      float64   `json:"total_cost_usd,omitempty"`
	TotalInputTokens  int       `json:"total_input_tokens,omitempty"`
	TotalOutputTokens int       `json:"total_output_tokens,omitempty"`
}

// Store is the interface for session persistence.
type Store interface {
	Get(userKey string) (Session, bool)
	Set(userKey string, s Session)
	Delete(userKey string)
}

// MemoryStore is an in-process, non-persistent Store implementation.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]Session
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]Session)}
}

func (m *MemoryStore) Get(userKey string) (Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.data[userKey]
	return s, ok
}

func (m *MemoryStore) Set(userKey string, s Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[userKey] = s
}

func (m *MemoryStore) Delete(userKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, userKey)
}

// FileStore persists sessions to a JSON file so they survive process restarts.
type FileStore struct {
	mu   sync.RWMutex
	path string
	data map[string]Session
}

// NewFileStore loads (or creates) the session file at path and returns a FileStore.
func NewFileStore(path string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fs := &FileStore{path: path, data: make(map[string]Session)}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &fs.data); err != nil {
			slog.Warn("session file corrupt, starting fresh", "path", path, "err", err)
			fs.data = make(map[string]Session)
		}
	}
	return fs, nil
}

func (f *FileStore) Get(userKey string) (Session, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, ok := f.data[userKey]
	return s, ok
}

func (f *FileStore) Set(userKey string, s Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[userKey] = s
	f.save()
}

func (f *FileStore) Delete(userKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, userKey)
	f.save()
}

// save writes the current data to disk. Must be called with f.mu held.
func (f *FileStore) save() {
	raw, err := json.MarshalIndent(f.data, "", "  ")
	if err != nil {
		slog.Error("session marshal failed", "err", err)
		return
	}
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		slog.Error("session save failed", "path", f.path, "err", err)
	}
}
