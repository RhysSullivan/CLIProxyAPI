package registry

import "testing"

func TestMergePinnedClaudeModelsPreservesLaunchModelUntilRemotePublishesIt(t *testing.T) {
	remote := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-opus-4-8", DisplayName: "Remote 4.8"}}}
	embedded := &staticModelsJSON{Claude: []*ModelInfo{
		{ID: "claude-opus-5", DisplayName: "Embedded Opus 5"},
		{ID: "claude-sonnet-5", DisplayName: "Embedded Sonnet 5"},
	}}

	mergePinnedClaudeModels(remote, embedded, map[string]struct{}{"claude-opus-5": {}})

	if len(remote.Claude) != 2 || remote.Claude[1].ID != "claude-opus-5" {
		t.Fatalf("merged Claude models = %+v, want remote models plus pinned Opus 5", remote.Claude)
	}
	remote.Claude[1].DisplayName = "mutated"
	if embedded.Claude[0].DisplayName != "Embedded Opus 5" {
		t.Fatal("merged launch model must be cloned from the embedded catalog")
	}
}

func TestMergePinnedClaudeModelsPrefersRemoteDefinition(t *testing.T) {
	remote := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-opus-5", DisplayName: "Remote Opus 5"}}}
	embedded := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-opus-5", DisplayName: "Embedded Opus 5"}}}

	mergePinnedClaudeModels(remote, embedded, map[string]struct{}{"claude-opus-5": {}})

	if len(remote.Claude) != 1 || remote.Claude[0].DisplayName != "Remote Opus 5" {
		t.Fatalf("remote definition was not preserved: %+v", remote.Claude)
	}
}
