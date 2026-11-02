package bundle

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestManifestRejectsUnsupportedAlgorithms(t *testing.T) {
	base := validManifestForTest()
	checks := []struct {
		name string
		set  func(*Manifest)
	}{
		{name: "compression", set: func(m *Manifest) { m.Payload.Compression = "zstd" }},
		{name: "encryption", set: func(m *Manifest) { m.Payload.Encryption = "AES-256-GCM-chunked-v1" }},
		{name: "signing", set: func(m *Manifest) { m.Signing.Algorithm = "RSA" }},
		{name: "key id", set: func(m *Manifest) { m.Signing.KeyID = "" }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			m := validManifestForTest()
			check.set(m)
			if err := m.Validate(); err == nil {
				t.Fatalf("invalid %s was accepted", check.name)
			}
		})
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestManifestAssetSchemaValidation(t *testing.T) {
	valid := validAssetManifestForTest()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid schema v2 manifest rejected: %v", err)
	}

	checks := []struct {
		name string
		set  func(*Manifest)
	}{
		{name: "v1 with assets", set: func(m *Manifest) { m.SchemaVersion = 1 }},
		{name: "v2 without entries", set: func(m *Manifest) { m.Assets = nil }},
		{name: "missing artifact", set: func(m *Manifest) { m.Payload.AssetsArtifact = "" }},
		{name: "missing hash", set: func(m *Manifest) { m.Payload.AssetsSHA256 = "" }},
		{name: "unsafe artifact", set: func(m *Manifest) { m.Payload.AssetsArtifact = "../assets.enc" }},
		{name: "duplicate artifact", set: func(m *Manifest) { m.Payload.AssetsArtifact = m.Payload.ImagesArtifact }},
		{name: "unsafe path", set: func(m *Manifest) { m.Assets[0].Path = "../secret" }},
		{name: "invalid hash", set: func(m *Manifest) { m.Assets[0].SHA256 = "sha256:bad" }},
		{name: "special mode", set: func(m *Manifest) { m.Assets[0].Mode = 04755 }},
		{name: "file too large", set: func(m *Manifest) { m.Assets[0].Bytes = MaxAssetFileBytes + 1 }},
		{name: "duplicate path", set: func(m *Manifest) { m.Assets = append(m.Assets, m.Assets[0]) }},
		{name: "parent conflict", set: func(m *Manifest) {
			m.Assets = append(m.Assets, AssetArtifact{Path: "assets", SHA256: testSHA('d'), Bytes: 1, Mode: 0600})
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			m := validAssetManifestForTest()
			check.set(m)
			if err := m.Validate(); err == nil {
				t.Fatalf("invalid asset manifest was accepted: %+v", m)
			}
		})
	}
}

func TestDeltaManifestCanReferenceBaseAssets(t *testing.T) {
	m := validAssetManifestForTest()
	m.SchemaVersion = 3
	m.BundleType = "delta"
	m.UpdateKind = "config-only"
	m.BaseBundleID = "test"
	m.BaseRelease = "r0"
	m.BaseManifest = testSHA('c')
	m.AssetsBaseManifest = testSHA('d')
	m.RequiredImages = append([]ImageArtifact(nil), m.Images...)
	m.Images = nil
	m.Payload.AssetsArtifact = ""
	m.Payload.AssetsSHA256 = ""
	m.Payload.AssetsBytes = 0
	if err := m.Validate(); err != nil {
		t.Fatalf("base asset reference rejected: %v", err)
	}

	m.AssetsBaseManifest = ""
	if err := m.Validate(); err == nil {
		t.Fatal("delta asset metadata without artifact or base reference was accepted")
	}
}

func TestManifestRejectsInvalidUpdateKind(t *testing.T) {
	m := validManifestForTest()
	m.UpdateKind = "image-only"
	if err := m.Validate(); err == nil {
		t.Fatal("invalid update kind was accepted")
	}
}

func TestStandaloneConfigManifestRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, withAssets := range []bool{false, true} {
		name := "compose"
		if withAssets {
			name = "compose-and-assets"
		}
		t.Run(name, func(t *testing.T) {
			m := standaloneConfigManifestForTest(withAssets)
			raw, err := MarshalManifest(m)
			if err != nil {
				t.Fatalf("standalone config rejected: %v", err)
			}
			sig, err := SignManifest(m, priv)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseManifest(raw)
			if err != nil {
				t.Fatalf("parse standalone config: %v", err)
			}
			if parsed.BundleType != "full" || parsed.UpdateKind != "config-only" || len(parsed.Images) != 0 || parsed.Payload.ImagesArtifact != "" || parsed.BaseManifest != "" {
				t.Fatalf("unexpected standalone config metadata: %+v", parsed)
			}
			if err := VerifyManifest(parsed, sig, pub); err != nil {
				t.Fatalf("verify standalone config: %v", err)
			}
			parsed.Compose.PackagedSHA256 = "sha256:changed"
			if err := VerifyManifest(parsed, sig, pub); err == nil {
				t.Fatal("modified standalone Compose metadata retained valid signature")
			}
		})
	}
}

func TestStandaloneConfigManifestRejectsImageAndBaseMetadata(t *testing.T) {
	checks := []struct {
		name string
		set  func(*Manifest)
	}{
		{name: "image set", set: func(m *Manifest) { m.Images = validManifestForTest().Images }},
		{name: "image artifact", set: func(m *Manifest) { m.Payload.ImagesArtifact = "images.tar.gzip.enc" }},
		{name: "image hash", set: func(m *Manifest) { m.Payload.ImagesSHA256 = testSHA('a') }},
		{name: "image bytes", set: func(m *Manifest) { m.Payload.ImagesBytes = 1 }},
		{name: "base bundle", set: func(m *Manifest) { m.BaseBundleID = "previous" }},
		{name: "base release", set: func(m *Manifest) { m.BaseRelease = "r0" }},
		{name: "base manifest", set: func(m *Manifest) { m.BaseManifest = testSHA('a') }},
		{name: "base assets", set: func(m *Manifest) { m.AssetsBaseManifest = testSHA('a') }},
		{name: "required images", set: func(m *Manifest) { m.RequiredImages = validManifestForTest().Images }},
		{name: "delta schema", set: func(m *Manifest) { m.SchemaVersion = 3 }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			m := standaloneConfigManifestForTest(false)
			check.set(m)
			if err := m.Validate(); err == nil {
				t.Fatalf("standalone config accepted %s", check.name)
			}
		})
	}
}

func TestLegacyImageManifestRequiresArchiveMetadata(t *testing.T) {
	for _, field := range []string{"artifact", "hash", "size"} {
		t.Run(field, func(t *testing.T) {
			m := validManifestForTest()
			switch field {
			case "artifact":
				m.Payload.ImagesArtifact = ""
			case "hash":
				m.Payload.ImagesSHA256 = ""
			case "size":
				m.Payload.ImagesBytes = 0
			}
			if err := m.Validate(); err == nil {
				t.Fatalf("legacy images accepted missing archive %s", field)
			}
		})
	}
}

func TestLegacyFullAndDeltaManifestRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"implicit-full", "full", "mixed", "config-only"} {
		t.Run(kind, func(t *testing.T) {
			m := validManifestForTest()
			switch kind {
			case "full":
				m.BundleType, m.UpdateKind = "full", "full"
			case "mixed", "config-only":
				m.SchemaVersion, m.BundleType, m.UpdateKind = 3, "delta", kind
				m.BaseBundleID, m.BaseRelease, m.BaseManifest = "test", "r0", testSHA('a')
				m.RequiredImages = append([]ImageArtifact(nil), m.Images...)
				if kind == "config-only" {
					m.Images = nil
					m.Payload.ImagesArtifact, m.Payload.ImagesSHA256, m.Payload.ImagesBytes = "", "", 0
				}
			}
			raw, err := MarshalManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			sig, err := SignManifest(m, priv)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseManifest(raw)
			if err != nil {
				t.Fatalf("legacy manifest rejected: %v", err)
			}
			if err := VerifyManifest(parsed, sig, pub); err != nil {
				t.Fatalf("legacy manifest signature rejected: %v", err)
			}
		})
	}
}

func TestParseManifestRejectsAssetFieldsInSchemaV1(t *testing.T) {
	raw, err := MarshalManifest(validManifestForTest())
	if err != nil {
		t.Fatal(err)
	}
	withAssets := strings.Replace(string(raw), `"images":`, `"assets":[],"images":`, 1)
	if _, err := ParseManifest([]byte(withAssets)); err == nil {
		t.Fatal("schema v1 manifest containing an assets field was accepted")
	}
}

func TestAssetMetadataIsCoveredBySignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := validAssetManifestForTest()
	sig, err := SignManifest(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	m.Assets[0].Bytes++
	if err := VerifyManifest(m, sig, pub); err == nil {
		t.Fatal("modified asset metadata retained a valid signature")
	}
}

func validManifestForTest() *Manifest {
	return &Manifest{
		SchemaVersion: 1, BundleID: "test", ReleaseVersion: "r1", CreatedAt: "2026-01-01T00:00:00Z", Profile: "host-integrated-v1",
		TargetPlatform: TargetPlatform{OS: "linux", Architecture: "arm64"},
		Compose:        ComposeArtifact{SourceSHA256: "sha256:a", PackagedSHA256: "sha256:b", Bytes: 1, Artifact: "compose.enc", EncryptedSHA256: "sha256:c", EncryptedBytes: 2},
		Images:         []ImageArtifact{{Service: "web", OriginalRef: "nginx:latest", LocalImageID: "sha256:d", LocalTag: "bundle.local/test/web:d"}},
		Capabilities:   Capabilities{GPU: []GPUCapability{}, Devices: []DeviceCapability{}, HostPaths: []HostPathCapability{}, Privileged: []string{}, HostNetwork: []string{}, HostPID: []string{}},
		Binding:        Binding{Mode: "none", Values: []string{}},
		Payload:        PayloadSpec{ComposeArtifact: "compose.enc", ImagesArtifact: "images.tar.gzip.enc", ImagesSHA256: "sha256:e", ImagesBytes: 3, Compression: "gzip", Encryption: EncryptionName, ChunkSize: DefaultChunkSize},
		Signing:        SigningSpec{Algorithm: "Ed25519", KeyID: "sha256:1"},
	}
}

func validAssetManifestForTest() *Manifest {
	m := validManifestForTest()
	m.SchemaVersion = 2
	m.Assets = []AssetArtifact{{Path: "assets/config.txt", SHA256: testSHA('a'), Bytes: 1, Mode: 0600}}
	m.Payload.AssetsArtifact = "assets.tar.gzip.enc"
	m.Payload.AssetsSHA256 = testSHA('b')
	m.Payload.AssetsBytes = 128
	return m
}

func standaloneConfigManifestForTest(withAssets bool) *Manifest {
	m := validManifestForTest()
	if withAssets {
		m = validAssetManifestForTest()
	}
	m.BundleType, m.UpdateKind = "full", "config-only"
	m.Images = []ImageArtifact{}
	m.Payload.ImagesArtifact, m.Payload.ImagesSHA256, m.Payload.ImagesBytes = "", "", 0
	return m
}

func testSHA(value byte) string {
	return "sha256:" + strings.Repeat(string(value), 64)
}
