package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCreateRejectsMissingEnvFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	err := runCreate([]string{
		"--compose", filepath.Join(t.TempDir(), "compose.yaml"),
		"--out", filepath.Join(t.TempDir(), "bundle"),
		"--key-file", filepath.Join(t.TempDir(), "key.bin"),
		"--signing-key", filepath.Join(t.TempDir(), "signing.seed"),
		"--env-file", missing,
	})
	if err == nil || !strings.Contains(err.Error(), "--env-file") {
		t.Fatalf("runCreate error = %v, want missing env-file error", err)
	}
}
