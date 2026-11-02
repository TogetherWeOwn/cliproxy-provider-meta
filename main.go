package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	displayName = "Meta"
	version     = "0.1.0"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}

// makePlugin builds the quota-only v1 plugin: a QuotaProvider for the core
// "meta" auths with no auth, model or executor capability, so stock CLIProxy
// keeps serving Meta inference itself. The executor for retiring the carried
// Meta patches lives in this same plugin in a later change.
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	client := kit.NewQuotaHTTPClient(nil)
	quota := newQuotaProvider(client)
	return kit.New(kit.Definition{
		Metadata:     pluginapi.Metadata{Name: displayName, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-meta", Logo: "https://www.google.com/s2/favicons?domain=meta.ai"},
		Configure:    configureBase,
		Capabilities: pluginapi.Capabilities{QuotaProvider: quota.cache},
		OnShutdown:   func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
