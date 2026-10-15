package cachestatus

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Handle serves GET /cache-status: the current per-session prompt-cache state.
func (t *Tracker) Handle(c *gin.Context) {
	now := time.Now()
	c.JSON(http.StatusOK, gin.H{
		"now":      now.UTC(),
		"sessions": t.Snapshot(now),
	})
}

// DetectInboundCacheTTL inspects a client request body for a cache_control ttl
// of "1h" in any of the cacheable locations; returns 1h if present, otherwise
// the 5-minute default. Claude Code uses the 5m default, so this is near-always
// exact. Note the upstream request may be normalized server-side, which can
// downgrade a mixed 1h→5m; reading the post-normalization TTL is a future
// refinement.
func DetectInboundCacheTTL(rawJSON []byte) time.Duration {
	if len(rawJSON) == 0 {
		return defaultTTL
	}
	for _, path := range []string{
		"system.#.cache_control.ttl",
		"tools.#.cache_control.ttl",
		"messages.#.content.#.cache_control.ttl",
	} {
		if res := gjson.GetBytes(rawJSON, path); res.Exists() && strings.Contains(res.Raw, "1h") {
			return time.Hour
		}
	}
	return defaultTTL
}
