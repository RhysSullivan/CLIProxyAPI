package auth

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func newClaudeUsageCooldown(t *testing.T, id string) (*Manager, time.Time) {
	t.Helper()
	withQuotaCooldownEnabled(t)
	m := NewManager(nil, nil, nil)
	_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude", Status: StatusActive,
		Metadata: map[string]any{"access_token": "synthetic-token"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "usage-model-a"}, {ID: "usage-model-b"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	retry := 72 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "usage-model-a",
		CredentialScope: true, RetryAfter: &retry, Error: &Error{HTTPStatus: 429, Message: "synthetic quota limit"}})
	now := time.Now()
	m.mu.Lock()
	m.auths[id].UpdatedAt = now.Add(-10 * time.Minute)
	m.mu.Unlock()
	return m, now
}

func healthyClaudeObservation(id string, now time.Time) ClaudeUsageObservation {
	zero := 0.0
	return ClaudeUsageObservation{AuthID: id, AccessToken: "synthetic-token", StartedAt: now,
		FiveHourUtil: &zero, SevenDayUtil: &zero}
}

func TestClaudeUsageReconciliationReleasesSchedulerAndPersistence(t *testing.T) {
	id := "usage-recovery@example.test.json"
	m, now := newClaudeUsageCooldown(t, id)
	store := NewFileCooldownStateStore(t.TempDir())
	m.SetCooldownStateStore(store)
	m.persistCooldownStates(context.Background())
	if _, err := m.scheduler.pickSingle(context.Background(), "claude", "usage-model-a", cliproxyexecutor.Options{}, nil); err == nil {
		t.Fatal("expected scheduler to reject the old credential cooldown")
	}
	if !m.reconcileClaudeUsage(context.Background(), healthyClaudeObservation(id, now), now) {
		t.Fatal("fresh healthy usage did not release the stale credential cooldown")
	}
	if picked, err := m.scheduler.pickSingle(context.Background(), "claude", "usage-model-a", cliproxyexecutor.Options{}, nil); err != nil || picked == nil {
		t.Fatalf("scheduler remained blocked: %v", err)
	}
	got, _ := m.GetByID(id)
	if got.Quota.Exceeded || got.Unavailable || got.LastError != nil {
		t.Fatalf("stale quota retained: %+v", got.Quota)
	}
	if status := m.RoutingAvailability(id, now); status.Status != "ready" {
		t.Fatalf("routing status = %+v", status)
	}
	records, err := store.Load(context.Background())
	if err != nil || len(records) != 0 {
		t.Fatalf("cleared cooldown would return after restart: records=%+v error=%v", records, err)
	}
}

func TestClaudeUsageReconciliationPreservesIndependentBlocks(t *testing.T) {
	id := "usage-independent@example.test.json"
	m, now := newClaudeUsageCooldown(t, id)
	if _, _, err := m.ResetQuota(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	modelRetry := time.Hour
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "usage-model-b",
		RetryAfter: &modelRetry, Error: &Error{HTTPStatus: 503, Message: "synthetic model outage"}})
	before, _ := m.GetByID(id)
	independentUntil := before.ModelStates["usage-model-b"].NextRetryAfter
	credentialRetry := 72 * time.Hour
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "claude", Model: "usage-model-a",
		CredentialScope: true, RetryAfter: &credentialRetry, Error: &Error{HTTPStatus: 429}})
	now = time.Now()
	m.mu.Lock()
	m.auths[id].UpdatedAt = now.Add(-10 * time.Minute)
	m.mu.Unlock()
	if !m.reconcileClaudeUsage(context.Background(), healthyClaudeObservation(id, now), now) {
		t.Fatal("expected credential quota release")
	}
	got, _ := m.GetByID(id)
	blocked, _, next := isAuthBlockedForModel(got, "usage-model-b", now)
	if !blocked || !next.Equal(independentUntil) || got.ModelStates["usage-model-b"].LastError.HTTPStatus != 503 {
		t.Fatal("independent model failure was erased")
	}
	if blocked, _, _ := isAuthBlockedForModel(got, "usage-model-a", now); blocked {
		t.Fatal("credential-derived reporting-model cooldown survived")
	}
	if status := m.RoutingAvailability(id, now); status.Status != "partial" {
		t.Fatalf("partially blocked routing status = %+v", status)
	}
}

func TestClaudeUsageReconciliationRejectsUnsafeObservations(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ClaudeUsageObservation, *Auth, time.Time)
	}{
		{"weekly exhausted", func(o *ClaudeUsageObservation, _ *Auth, _ time.Time) { v := 100.0; o.SevenDayUtil = &v }},
		{"session exhausted", func(o *ClaudeUsageObservation, _ *Auth, _ time.Time) { v := 100.0; o.FiveHourUtil = &v }},
		{"weekly missing", func(o *ClaudeUsageObservation, _ *Auth, _ time.Time) { o.SevenDayUtil = nil }},
		{"invalid utilization", func(o *ClaudeUsageObservation, _ *Auth, _ time.Time) { v := math.NaN(); o.FiveHourUtil = &v }},
		{"negative utilization", func(o *ClaudeUsageObservation, _ *Auth, _ time.Time) { v := -1.0; o.FiveHourUtil = &v }},
		{"stale report", func(o *ClaudeUsageObservation, _ *Auth, now time.Time) { o.StartedAt = now.Add(-6 * time.Minute) }},
		{"future report", func(o *ClaudeUsageObservation, _ *Auth, now time.Time) { o.StartedAt = now.Add(time.Minute) }},
		{"newer failure", func(_ *ClaudeUsageObservation, a *Auth, now time.Time) { a.UpdatedAt = now.Add(time.Second) }},
		{"grace period", func(_ *ClaudeUsageObservation, a *Auth, now time.Time) { a.UpdatedAt = now.Add(-time.Minute) }},
		{"rotated token", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) {
			a.Metadata["access_token"] = "new-synthetic-token"
		}},
		{"disabled credential", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) { a.Disabled = true }},
		{"unauthorized", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) {
			a.LastError = &Error{HTTPStatus: 401}
			a.RequiresSignIn = true
		}},
		{"other failure", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) { a.LastError = &Error{HTTPStatus: 503} }},
		{"other provider", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) { a.Provider = "codex" }},
		{"other quota", func(_ *ClaudeUsageObservation, a *Auth, _ time.Time) { a.Quota.Reason = "quota" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := "usage-negative-" + tt.name
			m, now := newClaudeUsageCooldown(t, id)
			o := healthyClaudeObservation(id, now)
			m.mu.Lock()
			tt.change(&o, m.auths[id], now)
			m.mu.Unlock()
			if m.reconcileClaudeUsage(context.Background(), o, now) {
				t.Fatal("unsafe observation released cooldown")
			}
			got, _ := m.GetByID(id)
			if !got.Quota.Exceeded {
				t.Fatal("quota block changed")
			}
		})
	}
}
