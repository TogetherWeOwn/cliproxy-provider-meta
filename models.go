package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file serves the Meta Muse catalog under the meta-muse/ namespace
// through the kit ModelCatalog (cliproxy-provider-kit v0.6.1). The discovery
// source is the host's public models.json "meta" section: a new Meta model
// becomes servable when the host catalog gains it and the host's one-shot
// catalog-change callback re-invokes ModelsForAuth. That path covers the
// fresh-success timeline only; a failed callback fetch keeps serving
// last-good/static until the next host event (next catalog "meta" section
// change, auth upsert, config reload, or plugin re-registration) — an
// unchanged catalog poll is not a re-trigger, and v0.6.1 performs a single
// fetch per ModelsForAuth invocation (no bounded retry). Recovery is not
// guaranteed; see the Timeline-B fixture in models_catalog_test.go.

// Model IDs are path-based and stable across the legacy takeover:
// meta-muse/<upstream-id> maps to upstream <upstream-id>, so routing never
// needs a rename table.

// metaUpstreamModels is the static fallback as upstream (pre-namespace) ids,
// mirroring the host registry "meta" section plus the later-added Muse
// Ember entry. Thinking levels are informational only; the plugin never
// applies thinking itself because the host applies it before RPC. The kit
// prefixes every id with the meta-muse/ namespace before serving, so these
// stay bare.
var metaUpstreamModels = []pluginapi.ModelInfo{
	metaModel("muse-wonder-1.0", "Muse Wonder 1.0", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-1.0-contributor", "Muse Wonder 1.0 Contributor", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-full-1.0", "Muse Wonder Full 1.0", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-full-1.0-contributor", "Muse Wonder Full 1.0 Contributor", 1791763200, 1007997, 128000),
	metaModel("muse-spark-1.3", "Muse Spark 1.3", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.3-contributor", "Muse Spark 1.3 Contributor", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.2", "Muse Spark 1.2", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.1", "Muse Spark 1.1", 1788307200, 1048576, 65536),
	// Muse Ember has no Contributor-mode variant (https://dev.meta.ai/docs/muse-ember).
	metaModel("muse-ember-1.0", "Muse Ember 1.0", 1794182400, 1048576, 524288),
}

func metaModel(id, display string, created int64, ctx, maxOut int64) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                  id,
		Object:              "model",
		Created:             created,
		OwnedBy:             "meta",
		Type:                "meta",
		DisplayName:         display,
		Name:                id,
		ContextLength:       ctx,
		MaxCompletionTokens: maxOut,
		Thinking: &pluginapi.ThinkingSupport{
			Levels: []string{"minimal", "low", "medium", "high", "xhigh", "max"},
		},
	}
}

// metaModelsCatalogURLs are the host public catalog fetch targets, in the
// order the host tries them (internal/registry/model_updater.go in
// CLIProxyAPI v8.0.63).
const (
	metaModelsCatalogPrimary  = "https://models.router-for.me/models.json"
	metaModelsCatalogFallback = "https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json"
)

// newMetaModelCatalog wires the plugin's discovery catalog: static fallback
// (above), fresh fetch from the host models.json "meta" section, namespace
// meta-muse. The plugin has no allow/deny globs today, so the kit default
// (no allow = allow all, no deny) applies. The catalog implements both
// ModelProvider and ModelRegistrar.
func newMetaModelCatalog() *metaModelCatalog {
	return &metaModelCatalog{ModelCatalog: providerkit.NewModelCatalog(providerkit.ModelCatalogConfig{
		Namespace: metaMuseNamespace,
		Static:    metaUpstreamModels,
		Fetch:     fetchMetaModels,
	})}
}

// metaModelCatalog wraps the kit ModelCatalog so every model response is
// attributed to the plugin's credential provider (meta-muse), matching the
// pre-migration provider. The kit catalog otherwise echoes req.Plugin.Name,
// which for this plugin is the display name ("Meta"), not the credential
// provider; static and registration responses must not be recorded under it.
type metaModelCatalog struct {
	*providerkit.ModelCatalog
}

func (c *metaModelCatalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.ModelCatalog.StaticModels(ctx, req)
	resp.Provider = metaMuseNamespace
	return resp, err
}

func (c *metaModelCatalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	resp, err := c.ModelCatalog.RegisterModels(ctx, req)
	resp.Provider = metaMuseNamespace
	return resp, err
}

func (c *metaModelCatalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := c.ModelCatalog.ModelsForAuth(ctx, req)
	resp.Provider = metaMuseNamespace
	return resp, err
}

var (
	_ pluginapi.ModelProvider  = (*metaModelCatalog)(nil)
	_ pluginapi.ModelRegistrar = (*metaModelCatalog)(nil)
)

// metaCatalogEntry is one models.json "meta" section entry. Only the fields
// the static list carries are parsed; everything else is ignored.
type metaCatalogEntry struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created"`
	OwnedBy             string `json:"owned_by"`
	Type                string `json:"type"`
	DisplayName         string `json:"display_name"`
	Name                string `json:"name"`
	ContextLength       int64  `json:"context_length"`
	MaxCompletionTokens int64  `json:"max_completion_tokens"`
	Thinking            *struct {
		Levels []string `json:"levels"`
	} `json:"thinking"`
	SupportedInputModalities  []string `json:"supportedInputModalities"`
	SupportedOutputModalities []string `json:"supportedOutputModalities"`
}

// fetchMetaModels returns the "meta" section candidate models as upstream
// (pre-namespace) ids for one auth. It is a custom fetch (not
// OpenAIModelsFetcher) because the payload is the host models.json section
// object, not an OpenAI-style id list. No credential is ever sent: the
// catalog is public and the fetch carries only an Accept header. A buffered
// body larger than providerkit.MaxModelsBody is a failed fetch (post-return
// parse cap) and is never parsed; the catalog falls back to last-good or
// the static list per the uniform pipeline.
func fetchMetaModels(ctx context.Context, req pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	if req.HTTPClient == nil {
		return nil, errors.New("model discovery client is unavailable")
	}
	var lastErr error
	for _, url := range []string{metaModelsCatalogPrimary, metaModelsCatalogFallback} {
		models, err := fetchMetaModelsFromURL(ctx, req.HTTPClient, url)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func fetchMetaModelsFromURL(ctx context.Context, client pluginapi.HostHTTPClient, url string) ([]pluginapi.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     url,
		Headers: http.Header{"Accept": {"application/json"}},
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("model discovery fetch failed")
	}
	if len(resp.Body) > providerkit.MaxModelsBody {
		return nil, errors.New("model discovery response exceeds the parse cap")
	}
	var wire struct {
		Meta []metaCatalogEntry `json:"meta"`
	}
	if err := json.Unmarshal(resp.Body, &wire); err != nil {
		return nil, errors.New("model discovery response parse failed")
	}
	out := make([]pluginapi.ModelInfo, 0, len(wire.Meta))
	for _, e := range wire.Meta {
		if e.ID == "" {
			continue
		}
		m := pluginapi.ModelInfo{
			ID:                        e.ID,
			Object:                    e.Object,
			Created:                   e.Created,
			OwnedBy:                   e.OwnedBy,
			Type:                      e.Type,
			DisplayName:               e.DisplayName,
			Name:                      e.Name,
			ContextLength:             e.ContextLength,
			MaxCompletionTokens:       e.MaxCompletionTokens,
			SupportedInputModalities:  e.SupportedInputModalities,
			SupportedOutputModalities: e.SupportedOutputModalities,
		}
		if e.Thinking != nil {
			m.Thinking = &pluginapi.ThinkingSupport{Levels: e.Thinking.Levels}
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("model discovery response has no meta models")
	}
	return out, nil
}

// stripMetaMuseModelID maps a host-supplied model (possibly namespaced) to the
// upstream Muse model name.
func stripMetaMuseModelID(model string) string {
	model = strings.TrimSpace(model)
	if rest, ok := strings.CutPrefix(model, metaMuseNamespace+"/"); ok && rest != "" {
		return rest
	}
	return model
}
