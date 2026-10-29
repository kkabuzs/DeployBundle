package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	assetpkg "github.com/openai/deploybundle/internal/assets"
	"github.com/openai/deploybundle/pkg/bundle"
)

func TestHelpIsChineseAndAvailableBeforeRequiredArguments(t *testing.T) {
	var out bytes.Buffer
	printApplyUsage(&out)
	if !strings.Contains(out.String(), "持久化 Compose 资产根目录") || !strings.Contains(out.String(), "--bundle") {
		t.Fatalf("apply help = %q", out.String())
	}
	if err := runApply([]string{"--help"}); err != nil {
		t.Fatalf("apply help returned error: %v", err)
	}
}

func TestCheckHostCapabilitiesAcceptsExistingResources(t *testing.T) {
	root := t.TempDir()
	hostPath := filepath.Join(root, "data")
	if err := os.Mkdir(hostPath, 0700); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(root, "device")
	if err := os.WriteFile(devicePath, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &bundle.Manifest{Capabilities: bundle.Capabilities{
		Devices:   []bundle.DeviceCapability{{Service: "camera", Host: devicePath, Container: "/dev/camera", Permissions: "r"}},
		HostPaths: []bundle.HostPathCapability{{Service: "web", Source: hostPath, Target: "/data", Mode: "ro"}},
	}}
	if err := checkHostCapabilities(m); err != nil {
		t.Fatalf("existing resources rejected: %v", err)
	}
}

func TestCheckHostCapabilitiesReportsMissingResource(t *testing.T) {
	m := &bundle.Manifest{Capabilities: bundle.Capabilities{
		Devices: []bundle.DeviceCapability{{Service: "camera", Host: "/definitely/missing/device", Container: "/dev/camera", Permissions: "r"}},
	}}
	err := checkHostCapabilities(m)
	if err == nil {
		t.Fatal("missing device accepted")
	}
	if !strings.Contains(err.Error(), "camera") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing device error = %v", err)
	}

	m = &bundle.Manifest{Capabilities: bundle.Capabilities{
		HostPaths: []bundle.HostPathCapability{{Service: "web", Source: "/definitely/missing/path", Target: "/data", Mode: "rw"}},
	}}
	err = checkHostCapabilities(m)
	if err == nil {
		t.Fatal("missing host path accepted")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing host path error = %v", err)
	}
}

func TestArtifactPathRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../compose.enc", `..\\compose.enc`, "nested/compose.enc"} {
		if _, err := artifactPath(t.TempDir(), name); err == nil {
			t.Fatalf("artifact path %q accepted", name)
		}
	}
	if path, err := artifactPath("/tmp/bundle", "compose.enc"); err != nil || path != "/tmp/bundle/compose.enc" {
		t.Fatalf("valid artifact path = %q, %v", path, err)
	}
}

func TestSanitizeProjectKeepsBundleIDsDistinct(t *testing.T) {
	left := sanitizeProject("foo/bar")
	right := sanitizeProject("foo-bar")
	if left == right {
		t.Fatalf("sanitized project names collide: %q", left)
	}
	if !strings.HasPrefix(left, "foo-bar-") || !strings.HasPrefix(right, "foo-bar-") {
		t.Fatalf("unexpected project names: %q, %q", left, right)
	}
}

func TestParseRunOptionsServices(t *testing.T) {
	o, err := parseRunOptions([]string{
		"--bundle", "bundle", "--key-file", "key", "--verify-key", "pub",
		"--service", "database", "--service", "api",
	}, "apply", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(o.services, ",") != "database,api" {
		t.Fatalf("services = %v", o.services)
	}
	for _, args := range [][]string{
		{"--bundle", "bundle", "--key-file", "key", "--verify-key", "pub", "--service", "api", "--service", "api"},
		{"--bundle", "bundle", "--key-file", "key", "--verify-key", "pub", "--service="},
		{"--bundle", "bundle", "--key-file", "key", "--verify-key", "pub", "extra"},
	} {
		if _, err := parseRunOptions(args, "apply", true); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	if _, err := parseRunOptions([]string{"--bundle", "bundle", "--verify-key", "pub", "--assets-dir", "/tmp/assets"}, "verify", false); err == nil {
		t.Fatal("verify accepted apply-only --assets-dir")
	}
}

func TestValidateSelectedServices(t *testing.T) {
	model := []byte(`{"services":{"api":{},"worker":{}}}`)
	if err := validateSelectedServices(model, []string{"worker", "api"}); err != nil {
		t.Fatal(err)
	}
	if err := validateSelectedServices(model, []string{"missing"}); err == nil {
		t.Fatal("unknown service was accepted")
	}
}

func TestReadAssetsPayload(t *testing.T) {
	data := []byte("asset-canary")
	source := assetpkg.Source{Manifest: bundle.AssetArtifact{
		Path: "assets/config.txt", SHA256: bundle.SHA256Hex(data), Bytes: int64(len(data)), Mode: 0640,
	}, Data: data}
	var plain bytes.Buffer
	if err := assetpkg.WriteTar(&plain, []assetpkg.Source{source}); err != nil {
		t.Fatal(err)
	}
	for _, compression := range []string{"none", "gzip"} {
		t.Run(compression, func(t *testing.T) {
			archive := plain.Bytes()
			if compression == "gzip" {
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				if _, err := gz.Write(archive); err != nil {
					t.Fatal(err)
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				archive = compressed.Bytes()
			}
			m := &bundle.Manifest{Assets: []bundle.AssetArtifact{source.Manifest}, Payload: bundle.PayloadSpec{Compression: compression}}
			if err := readAssetsPayload(m, archive, ""); err != nil {
				t.Fatalf("valid archive rejected: %v", err)
			}
		})
	}
}

func TestReadAssetsPayloadRejectsUnsafeTar(t *testing.T) {
	data := []byte("asset-canary")
	entry := bundle.AssetArtifact{Path: "assets/config.txt", SHA256: bundle.SHA256Hex(data), Bytes: int64(len(data)), Mode: 0600}
	cases := []struct {
		name     string
		headers  []tar.Header
		contents [][]byte
	}{
		{name: "traversal", headers: []tar.Header{{Name: "../escape", Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}}, contents: [][]byte{data}},
		{name: "absolute", headers: []tar.Header{{Name: "/escape", Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}}, contents: [][]byte{data}},
		{name: "backslash", headers: []tar.Header{{Name: `assets\\config.txt`, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}}, contents: [][]byte{data}},
		{name: "symlink", headers: []tar.Header{{Name: entry.Path, Mode: 0600, Typeflag: tar.TypeSymlink, Linkname: "target"}}, contents: [][]byte{nil}},
		{name: "hardlink", headers: []tar.Header{{Name: entry.Path, Mode: 0600, Typeflag: tar.TypeLink, Linkname: "target"}}, contents: [][]byte{nil}},
		{name: "directory", headers: []tar.Header{{Name: entry.Path, Mode: 0700, Typeflag: tar.TypeDir}}, contents: [][]byte{nil}},
		{name: "fifo", headers: []tar.Header{{Name: entry.Path, Mode: 0600, Typeflag: tar.TypeFifo}}, contents: [][]byte{nil}},
		{name: "character device", headers: []tar.Header{{Name: entry.Path, Mode: 0600, Typeflag: tar.TypeChar}}, contents: [][]byte{nil}},
		{name: "mode mismatch", headers: []tar.Header{{Name: entry.Path, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}}, contents: [][]byte{data}},
		{name: "duplicate", headers: []tar.Header{
			{Name: entry.Path, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg},
			{Name: entry.Path, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg},
		}, contents: [][]byte{data, data}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for i := range tc.headers {
				if err := tw.WriteHeader(&tc.headers[i]); err != nil {
					t.Fatal(err)
				}
				if len(tc.contents[i]) > 0 {
					if _, err := tw.Write(tc.contents[i]); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			m := &bundle.Manifest{Assets: []bundle.AssetArtifact{entry}, Payload: bundle.PayloadSpec{Compression: "none"}}
			if err := readAssetsPayload(m, archive.Bytes(), ""); err == nil {
				t.Fatal("unsafe tar was accepted")
			}
		})
	}
}

func TestReadAssetsPayloadRejectsTrailingAndTruncatedData(t *testing.T) {
	data := []byte("asset-canary")
	source := assetpkg.Source{Manifest: bundle.AssetArtifact{
		Path: "assets/config.txt", SHA256: bundle.SHA256Hex(data), Bytes: int64(len(data)), Mode: 0600,
	}, Data: data}
	var valid bytes.Buffer
	if err := assetpkg.WriteTar(&valid, []assetpkg.Source{source}); err != nil {
		t.Fatal(err)
	}
	m := &bundle.Manifest{Assets: []bundle.AssetArtifact{source.Manifest}, Payload: bundle.PayloadSpec{Compression: "none"}}
	withTrailing := append(append([]byte(nil), valid.Bytes()...), 1)
	if err := readAssetsPayload(m, withTrailing, ""); err == nil {
		t.Fatal("tar trailing data was accepted")
	}
	if err := readAssetsPayload(m, valid.Bytes()[:512+len(data)-1], ""); err == nil {
		t.Fatal("truncated tar file was accepted")
	}

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(withTrailing); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	m.Payload.Compression = "gzip"
	if err := readAssetsPayload(m, compressed.Bytes(), ""); err == nil {
		t.Fatal("compressed tar trailing data was accepted")
	}
	compressedArtifactTrailing := append(append([]byte(nil), compressed.Bytes()...), 1)
	if err := readAssetsPayload(m, compressedArtifactTrailing, ""); err == nil {
		t.Fatal("compressed artifact trailing data was accepted")
	}
}

func TestMaterializeAssetsPublishesAndReusesExactDirectory(t *testing.T) {
	bundleDir := t.TempDir()
	key := bytes.Repeat([]byte{0x41}, 32)
	data := []byte("private-config")
	source := assetpkg.Source{Manifest: bundle.AssetArtifact{
		Path: "assets/config.txt", SHA256: bundle.SHA256Hex(data), Bytes: int64(len(data)), Mode: 0640,
	}, Data: data}
	var tarData bytes.Buffer
	if err := assetpkg.WriteTar(&tarData, []assetpkg.Source{source}); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	if err := bundle.Encrypt(&encrypted, bytes.NewReader(tarData.Bytes()), key, bundle.WithChunkSize(4096)); err != nil {
		t.Fatal(err)
	}
	assetName := "assets.tar.none.enc"
	if err := os.WriteFile(filepath.Join(bundleDir, assetName), encrypted.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	m := runnerAssetManifest(source.Manifest, assetName, encrypted.Bytes())
	assetsRoot := filepath.Join(t.TempDir(), "runtime")
	final, err := materializeAssets(m, bundleDir, key, assetsRoot)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("final mode = %o, want 700", info.Mode().Perm())
	}
	got, err := os.ReadFile(filepath.Join(final, filepath.FromSlash(source.Manifest.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("materialized data = %q", got)
	}
	if reused, err := materializeAssets(m, bundleDir, key, assetsRoot); err != nil || reused != final {
		t.Fatalf("reuse = %q, %v; want %q", reused, err, final)
	}
	if err := os.WriteFile(filepath.Join(final, filepath.FromSlash(source.Manifest.Path)), []byte("changed"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeAssets(m, bundleDir, key, assetsRoot); err == nil {
		t.Fatal("modified runtime directory was silently reused")
	}
	matches, err := filepath.Glob(filepath.Join(assetsRoot, ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging directories remain: %v", matches)
	}
}

func TestMaterializeAssetsReusesBaseManifestDirectory(t *testing.T) {
	baseDigest := bytes.Repeat([]byte{0x5a}, 32)
	m := runnerAssetManifest(bundle.AssetArtifact{
		Path: "assets/config.txt", SHA256: bundle.SHA256Hex([]byte("base")), Bytes: 4, Mode: 0600,
	}, "", nil)
	m.SchemaVersion = 3
	m.BundleType = "delta"
	m.UpdateKind = "config-only"
	m.BaseBundleID = m.BundleID
	m.BaseRelease = "r0"
	m.BaseManifest = bundle.SHA256Hex(bytes.Repeat([]byte{0x4a}, 32))
	m.RequiredImages = append([]bundle.ImageArtifact(nil), m.Images...)
	m.AssetsBaseManifest = "sha256:" + hex.EncodeToString(baseDigest)
	m.Payload.AssetsArtifact = ""
	m.Payload.AssetsSHA256 = ""
	m.Payload.AssetsBytes = 0

	assetsRoot := filepath.Join(t.TempDir(), "runtime")
	baseDir := filepath.Join(assetsRoot, assetDirectoryName(m.BundleID, sha256Bytes(baseDigest)))
	if err := os.MkdirAll(filepath.Join(baseDir, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "assets", "config.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := materializeAssets(m, t.TempDir(), bytes.Repeat([]byte{0x41}, 32), assetsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != baseDir {
		t.Fatalf("reused asset directory = %q, want %q", got, baseDir)
	}
}

func sha256Bytes(value []byte) [32]byte {
	var digest [32]byte
	copy(digest[:], value)
	return digest
}

func runnerAssetManifest(entry bundle.AssetArtifact, assetName string, encrypted []byte) *bundle.Manifest {
	return &bundle.Manifest{
		SchemaVersion: 2, BundleID: "asset-test", ReleaseVersion: "r1", CreatedAt: "2026-01-01T00:00:00Z", Profile: "host-integrated-v1",
		TargetPlatform: bundle.TargetPlatform{OS: "linux", Architecture: "amd64"},
		Compose:        bundle.ComposeArtifact{SourceSHA256: "sha256:a", PackagedSHA256: "sha256:b", Bytes: 1, Artifact: "compose.enc", EncryptedSHA256: "sha256:c", EncryptedBytes: 2},
		Images:         []bundle.ImageArtifact{{Service: "web", OriginalRef: "image:1", LocalImageID: "sha256:d", LocalTag: "bundle.local/test/web:d"}},
		Assets:         []bundle.AssetArtifact{entry},
		Capabilities:   bundle.Capabilities{GPU: []bundle.GPUCapability{}, Devices: []bundle.DeviceCapability{}, HostPaths: []bundle.HostPathCapability{}, Privileged: []string{}, HostNetwork: []string{}, HostPID: []string{}},
		Binding:        bundle.Binding{Mode: "none", Values: []string{}},
		Payload: bundle.PayloadSpec{
			ComposeArtifact: "compose.enc", ImagesArtifact: "images.tar.none.enc", ImagesSHA256: "sha256:e", ImagesBytes: 3,
			AssetsArtifact: assetName, AssetsSHA256: bundle.SHA256Hex(encrypted), AssetsBytes: int64(len(encrypted)),
			Compression: "none", Encryption: bundle.EncryptionName, ChunkSize: 4096,
		},
		Signing: bundle.SigningSpec{Algorithm: "Ed25519", KeyID: "sha256:1"},
	}
}
