package registry

import (
	"context"
	"encoding/json"
	"fmt"

	log "github.com/sirupsen/logrus"
)

const maxCodexClientModelsSize = 8 << 20

var codexClientModelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json",
	"https://models.router-for.me/codex_client_models.json",
}

// StartCodexClientModelsUpdater starts a background updater that fetches the
// Codex client model catalog immediately and then refreshes it every 3 hours.
// Safe to call multiple times; only one updater will run.
func StartCodexClientModelsUpdater(ctx context.Context) {
	codexCatalogUpdater.configure(ctx, "")
}

func tryRefreshCodexClientModels(ctx context.Context, label string) {
	data, sourceURL := fetchCodexClientModelsFromRemote(ctx)
	if data == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data", label)
		return
	}

	changed, err := loadCodexClientModelsFromBytes(data, sourceURL)
	if err != nil {
		log.Warnf("%s: fetched catalog rejected, keeping current data: %v", label, err)
		return
	}
	if !changed {
		log.Infof("%s completed from %s, no changes detected", label, sourceURL)
		return
	}
	log.Infof("%s completed from %s, catalog updated", label, sourceURL)
}

func fetchCodexClientModelsFromRemote(ctx context.Context) ([]byte, string) {
	for _, source := range codexClientModelsURLs {
		data, errRead := readCatalogSource(ctx, source)
		if errRead == nil && ValidateCodexClientModelsJSON(data) == nil {
			return data, source
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, ""
}

// mergePinnedEmbeddedCodexClientModels appends pinned embedded client models
// that the remote catalog lacks. Remote definitions win once they exist.
func mergePinnedEmbeddedCodexClientModels(remote, embedded []byte, pinned map[string]struct{}) ([]byte, error) {
	if len(pinned) == 0 {
		return remote, nil
	}
	var remotePayload, embeddedPayload codexClientModelsPayload
	if err := json.Unmarshal(remote, &remotePayload); err != nil {
		return nil, fmt.Errorf("decode remote catalog: %w", err)
	}
	if err := json.Unmarshal(embedded, &embeddedPayload); err != nil {
		return nil, fmt.Errorf("decode embedded catalog: %w", err)
	}
	present := make(map[string]struct{}, len(remotePayload.Models))
	for _, model := range remotePayload.Models {
		if slug, ok := model["slug"].(string); ok {
			present[slug] = struct{}{}
		}
	}
	added := false
	for _, model := range embeddedPayload.Models {
		slug, _ := model["slug"].(string)
		if _, keep := pinned[slug]; !keep {
			continue
		}
		if _, exists := present[slug]; exists {
			continue
		}
		remotePayload.Models = append(remotePayload.Models, model)
		present[slug] = struct{}{}
		added = true
	}
	if !added {
		return remote, nil
	}
	merged, err := json.Marshal(remotePayload)
	if err != nil {
		return nil, fmt.Errorf("encode merged catalog: %w", err)
	}
	if err := ValidateCodexClientModelsJSON(merged); err != nil {
		return nil, err
	}
	return merged, nil
}
