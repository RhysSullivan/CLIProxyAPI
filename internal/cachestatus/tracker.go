// Package cachestatus maintains an in-memory, per-session view of Claude
// prompt-cache TTL state and exposes it over HTTP. It lets external tooling
// (e.g. a menu bar app) show how long each session's cached prefix stays warm.
package cachestatus

import (
	"sort"
	"sync"
	"time"
)

const (
	defaultTTL   = 5 * time.Minute
	cleanupGrace = 2 * time.Minute
	cleanupEvery = 30 * time.Second
)

// Entry holds the tracked prompt-cache state for one affinity session.
type Entry struct {
	SessionID           string
	Account             string
	AuthID              string
	Model               string
	LastActivity        time.Time
	TTL                 time.Duration
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// SnapshotEntry is the JSON-serialized, read-time view of an Entry with the
// derived expiry fields computed.
type SnapshotEntry struct {
	SessionID                string    `json:"sessionId"`
	Account                  string    `json:"account"`
	AuthID                   string    `json:"authId"`
	Model                    string    `json:"model"`
	LastActivity             time.Time `json:"lastActivity"`
	TTL                      string    `json:"ttl"`
	TTLSeconds               int       `json:"ttlSeconds"`
	ExpiresAt                time.Time `json:"expiresAt"`
	SecondsRemaining         int       `json:"secondsRemaining"`
	CacheReadInputTokens     int64     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64     `json:"cacheCreationInputTokens"`
}

// Tracker keeps per-session cache state behind an RWMutex with a background
// cleanup goroutine, mirroring the concurrency shape of
// sdk/cliproxy/auth/session_cache.go.
type Tracker struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewTracker constructs a Tracker and starts its cleanup loop.
func NewTracker() *Tracker {
	t := &Tracker{
		entries: make(map[string]*Entry),
		stopCh:  make(chan struct{}),
	}
	go t.cleanupLoop()
	return t
}

// Record upserts the cache state for a session, refreshing its last-activity
// timestamp (the prompt cache TTL refreshes on every hit). A non-positive ttl
// falls back to the 5-minute default.
func (t *Tracker) Record(sessionID, account, authID, model string, ttl time.Duration, cacheRead, cacheCreation int64) {
	if t == nil || sessionID == "" {
		return
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	now := time.Now()
	t.mu.Lock()
	e := t.entries[sessionID]
	if e == nil {
		e = &Entry{SessionID: sessionID}
		t.entries[sessionID] = e
	}
	e.Account = account
	e.AuthID = authID
	e.Model = model
	e.LastActivity = now
	e.TTL = ttl
	e.CacheReadTokens = cacheRead
	e.CacheCreationTokens = cacheCreation
	t.mu.Unlock()
}

// Snapshot returns a copy of all tracked sessions with derived expiry fields,
// sorted soonest-to-expire first. Times are normalized to UTC.
func (t *Tracker) Snapshot(now time.Time) []SnapshotEntry {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	out := make([]SnapshotEntry, 0, len(t.entries))
	for _, e := range t.entries {
		expiresAt := e.LastActivity.Add(e.TTL)
		remaining := int(expiresAt.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, SnapshotEntry{
			SessionID:                e.SessionID,
			Account:                  e.Account,
			AuthID:                   e.AuthID,
			Model:                    e.Model,
			LastActivity:             e.LastActivity.UTC(),
			TTL:                      ttlString(e.TTL),
			TTLSeconds:               int(e.TTL.Seconds()),
			ExpiresAt:                expiresAt.UTC(),
			SecondsRemaining:         remaining,
			CacheReadInputTokens:     e.CacheReadTokens,
			CacheCreationInputTokens: e.CacheCreationTokens,
		})
	}
	t.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].SecondsRemaining < out[j].SecondsRemaining
	})
	return out
}

// Stop terminates the cleanup loop.
func (t *Tracker) Stop() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stopCh) })
}

func (t *Tracker) cleanupLoop() {
	ticker := time.NewTicker(cleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			t.mu.Lock()
			for k, e := range t.entries {
				if now.After(e.LastActivity.Add(e.TTL).Add(cleanupGrace)) {
					delete(t.entries, k)
				}
			}
			t.mu.Unlock()
		}
	}
}

// ttlString renders common TTLs as "5m"/"1h", falling back to the duration's
// own string form.
func ttlString(ttl time.Duration) string {
	switch ttl {
	case 5 * time.Minute:
		return "5m"
	case time.Hour:
		return "1h"
	default:
		return ttl.String()
	}
}
