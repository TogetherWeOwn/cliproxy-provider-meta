//go:build !hostcompat

package main

import (
	"context"
	"sync/atomic"
)

// pluginCfgHolder carries the latest plugin configuration subtree to the
// executor, which reads it per request.
var pluginCfgHolder atomic.Value

func pluginConfigNow() pluginConfig {
	cfg, _ := pluginCfgHolder.Load().(pluginConfig)
	return cfg
}

func selectedEndpoint(endpoint string) string { return endpoint }

func configureBase(_ context.Context, raw []byte) error {
	pluginCfgHolder.Store(parsePluginConfig(raw))
	return nil
}
