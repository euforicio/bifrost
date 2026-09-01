package datasheet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	bifrost "github.com/maximhq/bifrost/core"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

const (
	// DefaultModelsDevURL is the public models.dev provider catalog. It is used
	// only during the normal pricing sync; inference never depends on it.
	DefaultModelsDevURL = "https://models.dev/api.json"
	modelsDevMaxBytes   = 32 << 20
	perMillion          = 1_000_000
)

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID         string              `json:"id"`
	Family     string              `json:"family"`
	Modalities modelsDevModalities `json:"modalities"`
	Limit      modelsDevLimit      `json:"limit"`
	Cost       *modelsDevCost      `json:"cost"`
}

type modelsDevModalities struct {
	Output []string `json:"output"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

type modelsDevCost struct {
	Input      *float64            `json:"input"`
	Output     *float64            `json:"output"`
	CacheRead  *float64            `json:"cache_read"`
	CacheWrite *float64            `json:"cache_write"`
	Tiers      []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input      *float64          `json:"input"`
	Output     *float64          `json:"output"`
	CacheRead  *float64          `json:"cache_read"`
	CacheWrite *float64          `json:"cache_write"`
	Tier       modelsDevTierSpec `json:"tier"`
}

type modelsDevTierSpec struct {
	Type string `json:"type"`
	Size int    `json:"size"`
}

type modelsDevOfficialProvider struct {
	source string
	target string
}

var modelsDevOfficialProviders = []modelsDevOfficialProvider{
	{source: "openai", target: "openai"},
	{source: "anthropic", target: "anthropic"},
	{source: "google", target: "gemini"},
	{source: "xai", target: "xai"},
}

// SyncOfficialPricingFromModelsDev refreshes the official-provider token-rate
// layer used to meter account-backed providers. Rows are cached in the normal
// pricing table and merged into memory, so a models.dev outage never enters the
// inference path. The primary Bifrost datasheet remains responsible for every
// pricing field models.dev does not publish.
func (s *Store) SyncOfficialPricingFromModelsDev(ctx context.Context, rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return nil
	}
	rows, err := loadModelsDevOfficialPricing(ctx, rawURL)
	if err != nil {
		s.modelsDevMu.RLock()
		cached := append([]configstoreTables.TableModelPricing(nil), s.modelsDevRows...)
		s.modelsDevMu.RUnlock()
		if len(cached) > 0 {
			if mergeErr := s.mergeOfficialPricingRows(ctx, cached); mergeErr != nil {
				return fmt.Errorf("models.dev refresh failed (%v) and cached pricing could not be reapplied: %w", err, mergeErr)
			}
		}
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("models.dev catalog did not contain usable official-provider pricing")
	}
	s.modelsDevMu.Lock()
	s.modelsDevRows = append([]configstoreTables.TableModelPricing(nil), rows...)
	s.modelsDevMu.Unlock()
	return s.mergeOfficialPricingRows(ctx, rows)
}

func loadModelsDevOfficialPricing(ctx context.Context, rawURL string) ([]configstoreTables.TableModelPricing, error) {
	if err := bifrost.ValidateExternalURL(rawURL, true); err != nil {
		return nil, fmt.Errorf("models.dev URL validation failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models.dev request: %w", err)
	}
	resp, err := (&http.Client{Timeout: DefaultPricingTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download models.dev pricing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download models.dev pricing: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read models.dev pricing: %w", err)
	}
	if len(data) > modelsDevMaxBytes {
		return nil, fmt.Errorf("models.dev pricing response exceeds %d bytes", modelsDevMaxBytes)
	}
	return parseModelsDevOfficialPricing(data)
}

func parseModelsDevOfficialPricing(data []byte) ([]configstoreTables.TableModelPricing, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal models.dev pricing: %w", err)
	}

	rows := make([]configstoreTables.TableModelPricing, 0)
	for _, official := range modelsDevOfficialProviders {
		provider, ok := providers[official.source]
		if !ok {
			continue
		}
		for mapKey, model := range provider.Models {
			modelID := strings.TrimSpace(model.ID)
			if modelID == "" {
				modelID = strings.TrimSpace(mapKey)
			}
			if modelID == "" || !isModelsDevTextGenerationModel(model) || model.Cost == nil || !hasPositiveModelsDevTokenCost(model.Cost) {
				continue
			}
			row := modelsDevPricingRow(modelID, official.target, model)
			rows = append(rows, row, rowWithMode(row, "chat"))
		}
	}
	return rows, nil
}

func isModelsDevTextGenerationModel(model modelsDevModel) bool {
	if strings.Contains(strings.ToLower(model.Family), "embedding") {
		return false
	}
	if len(model.Modalities.Output) == 0 {
		return true
	}
	for _, modality := range model.Modalities.Output {
		if modality == "text" {
			return true
		}
	}
	return false
}

func modelsDevPricingRow(modelID, provider string, model modelsDevModel) configstoreTables.TableModelPricing {
	row := configstoreTables.TableModelPricing{
		Model:    modelID,
		Provider: provider,
		Mode:     "responses",
	}
	if model.Limit.Context > 0 {
		row.ContextLength = intPtr(model.Limit.Context)
	}
	if model.Limit.Input > 0 {
		row.MaxInputTokens = intPtr(model.Limit.Input)
	}
	if model.Limit.Output > 0 {
		row.MaxOutputTokens = intPtr(model.Limit.Output)
	}
	row.InputCostPerToken = perToken(model.Cost.Input)
	row.OutputCostPerToken = perToken(model.Cost.Output)
	row.CacheReadInputTokenCost = perToken(model.Cost.CacheRead)
	row.CacheCreationInputTokenCost = perToken(model.Cost.CacheWrite)
	for _, tier := range model.Cost.Tiers {
		if tier.Tier.Type != "context" {
			continue
		}
		switch tier.Tier.Size {
		case TokenTierAbove128K:
			row.InputCostPerTokenAbove128kTokens = perToken(tier.Input)
			row.OutputCostPerTokenAbove128kTokens = perToken(tier.Output)
		case TokenTierAbove200K:
			row.InputCostPerTokenAbove200kTokens = perToken(tier.Input)
			row.OutputCostPerTokenAbove200kTokens = perToken(tier.Output)
			row.CacheReadInputTokenCostAbove200kTokens = perToken(tier.CacheRead)
			row.CacheCreationInputTokenCostAbove200kTokens = perToken(tier.CacheWrite)
		case TokenTierAbove272K:
			row.InputCostPerTokenAbove272kTokens = perToken(tier.Input)
			row.OutputCostPerTokenAbove272kTokens = perToken(tier.Output)
			row.CacheReadInputTokenCostAbove272kTokens = perToken(tier.CacheRead)
			row.CacheCreationInputTokenCostAbove272kTokens = perToken(tier.CacheWrite)
		}
	}
	return row
}

func rowWithMode(row configstoreTables.TableModelPricing, mode string) configstoreTables.TableModelPricing {
	row.Mode = mode
	return row
}

func perToken(value *float64) *float64 {
	if value == nil || *value <= 0 {
		return nil
	}
	converted := *value / perMillion
	return &converted
}

func intPtr(value int) *int { return &value }

func hasPositiveModelsDevTokenCost(cost *modelsDevCost) bool {
	return cost != nil && ((cost.Input != nil && *cost.Input > 0) || (cost.Output != nil && *cost.Output > 0))
}

func mergeModelsDevRow(existing, incoming configstoreTables.TableModelPricing) configstoreTables.TableModelPricing {
	if existing.Model == "" {
		existing.Model = incoming.Model
		existing.Provider = incoming.Provider
		existing.Mode = incoming.Mode
	}
	if incoming.ContextLength != nil {
		existing.ContextLength = incoming.ContextLength
	}
	if incoming.MaxInputTokens != nil {
		existing.MaxInputTokens = incoming.MaxInputTokens
	}
	if incoming.MaxOutputTokens != nil {
		existing.MaxOutputTokens = incoming.MaxOutputTokens
	}
	if incoming.InputCostPerToken != nil {
		existing.InputCostPerToken = incoming.InputCostPerToken
	}
	if incoming.OutputCostPerToken != nil {
		existing.OutputCostPerToken = incoming.OutputCostPerToken
	}
	if incoming.CacheReadInputTokenCost != nil {
		existing.CacheReadInputTokenCost = incoming.CacheReadInputTokenCost
	}
	if incoming.CacheCreationInputTokenCost != nil {
		existing.CacheCreationInputTokenCost = incoming.CacheCreationInputTokenCost
	}
	if incoming.InputCostPerTokenAbove128kTokens != nil {
		existing.InputCostPerTokenAbove128kTokens = incoming.InputCostPerTokenAbove128kTokens
	}
	if incoming.OutputCostPerTokenAbove128kTokens != nil {
		existing.OutputCostPerTokenAbove128kTokens = incoming.OutputCostPerTokenAbove128kTokens
	}
	if incoming.InputCostPerTokenAbove200kTokens != nil {
		existing.InputCostPerTokenAbove200kTokens = incoming.InputCostPerTokenAbove200kTokens
	}
	if incoming.OutputCostPerTokenAbove200kTokens != nil {
		existing.OutputCostPerTokenAbove200kTokens = incoming.OutputCostPerTokenAbove200kTokens
	}
	if incoming.CacheReadInputTokenCostAbove200kTokens != nil {
		existing.CacheReadInputTokenCostAbove200kTokens = incoming.CacheReadInputTokenCostAbove200kTokens
	}
	if incoming.CacheCreationInputTokenCostAbove200kTokens != nil {
		existing.CacheCreationInputTokenCostAbove200kTokens = incoming.CacheCreationInputTokenCostAbove200kTokens
	}
	if incoming.InputCostPerTokenAbove272kTokens != nil {
		existing.InputCostPerTokenAbove272kTokens = incoming.InputCostPerTokenAbove272kTokens
	}
	if incoming.OutputCostPerTokenAbove272kTokens != nil {
		existing.OutputCostPerTokenAbove272kTokens = incoming.OutputCostPerTokenAbove272kTokens
	}
	if incoming.CacheReadInputTokenCostAbove272kTokens != nil {
		existing.CacheReadInputTokenCostAbove272kTokens = incoming.CacheReadInputTokenCostAbove272kTokens
	}
	if incoming.CacheCreationInputTokenCostAbove272kTokens != nil {
		existing.CacheCreationInputTokenCostAbove272kTokens = incoming.CacheCreationInputTokenCostAbove272kTokens
	}
	return existing
}

func (s *Store) mergeOfficialPricingRows(ctx context.Context, rows []configstoreTables.TableModelPricing) error {
	if s.configStore != nil {
		existingRows, err := s.configStore.GetModelPrices(ctx)
		if err != nil {
			return fmt.Errorf("failed to load cached pricing before models.dev merge: %w", err)
		}
		existing := make(map[string]configstoreTables.TableModelPricing, len(existingRows))
		for _, row := range existingRows {
			existing[makeKey(row.Model, row.Provider, row.Mode)] = row
		}
		merged := make([]configstoreTables.TableModelPricing, 0, len(rows))
		for _, row := range rows {
			key := makeKey(row.Model, row.Provider, row.Mode)
			merged = append(merged, mergeModelsDevRow(existing[key], row))
		}
		if err := s.configStore.UpsertModelPricesBatch(ctx, merged); err != nil {
			return fmt.Errorf("failed to cache models.dev pricing: %w", err)
		}
		return s.LoadFromDB(ctx)
	}

	s.mu.Lock()
	for _, row := range rows {
		key := makeKey(row.Model, row.Provider, row.Mode)
		s.pricingData[key] = mergeModelsDevRow(s.pricingData[key], row)
	}
	s.rebuildDatasheetViewUnsafe()
	s.mu.Unlock()
	return nil
}
