package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (c *controller) configureClient(directory string) error {
	directory = strings.TrimSpace(directory)
	directory = strings.TrimSpace(strings.Trim(directory, `"`))
	if directory == "" {
		return errors.New("client directory is required")
	}
	if _, err := loadInstance(c.paths); err != nil {
		return err
	}
	clientRoot, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve client directory: %w", err)
	}
	info, err := os.Stat(clientRoot)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("client directory does not exist: %s", clientRoot)
	}
	if !isRegularFile(filepath.Join(clientRoot, "DNF.exe")) &&
		!isRegularFile(filepath.Join(clientRoot, "NoPack.exe")) {
		return fmt.Errorf("DNF.exe or NoPack.exe was not found in client directory: %s", clientRoot)
	}

	cfg, err := decodeInstance(c.paths.instance)
	if err != nil {
		return err
	}
	cfg.Client.Directory = filepath.Clean(clientRoot)
	data, err := marshalInstance(cfg)
	if err != nil {
		return err
	}
	if err := writeFile(c.paths.instance, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "Client directory configured:", cfg.Client.Directory)
	return nil
}
