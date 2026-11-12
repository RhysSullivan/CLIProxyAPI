package poolusage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFreshUsageRecoversOnlyHealthyAccountAndReportsLiveRouting(t *testing.T) {
	ctx := context.Background()
	m := coreauth.NewManager(nil, nil, nil)
	authDir := t.TempDir()
	now := time.Now()
	store := coreauth.NewFileCooldownStateStore(t.TempDir())
	var records []coreauth.CooldownStateRecord
	for _, name := range []string{"healthy", "exhausted"} {
		id := "claude-" + name + "@example.test.json"
		token := "synthetic-" + name
		writeAuthFile(t, authDir, id, fmt.Sprintf(`{"email":%q,"access_token":%q}`, name+"@example.test", token))
		if _, err := m.Register(ctx, &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": token}}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "pool-routing-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		records = append(records, coreauth.CooldownStateRecord{AuthID: id, Provider: "claude",
			NextRetryAfter: now.Add(72 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour),
			Quota:     coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(72 * time.Hour)},
			LastError: &coreauth.Error{HTTPStatus: 429, Message: "private-synthetic-error"}})
	}
	if err := store.Save(ctx, records); err != nil {
		t.Fatal(err)
	}
	m.SetCooldownStateStore(store)
	if err := m.RestoreCooldownStates(ctx); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/oauth/profile" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		weekly := 0
		if r.Header.Get("Authorization") == "Bearer synthetic-exhausted" {
			weekly = 100
		}
		_, _ = fmt.Fprintf(w, `{"five_hour":{"utilization":0},"seven_day":{"utilization":%d}}`, weekly)
	}))
	t.Cleanup(upstream.Close)
	h := newHandler(authDir, handlerOptions{manager: m, client: upstream.Client(), anthropicBase: upstream.URL})
	// Before refresh, the HTTP feed must already expose the live scheduler block.
	for _, a := range getUsageAccounts(t, h) {
		if a.Routing == nil || a.Routing.Status != "blocked" || a.Routing.Reason != "credential_quota" || a.Routing.RetryAt == nil {
			t.Fatalf("old cooldown hidden: %+v", a.Routing)
		}
	}
	h.refreshDue(ctx, true)
	for _, a := range getUsageAccounts(t, h) {
		want := "ready"
		if strings.HasPrefix(a.Email, "exhausted") {
			want = "blocked"
		}
		if a.Routing == nil || a.Routing.Status != want {
			t.Fatalf("%s routing=%+v, want %s", a.Email, a.Routing, want)
		}
	}
	router := gin.New()
	router.GET("/pool/usage", h.Handle)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pool/usage", nil))
	for _, private := range []string{"synthetic-healthy", "synthetic-exhausted", "private-synthetic-error", ".json", "auth_id"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("routing feed leaked private field %q", private)
		}
	}
}

func TestMalformedClaudeUsageIsNotHealthyCapacity(t *testing.T) {
	for _, raw := range []string{`"unknown"`, `null`, `{}`, `true`} {
		t.Run(raw, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"five_hour":{"utilization":%s},"seven_day":{"utilization":%s}}`, raw, raw)
			}))
			t.Cleanup(upstream.Close)
			h := newHandler(t.TempDir(), handlerOptions{client: upstream.Client(), anthropicBase: upstream.URL})
			a, err := h.fetchClaude(context.Background(), authAccount{Token: "synthetic"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if a.FiveHourUtil != nil || a.SevenDayUtil != nil {
				t.Fatal("malformed utilization was interpreted as unused quota")
			}
		})
	}
}
