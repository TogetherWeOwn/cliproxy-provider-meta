package main

import (
	"context"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	providerID   = "meta"
	providerName = "Meta"
)

// quotaProvider is a poll-only quota source for stock core Meta auths. It
// declares no auth, model or executor capability, so the host keeps serving
// Meta inference itself; this plugin only adds subscription quota cards.
// The cache is keyed per auth (provider, identity, credential hash) with
// single-flight polls, and errors are never cached.
type quotaProvider struct {
	cache  *providerkit.CachedQuota
	client *providerkit.QuotaHTTPClient
	// endpoint overrides the envelope URL in tests; production always uses
	// selectedEndpoint(quotaEndpoint).
	endpoint string
}

func newQuotaProvider(client *providerkit.QuotaHTTPClient) *quotaProvider {
	q := &quotaProvider{client: client}
	q.cache = providerkit.NewCachedQuotaWithTTL(providerID, providerName, func(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		token, err := extractDCAToken(req.StorageJSON, req.Metadata)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		endpoint := selectedEndpoint(quotaEndpoint)
		if q.endpoint != "" {
			endpoint = q.endpoint
		}
		return fetchMetaQuota(ctx, q.client, endpoint, token, storedAPIKey(req.Metadata))
	}, quotaCacheTTL)
	return q
}
