package main

import (
	"archive/zip"
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
)

func TestReleaseArchiveAndChecksums(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("release target is linux/amd64")
	}
	out := t.TempDir()
	cmd := exec.Command("bash", "scripts/package-release.sh", out)
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
		}
		if strings.Contains(f.Name, "/") || strings.HasSuffix(f.Name, ".h") {
			t.Fatalf("unexpected archive entry %q", f.Name)
		}
	}
	if !found {
		t.Fatal("archive lacks meta.so")
	}
}
