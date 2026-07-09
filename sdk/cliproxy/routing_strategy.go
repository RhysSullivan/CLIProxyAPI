package cliproxy

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func normalizeRoutingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "usage-aware", "usageaware", "ua":
		return "usage-aware"
	default:
		return "round-robin"
	}
}

func newRoutingSelector(strategy string, sessionAffinity bool, sessionAffinityTTL string) coreauth.Selector {
	var selector coreauth.Selector
	switch normalizeRoutingStrategy(strategy) {
	case "fill-first":
		selector = &coreauth.FillFirstSelector{}
	case "usage-aware":
		selector = &coreauth.UsageAwareSelector{}
	default:
		selector = &coreauth.RoundRobinSelector{}
	}

	if sessionAffinity {
		ttl := time.Hour
		if ttlStr := strings.TrimSpace(sessionAffinityTTL); ttlStr != "" {
			if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
				ttl = parsed
			}
		}
		selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
			Fallback: selector,
			TTL:      ttl,
		})
	}
	return selector
}
