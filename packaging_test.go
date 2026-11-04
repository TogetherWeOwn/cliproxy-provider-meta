package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReleaseRejectsEnclosingRepository(t *testing.T) {
	outer := t.TempDir()
	nested := filepath.Join(outer, "plugin")
	if err := os.MkdirAll(filepath.Join(nested, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"scripts/package-release.sh", "registry.json"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture.invalid", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = outer
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v\n%s", err, raw)
		}
	}
	// A build-tool sentinel ensures the provenance gate precedes any build.
	goSentinel := filepath.Join(t.TempDir(), "go-sentinel")
	if err := os.WriteFile(goSentinel, []byte("#!/bin/sh\nprintf 'BUILD REACHED\\n' >&2\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "must-not-exist")
	cmd := exec.Command("bash", filepath.Join(nested, "scripts/package-release.sh"), out)
	cmd.Env = append(os.Environ(), "GO="+goSentinel)
	raw, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "Source root is not the Git worktree root") || strings.Contains(string(raw), "BUILD REACHED") {
		t.Fatalf("nested source must refuse enclosing provenance before build: %v\n%s", err, raw)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("refused provenance created build output: %v", err)
	}
	// The exact same source is accepted once it owns a standalone repository.
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture.invalid", "commit", "-qm", "fixture"}} {
		git := exec.Command("git", args...)
		git.Dir = nested
		if raw, err := git.CombinedOutput(); err != nil {
			t.Fatalf("standalone fixture: %v\n%s", err, raw)
		}
	}
	cmd = exec.Command("bash", filepath.Join(nested, "scripts/package-release.sh"), out)
	cmd.Env = append(os.Environ(), "GO="+goSentinel)
	raw, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "BUILD REACHED") || strings.Contains(string(raw), "Source root is not") {
		t.Fatalf("standalone source did not pass provenance gate: %v\n%s", err, raw)
	}
}

func TestReleaseRejectsGitStatusFailure(t *testing.T) {
	for _, phase := range []string{"before", "after-clean", "after-modified"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range []string{"scripts/package-release.sh", "registry.json", "LICENSE", "docs/replay-event-v1.schema.json"} {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, file)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Fixture", "-c", "user.email=fixture.invalid", "commit", "-qm", "fixture"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("fixture: %v\n%s", err, out)
				}
			}
			if phase == "before" {
				if err := os.WriteFile(filepath.Join(root, ".git/index"), []byte("corrupt index"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if phase == "after-modified" {
				if err := os.WriteFile(filepath.Join(root, "untracked"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			bin := t.TempDir()
			// Simulate a successful build that corrupts the index before revalidation.
			build := `#!/bin/sh
set -eu
printf 'BUILD REACHED\n' >&2
root=$2
while [ "$1" != -o ]; do shift; done
printf 'void cliproxy_plugin_init(void) {}' | cc -shared -x c - -o "$2"
printf 'corrupt index' > "$root/.git/index"
`
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(build), 0700); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts/package-release.sh"), out)
			cmd.Env = append(os.Environ(), "GO="+filepath.Join(bin, "go"))
			raw, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(raw), "Cannot inspect source cleanliness") {
				t.Fatalf("Git status failure must refuse packaging: %v\n%s", err, raw)
			}
			if strings.Contains(string(raw), "BUILD REACHED") != (phase != "before") {
				t.Fatalf("Git failure did not stop at the required boundary: %s", raw)
			}
			entries, err := os.ReadDir(out)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed status published package/checksums/provenance: %v %v", entries, err)
			}
		})
	}
}

func verifyNativeReplayStdout(t *testing.T, library string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	header, err := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}", "github.com/TogetherWeOwn/cliproxy-provider-kit/cabi").Output()
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(t.TempDir(), "pipe-host")
	cmd := exec.CommandContext(ctx, "cc", "testdata/replay_pipe_host.c", "-I", strings.TrimSpace(string(header)), "-ldl", "-o", host)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native fixture: %v\n%s", err, out)
	}
	for _, broken := range []bool{false, true} {
		args := []string{library}
		if broken {
			args = append(args, "closed")
		}
		cmd := exec.CommandContext(ctx, host, args...)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "native host survived startup") || (!broken && !strings.Contains(string(out), "replay-producer-startup-v1")) {
			t.Fatalf("native packaged library, broken=%v: %v\n%s", broken, err, out)
		}
	}
}

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target is linux/amd64")
	}
	out := t.TempDir()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(linkedRoot, "scripts/package-release.sh"), out)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, result)
	}
	archive := filepath.Join(out, "meta_0.2.4_linux_amd64.zip")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	checksums, err := os.ReadFile(filepath.Join(out, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checksums), hex.EncodeToString(sum[:])+"  meta_0.2.4_linux_amd64.zip") {
		t.Fatalf("incorrect checksums %s", checksums)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	provenanceRaw, err := os.ReadFile(filepath.Join(out, "build-provenance.json"))
	if err != nil {
		t.Fatalf("source-bound packaging requires a provenance manifest: %v", err)
	}
	var provenance struct {
		SourceRevision string `json:"source_revision"`
		SourceTree     string `json:"source_tree"`
		ArchiveSHA256  string `json:"archive_sha256"`
		LibrarySHA256  string `json:"library_sha256"`
	}
	if err := json.Unmarshal(provenanceRaw, &provenance); err != nil {
		t.Fatal(err)
	}
	for ref, expected := range map[string]string{"HEAD": provenance.SourceRevision, "HEAD^{tree}": provenance.SourceTree} {
		actual, err := exec.Command("git", "rev-parse", ref).Output()
		if err != nil || strings.TrimSpace(string(actual)) != expected {
			t.Fatalf("package source %s = %q, expected actual worktree source %s", ref, expected, actual)
		}
	}
	if provenance.ArchiveSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("provenance archive digest mismatch")
	}
	found := false
	for _, f := range reader.File {
		if f.Name == "meta.so" && f.UncompressedSize64 > 0 {
			found = true
			lib, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			extractedPath := filepath.Join(out, "meta.so")
			extracted, err := os.Create(extractedPath)
			if err != nil {
				lib.Close()
				t.Fatal(err)
			}
			hash := sha256.New()
			_, err = io.Copy(io.MultiWriter(hash, extracted), lib)
			lib.Close()
			extracted.Close()
			if err != nil || hex.EncodeToString(hash.Sum(nil)) != provenance.LibrarySHA256 {
				t.Fatal("provenance extracted-library digest mismatch")
			}
			info, err := exec.Command("go", "version", "-m", extractedPath).CombinedOutput()
			if err != nil || strings.Contains(string(info), "vcs.revision=") {
				t.Fatalf("automatic VCS stamping may misbind a nested worktree: %v\n%s", err, info)
			}
			verifyNativeReplayStdout(t, extractedPath)
		}
		if strings.Contains(f.Name, "/") || strings.HasSuffix(f.Name, ".h") {
			t.Fatalf("unexpected archive entry %q", f.Name)
		}
	}
	if !found {
		t.Fatal("archive lacks meta.so")
	}
}
