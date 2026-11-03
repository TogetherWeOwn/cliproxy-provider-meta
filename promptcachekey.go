package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file ports the per-session Meta prompt_cache_key derivation from stock
// CLIProxy (internal/runtime/executor/meta_prompt_cache_key.go). The native
// executor injects a stable prompt_cache_key per downstream session so Meta's
// cache warms across turns; a client-supplied or already-carried key is never
// overridden, and a request with no session gets no key.

// metaPromptCacheKeyDomain separates this key from every other hash in the
// proxy (notably the Muse reasoning account key).
const metaPromptCacheKeyDomain = "cliproxy/meta-pck/v1"

// metaPromptCacheKeyForSession derives hex(sha256(domain + "\0" + session))[:32].
func metaPromptCacheKeyForSession(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(metaPromptCacheKeyDomain + "\x00" + sessionID))
	return hex.EncodeToString(sum[:16])
}

// resolveMetaPromptCacheKeySession resolves the downstream session with the
// same precedence the host uses for affinity: the pinned SDK canonical session
// helper over headers, client payload and merged metadata.
func resolveMetaPromptCacheKeySession(headers http.Header, query url.Values, originalPayload []byte, metadata map[string]any) string {
	_ = query
	return strings.TrimSpace(cliproxyauth.CanonicalSessionID(headers, originalPayload, metadata))
}

// applyMetaPromptCacheKey sets body prompt_cache_key when the client did not.
// A key already in the translated body wins, otherwise one carried in the
// client's own payload is preserved; only then is a derived session key set.
func applyMetaPromptCacheKey(body []byte, headers http.Header, query url.Values, originalPayload, clientPayload []byte, metadata map[string]any) []byte {
	if key := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); key != "" {
		return body
	}
	for _, payload := range [][]byte{clientPayload, originalPayload} {
		if len(payload) > 0 {
			if key := strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String()); key != "" {
				out, err := sjson.SetBytes(body, "prompt_cache_key", key)
				if err != nil {
					return body
				}
				return out
			}
		}
	}
	session := resolveMetaPromptCacheKeySession(headers, query, originalPayload, metadata)
	key := metaPromptCacheKeyForSession(session)
	if key == "" {
		return body
	}
	out, err := sjson.SetBytes(body, "prompt_cache_key", key)
	if err != nil {
		return body
	}
	return out
}
