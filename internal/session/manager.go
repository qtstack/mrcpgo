package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Manager is the concurrency safe registry of active sessions keyed by SIP
// Call-ID. A call may only own one session at a time.
type Manager struct {
	log *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewManager creates an empty session registry.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:      log,
		sessions: make(map[string]*Session),
	}
}

// Add registers s. It returns an error when a session for the same Call-ID is
// already present.
func (m *Manager) Add(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sessions[s.CallID()]; ok {
		return fmt.Errorf("session: call %s already registered (state %s)", s.CallID(), existing.State())
	}
	m.sessions[s.CallID()] = s
	return nil
}

// Get returns the session for callID.
func (m *Manager) Get(callID string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[callID]
	return s, ok
}

// Remove unregisters s. It only removes the entry when it still points at s,
// so a Call-ID that was reused by a newer session is left untouched.
func (m *Manager) Remove(s *Session) {
	if s == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.sessions[s.CallID()]; ok && current == s {
		delete(m.sessions, s.CallID())
	}
}

// Len returns the number of registered sessions.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Shutdown closes every registered session concurrently and waits for them to
// finish or for ctx to expire.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()

	if len(sessions) == 0 {
		return nil
	}
	m.log.Info("shutting down sessions", "count", len(sessions))

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Closing is idempotent, so a concurrent BYE is harmless.
			if err := s.Close(errors.New("server shutdown")); err != nil {
				m.log.Warn("session close failed during shutdown", "call_id", s.CallID(), "error", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Reclaim entries even when a session was closed by another path
		// (for example BYE) while shutdown was running.
		for _, s := range sessions {
			m.Remove(s)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
