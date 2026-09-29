package bundle

import "testing"

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
