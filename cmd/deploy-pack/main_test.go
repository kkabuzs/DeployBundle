package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/deploybundle/internal/assets"
	"github.com/openai/deploybundle/pkg/bundle"
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

func TestRunCreateRejectsRemovedImageOptions(t *testing.T) {
	for _, name := range []string{"base-bundle", "base-manifest", "reuse-images-from"} {
		t.Run(name, func(t *testing.T) {
			err := runCreate([]string{"--" + name, "old-bundle"})
			if err == nil || !strings.Contains(err.Error(), "--"+name+" has been removed") {
				t.Fatalf("removed option error = %v", err)
			}
		})
	}
}

func TestCreateConfigBundlePreservesComposeWithoutImageOperations(t *testing.T) {
	root := t.TempDir()
	composePath := filepath.Join(root, "compose.json")
	model := map[string]any{
		"services": map[string]any{
			"api": map[string]any{
				"image":       "private.invalid/api:new-version",
				"pull_policy": "always",
				"environment": map[string]any{"ENDPOINT": "config-canary"},
			},
			"db": map[string]any{
				"image":   "private.invalid/database:existing",
				"volumes": []any{map[string]any{"type": "volume", "source": "data", "target": "/data"}},
			},
		},
		"volumes":  map[string]any{"data": map[string]any{"name": "existing_database_data"}},
		"networks": map[string]any{"default": map[string]any{"name": "existing_network"}},
	}
	plain, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, plain, 0600); err != nil {
		t.Fatal(err)
	}
	dockerPath := filepath.Join(root, "docker")
	logPath := filepath.Join(root, "docker.log")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$PACK_TEST_LOG"
if [ "$1" != compose ]; then
  echo 'image access must not occur while packaging Compose' >&2
  exit 1
fi
cat "$PACK_TEST_COMPOSE"
`
	if err := os.WriteFile(dockerPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PACK_TEST_LOG", logPath)
	t.Setenv("PACK_TEST_COMPOSE", composePath)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x37}, 32)
	out := filepath.Join(root, "bundle")
	err = createBundle(context.Background(), createOptions{
		composePath: composePath, outDir: out, dockerBin: dockerPath,
		bundleID: "config-test", release: "r1", profile: "host-integrated-v1",
		compression: "gzip", chunkSize: 4096,
	}, key, private, bundle.TargetPlatform{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := bundle.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.BundleType != "full" || m.UpdateKind != "config-only" || len(m.Images) != 0 || m.Payload.ImagesArtifact != "" || m.BaseManifest != "" || m.AssetsBaseManifest != "" {
		t.Fatalf("unexpected config bundle metadata: %+v", m)
	}
	sig, err := os.ReadFile(filepath.Join(out, "manifest.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifyManifest(m, sig, public); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(filepath.Join(out, "compose.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("config-canary")) {
		t.Fatal("Compose environment appeared in plaintext")
	}
	var decrypted bytes.Buffer
	if err := bundle.Decrypt(&decrypted, bytes.NewReader(ciphertext), key); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(decrypted.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, model) {
		t.Fatalf("packaging changed Compose: got %s", decrypted.String())
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("config-only bundle has %d files, want manifest/signature/compose", len(entries))
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "\n") != 1 || !strings.HasPrefix(string(log), "compose ") {
		t.Fatalf("unexpected Docker operations: %s", log)
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
