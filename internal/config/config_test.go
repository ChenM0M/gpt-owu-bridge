package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPrecedence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	content := "listen: 127.0.0.1:7001\nlog_level: warn\ndata_dir: " + filepath.Join(directory, "yaml") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(
		[]string{"--config", path, "--listen", "127.0.0.1:7003"},
		[]string{"GATE_LISTEN=127.0.0.1:7002", "GATE_LOG_LEVEL=debug"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7003" || cfg.LogLevel != "debug" {
		t.Fatalf("wrong precedence result: listen=%q level=%q", cfg.Listen, cfg.LogLevel)
	}
	if cfg.DataDir != filepath.Join(directory, "yaml") {
		t.Fatalf("YAML data dir lost: %q", cfg.DataDir)
	}
}

func TestLoadRejectsUnknownInputs(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("mystery: value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{"--config", path}, nil); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("expected unknown YAML key error, got %v", err)
	}
	if _, err := Load(nil, []string{"GATE_UNDOCUMENTED=secret"}); err == nil || !strings.Contains(err.Error(), "GATE_UNDOCUMENTED") {
		t.Fatalf("expected unknown environment error, got %v", err)
	}
}

func TestSecretConflictAndRedaction(t *testing.T) {
	const canary = "CANARY-do-not-leak-7f36"
	directory := t.TempDir()
	secretPath := filepath.Join(directory, "owu-token")
	if err := os.WriteFile(secretPath, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(nil, []string{"GATE_OWU_TOKEN=" + canary, "GATE_OWU_TOKEN_FILE=" + secretPath})
	if err == nil {
		t.Fatal("expected token source conflict")
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "file-secret") {
		t.Fatalf("secret leaked in error: %v", err)
	}
	cfg, err := Load(nil, []string{"GATE_OWU_TOKEN=" + canary})
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := cfg.RedactedJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), canary) || !strings.Contains(string(redacted), `"owu_token_set": true`) {
		t.Fatalf("unexpected redacted config: %s", redacted)
	}
}

func TestValidationAndWritableDataDirectory(t *testing.T) {
	if _, err := Load([]string{"--listen", "0.0.0.0:8080"}, nil); err == nil {
		t.Fatal("non-loopback listen address should fail in M0")
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--data-dir", file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDataDir(); err == nil {
		t.Fatal("file used as data directory should fail")
	}
}
