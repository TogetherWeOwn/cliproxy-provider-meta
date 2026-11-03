package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	displayName = "Meta"
	version     = "0.2.0"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}

// makePlugin builds the Meta Muse provider plugin: auth ownership for the
// legacy "meta" key and the "meta-muse" key, static and per-auth models under
// the meta-muse/ namespace, the Meta executor carrying the four fork patches,
// and the converging subscription quota provider.
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	client := kit.NewQuotaHTTPClient(nil)
	quota := newQuotaProvider(client)
	auth := &metaAuthProvider{quota: quota}
	models := metaModelProvider{}
	executor := newMetaExecutor(quota, auth)
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: displayName, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-meta", Logo: "https://www.google.com/s2/favicons?domain=meta.ai"},
		ModelNamespace: metaMuseNamespace,
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider:          auth,
			ModelRegistrar:        models,
			ModelProvider:         models,
			Executor:              executor,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"openai-response", "claude", "gemini", "openai"},
			ExecutorOutputFormats: []string{"openai-response", "claude", "gemini", "openai"},
			QuotaProvider:         quota,
		},
		OnShutdown: func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
