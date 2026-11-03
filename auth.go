package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file ports Meta auth ownership from stock CLIProxy
// (internal/auth/meta/meta.go) into the plugin: ParseAuth first-crack claim of
// both "meta" and "meta-muse" (legacy takeover without file renames),
// single identifier "meta-muse" with credential-provider claim ["meta"] for
// SameAuthProvider bridging, credential resolution with DCA/API-key
// precedence, device-code login via stock StartLogin/PollLogin, DCA mint on
// refresh, and the readable credential filename helper. Reversal is a
// persisted `type` string flip back to "meta".

const (
	metaDefaultAPIBaseURL = "https://api.meta.ai/v1"
	metaAuthHost          = "https://auth.meta.com"
	metaDeviceEndpoint    = metaAuthHost + "/oidc/device/authorization/"
	metaTokenEndpoint     = metaAuthHost + "/oidc/device/token/"
	metaMintEndpoint      = "https://api.meta.ai/muse-code/key"
	metaOAuthClientID     = "1031625952748946"
)

// metaAuthProvider owns Meta credentials for the plugin's "meta-muse" key and
// the legacy "meta" key it takes over.
type metaAuthProvider struct {
	quota *quotaProvider
}

// Identifier is the plugin's single auth identifier. The kit exposes one auth
// identifier per plugin and rejects AuthProviders that implement
// pluginapi.MultiAuthProvider, so the legacy "meta" key is claimed through
// CredentialProviders below instead of a second identifier: the v8 host routes
// ParseAuth/RefreshAuth for "meta" files to this plugin via
// recordOwnsCredentialProvider and bridges the keys via SameAuthProvider.
func (p *metaAuthProvider) Identifier() string { return metaMuseNamespace }

// CredentialProviders implements the kit credential-provider source extension:
// legacy "meta" auths route to this plugin for management and recovery.
func (p *metaAuthProvider) CredentialProviders() []string { return []string{providerID} }

// metaStoredFields reads merged auth fields (metadata over storage) for one credential.
func metaStoredFields(storage []byte, metadata map[string]any) (map[string]any, error) {
	return providerkit.AuthFields(storage, metadata)
}

// metaCreds resolves baseURL and usable API token with native precedence:
// attributes, then metadata, then storage; DCA tokens never count as API keys.
// An explicitly configured attribute base_url always wins, even when it equals
// the default: attribute presence (not value comparison) gates the storage
// fallback so an operator-deselected endpoint cannot be reintroduced.
func metaCreds(storage []byte, metadata map[string]any, attributes map[string]string) (baseURL, token string) {
	baseURL = metaDefaultAPIBaseURL
	attrBaseURLSet := false
	if attributes != nil {
		if b := strings.TrimSpace(attributes["base_url"]); b != "" {
			baseURL = b
			attrBaseURLSet = true
		}
		if k := strings.TrimSpace(attributes["api_key"]); k != "" && !strings.HasPrefix(k, "dca:") {
			token = k
		} else if t := strings.TrimSpace(attributes["access_token"]); t != "" && !strings.HasPrefix(t, "dca:") {
			token = t
		}
	}
	fields, err := metaStoredFields(storage, metadata)
	if err == nil {
		if !attrBaseURLSet {
			if b, _ := fields["base_url"].(string); strings.TrimSpace(b) != "" {
				baseURL = strings.TrimSpace(b)
			} else if b, _ := fields["api_base_url"].(string); strings.TrimSpace(b) != "" {
				baseURL = strings.TrimSpace(b)
			}
		}
		if token == "" {
			if k, _ := fields["api_key"].(string); strings.TrimSpace(k) != "" && !strings.HasPrefix(strings.TrimSpace(k), "dca:") {
				token = strings.TrimSpace(k)
			} else if t, _ := fields["access_token"].(string); strings.TrimSpace(t) != "" && !strings.HasPrefix(strings.TrimSpace(t), "dca:") {
				token = strings.TrimSpace(t)
			}
		}
	}
	return baseURL, token
}

// metaDCAToken resolves the device token for mint/refresh, or "" when absent.
func metaDCAToken(storage []byte, metadata map[string]any, attributes map[string]string) string {
	if attributes != nil {
		if d := strings.TrimSpace(attributes["dca_token"]); d != "" {
			return d
		}
		if t := strings.TrimSpace(attributes["access_token"]); strings.HasPrefix(t, "dca:") {
			return t
		}
	}
	fields, err := metaStoredFields(storage, metadata)
	if err != nil {
		return ""
	}
	if d, _ := fields["dca_token"].(string); strings.TrimSpace(d) != "" {
		return strings.TrimSpace(d)
	}
	if t, _ := fields["access_token"].(string); strings.HasPrefix(strings.TrimSpace(t), "dca:") {
		return strings.TrimSpace(t)
	}
	return ""
}

// ParseAuth claims auth files of type "meta-muse" (this plugin) or "meta"
// (legacy core takeover). The plugin leaves ID empty so the host derives the
// same path-relative ID core uses (pluginAuthDataToCoreAuth falls back to
// authIDForPath): filenames, tokens and account keys stay stable across the
// cutover, rollback, and restarts, and only the persisted type flips, which
// makes reversal a type-string flip.
func (p *metaAuthProvider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var fields map[string]any
	if err := json.Unmarshal(req.RawJSON, &fields); err != nil || fields == nil {
		return pluginapi.AuthParseResponse{}, nil
	}
	kind, _ := fields["type"].(string)
	if kind != metaMuseNamespace && kind != providerID {
		return pluginapi.AuthParseResponse{}, nil
	}
	if req.Provider != "" && req.Provider != metaMuseNamespace && req.Provider != providerID {
		return pluginapi.AuthParseResponse{}, nil
	}
	text := func(name string) string { v, _ := fields[name].(string); return strings.TrimSpace(v) }
	auth := pluginapi.AuthData{
		Provider: metaMuseNamespace,
		FileName: req.FileName,
		Label:    text("label"),
		Prefix:   text("prefix"),
		ProxyURL: text("proxy_url"),
		Metadata: map[string]any{"type": metaMuseNamespace},
	}
	if disabled, _ := fields["disabled"].(bool); disabled {
		auth.Disabled = true
		auth.Metadata["disabled"] = true
	}
	for _, name := range []string{"weight", "priority"} {
		if v, ok := fields[name]; ok {
			auth.Metadata[name] = v
			if s, ok := v.(string); ok {
				if auth.Attributes == nil {
					auth.Attributes = map[string]string{}
				}
				auth.Attributes[name] = s
			} else if f, ok := v.(float64); ok {
				if auth.Attributes == nil {
					auth.Attributes = map[string]string{}
				}
				auth.Attributes[name] = strings.TrimSpace(fmt.Sprintf("%v", f))
			}
		}
	}
	keep := map[string]any{}
	for k, v := range fields {
		switch k {
		case "id", "label", "prefix", "proxy_url", "disabled", "weight", "priority":
		default:
			keep[k] = v
		}
	}
	keep["type"] = metaMuseNamespace
	raw, err := json.Marshal(keep)
	if err != nil {
		return pluginapi.AuthParseResponse{}, providerkit.MapError(fmt.Errorf("cannot encode auth storage: %w", err))
	}
	auth.StorageJSON = raw
	for k, v := range keep {
		auth.Metadata[k] = v
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth}, nil
}

// metaLoginState is the opaque StartLogin -> PollLogin state.
type metaLoginState struct {
	DeviceCode          string `json:"device_code"`
	VerificationURI     string `json:"verification_uri_complete"`
	TokenEndpoint       string `json:"token_endpoint"`
	IntervalSeconds     int    `json:"interval"`
	ExpiresAtUnix       int64  `json:"expires_at"`
	VerificationDisplay string `json:"verification_display"`
}

func encodeMetaLoginState(s metaLoginState) string {
	raw, _ := json.Marshal(s)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeMetaLoginState(state string) (metaLoginState, error) {
	var s metaLoginState
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(state))
	if err != nil {
		return s, fmt.Errorf("invalid login state")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("invalid login state")
	}
	if s.DeviceCode == "" {
		return s, fmt.Errorf("invalid login state")
	}
	// The state round-trips through the caller unsigned, so a tampered
	// token_endpoint must never be honored: polling always uses the hardcoded
	// Meta endpoint. Overwrite here and use the constant at the poll site.
	s.TokenEndpoint = metaTokenEndpoint
	return s, nil
}

// StartLogin begins the RFC 8628 device flow via the host HTTP client.
func (p *metaAuthProvider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.AuthLoginStartResponse{}, providerkit.MapError(fmt.Errorf("login requires host HTTP client"))
	}
	form := url.Values{"client_id": {metaOAuthClientID}}
	resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    metaDeviceEndpoint,
		Headers: http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Accept":       {"application/json"},
			"User-Agent":   {"muse-code/1.0.2"},
		},
		Body: []byte(form.Encode()),
	})
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, providerkit.MapError(err)
	}
	var dcr struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(resp.Body, &dcr); err != nil || dcr.DeviceCode == "" || dcr.UserCode == "" {
		return pluginapi.AuthLoginStartResponse{}, providerkit.MapError(fmt.Errorf("meta device flow: bad response (HTTP %d)", resp.StatusCode))
	}
	interval := dcr.Interval
	if interval <= 0 {
		interval = 5
	}
	display := dcr.VerificationURIComplete
	if display == "" {
		display = dcr.VerificationURI
	}
	var expiresAt int64
	if dcr.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(dcr.ExpiresIn) * time.Second).Unix()
	}
	state := metaLoginState{
		DeviceCode:          dcr.DeviceCode,
		VerificationURI:     dcr.VerificationURIComplete,
		VerificationDisplay: display,
		TokenEndpoint:       metaTokenEndpoint,
		IntervalSeconds:     interval,
		ExpiresAtUnix:       expiresAt,
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  metaMuseNamespace,
		URL:       display + " (code " + dcr.UserCode + ")",
		State:     encodeMetaLoginState(state),
		ExpiresAt: time.Now().Add(time.Duration(dcr.ExpiresIn) * time.Second),
	}, nil
}

// PollLogin performs one token-poll attempt. Pending authorization maps to
// pending; approval mints the API key and returns the completed auth.
func (p *metaAuthProvider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	state, err := decodeMetaLoginState(req.State)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login expired; restart login"}, nil
	}
	if state.ExpiresAtUnix != 0 && time.Now().Unix() > state.ExpiresAtUnix {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "device code expired; restart login"}, nil
	}
	if req.HTTPClient == nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login requires host HTTP client"}, nil
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {state.DeviceCode},
		"client_id":   {metaOAuthClientID},
	}
	// Always the hardcoded endpoint: the decoded state is unsigned and its
	// token_endpoint field is untrusted (see decodeMetaLoginState).
	resp, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    metaTokenEndpoint,
		Headers: http.Header{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Accept":       {"application/json"},
			"User-Agent":   {"muse-code/1.0.2"},
		},
		Body: []byte(form.Encode()),
	})
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login poll failed"}, nil
	}
	if resp.StatusCode == http.StatusOK {
		var token struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			ExpiresIn   int    `json:"expires_in"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal(resp.Body, &token); err != nil || token.AccessToken == "" {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "login response missing access_token"}, nil
		}
		minted, _ := mintMetaAPIKey(ctx, req.HTTPClient, token.AccessToken)
		storage := map[string]any{
			"type":         metaMuseNamespace,
			"auth_kind":    "oauth",
			"access_token": token.AccessToken,
			"dca_token":    token.AccessToken,
		}
		if minted != nil {
			storage["api_key"] = minted.APIKey
			storage["access_token"] = minted.APIKey
			if minted.BaseURL != "" {
				storage["base_url"] = minted.BaseURL
			}
			if minted.UserEmail != "" {
				storage["email"] = minted.UserEmail
			}
			if minted.UserFullName != "" {
				storage["name"] = minted.UserFullName
			}
		}
		raw, _ := json.Marshal(storage)
		// ID stays empty so the host derives the path-relative ID from
		// FileName (CredentialFileName below), exactly as it does for a
		// ParseAuth-claimed file: the account key survives re-minting and
		// restarts because it is never derived from the short-lived token.
		auth := pluginapi.AuthData{
			Provider:    metaMuseNamespace,
			StorageJSON: raw,
			Metadata:    map[string]any{"type": metaMuseNamespace},
		}
		for k, v := range storage {
			auth.Metadata[k] = v
		}
		// Core parity (meta.CredentialFileName(email, dcaToken)): a minted
		// email names the file; otherwise the DCA token does, so an approved
		// login with a failed mint still saves (and the host still derives an
		// ID) instead of failing the commit with no filename and no ID.
		email, _ := storage["email"].(string)
		auth.FileName = CredentialFileName(strings.TrimSpace(email), token.AccessToken)
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth}, nil
	}
	var errResp struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(resp.Body, &errResp)
	switch errResp.Error {
	case "authorization_pending", "slow_down":
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for Meta authorization"}, nil
	case "access_denied":
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "access denied by user"}, nil
	case "expired_token":
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "device code expired; restart login"}, nil
	default:
		if errResp.Error != "" {
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "authorization server: " + errResp.Error}, nil
		}
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for Meta authorization"}, nil
	}
}

// metaMintedKey is the subset of the mint response the plugin persists.
type metaMintedKey struct {
	APIKey       string `json:"api_key"`
	BaseURL      string `json:"base_url"`
	UserEmail    string `json:"user_email"`
	UserFullName string `json:"user_full_name"`
}

// mintMetaAPIKey exchanges a DCA token for an LLM API key.
func mintMetaAPIKey(ctx context.Context, client pluginapi.HostHTTPClient, dcaToken string) (*metaMintedKey, error) {
	dcaToken = strings.TrimSpace(dcaToken)
	if dcaToken == "" || client == nil {
		return nil, fmt.Errorf("missing dca token")
	}
	body, _ := json.Marshal(map[string]string{"dca_token": dcaToken})
	resp, err := client.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    metaMintEndpoint,
		Headers: http.Header{
			"Authorization": {"Bearer " + dcaToken},
			"Content-Type":  {"application/json"},
			"Accept":        {"application/json"},
			"User-Agent":    {"muse-code/1.0.2"},
		},
		Body: body,
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mint failed (HTTP %d)", resp.StatusCode)
	}
	var minted metaMintedKey
	if err := json.Unmarshal(resp.Body, &minted); err != nil || strings.TrimSpace(minted.APIKey) == "" {
		return nil, fmt.Errorf("mint response missing api_key")
	}
	return &minted, nil
}

// RefreshAuth mints an API key from the DCA token when the credential has no
// usable key. Credentials that already carry a key pass through unchanged.
func (p *metaAuthProvider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	_, token := metaCreds(req.StorageJSON, req.Metadata, req.Attributes)
	if token != "" {
		return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
			Provider: metaMuseNamespace, ID: req.AuthID,
			StorageJSON: bytes.Clone(req.StorageJSON), Metadata: req.Metadata, Attributes: req.Attributes,
		}}, nil
	}
	dca := metaDCAToken(req.StorageJSON, req.Metadata, req.Attributes)
	if dca == "" {
		return pluginapi.AuthRefreshResponse{}, providerkit.MapError(fmt.Errorf("meta executor: missing API key or DCA token"))
	}
	minted, err := mintMetaAPIKey(ctx, req.HTTPClient, dca)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, providerkit.MapError(fmt.Errorf("meta executor: mint API key failed"))
	}
	merged := map[string]any{}
	_ = json.Unmarshal(req.StorageJSON, &merged)
	if merged == nil {
		merged = map[string]any{}
	}
	baseURL := strings.TrimSpace(minted.BaseURL)
	if baseURL == "" {
		baseURL, _ = metaCreds(req.StorageJSON, req.Metadata, req.Attributes)
	}
	merged["type"] = metaMuseNamespace
	merged["api_key"] = minted.APIKey
	merged["access_token"] = minted.APIKey
	merged["dca_token"] = dca
	merged["base_url"] = baseURL
	if minted.UserEmail != "" {
		merged["email"] = minted.UserEmail
	}
	if minted.UserFullName != "" {
		merged["name"] = minted.UserFullName
	}
	raw, _ := json.Marshal(merged)
	metadata := map[string]any{}
	for k, v := range req.Metadata {
		metadata[k] = v
	}
	for k, v := range merged {
		metadata[k] = v
	}
	metadata["type"] = metaMuseNamespace
	attributes := map[string]string{}
	for k, v := range req.Attributes {
		attributes[k] = v
	}
	attributes["base_url"] = baseURL
	attributes["api_key"] = minted.APIKey
	attributes["access_token"] = minted.APIKey
	return pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
		Provider: metaMuseNamespace, ID: req.AuthID,
		StorageJSON: raw, Metadata: metadata, Attributes: attributes,
	}}, nil
}

// CredentialFileName keeps a readable account name and hashes the identity so
// distinct emails that sanitize identically cannot overwrite each other.
func CredentialFileName(email, sub string) string {
	clean := strings.TrimSpace(email)
	if clean != "" {
		sanitized := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, clean)
		if len(sanitized) > 120 {
			sanitized = sanitized[:120]
		}
		hash := sha256.Sum256([]byte(clean))
		return fmt.Sprintf("meta-%s-%x.json", sanitized, hash[:8])
	}
	cleanSub := strings.TrimSpace(sub)
	if cleanSub != "" {
		hash := sha256.Sum256([]byte(cleanSub))
		return fmt.Sprintf("meta-%x.json", hash[:8])
	}
	return "meta-oauth.json"
}
