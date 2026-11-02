package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	providerID   = "meta"
	providerName = "Meta"
)

// isMetaAuthType reports whether a storage type is a Meta credential: the
// legacy core key this plugin takes over or this plugin's own key.
func isMetaAuthType(typ string) bool {
	return typ == providerID || typ == metaMuseNamespace
}

// quotaProvider serves subscription quota for both the legacy "meta" key and
// this plugin's "meta-muse" key. Polls are per-auth envelope reads with
// single-flight and a 30-minute TTL; executor observations stored via observe()
// take precedence over polls for the same auth identity. Errors are never cached.
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

// Identifier implements pluginapi.QuotaProvider.
func (q *quotaProvider) Identifier() string { return providerID }

// DescribeQuota advertises both routed keys: legacy "meta" polls keep working
// and taken-over "meta-muse" auths resolve to this same cache.
func (q *quotaProvider) DescribeQuota(ctx context.Context, req pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	desc, err := q.cache.DescribeQuota(ctx, req)
	if err != nil {
		return desc, err
	}
	desc.SupportedProviders = []string{providerID, metaMuseNamespace}
	return desc, nil
}

// FetchQuota normalizes the taken-over key to the cache key before delegating,
// so polls for either provider share one per-auth entry.
func (q *quotaProvider) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider == metaMuseNamespace {
		req.Provider = providerID
	}
	return q.cache.FetchQuota(ctx, req)
}

// ResetQuota implements pluginapi.QuotaProvider: observations are poll-derived
// and not resettable.
func (q *quotaProvider) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return q.cache.ResetQuota(ctx, req)
}

// observe stores one passive subscription-usage snapshot keyed by auth
// identity. Fresh observations take precedence over polls, including older
// in-flight polls, without any extra envelope read.
func (q *quotaProvider) observe(authID string, headers http.Header) {
	authID = strings.TrimSpace(authID)
	if q == nil || q.cache == nil || authID == "" || len(headers) == 0 {
		return
	}
	card, ok := subscriptionHeadersToQuota(headers)
	if !ok {
		return
	}
	_ = q.cache.Store(pluginapi.QuotaFetchRequest{AuthID: authID}, card)
}

// subscriptionHeadersToQuota converts one parsed subscription-usage header set
// to a quota card. Buckets with no usable signal are dropped; with neither
// window usable there is no card to store.
func subscriptionHeadersToQuota(headers http.Header) (pluginapi.QuotaFetchResponse, bool) {
	var out pluginapi.QuotaFetchResponse
	tier := strings.TrimSpace(headers.Get(metaQuotaTierHeader))
	out.Subscription = &pluginapi.QuotaSubscription{Plan: "Meta", TierName: tier}
	var buckets []pluginapi.QuotaBucket
	if b, ok := subscriptionHeaderBucket(headers.Get(metaQuotaWindowUsedHeader), headers.Get(metaQuotaWindowResetHeader), "5h"); ok {
		buckets = append(buckets, b)
	}
	if b, ok := subscriptionHeaderBucket(headers.Get(metaQuotaWeeklyUsedHeader), headers.Get(metaQuotaWeeklyResetHeader), "weekly"); ok {
		buckets = append(buckets, b)
	}
	if len(buckets) == 0 {
		return pluginapi.QuotaFetchResponse{}, false
	}
	out.Groups = []pluginapi.QuotaGroup{{DisplayName: "Meta", Buckets: buckets}}
	return out, true
}

func subscriptionHeaderBucket(usedRaw, resetRaw, window string) (pluginapi.QuotaBucket, bool) {
	usedRaw = strings.TrimSpace(usedRaw)
	if usedRaw == "" {
		return pluginapi.QuotaBucket{}, false
	}
	used, err := strconv.ParseFloat(usedRaw, 64)
	if err != nil || used < 0 {
		return pluginapi.QuotaBucket{}, false
	}
	if used > 100 {
		used = 100
	}
	return providerkit.NewQuotaBucket(window, 1-used/100, subscriptionResetTime(resetRaw)), true
}

// subscriptionResetTime converts unix-seconds reset headers to RFC 3339.
func subscriptionResetTime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return resetTime(float64(n))
	}
	return resetTime(raw)
}
