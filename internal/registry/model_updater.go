package registry

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ModelsRefreshInterval is the shared cadence for model catalogs and account entitlements.
const ModelsRefreshInterval = 3 * time.Hour

var modelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json",
	"https://models.router-for.me/models.json",
}

// Models released before the shared remote catalog updates remain available from
// this binary's verified embedded catalog. Remote definitions win as soon as
// they exist, so this is only a narrow launch-window bridge.
var pinnedEmbeddedClaudeModelIDs = map[string]struct{}{
	"claude-fable-5-1": {},
	"claude-opus-5-5":  {},
}

// Codex models that are gated by account entitlement (Trusted Access for Cyber)
// and absent from the shared remote catalog. They stay pinned across every
// Codex tier until the remote catalog publishes them.
var pinnedEmbeddedCodexModelIDs = map[string]struct{}{
	"gpt-daybreak-blue-latest": {},
	"gpt-daybreak-red-latest":  {},
}

//go:embed models/models.json
var embeddedModelsJSON []byte

type modelStore struct {
	mu   sync.RWMutex
	data *staticModelsJSON
}

var modelsCatalogStore = &modelStore{}

// ModelRefreshCallback is invoked when startup or periodic model refresh detects changes.
// changedProviders contains the provider names whose model definitions changed.
type ModelRefreshCallback func(changedProviders []string)

var (
	refreshCallbackMu     sync.Mutex
	refreshCallback       ModelRefreshCallback
	pendingRefreshChanges []string
)

// SetModelRefreshCallback registers a callback that is invoked when startup or
// periodic model refresh detects changes. Only one callback is supported;
// subsequent calls replace the previous callback.
func SetModelRefreshCallback(cb ModelRefreshCallback) {
	refreshCallbackMu.Lock()
	refreshCallback = cb
	var pending []string
	if cb != nil && len(pendingRefreshChanges) > 0 {
		pending = append([]string(nil), pendingRefreshChanges...)
		pendingRefreshChanges = nil
	}
	refreshCallbackMu.Unlock()

	if cb != nil && len(pending) > 0 {
		cb(pending)
	}
}

func init() {
	// Load embedded data as fallback on startup.
	if err := loadModelsFromBytes(embeddedModelsJSON, "embed"); err != nil {
		log.Warnf("registry: failed to parse embedded models.json (embedded catalog may be incomplete or invalid; continuing startup and will rely on remote model refresh): %v", err)
	}
}

// StartModelsUpdater starts a background updater that fetches models
// immediately on startup and then refreshes the model catalog every 3 hours.
// Safe to call multiple times; only one updater will run.
func StartModelsUpdater(ctx context.Context) {
	generalCatalogUpdater.configure(ctx, "")
}

func preservePinnedEmbeddedModels(remote *staticModelsJSON) error {
	var embedded staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &embedded); err != nil {
		return fmt.Errorf("parse embedded catalog: %w", err)
	}
	mergePinnedClaudeModels(remote, &embedded, pinnedEmbeddedClaudeModelIDs)
	mergePinnedCodexModels(remote, &embedded, pinnedEmbeddedCodexModelIDs)
	return nil
}

func mergePinnedClaudeModels(remote, embedded *staticModelsJSON, pinned map[string]struct{}) {
	if remote == nil || embedded == nil {
		return
	}
	remote.Claude = mergePinnedModels(remote.Claude, embedded.Claude, pinned)
}

func mergePinnedCodexModels(remote, embedded *staticModelsJSON, pinned map[string]struct{}) {
	if remote == nil || embedded == nil {
		return
	}
	remote.CodexFree = mergePinnedModels(remote.CodexFree, embedded.CodexFree, pinned)
	remote.CodexTeam = mergePinnedModels(remote.CodexTeam, embedded.CodexTeam, pinned)
	remote.CodexPlus = mergePinnedModels(remote.CodexPlus, embedded.CodexPlus, pinned)
	remote.CodexPro = mergePinnedModels(remote.CodexPro, embedded.CodexPro, pinned)
}

// mergePinnedModels appends pinned embedded models that the remote list lacks.
func mergePinnedModels(remote, embedded []*ModelInfo, pinned map[string]struct{}) []*ModelInfo {
	if len(pinned) == 0 {
		return remote
	}
	remoteIDs := make(map[string]struct{}, len(remote))
	for _, model := range remote {
		if model != nil {
			remoteIDs[model.ID] = struct{}{}
		}
	}
	for _, model := range embedded {
		if model == nil {
			continue
		}
		if _, keep := pinned[model.ID]; !keep {
			continue
		}
		if _, exists := remoteIDs[model.ID]; exists {
			continue
		}
		remote = append(remote, cloneModelInfo(model))
		remoteIDs[model.ID] = struct{}{}
	}
	return remote
}

// detectChangedProviders compares two model catalogs and returns provider names
// whose model definitions differ. Gemini changes affect both Gemini protocols,
// while Codex tiers (free/team/plus/pro) are grouped under one "codex" provider.
func detectChangedProviders(oldData, newData *staticModelsJSON) []string {
	if oldData == nil || newData == nil {
		return nil
	}

	type section struct {
		provider string
		oldList  []*ModelInfo
		newList  []*ModelInfo
	}

	sections := []section{
		{"claude", oldData.Claude, newData.Claude},
		{"gemini", oldData.Gemini, newData.Gemini},
		{"gemini-interactions", oldData.Gemini, newData.Gemini},
		{"vertex", oldData.Vertex, newData.Vertex},
		{"aistudio", oldData.AIStudio, newData.AIStudio},
		{"codex", oldData.CodexFree, newData.CodexFree},
		{"codex", oldData.CodexTeam, newData.CodexTeam},
		{"codex", oldData.CodexPlus, newData.CodexPlus},
		{"codex", oldData.CodexPro, newData.CodexPro},
		{"kimi", oldData.Kimi, newData.Kimi},
		{"kimi-ai", oldData.Kimi, newData.Kimi},
		{"kimi.ai", oldData.Kimi, newData.Kimi},
		{"kimi.com", oldData.Kimi, newData.Kimi},
		{"antigravity", oldData.Antigravity, newData.Antigravity},
		{"xai", oldData.XAI, newData.XAI},
		{"devin", oldData.Devin, newData.Devin},
		{"meta", oldData.Meta, newData.Meta},
	}

	seen := make(map[string]bool, len(sections))
	var changed []string
	for _, s := range sections {
		if seen[s.provider] {
			continue
		}
		if modelSectionChanged(s.oldList, s.newList) {
			changed = append(changed, s.provider)
			seen[s.provider] = true
		}
	}
	return changed
}

// modelSectionChanged reports whether two model slices differ, including
// internal metadata that is intentionally omitted from normal JSON responses.
func modelSectionChanged(a, b []*ModelInfo) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != nil && b[i] != nil {
			if !reflect.DeepEqual(a[i].NativeCapabilities, b[i].NativeCapabilities) || a[i].SupportConfigurationUpdate != b[i].SupportConfigurationUpdate {
				return true
			}
		}
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(aj) != string(bj)
}

func notifyModelRefresh(changedProviders []string) {
	if len(changedProviders) == 0 {
		return
	}

	refreshCallbackMu.Lock()
	cb := refreshCallback
	if cb == nil {
		pendingRefreshChanges = mergeProviderNames(pendingRefreshChanges, changedProviders)
		refreshCallbackMu.Unlock()
		return
	}
	refreshCallbackMu.Unlock()
	cb(changedProviders)
}

func mergeProviderNames(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, provider := range existing {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	for _, provider := range incoming {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

func loadModelsFromBytes(data []byte, source string) error {
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%s: decode models catalog: %w", source, err)
	}
	if err := validateModelsCatalog(&parsed); err != nil {
		return fmt.Errorf("%s: validate models catalog: %w", source, err)
	}

	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &parsed
	modelsCatalogStore.mu.Unlock()
	return nil
}

func getModels() *staticModelsJSON {
	modelsCatalogStore.mu.RLock()
	defer modelsCatalogStore.mu.RUnlock()
	return modelsCatalogStore.data
}

func validateModelsCatalog(data *staticModelsJSON) error {
	if data == nil {
		return fmt.Errorf("catalog is nil")
	}

	requiredSections := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "claude", models: data.Claude},
		{name: "gemini", models: data.Gemini},
		{name: "vertex", models: data.Vertex},
		{name: "aistudio", models: data.AIStudio},
		{name: "codex-free", models: data.CodexFree},
		{name: "codex-team", models: data.CodexTeam},
		{name: "codex-plus", models: data.CodexPlus},
		{name: "codex-pro", models: data.CodexPro},
		{name: "kimi", models: data.Kimi},
		{name: "antigravity", models: data.Antigravity},
		{name: "xai", models: data.XAI},
		{name: "meta", models: data.Meta},
	}

	for _, section := range requiredSections {
		if err := validateModelSection(section.name, section.models); err != nil {
			return err
		}
	}
	return nil
}

func validateModelSection(section string, models []*ModelInfo) error {
	if len(models) == 0 {
		log.Warnf("models catalog: %s section is empty, continuing without those model definitions", section)
		return nil
	}

	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		if model == nil {
			return fmt.Errorf("%s[%d] is null", section, i)
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			return fmt.Errorf("%s[%d] has empty id", section, i)
		}
		if _, exists := seen[modelID]; exists {
			return fmt.Errorf("%s contains duplicate model id %q", section, modelID)
		}
		seen[modelID] = struct{}{}
	}
	return nil
}
