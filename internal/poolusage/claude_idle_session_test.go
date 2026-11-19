package poolusage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClaudeFiveHourIdleOnlyForExplicitNull(t *testing.T) {
	tests := []struct {
		name     string
		fiveHour string
		wantIdle bool
	}{
		{"explicit null", `"five_hour": null,`, true},
		{"missing field", ``, false},
		{"reported window", `"five_hour": {"utilization": 30, "resets_at": "2026-07-04T23:49:59Z"},`, false},
		{"malformed window", `"five_hour": "busy",`, false},
		{"invalid bare word", `"five_hour": nonsense,`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{` + tt.fiveHour + ` "seven_day": {"utilization": 0, "resets_at": "2026-07-11T08:59:59Z"}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/oauth/usage" {
					_, _ = w.Write([]byte(body))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			now := time.Date(2026, 7, 4, 20, 31, 0, 0, time.UTC)
			h := newHandler(t.TempDir(), handlerOptions{
				client:        server.Client(),
				anthropicBase: server.URL,
				codexBase:     server.URL,
				refreshEvery:  time.Minute,
				now:           func() time.Time { return now },
			})
			a, err := h.fetchClaude(context.Background(), authAccount{Provider: providerClaude, Token: "synthetic-token"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if a.fiveHourIdle != tt.wantIdle {
				t.Fatalf("fiveHourIdle = %v, want %v", a.fiveHourIdle, tt.wantIdle)
			}
			if tt.wantIdle && a.FiveHourUtil != nil {
				t.Fatalf("idle window reported utilization %v", *a.FiveHourUtil)
			}
		})
	}
}
