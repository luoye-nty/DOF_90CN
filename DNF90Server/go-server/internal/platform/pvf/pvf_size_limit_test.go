package pvf

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMaxBytesIsOneGiB(t *testing.T) {
	const want int64 = 1024 * 1024 * 1024
	if DefaultMaxBytes != want {
		t.Fatalf("DefaultMaxBytes = %d, want %d", DefaultMaxBytes, want)
	}
}

func TestLoadAllowsExplicitlyDisabledSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Script.pvf")
	if err := os.WriteFile(path, []byte("pvf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Options{Path: path, MaxBytes: 1}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limited load error = %v, want ErrTooLarge", err)
	}
	bundle, err := Load(Options{Path: path, MaxBytes: NoMaxBytes})
	if err != nil {
		t.Fatalf("unlimited load rejected: %v", err)
	}
	if got := bundle.Snapshot().Size; got != 3 {
		t.Fatalf("loaded size = %d, want 3", got)
	}
}

func TestLoadRejectsUnknownNegativeSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Script.pvf")
	if err := os.WriteFile(path, []byte("pvf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Options{Path: path, MaxBytes: NoMaxBytes - 1}); err == nil {
		t.Fatal("unknown negative size limit was accepted")
	}
}
