package auth

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const claudeQuotaRevalidationGrace = 2 * time.Minute
const claudeUsageObservationMaxAge = 5 * time.Minute

// ClaudeUsageObservation is a successful, uncached usage request. AccessToken
// binds it to the credential that was queried; it must never be logged or saved.
type ClaudeUsageObservation struct {
	AuthID       string
	AccessToken  string `json:"-"`
	StartedAt    time.Time
	FiveHourUtil *float64
	SevenDayUtil *float64
}

// ReconcileClaudeUsage releases an old credential quota when a newer usage
// request confirms both shared windows have capacity. It permits normal routing
// to try upstream again; it does not assert that inference will succeed.
func (m *Manager) ReconcileClaudeUsage(ctx context.Context, observation ClaudeUsageObservation) bool {
	return m.reconcileClaudeUsage(ctx, observation, time.Now())
}

func (m *Manager) reconcileClaudeUsage(ctx context.Context, observation ClaudeUsageObservation, now time.Time) bool {
	if m == nil || observation.AuthID == "" || observation.AccessToken == "" ||
		observation.StartedAt.IsZero() || observation.StartedAt.After(now) || now.Sub(observation.StartedAt) > claudeUsageObservationMaxAge ||
		!healthyClaudeUsage(observation.FiveHourUtil) || !healthyClaudeUsage(observation.SevenDayUtil) {
		return false
	}
	releaseMutation := m.lockAuthMutation(observation.AuthID)
	defer releaseMutation()
	m.mu.Lock()
	auth := m.auths[observation.AuthID]
	if auth == nil || auth.Provider != "claude" || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) ||
		authOuterAccessToken(auth) != observation.AccessToken || !auth.Quota.Exceeded || auth.Quota.Reason != "credential_quota" ||
		!auth.Quota.NextRecoverAt.After(now) || auth.UpdatedAt.IsZero() || observation.StartedAt.Sub(auth.UpdatedAt) < claudeQuotaRevalidationGrace ||
		(auth.LastError != nil && auth.LastError.HTTPStatus != 429) {
		m.mu.Unlock()
		return false
	}
	applyCooldownFields(&auth.Quota, QuotaState{})
	for _, state := range auth.ModelStates {
		if state == nil || (!state.credentialQuotaPropagated && state.Quota.Reason != "credential_quota") {
			continue
		}
		if shadow := state.credentialQuotaShadow; shadow != nil && isModelStateActiveCooldown(shadow, now) {
			*state = *shadow.cloneShallow()
			state.UpdatedAt = now
		} else {
			resetModelState(state, now)
		}
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.LastError = nil
	auth.StatusMessage = ""
	auth.Status = StatusActive
	updateAggregatedAvailability(auth, now)
	auth.Generation++
	auth.UpdatedAt = now
	snapshot := auth.Clone()
	m.mu.Unlock()
	releaseMutation()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	models, epoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(snapshot.ID)
	projections := make([]registry.ClientModelProjection, 0, len(models))
	for _, model := range models {
		if model != nil && strings.TrimSpace(model.ID) != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(snapshot.ID, epoch, snapshot.Generation, projections)
	m.persistCooldownStates(ctx)
	return true
}

func healthyClaudeUsage(util *float64) bool {
	return util != nil && !math.IsNaN(*util) && !math.IsInf(*util, 0) && *util >= 0 && *util < 100
}
