package app

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWriteAppConfig(t *testing.T) {
	t.Run("writes_yaml_when_config_present", func(t *testing.T) {
		dir := t.TempDir()
		config := map[string]interface{}{
			"feature_flags": map[string]interface{}{
				"demo_mode": true,
			},
		}

		if err := writeAppConfig(dir, config, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "app.yaml"))
		if err != nil {
			t.Fatalf("read app.yaml: %v", err)
		}

		var got map[string]interface{}
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal app.yaml: %v", err)
		}
		flags, ok := got["feature_flags"].(map[string]interface{})
		if !ok || flags["demo_mode"] != true {
			t.Errorf("unexpected structure: %v", got)
		}
	})

	t.Run("removes_file_when_config_nil", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.yaml")

		if err := os.WriteFile(path, []byte("old: config"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := writeAppConfig(dir, nil, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("expected app.yaml to be removed")
		}
	})

	t.Run("nil_config_noop_when_no_file", func(t *testing.T) {
		dir := t.TempDir()

		if err := writeAppConfig(dir, nil, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("writeAppConfig: %v", err)
		}
	})

}
