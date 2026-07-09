package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewRoutingSelectorDefaultUsageAware(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		want     any
	}{
		{
			name:     "empty defaults to usage aware",
			strategy: "",
			want:     (*coreauth.UsageAwareSelector)(nil),
		},
		{
			name:     "explicit round robin",
			strategy: "round-robin",
			want:     (*coreauth.RoundRobinSelector)(nil),
		},
		{
			name:     "round robin alias",
			strategy: "rr",
			want:     (*coreauth.RoundRobinSelector)(nil),
		},
		{
			name:     "explicit usage aware",
			strategy: "usage-aware",
			want:     (*coreauth.UsageAwareSelector)(nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newRoutingSelector(tt.strategy, false, "")
			switch tt.want.(type) {
			case *coreauth.UsageAwareSelector:
				if _, ok := got.(*coreauth.UsageAwareSelector); !ok {
					t.Fatalf("selector = %T, want *UsageAwareSelector", got)
				}
			case *coreauth.RoundRobinSelector:
				if _, ok := got.(*coreauth.RoundRobinSelector); !ok {
					t.Fatalf("selector = %T, want *RoundRobinSelector", got)
				}
			default:
				t.Fatalf("unhandled wanted selector type %T", tt.want)
			}
		})
	}
}
