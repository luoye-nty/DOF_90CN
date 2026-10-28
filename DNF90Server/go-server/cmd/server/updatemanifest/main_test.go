package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, root string, relative string, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunStagesEveryFileAndWritesTheManifest(t *testing.T) {
	root := t.TempDir()
	for _, path := range updateFiles {
		writeFixture(t, root, path, "content of "+path)
	}
	out := filepath.Join(t.TempDir(), "dist")

	if err := run(root, out, "v1.2.3", "abcdef0123456789"); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(out, manifestName))
	if err != nil {
		t.Fatalf("manifest was not written: %v", err)
	}
	if raw[0] == 0xEF {
		t.Fatal("manifest carries a byte order mark")
	}
	var manifest updateManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest does not parse: %v", err)
	}
	if manifest.SchemaVersion != 2 {
		t.Fatalf("schemaVersion = %d, want 2", manifest.SchemaVersion)
	}
	if manifest.Tag != "v1.2.3" || manifest.Commit != "abcdef0123456789" {
		t.Fatalf("tag/commit = %q/%q", manifest.Tag, manifest.Commit)
	}
	if len(manifest.Files) != len(updateFiles) {
		t.Fatalf("manifest has %d files, want %d", len(manifest.Files), len(updateFiles))
	}

	seen := map[string]bool{}
	for _, entry := range manifest.Files {
		if seen[entry.Asset] {
			t.Fatalf("duplicate asset name %s", entry.Asset)
		}
		seen[entry.Asset] = true
		if strings.ContainsAny(entry.Asset, `/\`) {
			t.Fatalf("asset name %s carries a separator", entry.Asset)
		}
		staged, err := os.Stat(filepath.Join(out, entry.Asset))
		if err != nil {
			t.Fatalf("asset %s was not staged: %v", entry.Asset, err)
		}
		if staged.Size() != entry.Size {
			t.Fatalf("asset %s staged %d bytes, manifest says %d", entry.Asset, staged.Size(), entry.Size)
		}
		if len(entry.SHA256) != 64 || entry.SHA256 != strings.ToUpper(entry.SHA256) {
			t.Fatalf("asset %s hash %q is not uppercase SHA256", entry.Asset, entry.SHA256)
		}
	}
}

func TestRunRefusesAMissingFile(t *testing.T) {
	root := t.TempDir()
	for _, path := range updateFiles[:len(updateFiles)-1] {
		writeFixture(t, root, path, "x")
	}
	err := run(root, filepath.Join(t.TempDir(), "dist"), "v0.0.1", "c0ffee")
	if err == nil || !strings.Contains(err.Error(), updateFiles[len(updateFiles)-1]) {
		t.Fatalf("err = %v, want the missing file named", err)
	}
}

func TestRunRequiresTagAndOut(t *testing.T) {
	if err := run(t.TempDir(), "", "v1.0.0", "c"); err == nil {
		t.Fatal("an empty -out was accepted")
	}
	if err := run(t.TempDir(), t.TempDir(), "", "c"); err == nil {
		t.Fatal("an empty -tag was accepted")
	}
}

func TestAssetNamesCannotCollide(t *testing.T) {
	entries, err := func() ([]updateEntry, error) {
		root := t.TempDir()
		for _, path := range updateFiles {
			writeFixture(t, root, path, "x")
		}
		return collectUpdateFiles(root)
	}()
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, entry := range entries {
		if entry.Asset == manifestName {
			t.Fatalf("asset %s collides with the manifest name", entry.Asset)
		}
	}
}

// The install side lives in another repository (DNF90Hub 的 update.go) and rejects
// the whole update package when any manifest entry falls outside its fixed
// whitelist. A path added here that it does not accept would not fail any build —
// it would fail on players' machines, at install time, with "清单中的安装路径不合法".
// This test is the only thing standing between those two facts.
func TestUpdateFilesStayInsideTheHubWhitelist(t *testing.T) {
	for _, path := range updateFiles {
		allowed := false
		for _, exact := range updatePathWhitelist.Exact {
			if path == exact {
				allowed = true
				break
			}
		}
		for _, prefix := range updatePathWhitelist.Prefixes {
			if strings.HasPrefix(path, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Errorf("update path %q is outside the install-side whitelist %v (+%v); "+
				"DNF90Hub would reject the entire package. Widen its whitelist and ship that first.",
				path, updatePathWhitelist.Prefixes, updatePathWhitelist.Exact)
		}
	}
}

// Forward slashes only, no traversal, no rooting — the same shape the install side
// validates. Keeping it here means a bad path is caught by go test, not by a
// player's failed update.
func TestUpdateFilesAreCleanRelativePaths(t *testing.T) {
	for _, path := range updateFiles {
		if path == "" || strings.Contains(path, `\`) {
			t.Errorf("update path %q must use forward slashes", path)
		}
		if strings.HasPrefix(path, "/") || strings.Contains(path, ":") {
			t.Errorf("update path %q must be relative", path)
		}
		for _, part := range strings.Split(path, "/") {
			if part == "" || part == "." || part == ".." {
				t.Errorf("update path %q has an empty or traversing segment", path)
			}
		}
	}
}
