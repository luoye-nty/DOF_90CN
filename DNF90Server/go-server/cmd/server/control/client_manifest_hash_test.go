package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientManifestPinsShippedPatchHashes keeps runtime/assets/client-compatibility.json
// honest about the two DLLs this repository builds.
//
// This controller validates client files by existence only, so a stale hash
// here costs it nothing and would never be noticed locally. DNF90Hub (another
// repository) is the consumer that compares them, and it treats any mismatch —
// including an absent hash, which compares against the empty string — as a
// permanent failure: its "verify and repair client files" action copies the
// file, reports success, and the very next check fails again on the same
// expectation. That is a defect the server side can only cause and only fix,
// so it is pinned from this side.
//
// Commercial client files (DNF.exe / ijl15*.dll) are deliberately not asserted:
// they are not built here and are not in the repository.
func TestClientManifestPinsShippedPatchHashes(t *testing.T) {
	t.Setenv("DNF90_PROJECT_ROOT", "")
	paths, err := discoverPaths()
	if err != nil {
		t.Fatalf("discoverPaths() error = %v", err)
	}

	var manifest clientManifest
	if err := readStrictJSON(paths.clientManifest, &manifest); err != nil {
		t.Fatalf("read client manifest: %v", err)
	}

	pinned := map[string]string{}
	for _, entry := range manifest.Files {
		for _, name := range entry.Names {
			pinned[strings.ToLower(name)] = entry.SHA256
		}
	}

	for _, name := range []string{"90CN.dll", "90CNLua.dll"} {
		declared, found := pinned[strings.ToLower(name)]
		if !found {
			t.Fatalf("client manifest does not declare %s", name)
		}
		if strings.TrimSpace(declared) == "" {
			t.Fatalf(
				"client manifest has no sha256 for %s; DNF90Hub compares this value and an "+
					"empty one makes its client repair loop forever",
				name,
			)
		}
		shipped := filepath.Join(paths.runtimeRoot, "patch", name)
		data, err := os.ReadFile(shipped)
		if err != nil {
			t.Fatalf("read shipped %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		actual := hex.EncodeToString(sum[:])
		if !strings.EqualFold(actual, declared) {
			t.Fatalf(
				"client manifest sha256 for %s is stale: manifest=%s runtime/patch=%s; "+
					"update runtime/assets/client-compatibility.json after rebuilding the client patch",
				name, strings.ToUpper(declared), strings.ToUpper(actual),
			)
		}
	}
}
