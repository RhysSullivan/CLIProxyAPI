package auth

import "time"

// RoutingAvailability reports core scheduler eligibility, independently of a
// provider's usage report. Ready means eligible to try, not guaranteed success.
type RoutingAvailability struct {
	Status  string              `json:"status"`
	Reason  string              `json:"reason,omitempty"`
	RetryAt *time.Time          `json:"retryAt,omitempty"`
	Models  []ModelAvailability `json:"models,omitempty"`
}

type ModelAvailability struct {
	Model     string     `json:"model"`
	Available bool       `json:"available"`
	Reason    string     `json:"reason,omitempty"`
	RetryAt   *time.Time `json:"retryAt,omitempty"`
}

// RoutingAvailability reads the same block predicate used during selection.
// It deliberately exposes no credentials, filenames, or upstream error bodies.
func (m *Manager) RoutingAvailability(authID string, now time.Time) RoutingAvailability {
	if m == nil {
		return RoutingAvailability{Status: "unknown", Reason: "scheduler_unavailable"}
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return RoutingAvailability{Status: "unknown", Reason: "credential_not_registered"}
	}
	models := modelsForRegisteredAuth(authID)
	if len(models) == 0 {
		return RoutingAvailability{Status: "unknown", Reason: "no_registered_models"}
	}
	result := RoutingAvailability{Status: "ready"}
	ready := 0
	for _, model := range models {
		blocked, reason, retryAt := isAuthBlockedForModel(auth, model, now)
		row := ModelAvailability{Model: model, Available: !blocked}
		if !blocked {
			ready++
		} else {
			switch {
			case reason == blockReasonDisabled:
				row.Reason = "disabled"
			case hasUnauthorizedAuthFailure(auth):
				row.Reason = "sign_in_required"
			case auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now):
				row.Reason = "credential_quota"
			case !retryAt.IsZero():
				row.Reason = "model_cooldown"
			default:
				row.Reason = "credential_unavailable"
			}
			if retryAt.After(now) {
				t := retryAt.UTC()
				row.RetryAt = &t
				if result.RetryAt == nil || t.Before(*result.RetryAt) {
					result.RetryAt = &t
				}
			}
			if result.Reason == "" {
				result.Reason = row.Reason
			} else if result.Reason != row.Reason {
				result.Reason = "multiple_blocks"
			}
		}
		result.Models = append(result.Models, row)
	}
	if ready == 0 {
		result.Status = "blocked"
	} else if ready < len(models) {
		result.Status = "partial"
	}
	return result
}
