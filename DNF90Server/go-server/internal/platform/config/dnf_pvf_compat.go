package config

import "strings"

const (
	defaultPVFMaxBytes        int64 = 1024 * 1024 * 1024
	defaultPVFChunkCacheBytes int64 = 96 * 1024 * 1024
	defaultPVFTextCacheBytes  int64 = 64 * 1024 * 1024
)

// PVFSection is the DNF project extension that keeps Script.pvf loading in
// the generic platform bootstrap without coupling the framework to DNF logic.
type PVFSection struct {
	Enabled         bool   `toml:"enabled"`
	Path            string `toml:"path"`
	MaxBytes        int64  `toml:"max_bytes"`
	PreloadChunks   bool   `toml:"preload_chunks"`
	ChunkCacheBytes int64  `toml:"chunk_cache_bytes"`
	TextCacheBytes  int64  `toml:"text_cache_bytes"`
}

func (c *ServiceConfig) normalizePVF() {
	if c.PVF.MaxBytes == 0 {
		c.PVF.MaxBytes = defaultPVFMaxBytes
	}
	if c.PVF.ChunkCacheBytes == 0 {
		c.PVF.ChunkCacheBytes = defaultPVFChunkCacheBytes
	}
	if c.PVF.TextCacheBytes == 0 {
		c.PVF.TextCacheBytes = defaultPVFTextCacheBytes
	}
}

func (c *ServiceConfig) trimPVF() {
	c.PVF.Path = strings.TrimSpace(c.PVF.Path)
}

func (c ServiceConfig) validatePVF(errs *[]error) {
	if c.PVF.Enabled {
		requireString(errs, "pvf.path", c.PVF.Path)
	}
	if c.PVF.MaxBytes != -1 {
		requirePositiveInt64(errs, "pvf.max_bytes", c.PVF.MaxBytes)
	}
	requirePositiveInt64(errs, "pvf.chunk_cache_bytes", c.PVF.ChunkCacheBytes)
	requirePositiveInt64(errs, "pvf.text_cache_bytes", c.PVF.TextCacheBytes)
}
