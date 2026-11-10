package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	providerkit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeMetaHostClient is a scripted HostHTTPClient for the models.json fetch.
// It records every request so tests can assert no credential is ever sent
// and that sequential ModelsForAuth invocations each fetch.
type fakeMetaHostClient struct {
	mu      sync.Mutex
	calls   int
	urls    []string
	status  int
	body    []byte
	err     error
	block   bool
	sawDone bool
	blocked chan struct{}
	sawAuth bool
}

func (f *fakeMetaHostClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls++
	f.urls = append(f.urls, req.URL)
	if req.Headers.Get("Authorization") != "" {
		f.sawAuth = true
	}
	block, status, body, doErr := f.block, f.status, f.body, f.err
	f.mu.Unlock()
	if block {
		if f.blocked != nil {
			select {
			case f.blocked <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		f.mu.Lock()
		f.sawDone = true
		f.mu.Unlock()
		return pluginapi.HTTPResponse{}, ctx.Err()
	}
	if doErr != nil {
		return pluginapi.HTTPResponse{}, doErr
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{}, Body: slices.Clone(body)}, nil
}

func (f *fakeMetaHostClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("streaming not supported in fixture")
}

func (f *fakeMetaHostClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeMetaHostClient) set(status int, body []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.err = status, body, err
}

func metaCatalogAuthReq(client pluginapi.HostHTTPClient, token string) pluginapi.AuthModelRequest {
	return pluginapi.AuthModelRequest{
		AuthID:       "auth-1",
		AuthProvider: metaMuseNamespace,
		StorageJSON:  []byte(`{"api_key":"` + token + `"}`),
		Plugin:       pluginapi.Metadata{Name: metaMuseNamespace},
		HTTPClient:   client,
	}
}

func metaFixtureBody(ids ...string) []byte {
	entries := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{
			"id":                    id,
			"object":                "model",
			"created":               1788307200,
			"owned_by":              "meta",
			"type":                  "meta",
			"display_name":          "Display " + id,
			"name":                  id,
			"context_length":        1048576,
			"max_completion_tokens": 65536,
			"thinking": map[string]any{
				"levels": []string{"low", "high"},
			},
		})
	}
	body, _ := json.Marshal(map[string]any{"meta": entries})
	return body
}

func metaServedIDs(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// unknownIDs proves the contributor rule without any per-model override: an
// unknown id with a -contributor sibling is served only under the
// contributor id, and an unknown id without a sibling keeps its base id.
// It also proves parse of display/context/thinking and that no credential
// header is ever sent to the public catalog.
func TestMetaCatalogContributorDedupForUnknownIDs(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("unknown-a", "unknown-a-contributor", "unknown-solo")}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{metaMuseNamespace + "/unknown-a-contributor", metaMuseNamespace + "/unknown-solo"}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
		t.Fatalf("served %v, want %v", got, want)
	}
	got := resp.Models[0]
	if got.DisplayName != "Display unknown-a-contributor" || got.ContextLength != 1048576 || got.MaxCompletionTokens != 65536 {
		t.Fatalf("parsed contributor entry = %+v, want display/context parsed", got)
	}
	if got.Thinking == nil || len(got.Thinking.Levels) != 2 {
		t.Fatalf("parsed thinking = %+v, want levels parsed", got.Thinking)
	}
	cli.mu.Lock()
	sawAuth := cli.sawAuth
	cli.mu.Unlock()
	if sawAuth {
		t.Fatal("fetch sent an Authorization header; the public catalog must receive no credential")
	}
}

// Executor mapping: every served id strips exactly one namespace to the
// upstream id the executor sends, so the contributor variant reaches upstream
// unchanged apart from that strip.
func TestMetaCatalogExecutorMapping(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("unknown-a", "unknown-a-contributor")}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("served %v, want the single contributor entry", metaServedIDs(resp.Models))
	}
	id := resp.Models[0].ID
	rest, ok := strings.CutPrefix(id, metaMuseNamespace+"/")
	if !ok || rest == "" || strings.HasPrefix(rest, metaMuseNamespace+"/") {
		t.Fatalf("served %q does not survive one-namespace strip", id)
	}
	if rest != "unknown-a-contributor" {
		t.Fatalf("executor upstream id %q, want contributor variant", rest)
	}
	if got := stripMetaMuseModelID(id); got != "unknown-a-contributor" {
		t.Fatalf("stripMetaMuseModelID(%q) = %q, want contributor variant", id, got)
	}
	if err := providerkit.CheckModelNamespace(metaMuseNamespace, resp.Models); err != nil {
		t.Fatalf("runtime namespace check: %v", err)
	}
}

// The pinned v8.0.59 catalog "meta" section is the discovery fixture: 9
// entries (no Muse Ember yet), parsed through the plugin fetch.
func TestMetaCatalogPinnedHostSection(t *testing.T) {
	body, err := os.ReadFile("testdata/models-meta-v8.0.59.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Meta []struct {
			ID string `json:"id"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Meta) != 9 {
		t.Fatalf("pinned fixture has %d entries, want 9", len(wire.Meta))
	}
	cli := &fakeMetaHostClient{status: 200, body: body}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	// 9 entries, 4 base/contributor pairs: bases drop, contributor wins.
	want := []string{
		metaMuseNamespace + "/muse-wonder-1.0-contributor",
		metaMuseNamespace + "/muse-wonder-full-1.0-contributor",
		metaMuseNamespace + "/muse-spark-1.3-contributor",
		metaMuseNamespace + "/muse-spark-1.2-contributor",
		metaMuseNamespace + "/muse-spark-1.1",
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
		t.Fatalf("served %v, want %v", got, want)
	}
}

// Static fallback serves the configured static list through the same
// contributor-dedup pipeline: the 10-entry list serves 6 (contributor
// winners plus the two solo models), all namespaced.
func TestMetaCatalogStaticFallback(t *testing.T) {
	cli := &fakeMetaHostClient{status: 503}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		metaMuseNamespace + "/muse-wonder-1.0-contributor",
		metaMuseNamespace + "/muse-wonder-full-1.0-contributor",
		metaMuseNamespace + "/muse-spark-1.3-contributor",
		metaMuseNamespace + "/muse-spark-1.2-contributor",
		metaMuseNamespace + "/muse-spark-1.1",
		metaMuseNamespace + "/muse-ember-1.0",
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
		t.Fatalf("static fallback served %v, want %v", got, want)
	}
}

// Timeline A — fresh convergence: seeded at A, fixture advanced to A+B, the
// next ModelsForAuth invocation (simulating the one-shot host catalog-change
// callback) serves A+B. A TTL larger than the invocation interval does not
// suppress the second fetch.
func TestMetaCatalogTimelineAFreshConverges(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("spark-a")}
	cat := providerkit.NewModelCatalogWithTTL(providerkit.ModelCatalogConfig{
		Namespace: metaMuseNamespace,
		Static:    metaUpstreamModels,
		Fetch:     fetchMetaModels,
	}, 10*time.Minute)
	req := metaCatalogAuthReq(cli, "dca:test")
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/spark-a"}) {
		t.Fatalf("t0 served %v, want A", got)
	}
	cli.set(200, metaFixtureBody("spark-a", "spark-b"), nil)
	resp, err = cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{metaMuseNamespace + "/spark-a", metaMuseNamespace + "/spark-b"}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
		t.Fatalf("t1 served %v, want %v", got, want)
	}
	if n := cli.callCount(); n != 2 {
		t.Fatalf("fetch calls = %d, want 2 (every invocation fetches; no TTL skip)", n)
	}
}

// Timeline B — failure then no guaranteed recovery: t0 serves A; t1 the
// callback fetch fails, so last-good A is served and B is NOT published. No
// further invocation exists until the next host event (next catalog "meta"
// section change, auth upsert, config reload, or plugin re-registration);
// an unchanged catalog poll is not a re-trigger. This test documents the
// accepted residual: it asserts last-good persistence, never recovery. It
// does not invoke ModelsForAuth again to "show" B appearing, because that
// second invocation is exactly what the host does not guarantee. v0.5.0
// performs a single fetch per invocation (no bounded retry) — do not add
// wait/retry claims here.
func TestMetaCatalogTimelineBFailureThenNoGuaranteedRecovery(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("spark-a")}
	cat := newMetaModelCatalog()
	req := metaCatalogAuthReq(cli, "dca:test")
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/spark-a"}) {
		t.Fatalf("t0 served %v, want A", got)
	}
	// t1: the one-shot callback fetch fails (catalog already advanced to
	// A+B upstream, but this invocation cannot see it).
	cli.set(200, metaFixtureBody("spark-a", "spark-b"), errors.New("transient network"))
	resp, err = cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/spark-a"}) {
		t.Fatalf("t1 served %v, want last-good A (B unpublished)", got)
	}
}

// TTL/freshness expiry never erases the durable last-good store.
func TestMetaCatalogFreshnessExpiryNeverErasesLastGood(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("fresh-a")}
	cat := providerkit.NewModelCatalogWithTTL(providerkit.ModelCatalogConfig{
		Namespace: metaMuseNamespace,
		Static:    metaUpstreamModels,
		Fetch:     fetchMetaModels,
	}, time.Millisecond)
	req := metaCatalogAuthReq(cli, "dca:test")
	if _, err := cat.ModelsForAuth(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	cli.set(503, nil, nil)
	resp, err := cat.ModelsForAuth(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/fresh-a"}) {
		t.Fatalf("post-expiry served %v, want last-good fresh-a", got)
	}
}

// Credential-generation isolation: a rotated credential never inherits a
// previous generation's list, and Invalidate drops the generation entirely.
func TestMetaCatalogCredentialGenerationIsolation(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("gen-a")}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:one"))
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/gen-a"}) {
		t.Fatalf("gen-1 served %v, want gen-a", got)
	}
	cli.set(200, metaFixtureBody("gen-b"), nil)
	resp, err = cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:two"))
	if err != nil {
		t.Fatal(err)
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, []string{metaMuseNamespace + "/gen-b"}) {
		t.Fatalf("gen-2 served %v, want gen-b (never a stale gen-a list)", got)
	}
	if n := cli.callCount(); n != 2 {
		t.Fatalf("fetch calls = %d, want 2 (rotated generation fetches fresh)", n)
	}
	cat.Invalidate("auth-1")
	cli.set(503, nil, nil)
	resp, err = cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:two"))
	if err != nil {
		t.Fatal(err)
	}
	// Invalidated: no last-good remains, so the static fallback serves.
	want := []string{
		metaMuseNamespace + "/muse-wonder-1.0-contributor",
		metaMuseNamespace + "/muse-wonder-full-1.0-contributor",
		metaMuseNamespace + "/muse-spark-1.3-contributor",
		metaMuseNamespace + "/muse-spark-1.2-contributor",
		metaMuseNamespace + "/muse-spark-1.1",
		metaMuseNamespace + "/muse-ember-1.0",
	}
	if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
		t.Fatalf("post-invalidate served %v, want static fallback %v", got, want)
	}
}

// Cancellation: a pre-cancelled request returns its context error and makes
// no fetch; parent cancellation during a blocked fetch falls back without
// hanging and the host client sees the cancellation.
func TestMetaCatalogCancellation(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("a")}
	cat := newMetaModelCatalog()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cat.ModelsForAuth(ctx, metaCatalogAuthReq(cli, "dca:test")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled err = %v, want context.Canceled", err)
	}
	if n := cli.callCount(); n != 0 {
		t.Fatalf("pre-cancelled made %d fetches, want 0", n)
	}

	blocking := &fakeMetaHostClient{status: 200, block: true, blocked: make(chan struct{}, 1)}
	req := metaCatalogAuthReq(blocking, "dca:test")
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan pluginapi.ModelResponse, 1)
	go func() {
		resp, _ := cat.ModelsForAuth(ctx, req)
		done <- resp
	}()
	select {
	case <-blocking.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the blocking host call")
	}
	cancel()
	select {
	case resp := <-done:
		want := []string{
			metaMuseNamespace + "/muse-wonder-1.0-contributor",
			metaMuseNamespace + "/muse-wonder-full-1.0-contributor",
			metaMuseNamespace + "/muse-spark-1.3-contributor",
			metaMuseNamespace + "/muse-spark-1.2-contributor",
			metaMuseNamespace + "/muse-spark-1.1",
			metaMuseNamespace + "/muse-ember-1.0",
		}
		if got := metaServedIDs(resp.Models); !slices.Equal(got, want) {
			t.Fatalf("cancelled served %v, want static fallback %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call did not return")
	}
	blocking.mu.Lock()
	saw := blocking.sawDone
	blocking.mu.Unlock()
	if !saw {
		t.Fatal("host client never saw parent cancellation")
	}
}

// Enforceable limits: an oversized buffered body is a failed fetch that
// falls back (never parsed, never cached), and a parent deadline earlier
// than the kit's 15s fetch timeout bounds a blocked fetch (the kit derives
// the earlier of the two). No wire/memory bound is claimed.
func TestMetaCatalogSizeAndDeadlineFallback(t *testing.T) {
	big := &fakeMetaHostClient{status: 200, body: make([]byte, providerkit.MaxModelsBody+1)}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(big, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 6 {
		t.Fatalf("oversized served %d models, want 6-model static fallback", len(resp.Models))
	}

	blocked := &fakeMetaHostClient{status: 200, block: true}
	req := metaCatalogAuthReq(blocked, "dca:test")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err = cat.ModelsForAuth(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("blocked fetch returned after %s, want bounded by the parent deadline", elapsed)
	}
	if len(resp.Models) != 6 {
		t.Fatalf("deadline served %d models, want 6-model static fallback", len(resp.Models))
	}
}

// A 200 body with no usable meta entries is a failed fetch, not an empty
// success: both catalog URLs are tried and the static fallback serves.
func TestMetaCatalogEmptySectionFallsBack(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: []byte(`{"meta":[]}`)}
	cat := newMetaModelCatalog()
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 6 {
		t.Fatalf("empty section served %d models, want 6-model static fallback", len(resp.Models))
	}
	if n := cli.callCount(); n != 2 {
		t.Fatalf("fetch calls = %d, want 2 (empty primary must try the fallback URL)", n)
	}
	if resp.Provider != metaMuseNamespace {
		t.Fatalf("provider = %q, want %q", resp.Provider, metaMuseNamespace)
	}
}

// Overrides apply through the uniform pipeline: a hand override rewrites a
// known id's display/context and swaps to a contributor id.
func TestMetaCatalogOverridesApply(t *testing.T) {
	cli := &fakeMetaHostClient{status: 200, body: metaFixtureBody("known")}
	cat := providerkit.NewModelCatalog(providerkit.ModelCatalogConfig{
		Namespace: metaMuseNamespace,
		Static:    metaUpstreamModels,
		Fetch:     fetchMetaModels,
		Overrides: map[string]providerkit.ModelOverride{
			"known": {DisplayName: "Known Renamed", ContextLength: 111, MaxCompletionTokens: 222, ContributorID: "known-contributor"},
		},
	})
	resp, err := cat.ModelsForAuth(context.Background(), metaCatalogAuthReq(cli, "dca:test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("served %v, want single rewritten entry", metaServedIDs(resp.Models))
	}
	got := resp.Models[0]
	if got.ID != metaMuseNamespace+"/known-contributor" || got.DisplayName != "Known Renamed" || got.ContextLength != 111 || got.MaxCompletionTokens != 222 {
		t.Fatalf("override applied = %+v, want contributor rewrite + renamed", got)
	}
}

// RegisterModels and StaticModels serve the static list (not a fetch) through
// the same pipeline. The requests carry the plugin's display name (what the
// host populates req.Plugin.Name with), so this also pins that responses are
// attributed to the credential provider, not the display name.
func TestMetaCatalogRegisterAndStaticServeStatic(t *testing.T) {
	cat := newMetaModelCatalog()
	reg, err := cat.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{
		Plugin: pluginapi.Metadata{Name: displayName},
	})
	if err != nil {
		t.Fatal(err)
	}
	sta, err := cat.StaticModels(context.Background(), pluginapi.StaticModelRequest{
		Plugin: pluginapi.Metadata{Name: displayName},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		metaMuseNamespace + "/muse-wonder-1.0-contributor",
		metaMuseNamespace + "/muse-wonder-full-1.0-contributor",
		metaMuseNamespace + "/muse-spark-1.3-contributor",
		metaMuseNamespace + "/muse-spark-1.2-contributor",
		metaMuseNamespace + "/muse-spark-1.1",
		metaMuseNamespace + "/muse-ember-1.0",
	}
	if got := metaServedIDs(reg.Models); !slices.Equal(got, want) {
		t.Fatalf("registered %v, want static %v", got, want)
	}
	if got := metaServedIDs(sta.Models); !slices.Equal(got, want) {
		t.Fatalf("static %v, want %v", got, want)
	}
	if reg.Provider != metaMuseNamespace || sta.Provider != metaMuseNamespace {
		t.Fatalf("providers = %q/%q, want %q", reg.Provider, sta.Provider, metaMuseNamespace)
	}
}
