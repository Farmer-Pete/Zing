package main

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"zing/internal/config"
)

func TestE2EModelsMatchConfigDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "zing.toml")
	body := "user = \"peter\"\ngithub_token = \"ghp_test_token_0123456789\"\n\n[[projects]]\nname = \"zing\"\nrepo = \"git@github.com:x/zing.git\"\npath = \"/home/peter/zing\"\ntracker = \"github\"\n\n[projects.commands]\ntest = \"go test ./...\"\nlint = \"golangci-lint run\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write zing.toml: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	want := map[string]string{
		modelAliasSonnet: cfg.Models.Sonnet, modelAliasOpus: cfg.Models.Opus,
		modelAliasFable: cfg.Models.Fable, modelAliasCodex: cfg.Models.Codex,
	}
	if !maps.Equal(e2eModels, want) {
		t.Errorf("e2eModels = %v, want config defaults %v", e2eModels, want)
	}
}
