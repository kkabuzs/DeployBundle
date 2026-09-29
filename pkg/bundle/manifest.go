package bundle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Manifest is the signed, non-secret description of a DeployBundle package.
// Artifact contents (Compose and image archive) are encrypted separately.
type Manifest struct {
	SchemaVersion  int             `json:"schemaVersion"`
	BundleID       string          `json:"bundleId"`
	ReleaseVersion string          `json:"releaseVersion"`
	CreatedAt      string          `json:"createdAt"`
	Profile        string          `json:"profile"`
	TargetPlatform TargetPlatform  `json:"targetPlatform"`
	Compose        ComposeArtifact `json:"compose"`
	Images         []ImageArtifact `json:"images"`
	Capabilities   Capabilities    `json:"capabilities"`
	Binding        Binding         `json:"binding"`
	Payload        PayloadSpec     `json:"payload"`
	Signing        SigningSpec     `json:"signing"`
}

type TargetPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type ComposeArtifact struct {
	SourceSHA256    string `json:"sourceSha256"`
	PackagedSHA256  string `json:"packagedSha256"`
	Bytes           int64  `json:"bytes"`
	Artifact        string `json:"artifact"`
	EncryptedSHA256 string `json:"encryptedSha256"`
	EncryptedBytes  int64  `json:"encryptedBytes"`
}

type ImageArtifact struct {
	Service          string   `json:"service"`
	OriginalRef      string   `json:"originalRef"`
	SourceRepoDigest string   `json:"sourceRepoDigest"`
	LocalImageID     string   `json:"localImageId"`
	RootfsDiffIDs    []string `json:"rootfsDiffIds"`
	LocalTag         string   `json:"localTag"`
}

type Capabilities struct {
	GPU         []GPUCapability      `json:"gpu"`
	Devices     []DeviceCapability   `json:"devices"`
	HostPaths   []HostPathCapability `json:"hostPaths"`
	Privileged  []string             `json:"privilegedServices"`
	HostNetwork []string             `json:"hostNetworkServices"`
	HostPID     []string             `json:"hostPIDServices"`
}

type GPUCapability struct {
	Service      string   `json:"service"`
	Driver       string   `json:"driver"`
	Capabilities []string `json:"capabilities"`
	Count        string   `json:"count,omitempty"`
	DeviceIDs    []string `json:"deviceIds,omitempty"`
}

type DeviceCapability struct {
	Service     string `json:"service"`
	Host        string `json:"host"`
	Container   string `json:"container"`
	Permissions string `json:"permissions"`
}

type HostPathCapability struct {
	Service string `json:"service"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	Mode    string `json:"mode"`
}

type Binding struct {
	Mode   string   `json:"mode"`
	Salt   string   `json:"salt,omitempty"`
	Values []string `json:"values"`
}

type PayloadSpec struct {
	ComposeArtifact string `json:"composeArtifact"`
	ImagesArtifact  string `json:"imagesArtifact"`
	ImagesSHA256    string `json:"imagesSha256"`
	ImagesBytes     int64  `json:"imagesBytes"`
	Compression     string `json:"compression"`
	Encryption      string `json:"encryption"`
	ChunkSize       int    `json:"chunkSize"`
}

type SigningSpec struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"keyId"`
}

func (m *Manifest) Validate() error {
	if m == nil {
		return errors.New("manifest is nil")
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported manifest schemaVersion %d", m.SchemaVersion)
	}
	for name, value := range map[string]string{
		"bundleId": m.BundleID, "releaseVersion": m.ReleaseVersion, "createdAt": m.CreatedAt,
		"profile": m.Profile, "targetPlatform.os": m.TargetPlatform.OS,
		"targetPlatform.architecture": m.TargetPlatform.Architecture,
		"compose.sourceSha256":        m.Compose.SourceSHA256, "compose.packagedSha256": m.Compose.PackagedSHA256,
		"compose.artifact": m.Compose.Artifact, "payload.composeArtifact": m.Payload.ComposeArtifact,
		"payload.imagesArtifact": m.Payload.ImagesArtifact, "payload.compression": m.Payload.Compression,
		"payload.encryption": m.Payload.Encryption, "signing.algorithm": m.Signing.Algorithm,
		"signing.keyId": m.Signing.KeyID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("manifest field %s is empty", name)
		}
	}
	if m.Compose.Bytes < 0 || m.Compose.EncryptedBytes < 0 || m.Payload.ImagesBytes < 0 {
		return errors.New("compose.bytes must be non-negative")
	}
	if strings.TrimSpace(m.Compose.EncryptedSHA256) == "" || strings.TrimSpace(m.Payload.ImagesSHA256) == "" {
		return errors.New("encrypted artifact hashes are required")
	}
	if m.Payload.Compression != "gzip" && m.Payload.Compression != "none" {
		return fmt.Errorf("unsupported payload compression %q", m.Payload.Compression)
	}
	if m.Payload.Encryption != EncryptionName {
		return fmt.Errorf("unsupported payload encryption %q", m.Payload.Encryption)
	}
	if m.Signing.Algorithm != "Ed25519" {
		return fmt.Errorf("unsupported signing algorithm %q", m.Signing.Algorithm)
	}
	if m.Payload.ChunkSize < 4096 || m.Payload.ChunkSize > 64*1024*1024 {
		return fmt.Errorf("payload.chunkSize out of range: %d", m.Payload.ChunkSize)
	}
	if m.Compose.Artifact != m.Payload.ComposeArtifact {
		return errors.New("compose artifact fields disagree")
	}
	if m.Binding.Mode != "none" && m.Binding.Mode != "mac-sha256" {
		return fmt.Errorf("unsupported binding mode %q", m.Binding.Mode)
	}
	if m.Binding.Mode == "mac-sha256" && (strings.TrimSpace(m.Binding.Salt) == "" || len(m.Binding.Values) == 0) {
		return errors.New("MAC binding requires a salt and at least one value")
	}
	if m.Binding.Mode == "none" && len(m.Binding.Values) != 0 {
		return errors.New("binding values are not allowed when binding mode is none")
	}
	if len(m.Images) == 0 {
		return errors.New("manifest has no images")
	}
	seenServices := make(map[string]bool, len(m.Images))
	seenTags := make(map[string]bool, len(m.Images))
	for i, image := range m.Images {
		if strings.TrimSpace(image.Service) == "" || strings.TrimSpace(image.OriginalRef) == "" ||
			strings.TrimSpace(image.LocalImageID) == "" || strings.TrimSpace(image.LocalTag) == "" {
			return fmt.Errorf("image %d has an empty required field", i)
		}
		if seenServices[image.Service] {
			return fmt.Errorf("duplicate image service %q", image.Service)
		}
		if seenTags[image.LocalTag] {
			return fmt.Errorf("duplicate image localTag %q", image.LocalTag)
		}
		seenServices[image.Service] = true
		seenTags[image.LocalTag] = true
	}
	return nil
}

// MarshalCanonical produces the exact byte representation used for hashes and signatures.
func MarshalCanonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func MarshalManifest(m *Manifest) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return MarshalCanonical(m)
}

func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ManifestDigest(m *Manifest) ([32]byte, error) {
	b, err := MarshalManifest(m)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func SignManifest(m *Manifest, privateKey ed25519.PrivateKey) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid Ed25519 private key length %d", len(privateKey))
	}
	b, err := MarshalManifest(m)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, b), nil
}

func VerifyManifest(m *Manifest, signature []byte, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid Ed25519 public key length %d", len(publicKey))
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("invalid Ed25519 signature length %d", len(signature))
	}
	b, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, b, signature) {
		return errors.New("manifest signature verification failed")
	}
	return nil
}
