package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"
)

// This file ports the Meta reasoning provenance tag and envelope inspector
// from stock CLIProxy (internal/signature/meta_reasoning.go and
// meta_validation.go). They are pure functions with no host dependencies, so
// the port is verbatim apart from package placement and logging-free errors.
//
// Muse reasoning encrypted_content (SignatureProviderMeta) is an opaque
// envelope bound to the Meta account that issued it: Meta rejects a replay
// from any other account with "reasoning encrypted_content was not issued to
// this caller". The envelope shape says nothing about the issuing account, so
// the proxy records it in the provenance tag below.

// MetaReasoningTagPrefix opens the provenance tag the proxy wraps around Muse
// reasoning handed to a client: meta#<account-key>#<envelope>. The client
// replays the whole string; the executor keeps the envelope only when the
// account key names the credential it selected for the request.
const MetaReasoningTagPrefix = "meta#"

// maxMetaReasoningAccountKeyLen bounds the account key so a hostile or
// corrupted replay cannot make the tag parser retain an arbitrarily long
// prefix.
const maxMetaReasoningAccountKeyLen = 64

// TagMetaReasoning binds a Muse reasoning envelope to the account that issued
// it. It returns the envelope unchanged when either part is empty or the key
// is not a valid account key, so a caller that cannot identify the account
// never emits a tag that would later be mistaken for provenance.
func TagMetaReasoning(accountKey, envelope string) string {
	if envelope == "" || !validMetaReasoningAccountKey(accountKey) {
		return envelope
	}
	if _, _, tagged := ParseMetaReasoningTag(envelope); tagged {
		return envelope
	}
	return MetaReasoningTagPrefix + accountKey + "#" + envelope
}

// ParseMetaReasoningTag splits meta#<account-key>#<envelope>. ok is false for
// anything that is not a well-formed tag, including every legacy untagged Muse
// envelope, so callers can treat "not a tag" and "foreign tag" differently.
func ParseMetaReasoningTag(raw string) (accountKey, envelope string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, MetaReasoningTagPrefix) {
		return "", "", false
	}
	accountKey, envelope, ok = strings.Cut(raw[len(MetaReasoningTagPrefix):], "#")
	if !ok || envelope == "" || !validMetaReasoningAccountKey(accountKey) {
		return "", "", false
	}
	return accountKey, envelope, true
}

// IsMetaReasoningTagged reports whether raw is a well-formed Muse provenance tag.
func IsMetaReasoningTagged(raw string) bool {
	_, _, ok := ParseMetaReasoningTag(raw)
	return ok
}

func validMetaReasoningAccountKey(key string) bool {
	if key == "" || len(key) > maxMetaReasoningAccountKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// MaxMetaReasoningSignatureLen bounds the raw base64url text accepted by
// InspectMetaReasoningSignature. It mirrors the GPT bound; the bucket-length
// rule below is far more restrictive in practice.
const MaxMetaReasoningSignatureLen = 32 * 1024 * 1024

const (
	metaReasoningSignatureOverheadLen = 45
	metaReasoningSignatureMinPayload  = 1024
	// maxMetaRouterSuffixLen bounds the dot-delimited suffix a router such as
	// OpenRouter appends after the native envelope. Observed values are well
	// under this bound.
	maxMetaRouterSuffixLen = 128
)

// metaReasoningSignatureHeader is the fixed five-byte prefix observed on every
// captured Meta Muse reasoning encrypted_content value (api.meta.ai,
// 2026-10-15 corpus: 82 values across nine muse-* models). Its semantics are
// unresolved - it may be a format magic, a key/context fingerprint, or a
// versioned identifier ending in 0x01 - and every capture came from one
// account on one day, so a future rotation would fall back to Unknown rather
// than being claimed here on weaker evidence.
var metaReasoningSignatureHeader = []byte{0x43, 0xe3, 0xda, 0x0e, 0x01}

// MetaReasoningSignatureInfo describes the transport shape of a Meta Muse
// reasoning encrypted_content envelope.
type MetaReasoningSignatureInfo struct {
	DecodedLen int
	// PayloadLen is DecodedLen minus the fixed 45-byte envelope overhead. It
	// is the power-of-two padded region, not a proven plaintext length.
	PayloadLen     int
	BucketExponent int
	// RouterSuffix is the dot-delimited suffix a router such as OpenRouter
	// appends after the native envelope; empty for native values. Replay must
	// forward the full value verbatim; never strip the suffix.
	RouterSuffix string
}

// IsValidMetaReasoningSignature reports whether rawSignature has the observed
// Meta Muse envelope shape. This is only a transport-shape check; it does not
// prove decryptability or upstream replay acceptance.
func IsValidMetaReasoningSignature(rawSignature string) bool {
	_, err := InspectMetaReasoningSignature(rawSignature)
	return err == nil
}

// InspectMetaReasoningSignature validates the outer envelope shape observed
// for Meta Muse reasoning encrypted_content: canonical unpadded base64url,
// the fixed five-byte header above, and a decoded length of exactly
// 45 + 1024*2^k bytes. A router such as OpenRouter may serve the same model
// family and append one "." plus a short alphanumeric routing suffix to the
// native value; that suffix is validated here and reported via
// MetaReasoningSignatureInfo.RouterSuffix, and the full value must be
// replayed verbatim.
func InspectMetaReasoningSignature(rawSignature string) (*MetaReasoningSignatureInfo, error) {
	sig := strings.TrimSpace(rawSignature)
	if sig == "" {
		return nil, fmt.Errorf("empty Meta reasoning signature")
	}
	if len(sig) > MaxMetaReasoningSignatureLen {
		return nil, fmt.Errorf("Meta reasoning signature exceeds maximum length (%d bytes)", MaxMetaReasoningSignatureLen)
	}
	// Reject other providers' signatures before scanning for a router suffix.
	if !strings.HasPrefix(sig, "Q-PaDg") {
		return nil, fmt.Errorf("invalid Meta reasoning signature: expected Q-PaDg prefix")
	}
	native := sig
	routerSuffix := ""
	if head, tail, ok := strings.Cut(sig, "."); ok {
		// The native alphabet never contains '.', so the first dot is an
		// unambiguous boundary. The suffix is restricted to the observed
		// router form: a single run of alphanumeric characters.
		if !isValidMetaRouterSuffix(tail) {
			return nil, fmt.Errorf("invalid Meta reasoning signature: router suffix must be 1-%d alphanumeric characters", maxMetaRouterSuffixLen)
		}
		native = head
		routerSuffix = tail
	}
	info, err := inspectNativeMetaReasoningSignature(native)
	if err != nil {
		return nil, err
	}
	info.RouterSuffix = routerSuffix
	return info, nil
}

// inspectNativeMetaReasoningSignature validates the native (unsuffixed)
// envelope shape described on InspectMetaReasoningSignature.
func inspectNativeMetaReasoningSignature(sig string) (*MetaReasoningSignatureInfo, error) {
	// The literal prefix is the cheapest discriminator: every observed value
	// starts with "Q-PaDg", and probing this validator is on the hot path for
	// signatures of every provider.
	if !strings.HasPrefix(sig, "Q-PaDg") {
		return nil, fmt.Errorf("invalid Meta reasoning signature: expected Q-PaDg prefix")
	}
	// Reject by encoded length before scanning or allocating a decoded copy:
	// only bucket-sized candidates ever reach the decoder.
	if len(sig)%4 == 1 {
		return nil, fmt.Errorf("invalid Meta reasoning signature: base64url length %d cannot decode", len(sig))
	}
	decodedLen := len(sig) / 4 * 3
	switch len(sig) % 4 {
	case 2:
		decodedLen++
	case 3:
		decodedLen += 2
	}
	payloadLen := decodedLen - metaReasoningSignatureOverheadLen
	if payloadLen < metaReasoningSignatureMinPayload || payloadLen&(payloadLen-1) != 0 {
		return nil, fmt.Errorf("invalid Meta reasoning signature: payload length %d is not a power-of-two bucket", payloadLen)
	}
	if index, r, ok := firstInvalidMetaReasoningSignatureChar(sig); ok {
		return nil, fmt.Errorf("invalid Meta reasoning signature: contains non-base64url character U+%04X at byte %d", r, index)
	}
	// Strict decoding rejects non-canonical trailing bits, so no re-encode
	// comparison is needed to prove canonical form.
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("invalid Meta reasoning signature: base64url decode failed")
	}
	if !bytes.Equal(decoded[:len(metaReasoningSignatureHeader)], metaReasoningSignatureHeader) {
		return nil, fmt.Errorf("invalid Meta reasoning signature: unexpected envelope header %x", decoded[:len(metaReasoningSignatureHeader)])
	}
	exponent := 0
	for p := payloadLen; p > metaReasoningSignatureMinPayload; p >>= 1 {
		exponent++
	}
	return &MetaReasoningSignatureInfo{
		DecodedLen:     len(decoded),
		PayloadLen:     payloadLen,
		BucketExponent: exponent,
	}, nil
}

// metaReasoningSignatureCharSet is the unpadded base64url alphabet. Observed
// Meta values never carry '=' padding, so padding is rejected rather than
// normalized away.
var metaReasoningSignatureCharSet = base64AlphabetSet("-_")

func firstInvalidMetaReasoningSignatureChar(sig string) (int, rune, bool) {
	for index := 0; index < len(sig); index++ {
		if !metaReasoningSignatureCharSet[sig[index]] {
			r, _ := utf8.DecodeRuneInString(sig[index:])
			return index, r, true
		}
	}
	return 0, 0, false
}

// metaRouterSuffixCharSet is the alphanumeric-only alphabet observed on
// router-appended suffixes; base64url's '-'/'_' and any second '.' are all
// rejected so the suffix can never be mistaken for more envelope payload.
var metaRouterSuffixCharSet = base64AlphabetSet("")

func isValidMetaRouterSuffix(suffix string) bool {
	if len(suffix) == 0 || len(suffix) > maxMetaRouterSuffixLen {
		return false
	}
	for index := 0; index < len(suffix); index++ {
		if !metaRouterSuffixCharSet[suffix[index]] {
			return false
		}
	}
	return true
}

// base64AlphabetSet builds the lookup set for unpadded base64url plus extra
// characters (for example the router suffix alphabet passes "").
func base64AlphabetSet(extra string) [256]bool {
	var set [256]bool
	for c := 'A'; c <= 'Z'; c++ {
		set[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		set[c] = true
	}
	for c := '0'; c <= '9'; c++ {
		set[c] = true
	}
	for i := 0; i < len(extra); i++ {
		set[extra[i]] = true
	}
	return set
}
