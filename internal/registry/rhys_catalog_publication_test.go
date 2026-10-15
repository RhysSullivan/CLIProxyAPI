package registry

import (
	"encoding/json"
	"testing"
)

// Exercise v8's actual publication boundary, including source changes and reloads.
func TestCatalogPublicationPreservesPersonalModelPins(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
	var remote staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &remote); err != nil {
		t.Fatal(err)
	}
	withoutPins := func(models []*ModelInfo, pins map[string]struct{}) []*ModelInfo {
		var out []*ModelInfo
		for _, model := range models {
			if _, pinned := pins[model.ID]; !pinned {
				out = append(out, model)
			}
		}
		return out
	}
	remote.Claude = withoutPins(remote.Claude, pinnedEmbeddedClaudeModelIDs)
	remote.CodexFree = withoutPins(remote.CodexFree, pinnedEmbeddedCodexModelIDs)
	remote.CodexTeam = withoutPins(remote.CodexTeam, pinnedEmbeddedCodexModelIDs)
	remote.CodexPlus = withoutPins(remote.CodexPlus, pinnedEmbeddedCodexModelIDs)
	remote.CodexPro = withoutPins(remote.CodexPro, pinnedEmbeddedCodexModelIDs)
	payload, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generalCatalogUpdater.publish(payload); err != nil {
		t.Fatal(err)
	}
	assertPins := func(models []*ModelInfo, pins map[string]struct{}) {
		t.Helper()
		present := map[string]bool{}
		for _, m := range models {
			present[m.ID] = true
		}
		for id := range pins {
			if !present[id] {
				t.Errorf("publication lost pinned model %s", id)
			}
		}
	}
	published := getModels()
	assertPins(published.Claude, pinnedEmbeddedClaudeModelIDs)
	for _, models := range [][]*ModelInfo{published.CodexFree, published.CodexTeam, published.CodexPlus, published.CodexPro} {
		assertPins(models, pinnedEmbeddedCodexModelIDs)
	}
}

func TestCodexClientPublicationPreservesPersonalModelPins(t *testing.T) {
	original := GetCodexClientModelsJSON()
	t.Cleanup(func() {
		if _, err := loadCodexClientModelsFromBytes(original, "cleanup"); err != nil {
			t.Error(err)
		}
	})
	var remote codexClientModelsPayload
	if err := json.Unmarshal(embeddedCodexClientModelsJSON, &remote); err != nil {
		t.Fatal(err)
	}
	var filtered []map[string]any
	for _, m := range remote.Models {
		if _, pinned := pinnedEmbeddedCodexModelIDs[m["slug"].(string)]; !pinned {
			filtered = append(filtered, m)
		}
	}
	remote.Models = filtered
	payload, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codexCatalogUpdater.publish(payload); err != nil {
		t.Fatal(err)
	}
	var published codexClientModelsPayload
	if err := json.Unmarshal(GetCodexClientModelsJSON(), &published); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, m := range published.Models {
		present[m["slug"].(string)] = true
	}
	for id := range pinnedEmbeddedCodexModelIDs {
		if !present[id] {
			t.Errorf("publication lost pinned client model %s", id)
		}
	}
}
