package main

// 本地配置读写：程序同目录 config.json（便携）。
// 注意：配对码仅在选择“记住”时写入；证书指纹必写，用于下次连接校验。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type LocalConfig struct {
	ServerIP        string `json:"server_ip"`
	Port            int    `json:"port"`
	RememberCode    bool   `json:"remember_code"`
	PairingCode     string `json:"pairing_code,omitempty"`
	CertFingerprint string `json:"cert_fingerprint,omitempty"`
	AcctPVFPath     string `json:"acct_pvf_path,omitempty"` // 账号管理页本地搜索用 PVF 绝对路径（空=每次手动选择）
}

func DefaultLocalConfig() *LocalConfig {
	return &LocalConfig{
		ServerIP:     "",
		Port:         18113,
		RememberCode: false,
	}
}

// ConfigStore 管理 config.json 的加载与保存（加锁防并发写坏）。
type ConfigStore struct {
	mu   sync.Mutex
	path string
}

func NewConfigStore(path string) *ConfigStore {
	return &ConfigStore{path: path}
}

// DefaultConfigPath 返回程序同目录的 config.json 绝对路径。
func DefaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func (cs *ConfigStore) Load() (*LocalConfig, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	data, err := os.ReadFile(cs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultLocalConfig(), nil
		}
		return nil, err
	}
	cfg := DefaultLocalConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Port <= 0 {
		cfg.Port = 18113
	}
	return cfg, nil
}

func (cs *ConfigStore) Save(cfg *LocalConfig) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.path, data, 0o600)
}
