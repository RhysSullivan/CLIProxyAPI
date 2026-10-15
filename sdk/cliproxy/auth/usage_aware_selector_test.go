package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type usageSnapshotProviderFunc func() []AccountUsage

func (f usageSnapshotProviderFunc) UsageSnapshot() []AccountUsage {
	return f()
}

func ptrFloat(v float64) *float64 {
	return &v
}

func ptrTime(year int, month time.Month, day, hour, minute int) *time.Time {
	t := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	return &t
}

func claudeTestAuth(id, email string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Label:    email,
		Metadata: map[string]any{"email": email},
	}
}

func TestUsageAwareSelectorPick(t *testing.T) {
	tests := []struct {
		name  string
		model string
		auths []*Auth
		rows  []AccountUsage
		want  string
	}{
		{
			name:  "fable model picks capable account with lowest fable percent",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-high", "high@example.com"),
				claudeTestAuth("auth-low", "low@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "high@example.com", FableAvailable: true, FablePercent: ptrFloat(80), OK: true},
				{Provider: "claude", Email: "low@example.com", FableAvailable: true, FablePercent: ptrFloat(20), OK: true},
			},
			want: "auth-low",
		},
		{
			name:  "fable model picks earlier reset before lower percent",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-soon", "soon@example.com"),
				claudeTestAuth("auth-late", "late@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "soon@example.com", FableAvailable: true, FablePercent: ptrFloat(90), FableResetsAt: ptrTime(2026, time.July, 10, 5, 45), OK: true},
				{Provider: "claude", Email: "late@example.com", FableAvailable: true, FablePercent: ptrFloat(10), FableResetsAt: ptrTime(2026, time.July, 14, 5, 45), OK: true},
			},
			want: "auth-soon",
		},
		{
			name:  "fable model avoids ok incapable account when capable exists",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-capable", "capable@example.com"),
				claudeTestAuth("auth-incapable", "incapable@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "capable@example.com", FableAvailable: true, FablePercent: ptrFloat(90), OK: true},
				{Provider: "claude", Email: "incapable@example.com", FableAvailable: false, OK: true},
			},
			want: "auth-capable",
		},
		{
			name:  "fable model can pick incapable account when only candidate",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-incapable", "incapable@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "incapable@example.com", FableAvailable: false, OK: true},
			},
			want: "auth-incapable",
		},
		{
			name:  "opus model picks ok incapable account first",
			model: "claude-opus-4-5",
			auths: []*Auth{
				claudeTestAuth("auth-capable", "capable@example.com"),
				claudeTestAuth("auth-incapable", "incapable@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "capable@example.com", FableAvailable: true, FablePercent: ptrFloat(99), OK: true},
				{Provider: "claude", Email: "incapable@example.com", FableAvailable: false, OK: true},
			},
			want: "auth-incapable",
		},
		{
			name:  "opus model among capable picks highest fable percent",
			model: "claude-opus-4-5",
			auths: []*Auth{
				claudeTestAuth("auth-low", "low@example.com"),
				claudeTestAuth("auth-high", "high@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "low@example.com", FableAvailable: true, FablePercent: ptrFloat(25), OK: true},
				{Provider: "claude", Email: "high@example.com", FableAvailable: true, FablePercent: ptrFloat(85), OK: true},
			},
			want: "auth-high",
		},
		{
			name:  "opus model among capable picks earlier seven day reset before percent",
			model: "claude-opus-4-5",
			auths: []*Auth{
				claudeTestAuth("auth-soon", "soon@example.com"),
				claudeTestAuth("auth-late", "late@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "soon@example.com", FableAvailable: true, FablePercent: ptrFloat(10), SevenDayResetsAt: ptrTime(2026, time.July, 10, 5, 45), OK: true},
				{Provider: "claude", Email: "late@example.com", FableAvailable: true, FablePercent: ptrFloat(90), SevenDayResetsAt: ptrTime(2026, time.July, 14, 5, 45), OK: true},
			},
			want: "auth-soon",
		},
		{
			name:  "opus incapable category beats capable earlier reset",
			model: "claude-opus-4-5",
			auths: []*Auth{
				claudeTestAuth("auth-incapable", "incapable@example.com"),
				claudeTestAuth("auth-capable", "capable@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "incapable@example.com", FableAvailable: false, SevenDayResetsAt: ptrTime(2026, time.July, 14, 5, 45), OK: true},
				{Provider: "claude", Email: "capable@example.com", FableAvailable: true, FablePercent: ptrFloat(99), SevenDayResetsAt: ptrTime(2026, time.July, 10, 5, 45), OK: true},
			},
			want: "auth-incapable",
		},
		{
			name:  "nil reset loses to known reset within category",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-unknown", "unknown@example.com"),
				claudeTestAuth("auth-known", "known@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "unknown@example.com", FableAvailable: true, FablePercent: ptrFloat(10), OK: true},
				{Provider: "claude", Email: "known@example.com", FableAvailable: true, FablePercent: ptrFloat(90), FableResetsAt: ptrTime(2026, time.July, 10, 5, 45), OK: true},
			},
			want: "auth-known",
		},
		{
			name:  "tie on fable percent lower five hour wins",
			model: "claude-fable-5",
			auths: []*Auth{
				claudeTestAuth("auth-busy", "busy@example.com"),
				claudeTestAuth("auth-cool", "cool@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "busy@example.com", FableAvailable: true, FablePercent: ptrFloat(40), FiveHourUtil: ptrFloat(0.7), OK: true},
				{Provider: "claude", Email: "cool@example.com", FableAvailable: true, FablePercent: ptrFloat(40), FiveHourUtil: ptrFloat(0.2), OK: true},
			},
			want: "auth-cool",
		},
		{
			name:  "thinking suffix fable model is classified as fable",
			model: "claude-fable-5(high)",
			auths: []*Auth{
				claudeTestAuth("auth-high", "high@example.com"),
				claudeTestAuth("auth-low", "low@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "high@example.com", FableAvailable: true, FablePercent: ptrFloat(80), OK: true},
				{Provider: "claude", Email: "low@example.com", FableAvailable: true, FablePercent: ptrFloat(20), OK: true},
			},
			want: "auth-low",
		},
		{
			name:  "bracket fable model is classified as fable",
			model: "claude-fable-5[1m]",
			auths: []*Auth{
				claudeTestAuth("auth-high", "high@example.com"),
				claudeTestAuth("auth-low", "low@example.com"),
			},
			rows: []AccountUsage{
				{Provider: "claude", Email: "high@example.com", FableAvailable: true, FablePercent: ptrFloat(80), OK: true},
				{Provider: "claude", Email: "low@example.com", FableAvailable: true, FablePercent: ptrFloat(20), OK: true},
			},
			want: "auth-low",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := &UsageAwareSelector{}
			selector.SetUsageSnapshotProvider(usageSnapshotProviderFunc(func() []AccountUsage {
				return tt.rows
			}))
			got, err := selector.Pick(context.Background(), "mixed", tt.model, cliproxyexecutor.Options{}, tt.auths)
			if err != nil {
				t.Fatalf("Pick() error = %v", err)
			}
			if got == nil {
				t.Fatalf("Pick() auth = nil")
			}
			if got.ID != tt.want {
				t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, tt.want)
			}
		})
	}
}

func TestUsageAwareSelectorPick_ExactTieRoundRobins(t *testing.T) {
	selector := &UsageAwareSelector{}
	selector.SetUsageSnapshotProvider(usageSnapshotProviderFunc(func() []AccountUsage {
		return []AccountUsage{
			{Provider: "claude", Email: "a@example.com", FableAvailable: true, FablePercent: ptrFloat(30), FiveHourUtil: ptrFloat(0.1), SevenDayUtil: ptrFloat(0.2), OK: true},
			{Provider: "claude", Email: "b@example.com", FableAvailable: true, FablePercent: ptrFloat(30), FiveHourUtil: ptrFloat(0.1), SevenDayUtil: ptrFloat(0.2), OK: true},
		}
	}))
	auths := []*Auth{
		claudeTestAuth("auth-a", "a@example.com"),
		claudeTestAuth("auth-b", "b@example.com"),
	}

	first, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("sequential tied picks both returned %q, want round-robin across tied auths", first.ID)
	}
}

func TestUsageAwareSelectorPick_ResetBucketTieRoundRobins(t *testing.T) {
	selector := &UsageAwareSelector{}
	selector.SetUsageSnapshotProvider(usageSnapshotProviderFunc(func() []AccountUsage {
		return []AccountUsage{
			{Provider: "claude", Email: "a@example.com", FableAvailable: true, FablePercent: ptrFloat(30), FableResetsAt: ptrTime(2026, time.July, 10, 5, 1), OK: true},
			{Provider: "claude", Email: "b@example.com", FableAvailable: true, FablePercent: ptrFloat(30), FableResetsAt: ptrTime(2026, time.July, 10, 5, 6), OK: true},
		}
	}))
	auths := []*Auth{
		claudeTestAuth("auth-a", "a@example.com"),
		claudeTestAuth("auth-b", "b@example.com"),
	}

	first, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("sequential same-bucket picks both returned %q, want round-robin across tied auths", first.ID)
	}
}

func TestUsageAwareSelectorPick_FallbackRoundRobin(t *testing.T) {
	auths := []*Auth{
		claudeTestAuth("auth-b", "b@example.com"),
		claudeTestAuth("auth-a", "a@example.com"),
	}

	t.Run("no provider set", func(t *testing.T) {
		selector := &UsageAwareSelector{}
		first, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("first Pick() error = %v", err)
		}
		second, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("second Pick() error = %v", err)
		}
		if first.ID != "auth-a" || second.ID != "auth-b" {
			t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", first.ID, second.ID)
		}
	})

	t.Run("empty snapshot", func(t *testing.T) {
		selector := &UsageAwareSelector{}
		selector.SetUsageSnapshotProvider(usageSnapshotProviderFunc(func() []AccountUsage { return nil }))
		first, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("first Pick() error = %v", err)
		}
		second, err := selector.Pick(context.Background(), "mixed", "claude-fable-5", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("second Pick() error = %v", err)
		}
		if first.ID != "auth-a" || second.ID != "auth-b" {
			t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", first.ID, second.ID)
		}
	})
}

func TestUsageAwareSelectorPick_NonClaudeFallbackRoundRobin(t *testing.T) {
	selector := &UsageAwareSelector{}
	selector.SetUsageSnapshotProvider(usageSnapshotProviderFunc(func() []AccountUsage {
		return []AccountUsage{
			{Provider: "claude", Email: "a@example.com", FableAvailable: true, FablePercent: ptrFloat(10), OK: true},
		}
	}))
	auths := []*Auth{
		{ID: "auth-b", Provider: "codex"},
		{ID: "auth-a", Provider: "codex"},
	}

	first, err := selector.Pick(context.Background(), "codex", "gpt-5.2-codex", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "codex", "gpt-5.2-codex", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("second Pick() error = %v", err)
	}
	if first.ID != "auth-a" || second.ID != "auth-b" {
		t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", first.ID, second.ID)
	}
}
