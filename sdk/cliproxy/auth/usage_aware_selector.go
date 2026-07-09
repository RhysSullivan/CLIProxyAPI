package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// AccountUsage is the minimal usage snapshot a selector needs for account choice.
type AccountUsage struct {
	Provider       string
	Email          string
	FableAvailable bool
	FablePercent   *float64
	FiveHourUtil   *float64
	SevenDayUtil   *float64
	OK             bool
}

// UsageSnapshotProvider supplies cached account usage rows.
type UsageSnapshotProvider interface {
	UsageSnapshot() []AccountUsage
}

// UsageAwareSnapshotSetter is implemented by selectors that can receive usage snapshots.
type UsageAwareSnapshotSetter interface {
	SetUsageSnapshotProvider(UsageSnapshotProvider)
}

// UsageAwareSelector routes Claude accounts to preserve Fable allowance.
type UsageAwareSelector struct {
	mu       sync.RWMutex
	provider UsageSnapshotProvider
	rr       RoundRobinSelector
}

type usageAwareCandidate struct {
	auth         *Auth
	fablePercent float64
	fiveHour     float64
	sevenDay     float64
	category     int
}

// SetUsageSnapshotProvider attaches or replaces the cached usage source.
func (s *UsageAwareSelector) SetUsageSnapshotProvider(provider UsageSnapshotProvider) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.provider = provider
	s.mu.Unlock()
}

// Pick selects an auth using cached usage for Claude Fable/non-Fable routing.
func (s *UsageAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if s == nil {
		return (&RoundRobinSelector{}).Pick(ctx, provider, model, opts, auths)
	}
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if !allClaudeAuths(available) {
		return s.rr.Pick(ctx, provider, model, opts, auths)
	}

	s.mu.RLock()
	usageProvider := s.provider
	s.mu.RUnlock()
	if usageProvider == nil {
		return s.rr.Pick(ctx, provider, model, opts, auths)
	}
	usageRows := usageProvider.UsageSnapshot()
	usageByEmail, okRows := claudeUsageByEmail(usageRows)
	if okRows == 0 {
		return s.rr.Pick(ctx, provider, model, opts, auths)
	}

	fableRequest := isFableModel(model)
	candidates := make([]usageAwareCandidate, 0, len(available))
	for _, auth := range available {
		candidates = append(candidates, usageAwareCandidateForAuth(auth, usageByEmail, fableRequest))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return usageAwareLess(candidates[i], candidates[j], fableRequest)
	})

	best := candidates[:1]
	for i := 1; i < len(candidates); i++ {
		if !usageAwareEquivalent(candidates[0], candidates[i], fableRequest) {
			break
		}
		best = append(best, candidates[i])
	}
	tiedAuths := make([]*Auth, 0, len(best))
	for _, candidate := range best {
		tiedAuths = append(tiedAuths, candidate.auth)
	}
	chosen, err := s.rr.Pick(ctx, provider, model, opts, tiedAuths)
	if err != nil {
		return nil, err
	}
	direction := "preserve-fable"
	if fableRequest {
		direction = "fable-max-headroom"
	}
	chosenPercent := usageAwarePercentForLog(chosen, usageByEmail)
	selectorLogEntry(ctx).Infof("usage-aware: selected auth | auth=%s model=%s direction=%s fable_percent=%.4f", chosen.ID, model, direction, chosenPercent)
	return chosen, nil
}

func allClaudeAuths(auths []*Auth) bool {
	if len(auths) == 0 {
		return false
	}
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
			return false
		}
	}
	return true
}

func isFableModel(model string) bool {
	return strings.Contains(strings.ToLower(canonicalModelKey(model)), "fable")
}

func claudeUsageByEmail(rows []AccountUsage) (map[string]AccountUsage, int) {
	out := make(map[string]AccountUsage)
	okRows := 0
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(row.Provider), "claude") {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(row.Email))
		if email == "" {
			continue
		}
		out[email] = row
		if row.OK {
			okRows++
		}
	}
	return out, okRows
}

func usageAwareCandidateForAuth(auth *Auth, usageByEmail map[string]AccountUsage, fableRequest bool) usageAwareCandidate {
	row, hasRow := usageByEmail[authEmailKey(auth)]
	known := hasRow && row.OK
	fableAvailable := true
	fablePercent := 50.0
	fiveHour := 0.0
	sevenDay := 0.0
	if known {
		fableAvailable = row.FableAvailable
		fablePercent = usageFloat(row.FablePercent, 50.0)
		fiveHour = usageFloat(row.FiveHourUtil, 0.0)
		sevenDay = usageFloat(row.SevenDayUtil, 0.0)
	}
	category := 1
	if known {
		if fableRequest {
			if fableAvailable {
				category = 0
			} else {
				category = 2
			}
		} else if fableAvailable {
			category = 1
		} else {
			category = 0
			fablePercent = 100.0
		}
	}
	return usageAwareCandidate{
		auth:         auth,
		fablePercent: fablePercent,
		fiveHour:     fiveHour,
		sevenDay:     sevenDay,
		category:     category,
	}
}

func usageAwareLess(a, b usageAwareCandidate, fableRequest bool) bool {
	if a.category != b.category {
		return a.category < b.category
	}
	if a.fablePercent != b.fablePercent {
		if fableRequest {
			return a.fablePercent < b.fablePercent
		}
		return a.fablePercent > b.fablePercent
	}
	if a.fiveHour != b.fiveHour {
		return a.fiveHour < b.fiveHour
	}
	if a.sevenDay != b.sevenDay {
		return a.sevenDay < b.sevenDay
	}
	return a.auth.ID < b.auth.ID
}

func usageAwareEquivalent(a, b usageAwareCandidate, fableRequest bool) bool {
	_ = fableRequest
	return a.category == b.category &&
		a.fablePercent == b.fablePercent &&
		a.fiveHour == b.fiveHour &&
		a.sevenDay == b.sevenDay
}

func usageAwarePercentForLog(auth *Auth, usageByEmail map[string]AccountUsage) float64 {
	if auth == nil {
		return 50.0
	}
	row, ok := usageByEmail[authEmailKey(auth)]
	if !ok || !row.OK {
		return 50.0
	}
	return usageFloat(row.FablePercent, 50.0)
}

func authEmailKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if len(auth.Metadata) > 0 {
		if raw, ok := auth.Metadata["email"]; ok {
			if email, okString := raw.(string); okString && strings.TrimSpace(email) != "" {
				return strings.ToLower(strings.TrimSpace(email))
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Label))
}

func usageFloat(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

// SetUsageSnapshotProvider attaches a usage provider to the wrapped fallback selector.
func (s *SessionAffinitySelector) SetUsageSnapshotProvider(provider UsageSnapshotProvider) {
	if s == nil {
		return
	}
	if setter, ok := s.fallback.(UsageAwareSnapshotSetter); ok {
		setter.SetUsageSnapshotProvider(provider)
	}
}

var _ Selector = (*UsageAwareSelector)(nil)
var _ UsageAwareSnapshotSetter = (*UsageAwareSelector)(nil)
var _ UsageAwareSnapshotSetter = (*SessionAffinitySelector)(nil)
