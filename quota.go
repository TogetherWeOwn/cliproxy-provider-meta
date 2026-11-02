package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaEndpoint is the Meta key-envelope read. It returns the stored key
// envelope (including subscription usage) without minting or changing
// anything: the DCA token travels in the Authorization header only, and the
// POST body is an empty object.
const quotaEndpoint = "https://api.meta.ai/muse-code/key"

// quotaCacheTTL bounds how long one successful per-auth poll is reused.
// Envelope reads are expensive and rate-limited, and the connector polls
// every 5 minutes, so steady state stays far inside the provider's budget.
const quotaCacheTTL = 30 * time.Minute

// dcaTokenPattern matches Meta device-code tokens: a "dca:" prefix with no
// whitespace. The length cap keeps absurd inputs out of header construction.
var dcaTokenPattern = regexp.MustCompile(`^dca:\S+$`)

const maxDCATokenLength = 4096

// metaUsageWindow is one subscription usage window from the key envelope.
// Only the consumption signal is decoded; every other envelope field
// (api_key, email, name, payment state) is ignored by the parser.
type metaUsageWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	ResetsAt    any      `json:"resets_at"`
}

// metaKeyEnvelope decodes the minimum needed for a quota card: the two
// subscription usage windows and the tier. api_key is decoded only to compare
// against the stored key for the "rotated" warning; it is never retained in
// the normalized output, and email/name are not decoded at all.
type metaKeyEnvelope struct {
	APIKey       string `json:"api_key"`
	SubsTierName string `json:"subs_tier_name"`
	SubsUsage    *struct {
		Tier   string           `json:"tier"`
		Window *metaUsageWindow `json:"window"`
		Weekly *metaUsageWindow `json:"weekly"`
	} `json:"subs_usage"`
}

var errNoQuotaSignal = errors.New("quota payload has no usable subscription window")

// extractDCAToken reads the DCA token from the merged auth fields
// (host metadata over object storage) and verifies it belongs to a Meta
// credential. Errors are generic: the token value is never included.
func extractDCAToken(storage []byte, metadata map[string]any) (string, error) {
	fields, err := providerkit.AuthFields(storage, metadata)
	if err != nil {
		return "", err
	}
	if typ, _ := fields["type"].(string); !isMetaAuthType(typ) {
		return "", fmt.Errorf("quota requires a Meta credential")
	}
	raw, _ := fields["dca_token"].(string)
	if len(raw) > maxDCATokenLength || !dcaTokenPattern.MatchString(raw) {
		return "", fmt.Errorf("quota requires a Meta credential")
	}
	return raw, nil
}

// storedAPIKey returns the minted key the host currently stores for the auth,
// or "" when it is absent. Only presence and equality are ever used: the
// value is compared against the envelope's key for the "rotated" warning and
// never logged, persisted or returned.
func storedAPIKey(metadata map[string]any) string {
	key, _ := metadata["api_key"].(string)
	return key
}

// resetTime converts an envelope unix timestamp to RFC 3339, or "" when the
// value is missing or unusable (the panel then shows "-").
func resetTime(raw any) string {
	var unix int64
	switch v := raw.(type) {
	case float64:
		unix = int64(v)
	case int64:
		unix = v
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return ""
		}
		unix = n
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return ""
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			unix = n
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		} else {
			return ""
		}
	default:
		return ""
	}
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// windowBucket converts one usage window to a card bucket row. used_percent
// is capped at 100 before conversion; ok=false when the window is absent or
// carries no usable consumption signal.
func windowBucket(window string, w *metaUsageWindow) (pluginapi.QuotaBucket, bool) {
	if w == nil || w.UsedPercent == nil {
		return pluginapi.QuotaBucket{}, false
	}
	used := *w.UsedPercent
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return providerkit.NewQuotaBucket(window, 1-used/100, resetTime(w.ResetsAt)), true
}

// normalizeEnvelope converts the decoded envelope to a quota card. Buckets
// with no usable signal are dropped; when neither window is usable the card
// carries the tier only (cached quota-blind, never fabricated).
func normalizeEnvelope(env metaKeyEnvelope, storedKey string) pluginapi.QuotaFetchResponse {
	var out pluginapi.QuotaFetchResponse
	tier := strings.TrimSpace(env.SubsTierName)
	if env.SubsUsage != nil && strings.TrimSpace(env.SubsUsage.Tier) != "" && tier == "" {
		tier = strings.TrimSpace(env.SubsUsage.Tier)
	}
	// Plan is always Meta: this provider only serves Meta credentials, and
	// the tier chip drops (empty TierName) when the envelope names none.
	out.Subscription = &pluginapi.QuotaSubscription{Plan: "Meta", TierName: tier}
	var buckets []pluginapi.QuotaBucket
	if env.SubsUsage != nil {
		if b, ok := windowBucket("5h", env.SubsUsage.Window); ok {
			buckets = append(buckets, b)
		}
		if b, ok := windowBucket("weekly", env.SubsUsage.Weekly); ok {
			buckets = append(buckets, b)
		}
	}
	if len(buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{{DisplayName: "Meta", Buckets: buckets}}
	}
	// The envelope key differing from the stored key means the account was
	// reminted outside this host: quota still reads (the DCA token is what
	// authenticates the poll), but the card warns instead of silently
	// disagreeing with inference. Only the boolean is reported, never a key.
	if storedKey != "" && env.APIKey != "" && storedKey != env.APIKey {
		out.Summary = []pluginapi.QuotaMetric{{Key: "rotated", Label: "Minted key rotated", Value: 1, Format: "number"}}
	}
	return out
}

// fetchMetaQuota performs one bounded POST envelope read and normalizes it.
// The DCA token is sent in the Authorization header only; the response body
// envelope is parsed for usage windows and then dropped. Neither the token
// nor the raw body ever reaches an error or the normalized output.
func fetchMetaQuota(ctx context.Context, client *providerkit.QuotaHTTPClient, endpoint, dcaToken, storedKey string) (pluginapi.QuotaFetchResponse, error) {
	if client == nil || endpoint == "" || dcaToken == "" {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota poll not configured")
	}
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    endpoint,
		Headers: http.Header{
			"Accept":        {"application/json"},
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + dcaToken},
			"X-Api-Version": {"1.0.0"},
		},
		Body: []byte(`{}`),
	})
	if err != nil {
		if ctx.Err() != nil {
			return pluginapi.QuotaFetchResponse{}, ctx.Err()
		}
		return pluginapi.QuotaFetchResponse{}, classifyPollError(err)
	}
	var env metaKeyEnvelope
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	if err := decoder.Decode(&env); err != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode quota payload")
	}
	out := normalizeEnvelope(env, storedKey)
	if len(out.Groups) == 0 && tierMissing(out) {
		return pluginapi.QuotaFetchResponse{}, errNoQuotaSignal
	}
	return out, nil
}

func tierMissing(out pluginapi.QuotaFetchResponse) bool {
	return out.Subscription == nil || strings.TrimSpace(out.Subscription.TierName) == ""
}
