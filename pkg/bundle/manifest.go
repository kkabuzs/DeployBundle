package bundle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// Manifest is the signed, non-secret description of a DeployBundle package.
// Artifact contents (Compose and image archive) are encrypted separately.
type Manifest struct {
	SchemaVersion      int             `json:"schemaVersion"`
	BundleType         string          `json:"bundleType,omitempty"`
	BundleID           string          `json:"bundleId"`
	ReleaseVersion     string          `json:"releaseVersion"`
	UpdateKind         string          `json:"updateKind,omitempty"`
	BaseBundleID       string          `json:"baseBundleId,omitempty"`
	BaseRelease        string          `json:"baseRelease,omitempty"`
	BaseManifest       string          `json:"baseManifestSha256,omitempty"`
	AssetsBaseManifest string          `json:"assetsBaseManifestSha256,omitempty"`
	CreatedAt          string          `json:"createdAt"`
	Profile            string          `json:"profile"`
	TargetPlatform     TargetPlatform  `json:"targetPlatform"`
	Compose            ComposeArtifact `json:"compose"`
	Images             []ImageArtifact `json:"images"`
	// RequiredImages is retained as signed packaging/audit metadata for delta
	// Bundles. The runner does not require these images or compare their IDs.
	RequiredImages     []ImageArtifact `json:"requiredImages,omitempty"`
	Assets             []AssetArtifact `json:"assets,omitempty"`
	Capabilities       Capabilities    `json:"capabilities"`
	Binding            Binding         `json:"binding"`
	Payload            PayloadSpec     `json:"payload"`
	Signing            SigningSpec     `json:"signing"`
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
	// LocalImageID is packaging metadata used to select changed images. The
	// runner does not use it as a customer-host consistency lock.
	LocalImageID     string   `json:"localImageId"`
	RootfsDiffIDs    []string `json:"rootfsDiffIds"`
	// LocalTag is the image reference written into the packaged Compose. New
	// Bundles normally use OriginalRef; legacy-base updates may retain an old
	// tag for unchanged images.
	LocalTag         string   `json:"localTag"`
}

type AssetArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

const (
	MaxAssetFileBytes    int64 = 64 * 1024 * 1024
	MaxAssetBytes        int64 = 64 * 1024 * 1024
	MaxAssetArchiveBytes int64 = 80 * 1024 * 1024
	MaxAssetFiles              = 1024
)

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
	AssetsArtifact  string `json:"assetsArtifact,omitempty"`
	AssetsSHA256    string `json:"assetsSha256,omitempty"`
	AssetsBytes     int64  `json:"assetsBytes,omitempty"`
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
	if m.SchemaVersion != 1 && m.SchemaVersion != 2 && m.SchemaVersion != 3 {
		return fmt.Errorf("unsupported manifest schemaVersion %d", m.SchemaVersion)
	}
	bundleType := m.BundleType
	if bundleType == "" {
		bundleType = "full"
	}
	if bundleType != "full" && bundleType != "delta" {
		return fmt.Errorf("unsupported bundleType %q", bundleType)
	}
	if m.UpdateKind != "" && m.UpdateKind != "full" && m.UpdateKind != "config-only" && m.UpdateKind != "mixed" {
		return fmt.Errorf("unsupported updateKind %q", m.UpdateKind)
	}
	if bundleType == "full" && m.UpdateKind != "" && m.UpdateKind != "full" {
		return errors.New("full bundle must use updateKind full")
	}
	if bundleType == "delta" && m.UpdateKind == "full" {
		return errors.New("delta bundle cannot use updateKind full")
	}
	if bundleType == "delta" && m.UpdateKind == "config-only" && len(m.Images) != 0 {
		return errors.New("config-only delta cannot carry image artifacts")
	}
	if bundleType == "delta" && m.UpdateKind == "mixed" && len(m.Images) == 0 {
		return errors.New("mixed delta must carry at least one image artifact")
	}
	if m.SchemaVersion < 3 && (bundleType != "full" || m.BaseBundleID != "" || m.BaseRelease != "" || m.BaseManifest != "" || m.AssetsBaseManifest != "" || len(m.RequiredImages) != 0) {
		return fmt.Errorf("schemaVersion %d does not support delta metadata", m.SchemaVersion)
	}
	if bundleType == "delta" {
		if m.SchemaVersion != 3 || strings.TrimSpace(m.BaseBundleID) == "" || strings.TrimSpace(m.BaseRelease) == "" || !validSHA256(m.BaseManifest) {
			return errors.New("delta bundle requires schemaVersion 3 and a valid base release")
		}
		if len(m.RequiredImages) == 0 {
			return errors.New("delta bundle has no required image set")
		}
	}
	requiredFields := map[string]string{
		"bundleId": m.BundleID, "releaseVersion": m.ReleaseVersion, "createdAt": m.CreatedAt,
		"profile": m.Profile, "targetPlatform.os": m.TargetPlatform.OS,
		"targetPlatform.architecture": m.TargetPlatform.Architecture,
		"compose.sourceSha256":        m.Compose.SourceSHA256, "compose.packagedSha256": m.Compose.PackagedSHA256,
		"compose.artifact": m.Compose.Artifact, "payload.composeArtifact": m.Payload.ComposeArtifact,
		"payload.compression": m.Payload.Compression,
		"payload.encryption":  m.Payload.Encryption, "signing.algorithm": m.Signing.Algorithm,
		"signing.keyId": m.Signing.KeyID,
	}
	if bundleType == "full" || len(m.Images) > 0 {
		requiredFields["payload.imagesArtifact"] = m.Payload.ImagesArtifact
	}
	for name, value := range requiredFields {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("manifest field %s is empty", name)
		}
	}
	if m.Compose.Bytes < 0 || m.Compose.EncryptedBytes < 0 || m.Payload.ImagesBytes < 0 {
		return errors.New("artifact byte counts must be non-negative")
	}
	if strings.TrimSpace(m.Compose.EncryptedSHA256) == "" {
		return errors.New("encrypted artifact hashes are required")
	}
	if m.BundleType == "full" || len(m.Images) > 0 {
		if strings.TrimSpace(m.Payload.ImagesArtifact) == "" || strings.TrimSpace(m.Payload.ImagesSHA256) == "" {
			return errors.New("encrypted image artifact hashes are required")
		}
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
	if len(m.Images) == 0 && bundleType == "full" {
		return errors.New("manifest has no images")
	}
	if err := validateImages(m.Images, "images"); err != nil {
		return err
	}
	if bundleType == "delta" {
		if err := validateImages(m.RequiredImages, "requiredImages"); err != nil {
			return err
		}
		required := make(map[string]ImageArtifact, len(m.RequiredImages))
		for _, image := range m.RequiredImages {
			required[image.Service] = image
		}
		for _, image := range m.Images {
			if _, ok := required[image.Service]; !ok {
				return fmt.Errorf("delta image %q is not in required image set", image.Service)
			}
		}
	}

	if m.SchemaVersion == 1 {
		if m.Assets != nil || m.Payload.AssetsArtifact != "" || m.Payload.AssetsSHA256 != "" || m.Payload.AssetsBytes != 0 || m.AssetsBaseManifest != "" {
			return errors.New("schemaVersion 1 does not support asset metadata")
		}
		return nil
	}
	if m.SchemaVersion == 2 {
		if m.AssetsBaseManifest != "" {
			return errors.New("schemaVersion 2 does not support base asset references")
		}
		return validateAssets(m)
	}
	if m.Payload.AssetsArtifact != "" || m.Payload.AssetsSHA256 != "" || m.Payload.AssetsBytes != 0 || len(m.Assets) > 0 || m.AssetsBaseManifest != "" {
		return validateAssets(m)
	}
	return nil
}

func validateImages(images []ImageArtifact, field string) error {
	seenServices := make(map[string]bool, len(images))
	seenTags := make(map[string]bool, len(images))
	for i, image := range images {
		if strings.TrimSpace(image.Service) == "" || strings.TrimSpace(image.OriginalRef) == "" ||
			strings.TrimSpace(image.LocalImageID) == "" || strings.TrimSpace(image.LocalTag) == "" {
			return fmt.Errorf("%s image %d has an empty required field", field, i)
		}
		if seenServices[image.Service] {
			return fmt.Errorf("duplicate %s image service %q", field, image.Service)
		}
		if seenTags[image.LocalTag] {
			return fmt.Errorf("duplicate %s image localTag %q", field, image.LocalTag)
		}
		seenServices[image.Service] = true
		seenTags[image.LocalTag] = true
	}
	return nil
}

func validateAssets(m *Manifest) error {
	if len(m.Assets) == 0 {
		return errors.New("asset metadata requires at least one asset")
	}
	if m.Payload.AssetsArtifact == "" {
		if m.BundleType != "delta" || !validSHA256(m.AssetsBaseManifest) {
			return errors.New("asset metadata without an artifact requires a valid delta base asset reference")
		}
		if m.Payload.AssetsSHA256 != "" || m.Payload.AssetsBytes != 0 {
			return errors.New("base asset references cannot carry artifact hash or size")
		}
	} else {
		if m.AssetsBaseManifest != "" {
			return errors.New("asset artifact cannot also reference base assets")
		}
		if !validSHA256(m.Payload.AssetsSHA256) || m.Payload.AssetsBytes <= 0 {
			return errors.New("assets artifact hash and size are required")
		}
	}
	if m.Payload.AssetsArtifact != "" && (!safeArtifactName(m.Payload.AssetsArtifact) || m.Payload.AssetsArtifact == m.Payload.ComposeArtifact || m.Payload.AssetsArtifact == m.Payload.ImagesArtifact) {
		return errors.New("invalid assets artifact path")
	}
	if m.Payload.AssetsArtifact != "" && m.Payload.AssetsBytes > MaxAssetArchiveBytes+2*1024*1024 {
		return errors.New("assets artifact exceeds size limit")
	}
	if len(m.Assets) > MaxAssetFiles {
		return errors.New("assets exceed file count limit")
	}
	seenAssetPaths := make(map[string]bool, len(m.Assets))
	var total int64
	for i, asset := range m.Assets {
		if !safeAssetPath(asset.Path) {
			return fmt.Errorf("asset %d has an unsafe relative path %q", i, asset.Path)
		}
		if seenAssetPaths[asset.Path] {
			return fmt.Errorf("duplicate asset path %q", asset.Path)
		}
		if !validSHA256(asset.SHA256) || asset.Bytes < 0 || asset.Bytes > MaxAssetFileBytes || asset.Mode&^0777 != 0 {
			return fmt.Errorf("asset %q has invalid metadata", asset.Path)
		}
		if total > MaxAssetBytes-asset.Bytes {
			return errors.New("assets exceed 64 MiB limit")
		}
		total += asset.Bytes
		seenAssetPaths[asset.Path] = true
	}
	for name := range seenAssetPaths {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seenAssetPaths[parent] {
				return fmt.Errorf("asset path conflict: %s", name)
			}
		}
	}
	return nil
}

func safeArtifactName(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsRune(name, 0) && name != "" && name != "." && name != ".." && path.Base(name) == name && !strings.ContainsAny(name, `/\`)
}

func safeAssetPath(name string) bool {
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(name) == 0 || len(name) > 4096 || name == "." || name == ".." || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, `\`) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." || len(component) > 255 {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	payloadFields := map[string]json.RawMessage{}
	if raw, ok := fields["payload"]; ok {
		if err := json.Unmarshal(raw, &payloadFields); err != nil {
			return nil, fmt.Errorf("parse manifest payload: %w", err)
		}
	}
	_, hasAssets := fields["assets"]
	_, hasArtifact := payloadFields["assetsArtifact"]
	_, hasHash := payloadFields["assetsSha256"]
	_, hasBytes := payloadFields["assetsBytes"]
	_, hasBaseManifest := fields["assetsBaseManifestSha256"]
	if m.SchemaVersion == 1 && (hasAssets || hasArtifact || hasHash || hasBytes || hasBaseManifest) {
		return nil, errors.New("schemaVersion 1 does not support asset metadata")
	}
	if m.SchemaVersion == 2 && (!hasAssets || !hasArtifact || !hasHash || !hasBytes) {
		return nil, errors.New("schemaVersion 2 requires explicit asset metadata")
	}
	if m.SchemaVersion == 3 && m.BundleType != "delta" {
		return nil, errors.New("schemaVersion 3 requires a delta bundle")
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
