package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var residualDependencyPins = map[string]string{
	"github.com/andybalholm/brotli": "v1.2.7",
	"golang.org/x/crypto":           "v0.58.1-0.20261009181718-2f1f834330b2",
	"golang.org/x/net":              "v0.62.0",
	"golang.org/x/sync":             "v0.24.0",
	"golang.org/x/sys":              "v0.49.0",
	"golang.org/x/text":             "v0.43.0",
}

type dependencyModule struct {
	Path    string
	Version string
	Replace *dependencyModule
}

func isOpenPGP(path string) bool {
	return path == "golang.org/x/crypto/openpgp" || strings.HasPrefix(path, "golang.org/x/crypto/openpgp/")
}

func checkResidualReplacement(oldPath, newPath string, pins map[string]string) error {
	_, oldPinned := pins[oldPath]
	_, newPinned := pins[newPath]
	if oldPinned || newPinned || isOpenPGP(oldPath) || isOpenPGP(newPath) {
		return fmt.Errorf("residual dependency replacement: %s => %s", oldPath, newPath)
	}
	return nil
}

func checkResidualModules(modules []dependencyModule, pins map[string]string) error {
	seen := make(map[string]bool)
	for _, module := range modules {
		if isOpenPGP(module.Path) {
			return fmt.Errorf("unsafe OpenPGP module: %s", module.Path)
		}
		if module.Replace != nil {
			if err := checkResidualReplacement(module.Path, module.Replace.Path, pins); err != nil {
				return err
			}
		}
		if want, ok := pins[module.Path]; ok {
			if module.Version != want {
				return fmt.Errorf("%s must be unreplaced %s, got %+v", module.Path, want, module)
			}
			seen[module.Path] = true
		}
	}
	for path := range pins {
		if !seen[path] {
			return fmt.Errorf("missing pinned dependency %s", path)
		}
	}
	return nil
}

func dependencyJSON(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, stderr.String())
	}
	return out
}

func TestResidualDependencyGraphs(t *testing.T) {
	for _, dir := range []string{".", "integration/hostcompat"} {
		t.Run(dir, func(t *testing.T) {
			// Child-process reads are not tracked by the Go test cache.
			for _, name := range []string{"go.mod", "go.sum"} {
				if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			var manifest struct {
				Require []dependencyModule
				Exclude []dependencyModule
				Replace []struct{ Old, New dependencyModule }
			}
			if err := json.Unmarshal(dependencyJSON(t, dir, "mod", "edit", "-json"), &manifest); err != nil {
				t.Fatal(err)
			}
			if err := checkResidualModules(manifest.Require, residualDependencyPins); err != nil {
				t.Fatal(err)
			}
			for _, replacement := range manifest.Replace {
				if err := checkResidualReplacement(replacement.Old.Path, replacement.New.Path, residualDependencyPins); err != nil {
					t.Fatal(err)
				}
			}
			for _, exclusion := range manifest.Exclude {
				_, pinned := residualDependencyPins[exclusion.Path]
				if pinned || isOpenPGP(exclusion.Path) {
					t.Fatalf("residual dependency exclusion: %+v", exclusion)
				}
			}

			var modules []dependencyModule
			decoder := json.NewDecoder(bytes.NewReader(dependencyJSON(t, dir, "list", "-m", "-mod=readonly", "-json", "all")))
			for {
				var module dependencyModule
				if err := decoder.Decode(&module); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				modules = append(modules, module)
			}
			pins := make(map[string]string)
			for path, version := range residualDependencyPins {
				pins[path] = version
			}
			pins["golang.org/x/term"] = "v0.47.0"
			pins["golang.org/x/tools"] = "v0.51.0"
			pins["github.com/xyproto/randomstring"] = "v1.0.5"
			if err := checkResidualModules(modules, pins); err != nil {
				t.Fatal(err)
			}

			// Metadata covers package imports and dependency-owned test imports,
			// not call reachability or consumers outside these two modules.
			for _, tags := range []string{"", "hostcompat"} {
				decoder := json.NewDecoder(bytes.NewReader(dependencyJSON(t, dir, "list", "-deps", "-test", "-mod=readonly", "-json", "-tags="+tags, "./...")))
				count := 0
				for {
					var pkg struct {
						ImportPath                         string
						Imports, TestImports, XTestImports []string
					}
					if err := decoder.Decode(&pkg); err == io.EOF {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					paths := append([]string{pkg.ImportPath}, pkg.Imports...)
					paths = append(paths, pkg.TestImports...)
					paths = append(paths, pkg.XTestImports...)
					for _, path := range paths {
						if isOpenPGP(path) {
							t.Fatalf("%s imports removed OpenPGP API %s (tags=%q)", pkg.ImportPath, path, tags)
						}
					}
					count++
				}
				if count == 0 {
					t.Fatal("empty package inventory")
				}
				t.Logf("%s tags=%q: %d package records without OpenPGP imports", dir, tags, count)
			}
		})
	}
}

func TestResidualDependencyGuard(t *testing.T) {
	const crypto = "golang.org/x/crypto"
	pins := map[string]string{crypto: residualDependencyPins[crypto]}
	good := dependencyModule{Path: crypto, Version: pins[crypto]}
	for name, modules := range map[string][]dependencyModule{
		"downgrade":     {{Path: crypto, Version: "v0.58.0"}},
		"missing":       {},
		"replacement":   {{Path: crypto, Version: pins[crypto], Replace: &dependencyModule{Path: "example.com/crypto"}}},
		"split module":  {good, {Path: crypto + "/openpgp", Version: "v0.1.0"}},
		"split subpath": {good, {Path: crypto + "/openpgp/packet", Version: "v0.1.0"}},
		"split target":  {good, {Path: "example.com/crypto", Replace: &dependencyModule{Path: crypto + "/openpgp"}}},
		"alias target":  {good, {Path: "example.com/crypto", Replace: &dependencyModule{Path: crypto, Version: "v0.58.0"}}},
		"pinned target": {good, {Path: "example.com/crypto", Replace: &dependencyModule{Path: crypto, Version: pins[crypto]}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkResidualModules(modules, pins); err == nil {
				t.Fatal("unsafe graph accepted")
			}
		})
	}
	if err := checkResidualModules([]dependencyModule{good, {Path: "example.com/unrelated", Version: "v1.0.0"}}, pins); err != nil {
		t.Fatalf("unrelated dependency rejected: %v", err)
	}
	for name, paths := range map[string][2]string{
		"pinned source": {crypto, "example.com/crypto"},
		"alias target":  {"example.com/crypto", crypto},
		"split source":  {crypto + "/openpgp", "example.com/crypto"},
		"split target":  {"example.com/crypto", crypto + "/openpgp"},
	} {
		t.Run("manifest/"+name, func(t *testing.T) {
			if err := checkResidualReplacement(paths[0], paths[1], pins); err == nil {
				t.Fatal("unsafe manifest replacement accepted")
			}
		})
	}
	if err := checkResidualReplacement("example.com/unrelated", "example.com/other", pins); err != nil {
		t.Fatalf("unrelated replacement rejected: %v", err)
	}
	if err := checkResidualModules([]dependencyModule{good, {Path: "example.com/unrelated", Replace: &dependencyModule{Path: "example.com/other"}}}, pins); err != nil {
		t.Fatalf("unrelated graph replacement rejected: %v", err)
	}
	if isOpenPGP(crypto+"/ssh") || !isOpenPGP(crypto+"/openpgp/armor") {
		t.Fatal("OpenPGP import boundary is incorrect")
	}
}
