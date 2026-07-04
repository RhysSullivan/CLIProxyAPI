// Package poolusage serves GET /pool/usage: per-account Claude subscription
// usage (5-hour and weekly rate-limit utilization) across every pooled OAuth
// credential in the auth directory. It exists so a patched Claude desktop app
// can render one usage bar per pooled account instead of only the single
// account logged into its chat surface. Unauthenticated and tailnet-gated,
// same posture as /healthz and /cache-status.
package poolusage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	anthropicBase = "https://api.anthropic.com"
	oauthBeta     = "oauth-2025-04-20,claude-code-20250219"
	// userAgent mirrors the Claude Code CLI; subscription OAuth tokens require a
	// Claude-Code-shaped request or the API returns a spurious 429.
	userAgent = "claude-cli/2.1.201 (external, cli)"
)

// Handler enumerates claude-*.json auth files and queries each account's usage.
type Handler struct {
	authDir string
	client  *http.Client
}

// NewHandler returns a handler reading credentials from authDir.
func NewHandler(authDir string) *Handler {
	return &Handler{
		authDir: authDir,
		// Aux/management-style call (not the inference path): a bounded timeout
		// is acceptable here, mirroring the management APICall exception.
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

type account struct {
	Email            string        `json:"email"`
	Tier             string        `json:"tier,omitempty"`
	FiveHourUtil     *float64      `json:"fiveHourUtil"`
	SevenDayUtil     *float64      `json:"sevenDayUtil"`
	FiveHourResetsAt string        `json:"fiveHourResetsAt,omitempty"`
	SevenDayResetsAt string        `json:"sevenDayResetsAt,omitempty"`
	Scoped           []scopedLimit `json:"scoped,omitempty"`
	OK               bool          `json:"ok"`
	Error            string        `json:"error,omitempty"`
}

// scopedLimit is a per-model (or per-surface) rate-limit window from the
// usage response's `limits` array — e.g. the Fable weekly cap, which has no
// top-level bucket of its own (seven_day_opus-style fields stay null for it).
type scopedLimit struct {
	Name     string  `json:"name"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Handle serves GET /pool/usage.
func (h *Handler) Handle(c *gin.Context) {
	dir := expandHome(h.authDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Warnf("[pool-usage] read auth dir %s: %v", dir, err)
		c.JSON(http.StatusOK, gin.H{"now": time.Now().UTC(), "accounts": []account{}})
		return
	}

	var files []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "claude-") && strings.HasSuffix(n, ".json") {
			files = append(files, filepath.Join(dir, n))
		}
	}

	ctx := c.Request.Context()
	results := make([]account, len(files))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(idx int, path string) {
			defer wg.Done()
			results[idx] = h.fetchAccount(ctx, path)
		}(i, f)
	}
	wg.Wait()

	accounts := make([]account, 0, len(results))
	for _, a := range results {
		if a.Email != "" {
			accounts = append(accounts, a)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Email < accounts[j].Email })

	c.JSON(http.StatusOK, gin.H{"now": time.Now().UTC(), "accounts": accounts})
}

func (h *Handler) fetchAccount(ctx context.Context, path string) account {
	raw, err := os.ReadFile(path)
	if err != nil {
		return account{}
	}
	email := gjson.GetBytes(raw, "email").String()
	if email == "" {
		email = strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "claude-"), ".json")
	}
	a := account{Email: email}
	if gjson.GetBytes(raw, "disabled").Bool() {
		a.Error = "disabled"
		return a
	}
	token := gjson.GetBytes(raw, "access_token").String()
	if token == "" {
		a.Error = "no access_token"
		return a
	}

	usage, err := h.get(ctx, token, "/api/oauth/usage")
	if err != nil {
		a.Error = err.Error()
		return a
	}
	if v := gjson.GetBytes(usage, "five_hour.utilization"); v.Exists() && v.Type != gjson.Null {
		f := v.Float()
		a.FiveHourUtil = &f
	}
	a.FiveHourResetsAt = gjson.GetBytes(usage, "five_hour.resets_at").String()
	if v := gjson.GetBytes(usage, "seven_day.utilization"); v.Exists() && v.Type != gjson.Null {
		f := v.Float()
		a.SevenDayUtil = &f
	}
	a.SevenDayResetsAt = gjson.GetBytes(usage, "seven_day.resets_at").String()
	for _, lim := range gjson.GetBytes(usage, "limits").Array() {
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
			Name:     name,
			Percent:  lim.Get("percent").Float(),
			ResetsAt: lim.Get("resets_at").String(),
		})
	}
	a.OK = true

	// Tier is best-effort; a profile error must not drop the usage row.
	if profile, perr := h.get(ctx, token, "/api/oauth/profile"); perr == nil {
		a.Tier = gjson.GetBytes(profile, "organization.rate_limit_tier").String()
	}
	return a
}

func (h *Handler) get(ctx context.Context, token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", userAgent)

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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return body, nil
}
