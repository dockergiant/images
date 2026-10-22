package capture

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

const timeoutRestore = 30 * time.Second

// ErrRestoring refuses a capture while the previous one for the same key is
// still putting the settings back; a new apply would race that restore.
var ErrRestoring = errors.New("capture: the previous capture is still restoring")

type session struct {
	timer   *time.Timer
	restore func(ctx context.Context) error
	once    sync.Once
	err     error
	ending  bool
}

func (s *session) run(ctx context.Context) error {
	s.once.Do(func() {
		s.timer.Stop()
		s.err = s.restore(ctx)
	})
	return s.err
}

type Manager struct {
	log      *slog.Logger
	mu       sync.Mutex
	sessions map[string]*session
}

func New(log *slog.Logger) *Manager {
	return &Manager{log: log, sessions: map[string]*session{}}
}

func (m *Manager) Begin(key string, max time.Duration, restore func(ctx context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[key]; ok {
		if s.ending {
			return ErrRestoring
		}
		return errors.New("capture already active: " + key)
	}
	s := &session{restore: restore}
	s.timer = time.AfterFunc(max, func() {
		m.mu.Lock()
		s.ending = true
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), timeoutRestore)
		defer cancel()
		if err := m.finish(ctx, key, s); err != nil {
			m.log.Error("capture restore after timeout failed", "key", key, "err", err)
		}
	})
	m.sessions[key] = s
	return nil
}

// The session keeps its key until the restore is done, so a Begin in the
// meantime gets ErrRestoring instead of applying over a half-restored state.
func (m *Manager) finish(ctx context.Context, key string, s *session) error {
	err := s.run(ctx)
	m.mu.Lock()
	if m.sessions[key] == s {
		delete(m.sessions, key)
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) End(ctx context.Context, key string) error {
	m.mu.Lock()
	s, ok := m.sessions[key]
	if ok {
		s.ending = true
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return m.finish(ctx, key, s)
}

func (m *Manager) Active(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	return ok && !s.ending
}

func (m *Manager) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *Manager) EndAll(ctx context.Context) {
	for _, key := range m.Keys() {
		if err := m.End(ctx, key); err != nil {
			m.log.Error("capture restore failed", "key", key, "err", err)
		}
	}
}
