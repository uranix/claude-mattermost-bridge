package bridge

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
)

// convState is what survives a restart of the bridge or the app server:
// enough to re-attach the conversation to its Claude CLI session.
type convState struct {
	ChannelID    string   `json:"channel_id"`
	CliSessionID string   `json:"cli_session_id"`
	Mode         string   `json:"mode,omitempty"`
	Model        string   `json:"model,omitempty"`   // "" = default
	Trusted      []string `json:"trusted,omitempty"` // tools run without asking
	TokensIn     int64    `json:"tokens_in,omitempty"`
	TokensOut    int64    `json:"tokens_out,omitempty"`
}

// store is a small JSON file keyed by conversation key. A path of "" keeps
// everything in memory only.
type store struct {
	path string
	mu   sync.Mutex
	data map[string]convState
}

func openStore(path string) (*store, error) {
	s := &store{path: path, data: map[string]convState{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) get(key string) (convState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *store) put(key string, v convState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.data[key]; ok && reflect.DeepEqual(old, v) {
		return
	}
	s.data[key] = v
	s.saveLocked()
}

func (s *store) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		delete(s.data, key)
		s.saveLocked()
	}
}

// saveLocked writes atomically: temp file in the same directory, then rename.
func (s *store) saveLocked() {
	if s.path == "" {
		return
	}
	b, _ := json.MarshalIndent(s.data, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		logErr("state save", err)
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		logErr("state save", err)
		return
	}
	if err := tmp.Close(); err != nil {
		logErr("state save", err)
		return
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		logErr("state save", err)
	}
}

func logErr(what string, err error) { slog.Error(what, "err", err) }
