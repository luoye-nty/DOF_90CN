// Command updatemanifest assembles the launcher update package for one release.
//
// It collects the fixed set of files a shipped installation refreshes between
// full packages, copies each one into a flat staging directory under a
// collision-free asset name, and writes server-update.json describing them. A
// GitHub release then carries the staging directory's contents verbatim, and the
// launcher resolves every download from the manifest's own directory.
//
// The manifest is schema version 2: entries carry an install path relative to
// the project root plus the flat release asset name, because GitHub release
// assets have no directories. PVF, MySQL and the commercial client are never
// part of an update package.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const manifestName = "server-update.json"

// updateFiles is the complete update payload, deliberately fixed: adding a path
// here is a compatibility decision, not a packaging detail. Paths are relative
// to the project root, forward slashes.
//
// The install side (DNF90Hub 的 update.go，另一个仓库) 只接受落在
// runtime/{bin,assets,templates,patch}/ 之内的路径，加上 changlog.md 这个单文件白
// 名单，超出的一律以"清单中的安装路径不合法"拒收整个更新包。updatePathWhitelist
// 就是那份名单的副本，TestUpdateFilesStayInsideTheHubWhitelist 逐条比对。
//
// 这些路径同时也是安装位置，必须和 cmd/server/control/paths.go 解析的位置一致——
// 否则文件会被装到没人读的地方，留下一份看着像权威、实际陈旧的副本。
var updateFiles = []string{
	"runtime/bin/DNF90Control.exe",
	"runtime/bin/DNF90Doctor.exe",
	"runtime/bin/DNF90Launcher.exe",
	"runtime/bin/DNF90Server.exe",
	"runtime/assets/channel_info.etc",
	"runtime/assets/client-compatibility.json",
	"runtime/assets/manifest.json",
	"runtime/assets/mysql-portable.json",
	"runtime/assets/vcruntime-app-local.json",
	"runtime/templates/instance.example.json",
	"runtime/patch/90CN.dll",
	"runtime/patch/90CNLua.dll",
	"changlog.md",
}

// updatePathWhitelist mirrors DNF90Hub 的 updatePathPrefixes / updateExactPaths。
// 它不参与打包，只供测试比对——一旦这边加了对面不认的路径，测试先失败，而不是等到
// 玩家的启动器拒收整个更新包。
var updatePathWhitelist = struct {
	Prefixes []string
	Exact    []string
}{
	Prefixes: []string{"runtime/bin/", "runtime/assets/", "runtime/templates/", "runtime/patch/"},
	Exact:    []string{"changlog.md"},
}

type updateManifest struct {
	SchemaVersion int           `json:"schemaVersion"`
	Tag           string        `json:"tag"`
	Commit        string        `json:"commit"`
	Platform      string        `json:"platform"`
	GoVersion     string        `json:"goVersion"`
	Files         []updateEntry `json:"files"`
}

type updateEntry struct {
	Path   string `json:"path"`
	Asset  string `json:"asset"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func main() {
	root := flag.String("root", "..", "project root holding runtime/ and changlog.md")
	out := flag.String("out", "", "staging directory receiving the flat assets and the manifest")
	tag := flag.String("tag", "", "release tag, e.g. v1.0.0")
	commit := flag.String("commit", "", "commit the binaries were built from")
	flag.Parse()

	if err := run(*root, *out, *tag, *commit); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func run(root, out, tag, commit string) error {
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("-out is required")
	}
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("-tag is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	entries, err := collectUpdateFiles(absRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	for _, entry := range entries {
		source := filepath.Join(absRoot, filepath.FromSlash(entry.Path))
		if err := copyFile(source, filepath.Join(out, entry.Asset)); err != nil {
			return err
		}
	}

	manifest := updateManifest{
		SchemaVersion: 2,
		Tag:           tag,
		Commit:        commit,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		GoVersion:     runtime.Version(),
		Files:         entries,
	}
	if err := writeUpdateManifest(filepath.Join(out, manifestName), manifest); err != nil {
		return err
	}
	fmt.Printf("update package staged: %d files + %s in %s\n", len(entries), manifestName, out)
	return nil
}

func collectUpdateFiles(root string) ([]updateEntry, error) {
	entries := make([]updateEntry, 0, len(updateFiles))
	seenAssets := map[string]string{}
	for _, path := range updateFiles {
		asset := assetNameFor(path)
		if previous, taken := seenAssets[asset]; taken {
			return nil, fmt.Errorf("asset name collision: %s and %s both map to %s", previous, path, asset)
		}
		seenAssets[asset] = path
		if asset == manifestName {
			return nil, fmt.Errorf("update file %s collides with the manifest asset %s", path, manifestName)
		}

		full := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Stat(full)
		if err != nil {
			return nil, fmt.Errorf("update file is missing: %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("update file is not a regular file: %s", path)
		}
		digest, err := fileSHA256(full)
		if err != nil {
			return nil, err
		}
		entries = append(entries, updateEntry{
			Path:   path,
			Asset:  asset,
			Size:   info.Size(),
			SHA256: digest,
		})
	}
	return entries, nil
}

// assetNameFor flattens an install path into a release asset name. GitHub
// release assets have no directories, so the separator becomes a double
// underscore; the mapping is never inverted — the manifest records both sides.
func assetNameFor(path string) string {
	return strings.ReplaceAll(path, "/", "__")
}

// writeUpdateManifest writes BOM-free UTF-8: the launcher parses this as JSON,
// which rejects a byte order mark.
func writeUpdateManifest(path string, manifest updateManifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return strings.ToUpper(fmt.Sprintf("%x", digest.Sum(nil))), nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s: %w", filepath.Base(source), err)
	}
	return out.Close()
}
