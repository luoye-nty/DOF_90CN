package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type projectPaths struct {
	projectRoot          string
	goServerRoot         string
	runtimeRoot          string
	runtimeBin           string
	runtimeConfig        string
	runtimeConfigs       string
	runtimeData          string
	runtimeLogs          string
	runtimeState         string
	instance             string
	instanceExample      string
	assetManifest        string
	clientManifest       string
	mysqlManifest        string
	vcRuntimeManifest    string
	vcRuntimeRoot        string
	channelAsset         string
	serverExe            string
	doctorExe            string
	launcherExe          string
	controlExe           string
	processState         string
	binaryInstallState   string
	mysqlRoot            string
	mysqlServer          string
	mysqlData            string
	mysqlConfig          string
	mysqlPIDFile         string
	mysqldExe            string
	mysqlProcessState    string
	mysqlInstallState    string
	mysqlDataState       string
	mysqlLegacyDataState string
}

func discoverPaths() (projectPaths, error) {
	if configured := strings.TrimSpace(os.Getenv("DNF90_PROJECT_ROOT")); configured != "" {
		root, err := filepath.Abs(configured)
		if err != nil {
			return projectPaths{}, fmt.Errorf("resolve DNF90_PROJECT_ROOT: %w", err)
		}
		if isProjectRoot(root) {
			return newProjectPaths(root), nil
		}
		return projectPaths{}, fmt.Errorf("DNF90_PROJECT_ROOT is not a DNF90 project: %s", root)
	}

	var starts []string
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	seen := make(map[string]struct{})
	for _, start := range starts {
		current, err := filepath.Abs(start)
		if err != nil {
			continue
		}
		for {
			key := strings.ToLower(filepath.Clean(current))
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				if isProjectRoot(current) {
					return newProjectPaths(current), nil
				}
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return projectPaths{}, errors.New("cannot find DNF90 project root; set DNF90_PROJECT_ROOT")
}

// isProjectRoot recognises every shape this controller ships in. The instance
// template is the mandatory first anchor so an unrelated directory that merely
// happens to contain a runtime/ folder is never mistaken for a project root;
// one of the three second anchors must then confirm which shape it is.
func isProjectRoot(root string) bool {
	if !isRegularFile(filepath.Join(root, "runtime", "templates", "instance.example.json")) {
		return false
	}
	// Runtime-only installations distributed by the hub/launcher carry neither
	// the Go module nor RELEASE_INFO.json: the hub's update whitelist may only
	// write runtime/{bin,assets,templates,patch}/ plus changlog.md, so a release
	// marker in the root can never reach such a tree. The asset manifest is the
	// marker those installations do have, and it is one of the very files the
	// hub itself requires before it will run this controller at all.
	if isRegularFile(filepath.Join(root, "runtime", "assets", "manifest.json")) {
		return true
	}
	if isRegularFile(filepath.Join(root, "go-server", "go.mod")) {
		return true
	}
	// Binary-only external releases intentionally contain no source tree.
	return isRegularFile(filepath.Join(root, "RELEASE_INFO.json")) &&
		isRegularFile(filepath.Join(root, "runtime", "bin", platformExecutableName("DNF90Control")))
}

// hasGoServerSource reports whether this installation carries the go-server
// module. Runtime-only installations do not, and every code path that invokes
// the Go toolchain must check this first instead of failing inside a go command
// with a module error that says nothing about the real cause.
func hasGoServerSource(paths projectPaths) bool {
	return isRegularFile(filepath.Join(paths.goServerRoot, "go.mod"))
}

func platformExecutableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func newProjectPaths(root string) projectPaths {
	root = filepath.Clean(root)
	runtimeRoot := filepath.Join(root, "runtime")
	return projectPaths{
		projectRoot:          root,
		goServerRoot:         filepath.Join(root, "go-server"),
		runtimeRoot:          runtimeRoot,
		runtimeBin:           filepath.Join(runtimeRoot, "bin"),
		runtimeConfig:        filepath.Join(runtimeRoot, "config"),
		runtimeConfigs:       filepath.Join(runtimeRoot, "configs"),
		runtimeData:          filepath.Join(runtimeRoot, "data", "dnf"),
		runtimeLogs:          filepath.Join(runtimeRoot, "logs"),
		runtimeState:         filepath.Join(runtimeRoot, "state"),
		instance:             filepath.Join(runtimeRoot, "config", "instance.json"),
		instanceExample:      filepath.Join(runtimeRoot, "templates", "instance.example.json"),
		assetManifest:        filepath.Join(runtimeRoot, "assets", "manifest.json"),
		clientManifest:       filepath.Join(runtimeRoot, "assets", "client-compatibility.json"),
		mysqlManifest:        filepath.Join(runtimeRoot, "assets", "mysql-portable.json"),
		vcRuntimeManifest:    filepath.Join(runtimeRoot, "assets", "vcruntime-app-local.json"),
		vcRuntimeRoot:        filepath.Join(runtimeRoot, "vendor", "vcruntime", "x64"),
		channelAsset:         filepath.Join(runtimeRoot, "assets", "channel_info.etc"),
		serverExe:            filepath.Join(runtimeRoot, "bin", platformExecutableName("DNF90Server")),
		doctorExe:            filepath.Join(runtimeRoot, "bin", platformExecutableName("DNF90Doctor")),
		launcherExe:          filepath.Join(runtimeRoot, "bin", platformExecutableName("DNF90Launcher")),
		controlExe:           filepath.Join(runtimeRoot, "bin", platformExecutableName("DNF90Control")),
		processState:         filepath.Join(runtimeRoot, "state", "server-process.json"),
		binaryInstallState:   filepath.Join(runtimeRoot, "state", "binary-install.json"),
		mysqlRoot:            filepath.Join(runtimeRoot, "mysql"),
		mysqlServer:          filepath.Join(runtimeRoot, "mysql", "server"),
		mysqlData:            filepath.Join(runtimeRoot, "mysql", "data"),
		mysqlConfig:          filepath.Join(runtimeRoot, "config", "mysql.ini"),
		mysqlPIDFile:         filepath.Join(runtimeRoot, "state", "mysql.pid"),
		mysqldExe:            filepath.Join(runtimeRoot, "mysql", "server", "bin", "mysqld.exe"),
		mysqlProcessState:    filepath.Join(runtimeRoot, "state", "mysql-process.json"),
		mysqlInstallState:    filepath.Join(runtimeRoot, "mysql", "server", ".dnf90-install.json"),
		mysqlDataState:       filepath.Join(runtimeRoot, "mysql", "data-state.json"),
		mysqlLegacyDataState: filepath.Join(runtimeRoot, "mysql", "data", ".dnf90-data.json"),
	}
}

func (p projectPaths) ensureDirectories() error {
	for _, path := range []string{
		p.runtimeRoot,
		p.runtimeBin,
		p.runtimeConfig,
		p.runtimeConfigs,
		filepath.Join(p.runtimeConfigs, "dnf"),
		filepath.Join(p.runtimeConfigs, "servergroup"),
		p.runtimeData,
		p.runtimeLogs,
		p.runtimeState,
		p.mysqlRoot,
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", path, err)
		}
	}
	return nil
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
