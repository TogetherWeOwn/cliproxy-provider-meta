//go:build cgo && (linux || darwin || freebsd)

package hostcompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginstore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// The actual v8.0.49 loader performs dlopen, C-ABI registration, RPC dispatch
// and quota routing. Only the upstream is an httptest mock: it serves the key
// envelope path the plugin polls.
func TestRealHostLoader(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/muse-code/key" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Error("envelope read must POST")
		}
		if r.Header.Get("Authorization") != "Bearer dca:hostcompat-token" {
			t.Error("incorrect authorization")
		}
		if r.Header.Get("X-Api-Version") != "1.0.0" {
			t.Error("missing API version")
		}
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"api_key":"hostcompat-key","subs_tier_name":"Plus","subs_usage":{"tier":"plus","window":{"used_percent":25,"window_duration_mins":300,"resets_at":1893456000},"weekly":{"used_percent":10,"resets_at":1893456000}}}`)
	}))
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, runtime.GOOS, runtime.GOARCH)
	if err = os.MkdirAll(arch, 0755); err != nil {
		t.Fatal(err)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib := filepath.Join(arch, "meta"+ext)
	build := exec.Command("go", "build", "-tags=hostcompat", "-buildmode=c-shared", "-o", lib, ".")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("c-shared build: %v\n%s", err, out)
	}
	if out, err := exec.Command("nm", "-D", lib).CombinedOutput(); err != nil || !strings.Contains(string(out), " T cliproxy_plugin_init") {
		t.Fatalf("ABI entry point: %v\n%s", err, out)
	}
	enabled := true
	var raw yaml.Node
	if err = yaml.Unmarshal([]byte("enabled: true\nhostcompat_endpoint: "+server.URL+"/muse-code/key\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: t.TempDir(), Plugins: config.PluginsConfig{Enabled: true, Dir: dir, Configs: map[string]config.PluginInstanceConfig{"meta": {Enabled: &enabled, Raw: *raw.Content[0]}}}}
	host := pluginhost.New()
	defer host.ShutdownAll()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = logging.WithResponseHeadersHolder(ctx)
	host.ApplyConfig(ctx, cfg)
	// Full provider plugin v2: the plugin registers auth ownership (the
	// "meta-muse" identifier plus the legacy "meta" key claimed through the
	// credential-provider bridge), the patch-carrying executor, and the
	// converging quota provider for the "meta" auths.
	if !host.PluginLoaded("meta") || !host.PluginRegistered("meta") || !host.HasQuotaProvider("meta") {
		t.Fatalf("registration %+v", host.RegisteredPlugins())
	}
	if !host.HasAuthProvider("meta-muse") || !host.HasAuthProvider("meta") {
		t.Fatalf("auth takeover missing: %+v", host.RegisteredPlugins())
	}
	if !host.SameAuthProvider("meta", "meta-muse") {
		t.Fatalf("meta and meta-muse must bridge to one auth provider: %+v", host.RegisteredPlugins())
	}
	if !host.HasExecutorCandidateProvider("meta-muse") {
		t.Fatalf("executor candidate missing: %+v", host.RegisteredPlugins())
	}
	info := host.RegisteredPlugins()
	if len(info) != 1 || !info[0].SupportsQuota || info[0].Metadata.Version != "0.2.1" {
		t.Fatalf("metadata %+v", info)
	}
	storage, _ := json.Marshal(map[string]any{"type": "meta", "dca_token": "dca:hostcompat-token", "api_key": "hostcompat-key", "label": "Example"})
	quotaReq := pluginapi.QuotaFetchRequest{AuthID: "hostcompat-auth", Provider: "meta", StorageJSON: storage}
	quota, handled, err := host.FetchQuota(ctx, quotaReq)
	if err != nil || !handled || len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 2 || quota.Groups[0].Buckets[0].RemainingFraction != .75 || quota.Groups[0].Buckets[1].RemainingFraction != .9 || polls.Load() != 1 {
		t.Fatalf("quota %+v %v %v polls=%d", quota, handled, err, polls.Load())
	}
	if quota.Subscription == nil || quota.Subscription.Plan != "Meta" || quota.Subscription.TierName != "Plus" {
		t.Fatalf("subscription %+v", quota.Subscription)
	}
	// The second fetch is served from the 30-minute per-auth cache: no poll.
	if _, _, err = host.FetchQuota(ctx, quotaReq); err != nil || polls.Load() != 1 {
		t.Fatalf("cache missed polls=%d %v", polls.Load(), err)
	}
	rawOut, _ := json.Marshal(quota)
	for _, secret := range []string{"hostcompat-key", "dca:hostcompat-token"} {
		if strings.Contains(string(rawOut), secret) {
			t.Fatalf("host quota response retains %q", secret)
		}
	}
}

func TestRegistryUsesNativeStoreSchema(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := pluginstore.ParseRegistry(raw)
	if err != nil || reg.SchemaVersion != 1 || len(reg.Plugins) != 1 || reg.Plugins[0].ID != "meta" || reg.Plugins[0].Version != "0.2.1" || pluginstore.PluginInstallType(reg.Plugins[0]) != "github-release" {
		t.Fatalf("registry %+v %v", reg, err)
	}
}
