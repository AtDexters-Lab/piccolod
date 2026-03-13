package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"piccolod/internal/fsutil"
)

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{Enabled: true, NamekURL: defaultNamekURL}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}
