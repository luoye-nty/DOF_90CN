package config

import "testing"

func TestNormalizePVFUsesOneGiBDefaultLimit(t *testing.T) {
	var cfg ServiceConfig
	cfg.normalizePVF()
	const want int64 = 1024 * 1024 * 1024
	if cfg.PVF.MaxBytes != want {
		t.Fatalf("PVF.MaxBytes = %d, want %d", cfg.PVF.MaxBytes, want)
	}
}
