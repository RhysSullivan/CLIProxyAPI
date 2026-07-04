package poolusage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestClaudeAndCodexAccountsParsedAndMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "claude-claude@example.com.json", `{
		"email": "claude@example.com",
		"access_token": "claude-token"
	}`)
	writeAuthFile(t, authDir, "codex-codex@example.com-prolite.json", `{
		"access_token": "codex-token",
		"account_id": "acct-codex",
		"email": "codex@example.com",
		"type": "codex",
		"disabled": false
	}`)

	upstream := newPoolUsageFakeUpstream(t)
	defer upstream.Close()
	h := newTestHandler(authDir, upstream)
	h.refreshDue(context.Background(), true)

	accounts := getUsageAccounts(t, h)
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2: %+v", len(accounts), accounts)
	}

	claude := accountByProvider(t, accounts, "claude")
	if claude.Email != "claude@example.com" {
		t.Fatalf("claude email = %q", claude.Email)
	}
	if claude.Tier != "default_claude_max_20x" {
		t.Fatalf("claude tier = %q", claude.Tier)
	}
	expectFloat(t, claude.FiveHourUtil, 30)
	expectFloat(t, claude.SevenDayUtil, 23)
	if claude.FiveHourResetsAt != "2026-07-04T23:49:59Z" {
		t.Fatalf("claude five-hour reset = %q", claude.FiveHourResetsAt)
	}
	if len(claude.Scoped) != 1 || claude.Scoped[0].Name != "Fable" || claude.Scoped[0].Percent != 45 {
		t.Fatalf("claude scoped = %+v", claude.Scoped)
	}
	if !claude.OK || claude.Stale || claude.LastError != nil || claude.FetchedAt == nil {
		t.Fatalf("claude status fields = ok:%t stale:%t lastError:%v fetchedAt:%v", claude.OK, claude.Stale, claude.LastError, claude.FetchedAt)
	}

	codex := accountByProvider(t, accounts, "codex")
	if codex.Email != "codex@example.com" {
		t.Fatalf("codex email = %q", codex.Email)
	}
	if codex.Tier != "pro" {
		t.Fatalf("codex tier = %q", codex.Tier)
	}
	expectFloat(t, codex.FiveHourUtil, 12)
	expectFloat(t, codex.SevenDayUtil, 40)
	if codex.FiveHourResetsAt != "2026-07-05T01:10:00Z" {
		t.Fatalf("codex five-hour reset = %q", codex.FiveHourResetsAt)
	}
	if codex.SevenDayResetsAt != "2026-07-09T14:00:00Z" {
		t.Fatalf("codex weekly reset = %q", codex.SevenDayResetsAt)
	}
	if len(codex.Scoped) != 2 {
		t.Fatalf("codex scoped len = %d, want 2: %+v", len(codex.Scoped), codex.Scoped)
	}
	if codex.Scoped[0].Name != "Codex Spark 5-hour" || codex.Scoped[0].Percent != 7 {
		t.Fatalf("codex scoped[0] = %+v", codex.Scoped[0])
	}
	if codex.Scoped[1].Name != "Codex Spark Weekly" || codex.Scoped[1].Percent != 19 {
		t.Fatalf("codex scoped[1] = %+v", codex.Scoped[1])
	}
	if !codex.OK || codex.Stale || codex.LastError != nil || codex.FetchedAt == nil {
		t.Fatalf("codex status fields = ok:%t stale:%t lastError:%v fetchedAt:%v", codex.OK, codex.Stale, codex.LastError, codex.FetchedAt)
	}
}

func TestHandlerGETUsesCacheWithoutUpstreamCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "claude-claude@example.com.json", `{
		"email": "claude@example.com",
		"access_token": "claude-token"
	}`)
	writeAuthFile(t, authDir, "codex-codex@example.com-prolite.json", `{
		"access_token": "codex-token",
		"account_id": "acct-codex",
		"email": "codex@example.com",
		"type": "codex"
	}`)

	upstream := newPoolUsageFakeUpstream(t)
	defer upstream.Close()
	h := newTestHandler(authDir, upstream)
	h.refreshDue(context.Background(), true)
	before := upstream.Total()

	_ = getUsageAccounts(t, h)
	_ = getUsageAccounts(t, h)

	after := upstream.Total()
	if after != before {
		t.Fatalf("handler GET upstream calls = %d, want 0", after-before)
	}
}

func TestStaleGoodSnapshotServedAfterRefreshFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "claude-claude@example.com.json", `{
		"email": "claude@example.com",
		"access_token": "claude-token"
	}`)

	upstream := newPoolUsageFakeUpstream(t)
	defer upstream.Close()
	h := newTestHandler(authDir, upstream)
	h.refreshDue(context.Background(), true)
	first := getUsageAccounts(t, h)
	if len(first) != 1 || first[0].FetchedAt == nil {
		t.Fatalf("initial accounts = %+v", first)
	}
	goodFetchedAt := *first[0].FetchedAt

	upstream.SetFail(true)
	h.refreshDue(context.Background(), true)

	accounts := getUsageAccounts(t, h)
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d", len(accounts))
	}
	a := accounts[0]
	if !a.OK || !a.Stale {
		t.Fatalf("status = ok:%t stale:%t, want stale good", a.OK, a.Stale)
	}
	if a.LastError == nil || *a.LastError != "http 429" {
		t.Fatalf("lastError = %v, want http 429", a.LastError)
	}
	if a.Error != "" {
		t.Fatalf("legacy error = %q, want empty for stale good", a.Error)
	}
	if a.FetchedAt == nil || !a.FetchedAt.Equal(goodFetchedAt) {
		t.Fatalf("fetchedAt = %v, want %v", a.FetchedAt, goodFetchedAt)
	}
	expectFloat(t, a.FiveHourUtil, 30)
}

func TestBackoffDoublesAfterFailureAndResetsAfterSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	filename := "claude-claude@example.com.json"
	writeAuthFile(t, authDir, filename, `{
		"email": "claude@example.com",
		"access_token": "claude-token"
	}`)

	upstream := newPoolUsageFakeUpstream(t)
	defer upstream.Close()
	h := newTestHandler(authDir, upstream)
	upstream.SetFail(true)
	h.refreshDue(context.Background(), true)

	h.mu.RLock()
	failed := h.accounts[filename]
	failedDelay := failed.NextRefresh.Sub(failed.LastAttempt)
	h.mu.RUnlock()
	if failedDelay <= h.refreshEvery {
		t.Fatalf("failed delay = %s, want > base %s", failedDelay, h.refreshEvery)
	}

	upstream.SetFail(false)
	h.refreshDue(context.Background(), true)

	h.mu.RLock()
	succeeded := h.accounts[filename]
	successDelay := succeeded.NextRefresh.Sub(succeeded.LastAttempt)
	backoff := succeeded.Backoff
	h.mu.RUnlock()
	if backoff != h.refreshEvery {
		t.Fatalf("backoff = %s, want reset to %s", backoff, h.refreshEvery)
	}
	if successDelay != h.refreshEvery {
		t.Fatalf("success delay = %s, want %s", successDelay, h.refreshEvery)
	}
}

func TestDisabledCodexFileSkipped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex-disabled@example.com-pro.json", `{
		"access_token": "codex-token",
		"account_id": "acct-codex",
		"email": "disabled@example.com",
		"type": "codex",
		"disabled": true
	}`)

	upstream := newPoolUsageFakeUpstream(t)
	defer upstream.Close()
	h := newTestHandler(authDir, upstream)
	h.refreshDue(context.Background(), true)

	accounts := getUsageAccounts(t, h)
	if len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
	if upstream.Total() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstream.Total())
	}
}

func newTestHandler(authDir string, upstream *poolUsageFakeUpstream) *Handler {
	return newHandler(authDir, handlerOptions{
		client:        upstream.Server.Client(),
		anthropicBase: upstream.Server.URL,
		codexBase:     upstream.Server.URL,
		refreshEvery:  time.Minute,
		now: func() time.Time {
			return time.Date(2026, 7, 4, 20, 31, 0, 0, time.UTC)
		},
		startLoop: false,
	})
}

func writeAuthFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
}

func getUsageAccounts(t *testing.T, h *Handler) []account {
	t.Helper()
	router := gin.New()
	router.GET("/pool/usage", h.Handle)
	req := httptest.NewRequest(http.MethodGet, "/pool/usage", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out struct {
		Accounts []account `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v\n%s", err, w.Body.String())
	}
	return out.Accounts
}

func accountByProvider(t *testing.T, accounts []account, provider string) account {
	t.Helper()
	for _, a := range accounts {
		if a.Provider == provider {
			return a
		}
	}
	t.Fatalf("provider %q not found in %+v", provider, accounts)
	return account{}
}

func expectFloat(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("float = nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("float = %v, want %v", *got, want)
	}
}

type poolUsageFakeUpstream struct {
	Server *httptest.Server

	t    *testing.T
	mu   sync.Mutex
	fail bool
	hits map[string]int
}

func newPoolUsageFakeUpstream(t *testing.T) *poolUsageFakeUpstream {
	t.Helper()
	f := &poolUsageFakeUpstream{
		t:    t,
		hits: make(map[string]int),
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *poolUsageFakeUpstream) Close() {
	f.Server.Close()
}

func (f *poolUsageFakeUpstream) SetFail(fail bool) {
	f.mu.Lock()
	f.fail = fail
	f.mu.Unlock()
}

func (f *poolUsageFakeUpstream) Total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int
	for _, n := range f.hits {
		total += n
	}
	return total
}

func (f *poolUsageFakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	fail := f.fail
	f.mu.Unlock()
	if fail {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/oauth/usage":
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 30, "resets_at": "2026-07-04T23:49:59Z"},
			"seven_day": {"utilization": 23, "resets_at": "2026-07-11T08:59:59Z"},
			"limits": [{
				"kind": "weekly_scoped",
				"scope": {"model": {"display_name": "Fable"}},
				"percent": 45,
				"resets_at": "2026-07-11T08:59:59Z"
			}]
		}`))
	case "/api/oauth/profile":
		_, _ = w.Write([]byte(`{
			"organization": {"rate_limit_tier": "default_claude_max_20x"}
		}`))
	case "/wham/usage":
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-codex" {
			f.t.Errorf("ChatGPT-Account-Id = %q, want acct-codex", got)
		}
		_, _ = w.Write([]byte(`{
			"plan_type": "pro",
			"rate_limit": {
				"primary_window": {
					"used_percent": 12,
					"reset_at": 1783213800,
					"limit_window_seconds": 18000
				},
				"secondary_window": {
					"used_percent": 40,
					"reset_at": 1783605600,
					"limit_window_seconds": 604800
				}
			},
			"additional_rate_limits": [{
				"limit_name": "GPT-5.3-Codex-Spark",
				"metered_feature": "codex_spark",
				"rate_limit": {
					"primary_window": {
						"used_percent": 7,
						"reset_at": 1783217400,
						"limit_window_seconds": 18000
					},
					"secondary_window": {
						"used_percent": 19,
						"reset_at": 1783605600,
						"limit_window_seconds": 604800
					}
				}
			}],
			"credits": {"has_credits": true, "unlimited": false, "balance": 10.5}
		}`))
	default:
		http.NotFound(w, r)
	}
}
