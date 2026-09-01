package registry

import (
	"encoding/json"
	"testing"
)

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

// TestPinnedEmbeddedClaudeModelsExistInEmbeddedCatalog guards the launch-window
// bridge against a pin that names a model the embedded catalog does not define.
// Such a pin silently preserves nothing, so the model disappears on the first
// remote refresh.
func TestPinnedEmbeddedClaudeModelsExistInEmbeddedCatalog(t *testing.T) {
	var embedded staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &embedded); err != nil {
		t.Fatalf("parse embedded catalog: %v", err)
	}
	present := make(map[string]struct{}, len(embedded.Claude))
	for _, model := range embedded.Claude {
		if model != nil {
			present[model.ID] = struct{}{}
		}
	}
	for id := range pinnedEmbeddedClaudeModelIDs {
		if _, ok := present[id]; !ok {
			t.Errorf("pinned model %q is absent from the embedded catalog; add it to models/models.json or drop the pin", id)
		}
	}
}
