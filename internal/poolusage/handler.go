// Package poolusage serves GET /pool/usage: per-account Claude and Codex
// subscription usage (5-hour and weekly rate-limit utilization) across every
// pooled OAuth credential in the auth directory. It exists so a patched Claude
// desktop app can render one usage bar per pooled account instead of only the
// single account logged into its chat surface. Unauthenticated and
// tailnet-gated, same posture as /healthz and /cache-status.
package poolusage

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	defaultAnthropicBase = "https://api.anthropic.com"
	defaultCodexBase     = "https://chatgpt.com/backend-api"
	oauthBeta            = "oauth-2025-04-20,claude-code-20250219"
	// poolUsageRefreshInterval keeps menu-bar polling off the upstream APIs.
	poolUsageRefreshInterval = 120 * time.Second
	maxRefreshBackoff        = 30 * time.Minute
	accountRefreshStagger    = 500 * time.Millisecond
	accountRefreshJitter     = 500 * time.Millisecond
	// claudeUserAgent mirrors the Claude Code CLI; subscription OAuth tokens
	// require a Claude-Code-shaped request or the API returns a spurious 429.
	claudeUserAgent = "claude-cli/2.1.201 (external, cli)"
	codexUserAgent  = "CodexBar"
)

var (
	anthropicBaseURL = defaultAnthropicBase
	codexBaseURL     = defaultCodexBase
)

// Handler serves cached account usage snapshots and refreshes them in the background.
type Handler struct {
	authDir string
	client  *http.Client

	anthropicBase string
	codexBase     string
	refreshEvery  time.Duration
	stagger       time.Duration
	jitter        time.Duration
	now           func() time.Time

	mu       sync.RWMutex
	accounts map[string]*accountState

	randMu sync.Mutex
	rand   *rand.Rand

	stopCh   chan struct{}
	stopOnce sync.Once
}

type handlerOptions struct {
	client        *http.Client
	anthropicBase string
	codexBase     string
	refreshEvery  time.Duration
	stagger       time.Duration
	jitter        time.Duration
	now           func() time.Time
	startLoop     bool
}

// NewHandler returns a handler reading credentials from authDir.
func NewHandler(authDir string) *Handler {
	return newHandler(authDir, handlerOptions{
		anthropicBase: baseFromEnv("POOL_USAGE_ANTHROPIC_BASE_URL", anthropicBaseURL),
		codexBase:     baseFromEnv("POOL_USAGE_CODEX_BASE_URL", codexBaseURL),
		startLoop:     true,
	})
}

func newHandler(authDir string, opts handlerOptions) *Handler {
	client := opts.client
	if client == nil {
		client = http.DefaultClient
	}
	refreshEvery := opts.refreshEvery
	if refreshEvery <= 0 {
		refreshEvery = poolUsageRefreshInterval
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	h := &Handler{
		authDir:       authDir,
		client:        client,
		anthropicBase: cleanBase(opts.anthropicBase, defaultAnthropicBase),
		codexBase:     cleanBase(opts.codexBase, defaultCodexBase),
		refreshEvery:  refreshEvery,
		stagger:       opts.stagger,
		jitter:        opts.jitter,
		now:           now,
		accounts:      make(map[string]*accountState),
		rand:          rand.New(rand.NewSource(now().UnixNano())),
		stopCh:        make(chan struct{}),
	}
	if opts.stagger == 0 && opts.jitter == 0 && opts.startLoop {
		h.stagger = accountRefreshStagger
		h.jitter = accountRefreshJitter
	}
	if err := h.seedAccounts(); err != nil {
		log.Warnf("[pool-usage] seed accounts: %v", err)
	}
	if opts.startLoop {
		go h.refreshLoop()
	}
	return h
}

type accountProvider string

const (
	providerClaude accountProvider = "claude"
	providerCodex  accountProvider = "codex"
)

type authAccount struct {
	Filename  string
	Path      string
	Provider  accountProvider
	Email     string
	Tier      string
	Token     string
	AccountID string
	Disabled  bool
}

type accountState struct {
	Meta        authAccount
	Good        *account
	LastError   string
	LastAttempt time.Time
	NextRefresh time.Time
	Backoff     time.Duration
}

type account struct {
	Provider               string        `json:"provider"`
	Email                  string        `json:"email"`
	Tier                   string        `json:"tier,omitempty"`
	FiveHourUtil           *float64      `json:"fiveHourUtil"`
	SevenDayUtil           *float64      `json:"sevenDayUtil"`
	FiveHourResetsAt       string        `json:"fiveHourResetsAt,omitempty"`
	SevenDayResetsAt       string        `json:"sevenDayResetsAt,omitempty"`
	PrimaryWindowSeconds   int64         `json:"primaryWindowSeconds,omitempty"`
	SecondaryWindowSeconds int64         `json:"secondaryWindowSeconds,omitempty"`
	Scoped                 []scopedLimit `json:"scoped,omitempty"`
	OK                     bool          `json:"ok"`
	Stale                  bool          `json:"stale"`
	FetchedAt              *time.Time    `json:"fetchedAt"`
	LastError              *string       `json:"lastError"`
	Error                  string        `json:"error,omitempty"`
}

// scopedLimit is a per-model (or per-surface) rate-limit window from the
// usage response's `limits` array — e.g. the Fable weekly cap, which has no
// top-level bucket of its own (seven_day_opus-style fields stay null for it).
type scopedLimit struct {
	Name          string  `json:"name"`
	Percent       float64 `json:"percent"`
	ResetsAt      string  `json:"resetsAt,omitempty"`
	WindowSeconds int64   `json:"windowSeconds,omitempty"`
}

func baseFromEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func cleanBase(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = fallback
	}
	return strings.TrimRight(v, "/")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Stop terminates the refresh loop.
func (h *Handler) Stop() {
	if h == nil {
		return
	}
	h.stopOnce.Do(func() { close(h.stopCh) })
}

// Handle serves GET /pool/usage from the in-memory cache only.
func (h *Handler) Handle(c *gin.Context) {
	accounts := h.Snapshot()
	c.JSON(http.StatusOK, gin.H{"now": h.now().UTC(), "accounts": accounts})
}

// Snapshot returns cached account usage rows sorted by email.
func (h *Handler) Snapshot() []account {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	out := make([]account, 0, len(h.accounts))
	for _, st := range h.accounts {
		out = append(out, st.snapshot())
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Email == out[j].Email {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Email < out[j].Email
	})
	return out
}

// UsageSnapshot returns SDK-owned usage rows for routing decisions.
func (h *Handler) UsageSnapshot() []coreauth.AccountUsage {
	accounts := h.Snapshot()
	out := make([]coreauth.AccountUsage, 0, len(accounts))
	for _, account := range accounts {
		fableAvailable := false
		var fablePercent *float64
		var fableResetsAt *time.Time
		for _, scoped := range account.Scoped {
			if strings.EqualFold(strings.TrimSpace(scoped.Name), "fable") {
				fableAvailable = true
				percent := scoped.Percent
				fablePercent = &percent
				fableResetsAt = parseUsageResetTime(scoped.ResetsAt)
				break
			}
		}
		out = append(out, coreauth.AccountUsage{
			Provider:         account.Provider,
			Email:            account.Email,
			FableAvailable:   fableAvailable,
			FablePercent:     fablePercent,
			FableResetsAt:    fableResetsAt,
			FiveHourUtil:     account.FiveHourUtil,
			FiveHourResetsAt: parseUsageResetTime(account.FiveHourResetsAt),
			SevenDayUtil:     account.SevenDayUtil,
			SevenDayResetsAt: parseUsageResetTime(account.SevenDayResetsAt),
			OK:               account.OK,
		})
	}
	return out
}

func parseUsageResetTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

func (s *accountState) snapshot() account {
	if s.Good != nil {
		a := cloneAccount(*s.Good)
		if s.LastError != "" {
			lastErr := s.LastError
			a.Stale = true
			a.LastError = &lastErr
		} else {
			a.Stale = false
			a.LastError = nil
		}
		a.Error = ""
		return a
	}
	errMsg := s.LastError
	if errMsg == "" {
		errMsg = "pending first refresh"
	}
	a := account{
		Provider: string(s.Meta.Provider),
		Email:    s.Meta.Email,
		Tier:     s.Meta.Tier,
		OK:       false,
		Stale:    false,
		Error:    errMsg,
	}
	a.LastError = &errMsg
	if !s.LastAttempt.IsZero() {
		t := s.LastAttempt.UTC()
		a.FetchedAt = &t
	}
	return a
}

func cloneAccount(a account) account {
	out := a
	if a.FiveHourUtil != nil {
		v := *a.FiveHourUtil
		out.FiveHourUtil = &v
	}
	if a.SevenDayUtil != nil {
		v := *a.SevenDayUtil
		out.SevenDayUtil = &v
	}
	if a.FetchedAt != nil {
		v := *a.FetchedAt
		out.FetchedAt = &v
	}
	if a.LastError != nil {
		v := *a.LastError
		out.LastError = &v
	}
	out.Scoped = append([]scopedLimit(nil), a.Scoped...)
	return out
}

func (h *Handler) seedAccounts() error {
	records, err := h.discoverAccounts()
	if err != nil {
		return err
	}
	h.syncAccounts(records, h.now(), false)
	return nil
}

func (h *Handler) refreshLoop() {
	h.refreshDue(context.Background(), true)
	ticker := time.NewTicker(h.refreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.refreshDue(context.Background(), false)
		}
	}
}

func (h *Handler) refreshDue(ctx context.Context, force bool) {
	records, err := h.discoverAccounts()
	if err != nil {
		log.Warnf("[pool-usage] discover accounts: %v", err)
		return
	}
	due := h.syncAccounts(records, h.now(), force)
	sort.Slice(due, func(i, j int) bool { return due[i].Filename < due[j].Filename })
	for i, rec := range due {
		if i > 0 {
			if !h.waitStagger(ctx) {
				return
			}
		}
		fetchedAt := h.now().UTC()
		a, errFetch := h.fetchAccount(ctx, rec, fetchedAt)
		h.applyRefreshResult(rec, a, errFetch, fetchedAt)
	}
}

func (h *Handler) syncAccounts(records []authAccount, now time.Time, force bool) []authAccount {
	current := make(map[string]authAccount, len(records))
	for _, rec := range records {
		current[rec.Filename] = rec
	}

	h.mu.Lock()
	for name := range h.accounts {
		if _, ok := current[name]; !ok {
			delete(h.accounts, name)
		}
	}
	due := make([]authAccount, 0, len(records))
	for _, rec := range records {
		st := h.accounts[rec.Filename]
		if st == nil {
			st = &accountState{Backoff: h.refreshEvery}
			h.accounts[rec.Filename] = st
		}
		// A new access token (re-login or token refresh) can fix the error that
		// set the backoff, so refresh that account on this pass.
		if st.LastError != "" && st.Meta.Token != rec.Token {
			st.Backoff = h.refreshEvery
			st.NextRefresh = time.Time{}
		}
		st.Meta = rec
		if st.Backoff <= 0 {
			st.Backoff = h.refreshEvery
		}
		if force || st.NextRefresh.IsZero() || !now.Before(st.NextRefresh) {
			due = append(due, rec)
		}
	}
	h.mu.Unlock()
	return due
}

func (h *Handler) waitStagger(ctx context.Context) bool {
	delay := h.stagger + h.jitterDelay()
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-h.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (h *Handler) jitterDelay() time.Duration {
	if h.jitter <= 0 {
		return 0
	}
	h.randMu.Lock()
	n := h.rand.Int63n(int64(h.jitter) + 1)
	h.randMu.Unlock()
	return time.Duration(n)
}

func (h *Handler) applyRefreshResult(rec authAccount, a account, err error, fetchedAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.accounts[rec.Filename]
	if st == nil {
		st = &accountState{Meta: rec, Backoff: h.refreshEvery}
		h.accounts[rec.Filename] = st
	}
	st.Meta = rec
	st.LastAttempt = fetchedAt.UTC()
	if err == nil {
		st.Good = &a
		st.LastError = ""
		st.Backoff = h.refreshEvery
		st.NextRefresh = st.LastAttempt.Add(h.refreshEvery)
		return
	}
	st.LastError = err.Error()
	if st.Backoff <= 0 {
		st.Backoff = h.refreshEvery
	}
	st.Backoff *= 2
	if st.Backoff > maxRefreshBackoff {
		st.Backoff = maxRefreshBackoff
	}
	st.NextRefresh = st.LastAttempt.Add(st.Backoff)
}

func (h *Handler) discoverAccounts() ([]authAccount, error) {
	dir := expandHome(h.authDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read auth dir %s: %w", dir, err)
	}
	records := make([]authAccount, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		switch {
		case strings.HasPrefix(n, "claude-") && strings.HasSuffix(n, ".json"):
			rec, ok := h.readClaudeAuth(filepath.Join(dir, n), n)
			if ok {
				records = append(records, rec)
			}
		case strings.HasPrefix(n, "codex-") && strings.HasSuffix(n, ".json"):
			rec, ok := h.readCodexAuth(filepath.Join(dir, n), n)
			if ok {
				records = append(records, rec)
			}
		}
	}
	return records, nil
}

func (h *Handler) readClaudeAuth(fullPath, filename string) (authAccount, bool) {
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		log.Warnf("[pool-usage] read %s: %v", fullPath, err)
		return authAccount{}, false
	}
	email := ojson.GetBytes(raw, "email").String()
	if email == "" {
		email = strings.TrimSuffix(strings.TrimPrefix(filename, "claude-"), ".json")
	}
	return authAccount{
		Filename: filename,
		Path:     fullPath,
		Provider: providerClaude,
		Email:    email,
		Token:    ojson.GetBytes(raw, "access_token").String(),
		Disabled: ojson.GetBytes(raw, "disabled").Bool(),
	}, true
}

func (h *Handler) readCodexAuth(fullPath, filename string) (authAccount, bool) {
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		log.Warnf("[pool-usage] read %s: %v", fullPath, err)
		return authAccount{}, false
	}
	if ojson.GetBytes(raw, "disabled").Bool() {
		return authAccount{}, false
	}
	email, tier := codexIdentityFromFilename(filename)
	if v := ojson.GetBytes(raw, "email").String(); v != "" {
		email = v
	}
	return authAccount{
		Filename:  filename,
		Path:      fullPath,
		Provider:  providerCodex,
		Email:     email,
		Tier:      tier,
		Token:     ojson.GetBytes(raw, "access_token").String(),
		AccountID: ojson.GetBytes(raw, "account_id").String(),
	}, true
}

func codexIdentityFromFilename(filename string) (string, string) {
	base := strings.TrimSuffix(strings.TrimPrefix(filename, "codex-"), ".json")
	i := strings.LastIndex(base, "-")
	if i <= 0 || i == len(base)-1 {
		return base, ""
	}
	return base[:i], base[i+1:]
}

func (h *Handler) fetchAccount(ctx context.Context, rec authAccount, fetchedAt time.Time) (account, error) {
	switch rec.Provider {
	case providerClaude:
		return h.fetchClaude(ctx, rec, fetchedAt)
	case providerCodex:
		return h.fetchCodex(ctx, rec, fetchedAt)
	default:
		return account{}, fmt.Errorf("unknown provider %q", rec.Provider)
	}
}

func (h *Handler) fetchClaude(ctx context.Context, rec authAccount, fetchedAt time.Time) (account, error) {
	a := account{Provider: string(providerClaude), Email: rec.Email}
	if rec.Disabled {
		// Disabled Claude rows stay visible for legacy consumers; Codex disabled files are skipped at discovery.
		return a, fmt.Errorf("disabled")
	}
	if rec.Token == "" {
		return a, fmt.Errorf("no access_token")
	}

	usage, err := h.getClaude(ctx, rec.Token, "/api/oauth/usage")
	if err != nil {
		return a, err
	}
	if v := ojson.GetBytes(usage, "five_hour.utilization"); v.Exists() && v.Type != ojson.Null {
		f := v.Float()
		a.FiveHourUtil = &f
	}
	a.FiveHourResetsAt = ojson.GetBytes(usage, "five_hour.resets_at").String()
	a.PrimaryWindowSeconds = 18000
	if v := ojson.GetBytes(usage, "seven_day.utilization"); v.Exists() && v.Type != ojson.Null {
		f := v.Float()
		a.SevenDayUtil = &f
	}
	a.SevenDayResetsAt = ojson.GetBytes(usage, "seven_day.resets_at").String()
	a.SecondaryWindowSeconds = 604800
	for _, lim := range ojson.GetBytes(usage, "limits").Array() {
		if lim.Get("kind").String() != "weekly_scoped" {
			continue
		}
		name := lim.Get("scope.model.display_name").String()
		if name == "" {
			name = lim.Get("scope.surface").String()
		}
		if name == "" {
			continue
		}
		a.Scoped = append(a.Scoped, scopedLimit{
			Name:          name,
			Percent:       lim.Get("percent").Float(),
			ResetsAt:      lim.Get("resets_at").String(),
			WindowSeconds: 604800,
		})
	}
	a.OK = true
	a.Stale = false
	t := fetchedAt.UTC()
	a.FetchedAt = &t

	// Tier is best-effort; a profile error must not drop the usage row.
	if profile, perr := h.getClaude(ctx, rec.Token, "/api/oauth/profile"); perr == nil {
		a.Tier = ojson.GetBytes(profile, "organization.rate_limit_tier").String()
	}
	return a, nil
}

func (h *Handler) fetchCodex(ctx context.Context, rec authAccount, fetchedAt time.Time) (account, error) {
	a := account{Provider: string(providerCodex), Email: rec.Email, Tier: rec.Tier}
	if rec.Token == "" {
		return a, fmt.Errorf("no access_token")
	}
	usage, err := h.getCodex(ctx, rec.Token, rec.AccountID)
	if err != nil {
		return a, err
	}
	if v := ojson.GetBytes(usage, "plan_type").String(); v != "" {
		a.Tier = v
	}
	h.applyCodexWindow(&a.FiveHourUtil, &a.FiveHourResetsAt, ojson.GetBytes(usage, "rate_limit.primary_window"))
	h.applyCodexWindow(&a.SevenDayUtil, &a.SevenDayResetsAt, ojson.GetBytes(usage, "rate_limit.secondary_window"))
	a.PrimaryWindowSeconds = ojson.GetBytes(usage, "rate_limit.primary_window.limit_window_seconds").Int()
	a.SecondaryWindowSeconds = ojson.GetBytes(usage, "rate_limit.secondary_window.limit_window_seconds").Int()
	a.Scoped = codexScopedLimits(ojson.GetBytes(usage, "additional_rate_limits"))
	a.OK = true
	a.Stale = false
	t := fetchedAt.UTC()
	a.FetchedAt = &t
	return a, nil
}

func (h *Handler) applyCodexWindow(util **float64, resetsAt *string, window ojson.Result) {
	if !window.Exists() || window.Type == ojson.Null {
		return
	}
	if v := window.Get("used_percent"); v.Exists() && v.Type != ojson.Null {
		f := v.Float()
		*util = &f
	}
	*resetsAt = codexResetString(window.Get("reset_at"))
}

func codexScopedLimits(raw ojson.Result) []scopedLimit {
	if !raw.IsArray() {
		return nil
	}
	out := make([]scopedLimit, 0)
	for _, lim := range raw.Array() {
		if strings.Contains(strings.ToLower(firstNonEmpty(lim.Get("limit_name"), lim.Get("metered_feature"))), "spark") {
			out = append(out, codexSparkScopedLimits(lim)...)
			continue
		}
		name := firstNonEmpty(lim.Get("limit_name"), lim.Get("metered_feature"))
		if name == "" {
			continue
		}
		out = append(out, codexNamedScopedLimits(name, lim)...)
	}
	return out
}

func codexNamedScopedLimits(name string, lim ojson.Result) []scopedLimit {
	var out []scopedLimit
	for _, candidate := range []struct {
		name string
		path string
	}{
		{name: "Primary", path: "rate_limit.primary_window"},
		{name: "Secondary", path: "rate_limit.secondary_window"},
	} {
		window := lim.Get(candidate.path)
		if !window.Exists() || window.Type == ojson.Null {
			continue
		}
		seconds := window.Get("limit_window_seconds").Int()
		label := candidate.name
		switch seconds {
		case 18000:
			label = "5-hour"
		case 604800:
			label = "Weekly"
		default:
			if seconds > 0 {
				label = (time.Duration(seconds) * time.Second).String()
			}
		}
		out = append(out, scopedLimit{
			Name:          name + " " + label,
			Percent:       window.Get("used_percent").Float(),
			ResetsAt:      codexResetString(window.Get("reset_at")),
			WindowSeconds: seconds,
		})
	}
	return out
}

func codexSparkScopedLimits(lim ojson.Result) []scopedLimit {
	return codexNamedScopedLimits("Codex Spark", lim)
}

func firstNonEmpty(values ...ojson.Result) string {
	for _, v := range values {
		if s := strings.TrimSpace(v.String()); s != "" {
			return s
		}
	}
	return ""
}

func codexResetString(raw ojson.Result) string {
	if !raw.Exists() || raw.Type == ojson.Null {
		return ""
	}
	seconds := raw.Int()
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

func (h *Handler) getClaude(ctx context.Context, token, requestPath string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURLPath(h.anthropicBase, requestPath), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", claudeUserAgent)
	return h.do(req)
}

func (h *Handler) getCodex(ctx context.Context, token, accountID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURLPath(h.codexBase, "/wham/usage"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexUserAgent)
	return h.do(req)
}

func (h *Handler) do(req *http.Request) ([]byte, error) {
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Debugf("[pool-usage] close body: %v", cerr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return body, nil
}

func joinURLPath(base, p string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path.Clean("/"+p), "/")
}
