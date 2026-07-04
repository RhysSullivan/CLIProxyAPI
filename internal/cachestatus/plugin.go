package cachestatus

import (
	"context"
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// usagePlugin feeds the Tracker from the usage record stream. It is the single
// point that sees both the affinity session ID (propagated via context at the
// handler) and the response cache tokens (on the usage Record).
type usagePlugin struct {
	tracker *Tracker
}

// NewPlugin returns a usage.Plugin that records Claude prompt-cache state into t.
func NewPlugin(t *Tracker) coreusage.Plugin {
	return &usagePlugin{tracker: t}
}

// HandleUsage implements coreusage.Plugin.
func (p *usagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.tracker == nil {
		return
	}
	sid := coreusage.SessionIDFromContext(ctx)
	if !strings.HasPrefix(sid, "claude:") {
		return // only Claude Code affinity sessions are tracked
	}
	account := record.Source
	if strings.EqualFold(record.AuthType, "apikey") {
		account = "" // never surface a raw API key as the account
	}
	p.tracker.Record(
		sid,
		account,
		record.AuthID,
		record.Model,
		coreusage.CacheTTLFromContext(ctx),
		record.Detail.CacheReadTokens,
		record.Detail.CacheCreationTokens,
	)
}
