package registry

import (
	"encoding/json"
	"testing"
)

func TestDetectChangedProviders_CodexConfigurationUpdate(t *testing.T) {
	oldData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna"}},
	}
	newData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna", SupportConfigurationUpdate: true}},
	}

	changed := detectChangedProviders(oldData, newData)
	if len(changed) != 1 || changed[0] != "codex" {
		t.Fatalf("configuration_update-only change: got providers %v, want [codex]", changed)
	}
}

func TestDetectChangedProviders_KimiAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}},
	}
	newData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}, {ID: "kimi-k3"}},
	}

	changed := detectChangedProviders(oldData, newData)
	expected := map[string]bool{
		"kimi":     false,
		"kimi-ai":  false,
		"kimi.ai":  false,
		"kimi.com": false,
	}

	for _, p := range changed {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for p, found := range expected {
		if !found {
			t.Errorf("expected changed provider %q to be reported, got %v", p, changed)
		}
	}
}

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

func TestPinnedEmbeddedCodexModelsExistInEveryEmbeddedCodexTier(t *testing.T) {
	var embedded staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &embedded); err != nil {
		t.Fatalf("parse embedded catalog: %v", err)
	}
	tiers := map[string][]*ModelInfo{
		"codex-free": embedded.CodexFree,
		"codex-team": embedded.CodexTeam,
		"codex-plus": embedded.CodexPlus,
		"codex-pro":  embedded.CodexPro,
	}
	for tier, models := range tiers {
		present := make(map[string]struct{}, len(models))
		for _, model := range models {
			present[model.ID] = struct{}{}
		}
		for id := range pinnedEmbeddedCodexModelIDs {
			if _, ok := present[id]; !ok {
				t.Errorf("pinned Codex model %q is absent from embedded %s", id, tier)
			}
		}
	}

	remote := &staticModelsJSON{CodexPro: []*ModelInfo{{ID: "gpt-5.5"}}}
	mergePinnedCodexModels(remote, &embedded, pinnedEmbeddedCodexModelIDs)
	if len(remote.CodexPro) != 1+len(pinnedEmbeddedCodexModelIDs) {
		t.Fatalf("merged codex-pro = %d models, want remote plus pinned", len(remote.CodexPro))
	}
}

func TestMergePinnedEmbeddedCodexClientModelsKeepsPinnedSlugs(t *testing.T) {
	remote := []byte(`{"models":[` + mustCodexClientModelJSON(t, "gpt-5.5") + `]}`)
	merged, err := mergePinnedEmbeddedCodexClientModels(remote, embeddedCodexClientModelsJSON, pinnedEmbeddedCodexModelIDs)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var payload codexClientModelsPayload
	if err := json.Unmarshal(merged, &payload); err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	slugs := make(map[string]struct{}, len(payload.Models))
	for _, model := range payload.Models {
		slugs[model["slug"].(string)] = struct{}{}
	}
	for id := range pinnedEmbeddedCodexModelIDs {
		if _, ok := slugs[id]; !ok {
			t.Errorf("pinned client model %q missing after merge", id)
		}
	}
}

func mustCodexClientModelJSON(t *testing.T, slug string) string {
	t.Helper()
	var payload codexClientModelsPayload
	if err := json.Unmarshal(embeddedCodexClientModelsJSON, &payload); err != nil {
		t.Fatalf("decode embedded client catalog: %v", err)
	}
	for _, model := range payload.Models {
		if model["slug"] == slug {
			data, err := json.Marshal(model)
			if err != nil {
				t.Fatalf("encode %s: %v", slug, err)
			}
			return string(data)
		}
	}
	t.Fatalf("embedded client catalog lacks %s", slug)
	return ""
}
