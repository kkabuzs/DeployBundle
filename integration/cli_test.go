package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openai/deploybundle/internal/keys"
	"github.com/openai/deploybundle/pkg/bundle"
)

// TestCLIRoundTrip exercises the two command line tools together without a
// Docker daemon. The fake CLI implements only the Docker calls made by the
// tools, while carrying Compose bytes through the same encrypted stream used
// in production. Packaging must not require locally available Docker images.
func TestCLIRoundTrip(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()

	archiveSource := filepath.Join(work, "archive.tar")
	if err := os.WriteFile(archiveSource, []byte("fake docker archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(work, "camera-device")
	if err := os.WriteFile(devicePath, []byte("device placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(work, "host-data")
	if err := os.Mkdir(hostPath, 0700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(work, "compose.json")
	compose := map[string]any{
		"services": map[string]any{
			"web": map[string]any{
				"image":   "nginx:latest",
				"volumes": []string{hostPath + ":/var/lib/app:ro"},
				"devices": []string{devicePath + ":/dev/camera:r"},
			},
			"worker": map[string]any{"image": "redis:7", "pull_policy": "missing",
				"volumes":  []any{map[string]any{"type": "volume", "source": "db_data", "target": "/data"}},
				"networks": []any{"backend"}},
		},
		"volumes":  map[string]any{"db_data": map[string]any{"name": "existing-db-data"}},
		"networks": map[string]any{"backend": map[string]any{"name": "existing-backend-network"}},
	}
	composeBytes, err := json.Marshal(compose)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, append(composeBytes, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	fakeDocker := filepath.Join(work, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	loadOutput := filepath.Join(work, "loaded-archive.tar")
	composeOutput := filepath.Join(work, "applied-compose.json")
	logOutput := filepath.Join(work, "docker.log")
	fakeEnv := []string{
		"FAKE_COMPOSE_CONFIG=" + composePath,
		"FAKE_ARCHIVE_SOURCE=" + archiveSource,
		"FAKE_LOAD_OUTPUT=" + loadOutput,
		"FAKE_COMPOSE_OUTPUT=" + composeOutput,
		"FAKE_DOCKER_LOG=" + logOutput,
		"FAKE_IMAGE_OS=" + runtime.GOOS,
		"FAKE_IMAGE_ARCH=" + runtime.GOARCH,
	}

	pack := filepath.Join(work, "deploy-pack")
	run := filepath.Join(work, "deploy-run")
	buildBinary(t, root, "./cmd/deploy-pack", pack, work)
	buildBinary(t, root, "./cmd/deploy-run", run, work)

	keyPath := filepath.Join(work, "key.bin")
	privatePath := filepath.Join(work, "signing.seed")
	publicPath := filepath.Join(work, "signing.pub")
	runTool(t, root, fakeEnv, pack, "keygen", "--key-out", keyPath, "--signing-private-out", privatePath, "--signing-public-out", publicPath)

	bundleDir := filepath.Join(work, "bundle")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	runTool(t, root, fakeEnv, pack, "create", "--compose", composePath, "--out", bundleDir,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "cli-roundtrip",
		"--release", "test", "--platform", platform, "--docker", fakeDocker)

	manifestRaw, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bundle.ParseManifest(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.Assets != nil || manifest.Payload.AssetsArtifact != "" {
		t.Fatalf("schema v1 bundle unexpectedly contains assets: %+v", manifest)
	}
	if bytes.Contains(manifestRaw, []byte(`"assets"`)) || bytes.Contains(manifestRaw, []byte(`"assetsArtifact"`)) {
		t.Fatalf("schema v1 manifest contains asset fields: %s", manifestRaw)
	}
	if manifest.BundleType != "full" || manifest.UpdateKind != "config-only" || len(manifest.Images) != 0 || len(manifest.RequiredImages) != 0 || manifest.Payload.ImagesArtifact != "" || manifest.BaseManifest != "" {
		t.Fatalf("new package is not a standalone configuration snapshot: %+v", manifest)
	}
	if len(manifest.Capabilities.Devices) != 1 || len(manifest.Capabilities.HostPaths) != 1 {
		t.Fatalf("capabilities = devices:%d hostPaths:%d, want one of each", len(manifest.Capabilities.Devices), len(manifest.Capabilities.HostPaths))
	}
	if manifest.TargetPlatform.OS != runtime.GOOS || manifest.TargetPlatform.Architecture != runtime.GOARCH {
		t.Fatalf("manifest target = %s/%s, want %s/%s", manifest.TargetPlatform.OS, manifest.TargetPlatform.Architecture, runtime.GOOS, runtime.GOARCH)
	}

	inspectOutput := runTool(t, root, fakeEnv, run, "inspect", "--bundle", bundleDir, "--verify-key", publicPath)
	if !strings.Contains(inspectOutput, "images: 0") {
		t.Fatalf("inspect output missing image count: %s", inspectOutput)
	}
	if strings.Contains(inspectOutput, "assets:") || strings.Contains(inspectOutput, "schemaVersion:") {
		t.Fatalf("schema v1 inspect output changed: %s", inspectOutput)
	}
	verifyOutput := runTool(t, root, fakeEnv, run, "verify", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)
	if !strings.Contains(verifyOutput, "host preflight verified") {
		t.Fatalf("verify output missing success marker: %s", verifyOutput)
	}
	runTool(t, root, fakeEnv, run, "apply", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)

	if _, err := os.Stat(loadOutput); !os.IsNotExist(err) {
		t.Fatalf("default apply unexpectedly loaded images: %v", err)
	}
	applied, err := os.ReadFile(composeOutput)
	if err != nil {
		t.Fatalf("read fake compose input: %v", err)
	}
	if bundle.SHA256Hex(applied) != manifest.Compose.PackagedSHA256 {
		t.Fatalf("docker compose received bytes with hash %s, want %s", bundle.SHA256Hex(applied), manifest.Compose.PackagedSHA256)
	}
	var appliedModel map[string]any
	if err := json.Unmarshal(applied, &appliedModel); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustCanonical(t, appliedModel), mustCanonical(t, compose)) {
		t.Fatalf("packaging changed original image/policy/volume/network declarations: %s", applied)
	}
	log, err := os.ReadFile(logOutput)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "--env-file") {
		t.Fatalf("no-env create unexpectedly passed --env-file: %s", log)
	}
	if strings.Contains(string(log), "--project-directory") || strings.Contains(string(log), "--profile") {
		t.Fatalf("schema v1 commands contain v2/profile arguments: %s", log)
	}
	if strings.Contains(string(log), "tag ") || strings.Contains(string(log), "image inspect ") || strings.Contains(string(log), "save ") || strings.Contains(string(log), "load\n") || strings.Contains(string(log), " pull ") {
		t.Fatalf("standalone create/default apply unexpectedly operated on images: %s", log)
	}
	if !strings.Contains(string(log), "compose --project-name cli-roundtrip-") || !strings.Contains(string(log), " -f - up -d --pull never --no-build\n") {
		t.Fatalf("schema v1 apply command changed: %s", log)
	}
}

func TestCLIEnvironmentSnapshot(t *testing.T) {
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker CLI is required for Compose environment interpolation")
	}
	root := moduleRoot(t)
	work := t.TempDir()

	archiveSource := filepath.Join(work, "archive.tar")
	if err := os.WriteFile(archiveSource, []byte("fake docker archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(work, "runtime.env")
	if err := os.WriteFile(envFile, []byte("FROM_ENV_FILE=from-env-file\nSECRET_FROM_FILE=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	packEnvFile := filepath.Join(work, "pack.env")
	if err := os.WriteFile(packEnvFile, []byte("FROM_PACK_FILE=from-pack-env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(work, "compose.yaml")
	compose := `services:
  web:
    image: nginx:latest
    env_file:
      - ./runtime.env
    environment:
      FROM_PACK_FILE: "${FROM_PACK_FILE:?FROM_PACK_FILE is required}"
      FROM_SHELL: "${DEPLOYBUNDLE_TEST_SHELL:?DEPLOYBUNDLE_TEST_SHELL is required}"
      INLINE_VALUE: direct
`
	if err := os.WriteFile(composePath, []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}

	fakeDocker := filepath.Join(work, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	loadOutput := filepath.Join(work, "loaded-archive.tar")
	composeOutput := filepath.Join(work, "applied-compose.json")
	fakeEnv := []string{
		"REAL_DOCKER_BIN=" + realDocker,
		"FAKE_COMPOSE_CONFIG=" + composePath,
		"FAKE_ARCHIVE_SOURCE=" + archiveSource,
		"FAKE_LOAD_OUTPUT=" + loadOutput,
		"FAKE_COMPOSE_OUTPUT=" + composeOutput,
		"FAKE_IMAGE_OS=" + runtime.GOOS,
		"FAKE_IMAGE_ARCH=" + runtime.GOARCH,
		"DEPLOYBUNDLE_TEST_SHELL=from-shell",
	}

	pack := filepath.Join(work, "deploy-pack")
	run := filepath.Join(work, "deploy-run")
	buildBinary(t, root, "./cmd/deploy-pack", pack, work)
	buildBinary(t, root, "./cmd/deploy-run", run, work)

	keyPath := filepath.Join(work, "key.bin")
	privatePath := filepath.Join(work, "signing.seed")
	publicPath := filepath.Join(work, "signing.pub")
	runTool(t, root, fakeEnv, pack, "keygen", "--key-out", keyPath, "--signing-private-out", privatePath, "--signing-public-out", publicPath)

	bundleDir := filepath.Join(work, "bundle")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	runTool(t, root, fakeEnv, pack, "create", "--compose", composePath, "--out", bundleDir,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "env-snapshot",
		"--release", "test", "--platform", platform, "--docker", fakeDocker, "--env-file", packEnvFile)

	ciphertext, err := os.ReadFile(filepath.Join(bundleDir, "compose.enc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"from-pack-env", "from-shell", "from-env-file", "secret-value"} {
		if bytes.Contains(ciphertext, []byte(value)) {
			t.Fatalf("encrypted Compose contains plaintext environment value %q", value)
		}
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	composeFile, err := os.Open(filepath.Join(bundleDir, "compose.enc"))
	if err != nil {
		t.Fatal(err)
	}
	var decrypted bytes.Buffer
	if err := bundle.Decrypt(&decrypted, composeFile, key); err != nil {
		_ = composeFile.Close()
		t.Fatalf("decrypt packaged Compose: %v", err)
	}
	if err := composeFile.Close(); err != nil {
		t.Fatal(err)
	}
	packagedEnv := composeEnvironment(t, decrypted.Bytes())
	assertEnvironment(t, packagedEnv, "FROM_PACK_FILE", "from-pack-env")
	assertEnvironment(t, packagedEnv, "FROM_SHELL", "from-shell")
	assertEnvironment(t, packagedEnv, "FROM_ENV_FILE", "from-env-file")
	assertEnvironment(t, packagedEnv, "SECRET_FROM_FILE", "secret-value")
	assertEnvironment(t, packagedEnv, "INLINE_VALUE", "direct")

	runTool(t, root, fakeEnv, run, "apply", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)
	applied, err := os.ReadFile(composeOutput)
	if err != nil {
		t.Fatal(err)
	}
	appliedEnv := composeEnvironment(t, applied)
	assertEnvironment(t, appliedEnv, "FROM_PACK_FILE", "from-pack-env")
	assertEnvironment(t, appliedEnv, "FROM_SHELL", "from-shell")
	assertEnvironment(t, appliedEnv, "FROM_ENV_FILE", "from-env-file")
	assertEnvironment(t, appliedEnv, "SECRET_FROM_FILE", "secret-value")
	assertEnvironment(t, appliedEnv, "INLINE_VALUE", "direct")
}

func TestCLIImageUpdateIsExplicit(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()
	composePath := filepath.Join(work, "compose.json")
	writeFile(t, composePath, []byte(`{"services":{"web":{"image":"nginx:latest","pull_policy":"always"},"worker":{"image":"redis:7"}},"volumes":{"db":{"name":"keep-existing-db"}},"networks":{"backend":{"name":"keep-existing-network"}}}`))
	fakeDocker := filepath.Join(work, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(work, "docker.log")
	loadPath := filepath.Join(work, "loaded.tar")
	composeOutput := filepath.Join(work, "applied.json")
	env := []string{"FAKE_COMPOSE_CONFIG=" + composePath, "FAKE_DOCKER_LOG=" + logPath,
		"FAKE_LOAD_OUTPUT=" + loadPath, "FAKE_COMPOSE_OUTPUT=" + composeOutput}
	pack, run := filepath.Join(work, "pack"), filepath.Join(work, "run")
	buildBinary(t, root, "./cmd/deploy-pack", pack, work)
	buildBinary(t, root, "./cmd/deploy-run", run, work)
	keyPath, privatePath, publicPath := filepath.Join(work, "key.bin"), filepath.Join(work, "signing.seed"), filepath.Join(work, "signing.pub")
	runTool(t, root, env, pack, "keygen", "--key-out", keyPath, "--signing-private-out", privatePath, "--signing-public-out", publicPath)
	bundleDir := filepath.Join(work, "bundle")
	runTool(t, root, env, pack, "create", "--compose", composePath, "--out", bundleDir,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "explicit-images", "--docker", fakeDocker)
	common := []string{"apply", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker}
	resetLog := func() { t.Helper(); writeFile(t, logPath, nil); _ = os.Remove(composeOutput); _ = os.Remove(loadPath) }
	readLog := func() string {
		t.Helper()
		b, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	withEnv := func(extra string) []string { return append(append([]string{}, env...), extra) }
	update := append(append([]string{}, common...), "--update-images")

	// A save archive left beside a configuration package must not trigger an
	// image update unless the caller explicitly requests it.
	plainArchive := []byte("docker save archive canary\n")
	writeFile(t, filepath.Join(bundleDir, "images.tar"), plainArchive)
	resetLog()
	runTool(t, root, env, run, common...)
	if log := readLog(); strings.Contains(log, "load") || strings.Contains(log, " pull ") {
		t.Fatalf("default apply prepared images: %s", log)
	}

	resetLog()
	runTool(t, root, env, run, update...)
	loaded, err := os.ReadFile(loadPath)
	if err != nil || !bytes.Equal(loaded, plainArchive) {
		t.Fatalf("plain save archive did not reach docker load: %q, %v", loaded, err)
	}
	if log := readLog(); strings.Contains(log, " pull ") || strings.Index(log, "load") > strings.Index(log, " up ") {
		t.Fatalf("plain archive flow is incorrect: %s", log)
	}

	resetLog()
	runToolFailure(t, root, withEnv("FAKE_FAIL_LOAD=1"), run, update...)
	if log := readLog(); strings.Contains(log, " up ") || strings.Contains(log, " pull ") {
		t.Fatalf("failed load continued to pull/up: %s", log)
	}
	if err := os.Remove(filepath.Join(bundleDir, "images.tar")); err != nil {
		t.Fatal(err)
	}

	resetLog()
	runTool(t, root, env, run, append(update, "--service", "worker")...)
	log := readLog()
	if !strings.Contains(log, " pull --policy always --include-deps worker\n") || !strings.Contains(log, " up -d --pull never --no-build worker\n") || strings.Index(log, " pull ") > strings.Index(log, " up ") {
		t.Fatalf("pull/preparation order or service selection is wrong: %s", log)
	}

	resetLog()
	output := runTool(t, root, withEnv("FAKE_FAIL_PULL=1"), run, update...)
	if !strings.Contains(strings.ToLower(output), "warning") && !strings.Contains(output, "警告") {
		t.Fatalf("pull fallback was not explicitly reported: %s", output)
	}
	if log := readLog(); !strings.Contains(log, " pull ") || !strings.Contains(log, " up -d --pull never --no-build") {
		t.Fatalf("pull failure did not fall back to local Compose: %s", log)
	}

	resetLog()
	bothFail := append(append([]string{}, env...), "FAKE_FAIL_PULL=1", "FAKE_FAIL_UP=1")
	runToolFailure(t, root, bothFail, run, update...)
	resetLog()
	runToolFailure(t, root, withEnv("FAKE_FAIL_UP=1"), run, common...)
	if log := readLog(); strings.Contains(log, " pull ") || strings.Contains(log, "load") {
		t.Fatalf("missing local image silently triggered image preparation: %s", log)
	}

	// Corrupt configuration/signature must stop even explicit image updates
	// before any Docker command can have side effects.
	originalCompose, err := os.ReadFile(filepath.Join(bundleDir, "compose.enc"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte{}, originalCompose...)
	tampered[len(tampered)/2] ^= 1
	writeFile(t, filepath.Join(bundleDir, "compose.enc"), tampered)
	resetLog()
	runToolFailure(t, root, env, run, update...)
	if log := readLog(); log != "" {
		t.Fatalf("tampered configuration reached Docker: %s", log)
	}
	writeFile(t, filepath.Join(bundleDir, "compose.enc"), originalCompose)
	originalSig, err := os.ReadFile(filepath.Join(bundleDir, "manifest.sig"))
	if err != nil {
		t.Fatal(err)
	}
	sig := append([]byte{}, originalSig...)
	sig[0] ^= 1
	writeFile(t, filepath.Join(bundleDir, "manifest.sig"), sig)
	resetLog()
	runToolFailure(t, root, env, run, update...)
	if log := readLog(); log != "" {
		t.Fatalf("invalid signature reached Docker: %s", log)
	}
	writeFile(t, filepath.Join(bundleDir, "manifest.sig"), originalSig)

	// Previously issued encrypted-image bundles remain readable. A default
	// config update does not need to read their image archive, while an
	// explicit image update still authenticates/decrypts it before Docker load.
	legacyDir := filepath.Join(work, "legacy")
	if err := os.Mkdir(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := bundle.ParseManifest(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	legacy.UpdateKind = "full"
	legacy.Images = []bundle.ImageArtifact{{Service: "web", OriginalRef: "nginx:latest", LocalTag: "nginx:latest", LocalImageID: "sha256:legacy-image"}}
	legacy.Payload.Compression = "none"
	legacy.Payload.ImagesArtifact = "images.tar.none.enc"
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	legacyArchive := []byte("legacy encrypted docker save archive\n")
	if err := bundle.Encrypt(&encrypted, bytes.NewReader(legacyArchive), key); err != nil {
		t.Fatal(err)
	}
	legacy.Payload.ImagesSHA256 = bundle.SHA256Hex(encrypted.Bytes())
	legacy.Payload.ImagesBytes = int64(encrypted.Len())
	legacyManifest, err := bundle.MarshalManifest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := keys.ReadPrivate(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	legacySig, err := bundle.SignManifest(legacy, priv)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(legacyDir, "manifest.json"), legacyManifest)
	writeFile(t, filepath.Join(legacyDir, "manifest.sig"), legacySig)
	writeFile(t, filepath.Join(legacyDir, "compose.enc"), originalCompose)
	legacyArgs := append([]string{}, common...)
	for i, arg := range legacyArgs {
		if arg == bundleDir {
			legacyArgs[i] = legacyDir
		}
	}
	resetLog()
	runTool(t, root, env, run, legacyArgs...)
	if log := readLog(); strings.Contains(log, "load") || strings.Contains(log, " pull ") {
		t.Fatalf("default legacy apply touched image archive: %s", log)
	}
	writeFile(t, filepath.Join(legacyDir, legacy.Payload.ImagesArtifact), encrypted.Bytes())
	resetLog()
	runTool(t, root, env, run, append(legacyArgs, "--update-images")...)
	loaded, err = os.ReadFile(loadPath)
	if err != nil || !bytes.Equal(loaded, legacyArchive) {
		t.Fatalf("legacy archive load = %q, %v", loaded, err)
	}
	legacyCiphertext := append([]byte{}, encrypted.Bytes()...)
	legacyCiphertext[len(legacyCiphertext)/2] ^= 1
	writeFile(t, filepath.Join(legacyDir, legacy.Payload.ImagesArtifact), legacyCiphertext)
	resetLog()
	runToolFailure(t, root, env, run, append(legacyArgs, "--update-images")...)
	if log := readLog(); log != "" {
		t.Fatalf("tampered legacy archive reached Docker: %s", log)
	}
	runToolFailure(t, root, env, run, "verify", "--bundle", legacyDir, "--key-file", keyPath, "--verify-key", publicPath)
	writeFile(t, filepath.Join(legacyDir, "images.tar"), plainArchive)
	resetLog()
	runTool(t, root, env, run, append(legacyArgs, "--update-images")...)
	loaded, err = os.ReadFile(loadPath)
	if err != nil || !bytes.Equal(loaded, plainArchive) {
		t.Fatalf("plain archive did not take precedence over legacy encrypted archive: %q, %v", loaded, err)
	}
}

func mustCanonical(t *testing.T, v any) []byte {
	t.Helper()
	b, err := bundle.MarshalCanonical(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCLIAssetsAndStagedApply(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()
	project := filepath.Join(work, "project")
	if err := os.MkdirAll(filepath.Join(project, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	canary := "deploybundle-private-asset-canary"
	nginxAsset := filepath.Join(project, "assets", "nginx.conf")
	appAsset := filepath.Join(project, "assets", "app.conf")
	if err := os.WriteFile(nginxAsset, []byte(canary+"-nginx"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appAsset, []byte(canary+"-app"), 0600); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(work, "external")
	if err := os.Mkdir(hostPath, 0700); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(project, "compose.json")
	compose := map[string]any{
		"services": map[string]any{
			"web": map[string]any{
				"image": "nginx:latest",
				"volumes": []any{
					map[string]any{"type": "bind", "source": "./assets/nginx.conf", "target": "/etc/nginx/nginx.conf", "read_only": true},
					map[string]any{"type": "bind", "source": hostPath, "target": "/external"},
					map[string]any{"type": "volume", "source": "data", "target": "/data"},
				},
			},
			"worker": map[string]any{"image": "redis:7"},
		},
		"configs": map[string]any{"app": map[string]any{"file": "./assets/app.conf"}},
	}
	composeBytes, err := json.Marshal(compose)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(composePath, append(composeBytes, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	archiveSource := filepath.Join(work, "archive.tar")
	if err := os.WriteFile(archiveSource, []byte("fake docker archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeDocker := filepath.Join(work, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	loadOutput := filepath.Join(work, "loaded-archive.tar")
	composeOutput := filepath.Join(work, "applied-compose.json")
	logOutput := filepath.Join(work, "docker.log")
	fakeEnv := []string{
		"FAKE_COMPOSE_CONFIG=" + composePath,
		"FAKE_ARCHIVE_SOURCE=" + archiveSource,
		"FAKE_LOAD_OUTPUT=" + loadOutput,
		"FAKE_COMPOSE_OUTPUT=" + composeOutput,
		"FAKE_DOCKER_LOG=" + logOutput,
		"FAKE_IMAGE_OS=" + runtime.GOOS,
		"FAKE_IMAGE_ARCH=" + runtime.GOARCH,
	}

	pack := filepath.Join(work, "deploy-pack")
	run := filepath.Join(work, "deploy-run")
	buildBinary(t, root, "./cmd/deploy-pack", pack, work)
	buildBinary(t, root, "./cmd/deploy-run", run, work)
	keyPath := filepath.Join(work, "key.bin")
	privatePath := filepath.Join(work, "signing.seed")
	publicPath := filepath.Join(work, "signing.pub")
	runTool(t, root, fakeEnv, pack, "keygen", "--key-out", keyPath, "--signing-private-out", privatePath, "--signing-public-out", publicPath)

	bundleDir := filepath.Join(work, "bundle-assets")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	createOutput := runTool(t, root, fakeEnv, pack, "create",
		"--compose", composePath, "--out", bundleDir, "--key-file", keyPath,
		"--signing-key", privatePath, "--bundle-id", "assets/roundtrip", "--release", "test",
		"--profile", "business-label", "--compose-profile", "release", "--compose-profile", "gateway", "--package-assets",
		"--platform", platform, "--docker", fakeDocker)
	if strings.Contains(createOutput, canary) {
		t.Fatalf("create output leaked asset content: %s", createOutput)
	}

	manifestRaw, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bundle.ParseManifest(manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 2 || manifest.Profile != "business-label" || len(manifest.Assets) != 2 || len(manifest.Images) != 0 {
		t.Fatalf("unexpected v2 manifest summary: schema=%d profile=%q assets=%d images=%d", manifest.SchemaVersion, manifest.Profile, len(manifest.Assets), len(manifest.Images))
	}
	for _, name := range []string{"assets/app.conf", "assets/nginx.conf"} {
		found := false
		for _, asset := range manifest.Assets {
			found = found || asset.Path == name
		}
		if !found {
			t.Fatalf("manifest missing asset %q: %+v", name, manifest.Assets)
		}
	}
	bundleFiles, err := os.ReadDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range bundleFiles {
		raw, err := os.ReadFile(filepath.Join(bundleDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(canary)) {
			t.Fatalf("bundle file %s contains plaintext asset content", entry.Name())
		}
	}
	packLog, err := os.ReadFile(logOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packLog), "compose --profile release --profile gateway") || strings.Contains(string(packLog), "--profile business-label") {
		t.Fatalf("compose profile and manifest profile were not kept separate: %s", packLog)
	}

	inspectOutput := runTool(t, root, fakeEnv, run, "inspect", "--bundle", bundleDir, "--verify-key", publicPath)
	if !strings.Contains(inspectOutput, "schemaVersion: 2") || !strings.Contains(inspectOutput, "assets: 2") || !strings.Contains(inspectOutput, "asset bytes:") || !strings.Contains(inspectOutput, "asset artifact bytes:") {
		t.Fatalf("inspect output lacks v2 asset summary: %s", inspectOutput)
	}
	if strings.Contains(inspectOutput, "assets/nginx.conf") || strings.Contains(inspectOutput, manifest.Assets[0].SHA256) || strings.Contains(inspectOutput, canary) {
		t.Fatalf("inspect output leaked asset details: %s", inspectOutput)
	}
	verifyOutput := runTool(t, root, fakeEnv, run, "verify", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)
	if !strings.Contains(verifyOutput, "encrypted streams") || strings.Contains(verifyOutput, canary) {
		t.Fatalf("unexpected verify output: %s", verifyOutput)
	}
	if _, err := os.Stat(filepath.Join(work, "runtime-assets")); !os.IsNotExist(err) {
		t.Fatalf("verify persisted an asset runtime directory: %v", err)
	}

	noneBundle := filepath.Join(work, "bundle-assets-none")
	noneCreateOutput := runTool(t, root, fakeEnv, pack, "create",
		"--compose", composePath, "--out", noneBundle, "--key-file", keyPath,
		"--signing-key", privatePath, "--bundle-id", "assets-none", "--release", "test",
		"--package-assets", "--compression", "none", "--platform", platform, "--docker", fakeDocker)
	if strings.Contains(noneCreateOutput, canary) {
		t.Fatalf("none-compression create output leaked asset content: %s", noneCreateOutput)
	}
	noneManifestRaw, err := os.ReadFile(filepath.Join(noneBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	noneManifest, err := bundle.ParseManifest(noneManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if noneManifest.SchemaVersion != 2 || noneManifest.Payload.Compression != "none" || len(noneManifest.Assets) != 2 {
		t.Fatalf("unexpected none-compression manifest: %+v", noneManifest.Payload)
	}
	runTool(t, root, fakeEnv, run, "verify", "--bundle", noneBundle, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)

	assetsRoot := filepath.Join(work, "runtime-assets")
	if err := os.Remove(logOutput); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	applyOutput := runTool(t, root, fakeEnv, run, "apply", "--bundle", bundleDir, "--key-file", keyPath,
		"--verify-key", publicPath, "--docker", fakeDocker, "--assets-dir", assetsRoot,
		"--service", "web", "--service", "worker")
	if strings.Contains(applyOutput, canary) {
		t.Fatalf("apply output leaked asset content: %s", applyOutput)
	}
	entries, err := os.ReadDir(assetsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("runtime root entries = %v, want one final directory", entries)
	}
	final := filepath.Join(assetsRoot, entries[0].Name())
	for _, asset := range manifest.Assets {
		raw, err := os.ReadFile(filepath.Join(final, filepath.FromSlash(asset.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if bundle.SHA256Hex(raw) != asset.SHA256 {
			t.Fatalf("runtime asset %s hash mismatch", asset.Path)
		}
	}
	applyLog, err := os.ReadFile(logOutput)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := " -f - up -d --pull never --no-build web worker\n"
	if !strings.Contains(string(applyLog), "compose --project-name ") || !strings.Contains(string(applyLog), "--project-directory "+final) || !strings.Contains(string(applyLog), wantSuffix) {
		t.Fatalf("staged apply argv is incorrect: %s", applyLog)
	}
	if strings.Contains(string(applyLog), "--no-deps") || strings.Contains(string(applyLog), "--remove-orphans") {
		t.Fatalf("staged apply added unwanted Compose behavior: %s", applyLog)
	}
	appliedCompose, err := os.ReadFile(composeOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(appliedCompose, []byte("./assets/nginx.conf")) || !bytes.Contains(appliedCompose, []byte(hostPath)) {
		t.Fatalf("applied Compose lost relative or absolute bind semantics: %s", appliedCompose)
	}

	if err := os.Remove(logOutput); err != nil {
		t.Fatal(err)
	}
	runTool(t, root, fakeEnv, run, "apply", "--bundle", bundleDir, "--key-file", keyPath,
		"--verify-key", publicPath, "--docker", fakeDocker, "--assets-dir", assetsRoot, "--service", "worker")
	entries, err = os.ReadDir(assetsRoot)
	if err != nil || len(entries) != 1 || filepath.Join(assetsRoot, entries[0].Name()) != final {
		t.Fatalf("repeated apply did not reuse final directory: %v, %v", entries, err)
	}
	repeatLog, err := os.ReadFile(logOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(repeatLog), " -f - up -d --pull never --no-build worker\n") {
		t.Fatalf("second service stage argv is incorrect: %s", repeatLog)
	}

	// A configuration release must remain standalone while reusing the same
	// asset content directory. A manifest/release change alone must not change
	// bind-mount paths and recreate unrelated services.
	compose["services"].(map[string]any)["web"].(map[string]any)["environment"] = map[string]any{"CONFIG_RELEASE": "next"}
	writeFile(t, composePath, mustCanonical(t, compose))
	nextBundle := filepath.Join(work, "bundle-assets-next")
	runTool(t, root, fakeEnv, pack, "create", "--compose", composePath, "--out", nextBundle,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "assets/roundtrip",
		"--release", "next", "--package-assets", "--platform", platform, "--docker", fakeDocker)
	nextRaw, err := os.ReadFile(filepath.Join(nextBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	nextManifest, err := bundle.ParseManifest(nextRaw)
	if err != nil {
		t.Fatal(err)
	}
	if nextManifest.BaseManifest != "" || nextManifest.AssetsBaseManifest != "" || nextManifest.Payload.AssetsArtifact == "" || len(nextManifest.Images) != 0 {
		t.Fatalf("new configuration release retained a base or omitted its assets: %+v", nextManifest)
	}
	runTool(t, root, fakeEnv, run, "apply", "--bundle", nextBundle, "--key-file", keyPath,
		"--verify-key", publicPath, "--docker", fakeDocker, "--assets-dir", assetsRoot)
	entries, err = os.ReadDir(assetsRoot)
	if err != nil || len(entries) != 1 || filepath.Join(assetsRoot, entries[0].Name()) != final {
		t.Fatalf("configuration-only release changed unchanged-asset mount directory: %v, %v", entries, err)
	}

	assertFailsBeforeDocker := func(name string, args ...string) string {
		t.Helper()
		if err := os.Remove(logOutput); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		output := runToolFailure(t, root, fakeEnv, run, args...)
		if strings.Contains(output, canary) {
			t.Fatalf("%s failure leaked asset content: %s", name, output)
		}
		if raw, err := os.ReadFile(logOutput); err == nil && len(bytes.TrimSpace(raw)) != 0 {
			t.Fatalf("%s invoked Docker before failure: %s", name, raw)
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return output
	}
	assertVerifyFails := func(name string) {
		t.Helper()
		output := runToolFailure(t, root, fakeEnv, run, "verify", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath)
		if strings.Contains(output, canary) {
			t.Fatalf("%s verify failure leaked asset content: %s", name, output)
		}
	}
	commonApply := []string{"apply", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker, "--assets-dir", assetsRoot}
	unknownRoot := filepath.Join(work, "unknown-service-assets")
	unknownApply := []string{"apply", "--bundle", bundleDir, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker, "--assets-dir", unknownRoot, "--service", "missing"}
	assertFailsBeforeDocker("unknown service", unknownApply...)
	if _, err := os.Stat(unknownRoot); !os.IsNotExist(err) {
		t.Fatalf("unknown service created an asset root: %v", err)
	}

	wrongKey := filepath.Join(work, "wrong-key.bin")
	if err := os.WriteFile(wrongKey, bytes.Repeat([]byte{0x77}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	wrongKeyApply := append([]string(nil), commonApply...)
	for i := range wrongKeyApply {
		if wrongKeyApply[i] == keyPath {
			wrongKeyApply[i] = wrongKey
		}
	}
	assertFailsBeforeDocker("wrong key", wrongKeyApply...)

	conflictPath := filepath.Join(final, filepath.FromSlash(manifest.Assets[0].Path))
	originalRuntime, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflictPath, []byte("runtime-conflict"), os.FileMode(manifest.Assets[0].Mode)); err != nil {
		t.Fatal(err)
	}
	assertFailsBeforeDocker("runtime conflict", commonApply...)
	if err := os.WriteFile(conflictPath, originalRuntime, os.FileMode(manifest.Assets[0].Mode)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(conflictPath, os.FileMode(manifest.Assets[0].Mode)); err != nil {
		t.Fatal(err)
	}

	assetArtifact := filepath.Join(bundleDir, manifest.Payload.AssetsArtifact)
	originalArtifact, err := os.ReadFile(assetArtifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(assetArtifact); err != nil {
		t.Fatal(err)
	}
	assertFailsBeforeDocker("missing asset artifact", commonApply...)
	assertVerifyFails("missing asset artifact")
	if err := os.WriteFile(assetArtifact, originalArtifact, 0600); err != nil {
		t.Fatal(err)
	}

	tampered := append([]byte(nil), originalArtifact...)
	tampered[len(tampered)/2] ^= 1
	if err := os.WriteFile(assetArtifact, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	assertFailsBeforeDocker("tampered asset artifact", commonApply...)
	assertVerifyFails("tampered asset artifact")
	if err := os.WriteFile(assetArtifact, originalArtifact[:len(originalArtifact)-1], 0600); err != nil {
		t.Fatal(err)
	}
	assertFailsBeforeDocker("truncated asset artifact", commonApply...)
	assertVerifyFails("truncated asset artifact")
	if err := os.WriteFile(assetArtifact, originalArtifact, 0600); err != nil {
		t.Fatal(err)
	}
}

func composeEnvironment(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse Compose JSON: %v", err)
	}
	services, ok := document["services"].(map[string]any)
	if !ok {
		t.Fatalf("Compose JSON has no services object: %s", raw)
	}
	web, ok := services["web"].(map[string]any)
	if !ok {
		t.Fatalf("Compose JSON has no web service: %s", raw)
	}
	environment, ok := web["environment"].(map[string]any)
	if !ok {
		t.Fatalf("Compose JSON has no environment map: %s", raw)
	}
	return environment
}

func assertEnvironment(t *testing.T, environment map[string]any, name, want string) {
	t.Helper()
	got, ok := environment[name].(string)
	if !ok || got != want {
		t.Fatalf("environment[%q] = %#v, want %q", name, environment[name], want)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	// The integration package lives directly below the module root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

func buildBinary(t *testing.T, root, packagePath, output, work string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", output, packagePath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(work, "gocache"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", packagePath, err, output)
	}
}

func runTool(t *testing.T, root string, fakeEnv []string, binary string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), fakeEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s failed: %v\n%s", binary, strings.Join(args, " "), err, output.String())
	}
	return output.String()
}

func runToolFailure(t *testing.T, root string, fakeEnv []string, binary string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), fakeEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err == nil {
		t.Fatalf("%s %s unexpectedly succeeded\n%s", binary, strings.Join(args, " "), output.String())
	}
	return output.String()
}

const fakeDockerScript = `#!/bin/sh
set -eu
if [ -n "${FAKE_DOCKER_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
fi
case "${1:-}" in
  compose)
    operation=""
    for arg in "$@"; do
      case "$arg" in config|pull|up) operation="$arg" ;; esac
    done
    case "$operation" in
      config)
        if [ -n "${REAL_DOCKER_BIN:-}" ]; then
          exec "$REAL_DOCKER_BIN" "$@"
        fi
        cat "$FAKE_COMPOSE_CONFIG"
        ;;
      pull)
        cat > /dev/null
        if [ "${FAKE_FAIL_PULL:-0}" = "1" ]; then
          echo 'fake pull failure' >&2
          exit 1
        fi
        ;;
      up)
        cat > "$FAKE_COMPOSE_OUTPUT"
        if [ "${FAKE_FAIL_UP:-0}" = "1" ]; then
          echo 'fake local image missing' >&2
          exit 1
        fi
        ;;
      *) echo 'unsupported compose operation' >&2; exit 2 ;;
    esac
    ;;
  load)
    cat > "$FAKE_LOAD_OUTPUT"
    if [ "${FAKE_FAIL_LOAD:-0}" = "1" ]; then
      echo 'fake invalid save archive' >&2
      exit 1
    fi
    printf '%s\n' 'Loaded image: fake'
    ;;
  *)
    echo "unexpected image operation: ${1:-}" >&2
    exit 2
    ;;
esac
`
