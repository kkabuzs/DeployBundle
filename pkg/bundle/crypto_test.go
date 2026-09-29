package bundle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	for _, input := range [][]byte{nil, []byte("hello"), bytes.Repeat([]byte("x"), 4096), bytes.Repeat([]byte("y"), 10000)} {
		var encrypted bytes.Buffer
		if err := Encrypt(&encrypted, bytes.NewReader(input), key, WithChunkSize(4096)); err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		var got bytes.Buffer
		if err := Decrypt(&got, bytes.NewReader(encrypted.Bytes()), key); err != nil {
			t.Fatalf("decrypt (%d bytes): %v", len(input), err)
		}
		if !bytes.Equal(got.Bytes(), input) {
			t.Fatalf("roundtrip mismatch: got %d bytes, want %d", got.Len(), len(input))
		}
	}
}

func TestV2HeaderAndDecryptInfo(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader([]byte("hello")), key, WithChunkSize(4096)); err != nil {
		t.Fatal(err)
	}
	raw := encrypted.Bytes()
	if len(raw) <= headerSize {
		t.Fatalf("encrypted stream is shorter than its header: %d", len(raw))
	}
	if !bytes.Equal(raw[:len(streamMagic)], streamMagic[:]) {
		t.Fatalf("stream magic = %q, want %q", raw[:len(streamMagic)], streamMagic[:])
	}
	if got := int(binary.BigEndian.Uint32(raw[5:9])); got != 4096 {
		t.Fatalf("header chunk size = %d, want 4096", got)
	}
	if got := len(raw[9:headerSize]); got != noncePrefixSize {
		t.Fatalf("nonce prefix size = %d, want %d", got, noncePrefixSize)
	}

	var got bytes.Buffer
	info, err := DecryptWithInfo(&got, bytes.NewReader(raw), key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != 2 || info.ChunkSize != 4096 {
		t.Fatalf("stream info = %+v, want version 2/chunk size 4096", info)
	}
	if got.String() != "hello" {
		t.Fatalf("decrypted payload = %q, want hello", got.String())
	}
}

func TestV2NonceDiffersAcrossFramesAndStreams(t *testing.T) {
	prefixA := bytes.Repeat([]byte{0x11}, noncePrefixSize)
	prefixB := bytes.Repeat([]byte{0x22}, noncePrefixSize)
	if got := len(streamNonce(prefixA, 0)); got != 12 {
		t.Fatalf("nonce length = %d, want 12", got)
	}
	if bytes.Equal(streamNonce(prefixA, 0), streamNonce(prefixA, 1)) {
		t.Fatal("two frames in one stream reused a nonce")
	}
	if bytes.Equal(streamNonce(prefixA, 0), streamNonce(prefixB, 0)) {
		t.Fatal("two streams reused a nonce")
	}
}

func TestDecryptRejectsTamperAndWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader(bytes.Repeat([]byte("z"), 9000)), key, WithChunkSize(4096)); err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), encrypted.Bytes()...)
	tampered[len(tampered)-10] ^= 1
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(tampered), key); err == nil {
		t.Fatal("tampered stream was accepted")
	}
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(encrypted.Bytes()), bytes.Repeat([]byte{0x12}, 32)); err == nil {
		t.Fatal("wrong key was accepted")
	}
}

func TestDecryptRejectsTruncatedStreamAndLegacyMagic(t *testing.T) {
	key := bytes.Repeat([]byte{0x37}, 32)
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bytes.NewReader(bytes.Repeat([]byte("z"), 9000)), key, WithChunkSize(4096)); err != nil {
		t.Fatal(err)
	}
	raw := encrypted.Bytes()
	if len(raw) < headerSize+4+16 {
		t.Fatalf("encrypted stream is unexpectedly short: %d", len(raw))
	}

	// Keep the header and first complete frame, then append the original final
	// tag. The tag was made for the original frame index, so v2 must reject it
	// instead of accepting this shortened stream.
	firstLen := int(binary.BigEndian.Uint32(raw[headerSize : headerSize+4]))
	cut := headerSize + 4 + firstLen
	truncated := append([]byte(nil), raw[:cut]...)
	var finalHeader [4]byte
	binary.BigEndian.PutUint32(finalHeader[:], frameFinal)
	truncated = append(truncated, finalHeader[:]...)
	truncated = append(truncated, raw[len(raw)-16:]...)
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(truncated), key); err == nil {
		t.Fatal("truncated stream with a copied final tag was accepted")
	}

	legacy := append([]byte(nil), raw...)
	copy(legacy[:len(streamMagic)], []byte{'D', 'B', 'A', 'E', '1'})
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(legacy), key); err == nil {
		t.Fatal("legacy v1 stream magic was accepted by v2 decrypt")
	}
}

func TestDecryptRejectsFrameLargerThanDeclaredChunk(t *testing.T) {
	key := bytes.Repeat([]byte{0x19}, 32)
	header := make([]byte, headerSize)
	copy(header[:len(streamMagic)], streamMagic[:])
	binary.BigEndian.PutUint32(header[5:9], 4096)
	copy(header[9:], bytes.Repeat([]byte{0x44}, noncePrefixSize))
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 4096+16+1)
	raw := append(header, frame...)
	if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(raw), key); err == nil {
		t.Fatal("oversized frame was accepted")
	}
}

func TestManifestSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		SchemaVersion: 1, BundleID: "test", ReleaseVersion: "r1", CreatedAt: "2026-01-01T00:00:00Z", Profile: "host-integrated-v1",
		TargetPlatform: TargetPlatform{OS: "linux", Architecture: "arm64"},
		Compose:        ComposeArtifact{SourceSHA256: "sha256:a", PackagedSHA256: "sha256:b", Bytes: 1, Artifact: "compose.enc", EncryptedSHA256: "sha256:c", EncryptedBytes: 2},
		Images:         []ImageArtifact{{Service: "web", OriginalRef: "nginx:latest", LocalImageID: "sha256:d", LocalTag: "bundle.local/test/web:d"}},
		Capabilities:   Capabilities{GPU: []GPUCapability{}, Devices: []DeviceCapability{}, HostPaths: []HostPathCapability{}, Privileged: []string{}, HostNetwork: []string{}, HostPID: []string{}},
		Binding:        Binding{Mode: "none", Values: []string{}}, Payload: PayloadSpec{ComposeArtifact: "compose.enc", ImagesArtifact: "images.tar.gzip.enc", ImagesSHA256: "sha256:e", ImagesBytes: 3, Compression: "gzip", Encryption: EncryptionName, ChunkSize: DefaultChunkSize},
		Signing: SigningSpec{Algorithm: "Ed25519", KeyID: "sha256:1"},
	}
	sig, err := SignManifest(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(m, sig, pub); err != nil {
		t.Fatal(err)
	}
	m.ReleaseVersion = "tampered"
	if err := VerifyManifest(m, sig, pub); err == nil {
		t.Fatal("modified manifest was accepted")
	}
}
