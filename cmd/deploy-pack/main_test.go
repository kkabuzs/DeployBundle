package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/deploybundle/internal/assets"
)

func TestHelpIsChineseAndAvailableBeforeRequiredArguments(t *testing.T) {
	var out bytes.Buffer
	printCreateUsage(&out)
	if !strings.Contains(out.String(), "必填参数") || !strings.Contains(out.String(), "--compose") {
		t.Fatalf("create help = %q", out.String())
	}
	if err := runCreate([]string{"--help"}); err != nil {
		t.Fatalf("create help returned error: %v", err)
	}
}

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

func TestRunCreateRejectsInvalidComposeProfilesAndPositionals(t *testing.T) {
	base := []string{"--compose", "compose.yaml", "--out", "bundle", "--key-file", "key", "--signing-key", "signing"}
	for _, extra := range [][]string{
		{"--compose-profile", "api", "--compose-profile", "api"},
		{"--compose-profile="},
		{"unexpected"},
	} {
		args := append(append([]string(nil), base...), extra...)
		if err := runCreate(args); err == nil {
			t.Fatalf("invalid create arguments accepted: %v", extra)
		}
	}
}

func TestCollectAssetsClassifiesSources(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "nginx.conf"), []byte("server {}"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.conf"), []byte("setting=true"), 0600); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "external.conf")
	config := map[string]any{
		"services": map[string]any{
			"web": map[string]any{"volumes": []any{
				map[string]any{"type": "bind", "source": "./assets/nginx.conf", "target": "/etc/nginx/nginx.conf"},
				map[string]any{"type": "bind", "source": absolute, "target": "/external"},
				map[string]any{"type": "volume", "source": "data", "target": "/data"},
			}},
		},
		"configs": map[string]any{"app": map[string]any{"file": "./assets/app.conf"}},
	}
	got, err := assets.Collect(config, composePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Manifest.Path != "assets/app.conf" || got[1].Manifest.Path != "assets/nginx.conf" {
		t.Fatalf("collected assets = %+v", got)
	}
	volume := config["services"].(map[string]any)["web"].(map[string]any)["volumes"].([]any)
	if volume[1].(map[string]any)["source"] != absolute {
		t.Fatalf("absolute bind changed: %#v", volume[1])
	}
}

func TestCollectAssetsRejectsSymlinkHardlinkDirectoryAndTraversal(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(root, "hardlink")
	if err := os.Link(regular, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"./symlink", "./hardlink", "./directory", "../outside"} {
		config := map[string]any{"services": map[string]any{"web": map[string]any{"volumes": []any{
			map[string]any{"type": "bind", "source": source, "target": "/config"},
		}}}}
		if _, err := assets.Collect(config, composePath); err == nil {
			t.Fatalf("unsafe source %q was accepted", source)
		}
	}
}
