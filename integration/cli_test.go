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
	if len(manifest.Images) != 2 {
		t.Fatalf("manifest has %d images, want 2", len(manifest.Images))
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
	if !strings.Contains(string(applied), "pull_policy") || !strings.Contains(string(applied), "bundle.local/cli-roundtrip") {
		t.Fatalf("applied Compose was not rewritten for local images: %s", applied)
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
    case "${3:-}" in
      nginx:latest)
        printf '%s\n' '[{"Id":"sha256:1111111111111111111111111111111111111111111111111111111111111111","RepoDigests":["nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"],"Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'","RootFS":{"Layers":["sha256:layer-web"]}}]'
        ;;
      redis:7)
        printf '%s\n' '[{"Id":"sha256:2222222222222222222222222222222222222222222222222222222222222222","RepoDigests":["redis@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],"Architecture":"'"$FAKE_IMAGE_ARCH"'","Os":"'"$FAKE_IMAGE_OS"'","RootFS":{"Layers":["sha256:layer-worker"]}}]'
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
