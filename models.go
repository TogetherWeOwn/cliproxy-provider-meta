package main

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file serves the static Meta Muse catalog under the meta-muse/
// namespace. Model IDs are path-based and stable across the legacy takeover:
// meta-muse/<upstream-id> maps to upstream <upstream-id>, so routing never
// needs a rename table.

// metaStaticModels is the upstream catalog (mirrors the host registry "meta"
// section on the fork branch). Thinking levels are informational only; the
// plugin never applies thinking itself because the host applies it before RPC.
var metaStaticModels = []pluginapi.ModelInfo{
	metaModel("muse-wonder-1.0", "Muse Wonder 1.0", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-1.0-contributor", "Muse Wonder 1.0 Contributor", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-full-1.0", "Muse Wonder Full 1.0", 1791763200, 1007997, 128000),
	metaModel("muse-wonder-full-1.0-contributor", "Muse Wonder Full 1.0 Contributor", 1791763200, 1007997, 128000),
	metaModel("muse-spark-1.3", "Muse Spark 1.3", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.3-contributor", "Muse Spark 1.3 Contributor", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.2", "Muse Spark 1.2", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor", 1788307200, 1048576, 65536),
	metaModel("muse-spark-1.1", "Muse Spark 1.1", 1788307200, 1048576, 65536),
}

func metaModel(id, display string, created int64, ctx, maxOut int64) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                  metaMuseNamespace + "/" + id,
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

// metaModelProvider serves static and per-auth models for meta-muse.
type metaModelProvider struct{}

func (metaModelProvider) RegisterModels(_ context.Context, _ pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{Provider: metaMuseNamespace, Models: metaStaticModels}, nil
}

func (metaModelProvider) StaticModels(_ context.Context, _ pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: metaMuseNamespace, Models: metaStaticModels}, nil
}

func (metaModelProvider) ModelsForAuth(_ context.Context, _ pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: metaMuseNamespace, Models: metaStaticModels}, nil
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
