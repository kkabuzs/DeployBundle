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

	"github.com/openai/deploybundle/pkg/bundle"
)

// TestCLIRoundTrip exercises the two command line tools together without a
// Docker daemon. The fake CLI implements only the Docker calls made by the
// tools, while still carrying the image archive and Compose bytes through the
// same encrypted streams used in production.
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
			"worker": map[string]any{"image": "redis:7"},
		},
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
	if len(manifest.Images) != 2 {
		t.Fatalf("manifest has %d images, want 2", len(manifest.Images))
	}
	for _, image := range manifest.Images {
		if image.LocalTag != image.OriginalRef {
			t.Fatalf("new Bundle rewrote image tag for %s: local=%q original=%q", image.Service, image.LocalTag, image.OriginalRef)
		}
	}
	if len(manifest.Capabilities.Devices) != 1 || len(manifest.Capabilities.HostPaths) != 1 {
		t.Fatalf("capabilities = devices:%d hostPaths:%d, want one of each", len(manifest.Capabilities.Devices), len(manifest.Capabilities.HostPaths))
	}
	if manifest.TargetPlatform.OS != runtime.GOOS || manifest.TargetPlatform.Architecture != runtime.GOARCH {
		t.Fatalf("manifest target = %s/%s, want %s/%s", manifest.TargetPlatform.OS, manifest.TargetPlatform.Architecture, runtime.GOOS, runtime.GOARCH)
	}

	inspectOutput := runTool(t, root, fakeEnv, run, "inspect", "--bundle", bundleDir, "--verify-key", publicPath)
	if !strings.Contains(inspectOutput, "images: 2") {
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

	loaded, err := os.ReadFile(loadOutput)
	if err != nil {
		t.Fatalf("read fake docker load output: %v", err)
	}
	wantArchive, err := os.ReadFile(archiveSource)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, wantArchive) {
		t.Fatalf("docker load received %q, want %q", loaded, wantArchive)
	}
	applied, err := os.ReadFile(composeOutput)
	if err != nil {
		t.Fatalf("read fake compose input: %v", err)
	}
	if bundle.SHA256Hex(applied) != manifest.Compose.PackagedSHA256 {
		t.Fatalf("docker compose received bytes with hash %s, want %s", bundle.SHA256Hex(applied), manifest.Compose.PackagedSHA256)
	}
	if !strings.Contains(string(applied), "pull_policy") || !strings.Contains(string(applied), `"image":"nginx:latest"`) || !strings.Contains(string(applied), `"image":"redis:7"`) {
		t.Fatalf("applied Compose did not preserve original image references: %s", applied)
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
	if strings.Contains(string(log), "tag ") {
		t.Fatalf("new Bundle unexpectedly created synthetic image tags: %s", log)
	}
	if !strings.Contains(string(log), "compose --project-name cli-roundtrip-") || !strings.Contains(string(log), " -f - up -d --pull never\n") {
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

func TestCLIDeltaBundles(t *testing.T) {
	root := moduleRoot(t)
	work := t.TempDir()
	archiveSource := filepath.Join(work, "archive.tar")
	if err := os.WriteFile(archiveSource, []byte("fake docker archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	baseComposePath := filepath.Join(work, "compose-base.json")
	newComposePath := filepath.Join(work, "compose-new.json")
	envComposePath := filepath.Join(work, "compose-env.json")
	writeCompose := func(path, image, value string) {
		t.Helper()
		compose := map[string]any{"services": map[string]any{
			"web":    map[string]any{"image": image, "environment": map[string]any{"VALUE": value}},
			"worker": map[string]any{"image": "redis:7"},
		}}
		raw, err := json.Marshal(compose)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCompose(baseComposePath, "nginx:latest", "one")
	writeCompose(newComposePath, "nginx:new", "two")
	writeCompose(envComposePath, "nginx:latest", "two")

	fakeDocker := filepath.Join(work, "fake-docker")
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerScript), 0700); err != nil {
		t.Fatal(err)
	}
	loadOutput := filepath.Join(work, "loaded-archive.tar")
	composeOutput := filepath.Join(work, "applied-compose.json")
	logOutput := filepath.Join(work, "docker.log")
	baseEnv := []string{
		"FAKE_COMPOSE_CONFIG=" + baseComposePath, "FAKE_ARCHIVE_SOURCE=" + archiveSource,
		"FAKE_LOAD_OUTPUT=" + loadOutput, "FAKE_COMPOSE_OUTPUT=" + composeOutput, "FAKE_DOCKER_LOG=" + logOutput,
		"FAKE_IMAGE_OS=" + runtime.GOOS, "FAKE_IMAGE_ARCH=" + runtime.GOARCH,
	}
	withCompose := func(path string) []string {
		env := append([]string{}, baseEnv...)
		env[0] = "FAKE_COMPOSE_CONFIG=" + path
		return env
	}
	pack := filepath.Join(work, "deploy-pack")
	run := filepath.Join(work, "deploy-run")
	buildBinary(t, root, "./cmd/deploy-pack", pack, work)
	buildBinary(t, root, "./cmd/deploy-run", run, work)
	keyPath := filepath.Join(work, "key.bin")
	privatePath := filepath.Join(work, "signing.seed")
	publicPath := filepath.Join(work, "signing.pub")
	runTool(t, root, baseEnv, pack, "keygen", "--key-out", keyPath, "--signing-private-out", privatePath, "--signing-public-out", publicPath)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	baseBundle := filepath.Join(work, "base")
	runTool(t, root, baseEnv, pack, "create", "--compose", baseComposePath, "--out", baseBundle,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "delta-test", "--release", "r1", "--platform", platform, "--docker", fakeDocker)
	baseManifestRaw, err := os.ReadFile(filepath.Join(baseBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	baseManifest, err := bundle.ParseManifest(baseManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range baseManifest.Images {
		baseManifest.Images[i].LocalTag = "bundle.local/legacy/" + baseManifest.Images[i].Service + ":old"
	}
	legacyBaseManifestPath := filepath.Join(work, "legacy-base-manifest.json")
	legacyBaseManifestRaw, err := bundle.MarshalManifest(baseManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBaseManifestPath, legacyBaseManifestRaw, 0600); err != nil {
		t.Fatal(err)
	}

	// A standalone full Bundle can reuse the already encrypted image archive
	// from a matching full Bundle while still getting a new Compose and signed
	// manifest. This is the delivery path for IP-only/customer configuration changes.
	reusedBundle := filepath.Join(work, "reused-full")
	runTool(t, root, withCompose(envComposePath), pack, "create", "--compose", envComposePath, "--out", reusedBundle,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "delta-test", "--release", "r2-full", "--platform", platform, "--docker", fakeDocker,
		"--reuse-images-from", baseBundle)
	reusedManifestRaw, err := os.ReadFile(filepath.Join(reusedBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	reusedManifest, err := bundle.ParseManifest(reusedManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if reusedManifest.BundleType != "full" || reusedManifest.UpdateKind != "full" || reusedManifest.SchemaVersion != 1 || len(reusedManifest.Images) != 2 || reusedManifest.Payload.ImagesArtifact == "" {
		t.Fatalf("unexpected reused full manifest: schema=%d type=%q images=%d artifact=%q", reusedManifest.SchemaVersion, reusedManifest.BundleType, len(reusedManifest.Images), reusedManifest.Payload.ImagesArtifact)
	}
	baseImageArchive, err := os.ReadFile(filepath.Join(baseBundle, reusedManifest.Payload.ImagesArtifact))
	if err != nil {
		t.Fatal(err)
	}
	reusedImageArchive, err := os.ReadFile(filepath.Join(reusedBundle, reusedManifest.Payload.ImagesArtifact))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reusedImageArchive, baseImageArchive) {
		t.Fatal("reused full Bundle changed the encrypted image archive")
	}
	runTool(t, root, withCompose(envComposePath), run, "verify", "--bundle", reusedBundle, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)

	deltaBundle := filepath.Join(work, "delta")
	deltaEnv := withCompose(newComposePath)
	runTool(t, root, deltaEnv, pack, "create", "--compose", newComposePath, "--out", deltaBundle,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "delta-test", "--release", "r2", "--base-bundle", baseBundle, "--platform", platform, "--docker", fakeDocker)
	deltaManifestRaw, err := os.ReadFile(filepath.Join(deltaBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	deltaManifest, err := bundle.ParseManifest(deltaManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if deltaManifest.SchemaVersion != 3 || deltaManifest.BundleType != "delta" || deltaManifest.UpdateKind != "mixed" || len(deltaManifest.Images) != 1 || len(deltaManifest.RequiredImages) != 2 {
		t.Fatalf("unexpected image delta manifest: schema=%d type=%q images=%d required=%d", deltaManifest.SchemaVersion, deltaManifest.BundleType, len(deltaManifest.Images), len(deltaManifest.RequiredImages))
	}
	runTool(t, root, deltaEnv, run, "verify", "--bundle", deltaBundle, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)
	applyWithoutImageInspection := append(append([]string{}, deltaEnv...), "FAKE_FAIL_IMAGE_INSPECT=1")
	runTool(t, root, applyWithoutImageInspection, run, "apply", "--bundle", deltaBundle, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)

	envDeltaBundle := filepath.Join(work, "env-delta")
	envDeltaEnv := withCompose(envComposePath)
	runTool(t, root, envDeltaEnv, pack, "create", "--compose", envComposePath, "--out", envDeltaBundle,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "delta-test", "--release", "r3", "--base-manifest", legacyBaseManifestPath, "--platform", platform, "--docker", fakeDocker)
	envManifestRaw, err := os.ReadFile(filepath.Join(envDeltaBundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	envManifest, err := bundle.ParseManifest(envManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if envManifest.BundleType != "delta" || envManifest.UpdateKind != "config-only" || len(envManifest.Images) != 0 || envManifest.Payload.ImagesArtifact != "" {
		t.Fatalf("environment-only delta unexpectedly contains images: %+v", envManifest)
	}
	for _, image := range envManifest.RequiredImages {
		if !strings.HasPrefix(image.LocalTag, "bundle.local/legacy/") {
			t.Fatalf("legacy base tag was not preserved for %s: %q", image.Service, image.LocalTag)
		}
	}
	if _, err := os.Stat(filepath.Join(envDeltaBundle, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	envApplyWithoutImageInspection := append(append([]string{}, envDeltaEnv...), "FAKE_FAIL_IMAGE_INSPECT=1")
	runTool(t, root, envApplyWithoutImageInspection, run, "apply", "--bundle", envDeltaBundle, "--key-file", keyPath, "--verify-key", publicPath, "--docker", fakeDocker)

	assetProject := filepath.Join(work, "asset-project")
	if err := os.MkdirAll(filepath.Join(assetProject, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetProject, "assets", "config.txt"), []byte("customer-one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assetCompose := filepath.Join(assetProject, "compose.json")
	assetModel := map[string]any{"services": map[string]any{
		"web": map[string]any{"image": "nginx:latest", "volumes": []any{map[string]any{
			"type": "bind", "source": "./assets/config.txt", "target": "/etc/customer.conf",
		}}},
	}}
	assetRaw, err := json.Marshal(assetModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(assetCompose, append(assetRaw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	assetBase := filepath.Join(work, "asset-base")
	assetEnv := append([]string{}, baseEnv...)
	assetEnv[0] = "FAKE_COMPOSE_CONFIG=" + assetCompose
	runTool(t, root, assetEnv, pack, "create", "--compose", assetCompose, "--out", assetBase,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "asset-delta-test", "--release", "r1", "--platform", platform, "--docker", fakeDocker, "--package-assets")
	assetConfigDelta := filepath.Join(work, "asset-config-delta")
	runTool(t, root, assetEnv, pack, "create", "--compose", assetCompose, "--out", assetConfigDelta,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "asset-delta-test", "--release", "r1-config", "--base-manifest", filepath.Join(assetBase, "manifest.json"), "--platform", platform, "--docker", fakeDocker, "--package-assets")
	assetConfigRaw, err := os.ReadFile(filepath.Join(assetConfigDelta, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	assetConfigManifest, err := bundle.ParseManifest(assetConfigRaw)
	if err != nil {
		t.Fatal(err)
	}
	if assetConfigManifest.UpdateKind != "config-only" || assetConfigManifest.Payload.AssetsArtifact != "" || assetConfigManifest.AssetsBaseManifest == "" {
		t.Fatalf("unchanged assets were not referenced from base: %+v", assetConfigManifest)
	}
	assetDelta := filepath.Join(work, "asset-delta")
	if err := os.WriteFile(filepath.Join(assetProject, "assets", "config.txt"), []byte("customer-two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runTool(t, root, assetEnv, pack, "create", "--compose", assetCompose, "--out", assetDelta,
		"--key-file", keyPath, "--signing-key", privatePath, "--bundle-id", "asset-delta-test", "--release", "r2", "--base-bundle", assetBase, "--platform", platform, "--docker", fakeDocker, "--package-assets")
	assetManifestRaw, err := os.ReadFile(filepath.Join(assetDelta, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	assetManifest, err := bundle.ParseManifest(assetManifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	if assetManifest.SchemaVersion != 3 || assetManifest.BundleType != "delta" || len(assetManifest.Assets) != 1 || assetManifest.Payload.AssetsArtifact == "" {
		t.Fatalf("unexpected asset delta manifest: schema=%d type=%q assets=%d artifact=%q", assetManifest.SchemaVersion, assetManifest.BundleType, len(assetManifest.Assets), assetManifest.Payload.AssetsArtifact)
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
	if manifest.SchemaVersion != 2 || manifest.Profile != "business-label" || len(manifest.Assets) != 2 || len(manifest.Images) != 2 {
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
	wantSuffix := " -f - up -d --pull never web worker\n"
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
	if !strings.Contains(string(repeatLog), " -f - up -d --pull never worker\n") {
		t.Fatalf("second service stage argv is incorrect: %s", repeatLog)
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
    is_config=0
    for arg in "$@"; do
      if [ "$arg" = "config" ]; then
        is_config=1
      fi
    done
    if [ "$is_config" -eq 1 ]; then
      if [ -n "${REAL_DOCKER_BIN:-}" ]; then
        exec "$REAL_DOCKER_BIN" "$@"
      fi
      cat "$FAKE_COMPOSE_CONFIG"
    else
      cat > "$FAKE_COMPOSE_OUTPUT"
      printf '%s\n' 'fake compose up' >&2
    fi
    ;;
  image)
    if [ "${2:-}" != "inspect" ]; then
      echo 'unsupported image operation' >&2
      exit 2
    fi
    if [ "${FAKE_FAIL_IMAGE_INSPECT:-0}" = "1" ]; then
      echo 'image inspect intentionally unavailable during apply' >&2
      exit 1
    fi
    case "${3:-}" in
      nginx:latest)
        printf '%s\n' '[{"Id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","RepoDigests":["nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'","RootFS":{"Layers":["sha256:layer-web"]}}]'
        ;;
      nginx:new)
        printf '%s\n' '[{"Id":"sha256:3333333333333333333333333333333333333333333333333333333333333333","RepoDigests":["nginx@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"],"Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'","RootFS":{"Layers":["sha256:layer-web-new"]}}]'
        ;;
      redis:7)
        printf '%s\n' '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","RepoDigests":["redis@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],"Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'","RootFS":{"Layers":["sha256:layer-worker"]}}]'
        ;;
      bundle.local/*:111111111111)
        printf '%s\n' '[{"Id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'"}]'
        ;;
      bundle.local/*:222222222222)
        printf '%s\n' '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'"}]'
        ;;
      bundle.local/*:333333333333)
        printf '%s\n' '[{"Id":"sha256:3333333333333333333333333333333333333333333333333333333333333333","Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'"}]'
        ;;
      *)
        echo "unknown image ${3:-}" >&2
        exit 1
        ;;
    esac
    ;;
  tag)
    exit 0
    ;;
  save)
    cat "$FAKE_ARCHIVE_SOURCE"
    ;;
  load)
    cat > "$FAKE_LOAD_OUTPUT"
    printf '%s\n' 'Loaded image: fake'
    ;;
  *)
    echo "unsupported docker command: ${1:-}" >&2
    exit 2
    ;;
esac
`
