package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openai/deploybundle/internal/assets"
	"github.com/openai/deploybundle/pkg/bundle"
)

type limitedBuffer struct {
	bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()) > b.max-int64(len(p)) {
		return 0, fmt.Errorf("asset archive exceeds %d-byte limit", b.max)
	}
	return b.Buffer.Write(p)
}

func verifyAssetsStream(m *bundle.Manifest, dir string, key []byte) error {
	archive, err := decryptAssets(m, dir, key)
	if err != nil {
		return err
	}
	return readAssetsPayload(m, archive, "")
}

func decryptAssets(m *bundle.Manifest, dir string, key []byte) ([]byte, error) {
	name, err := artifactPath(dir, m.Payload.AssetsArtifact)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	counted := &countingWriter{w: h}
	out := &limitedBuffer{max: bundle.MaxAssetArchiveBytes}
	info, err := bundle.DecryptWithInfo(out, io.TeeReader(f, counted), key)
	if err != nil {
		return nil, fmt.Errorf("decrypt assets artifact: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != m.Payload.AssetsSHA256 || counted.n != m.Payload.AssetsBytes {
		return nil, errors.New("encrypted assets artifact changed during decryption")
	}
	if err := validateStreamInfo(m, info); err != nil {
		return nil, err
	}
	return append([]byte(nil), out.Bytes()...), nil
}

func readAssetsPayload(m *bundle.Manifest, archive []byte, dest string) error {
	var tarBytes []byte
	switch m.Payload.Compression {
	case "none":
		if int64(len(archive)) > bundle.MaxAssetArchiveBytes {
			return fmt.Errorf("asset archive exceeds %d-byte limit", bundle.MaxAssetArchiveBytes)
		}
		tarBytes = archive
	case "gzip":
		compressed := bytes.NewReader(archive)
		gz, err := gzip.NewReader(compressed)
		if err != nil {
			return fmt.Errorf("open compressed asset archive: %w", err)
		}
		gz.Multistream(false)
		var plain bytes.Buffer
		n, copyErr := io.CopyN(&plain, gz, bundle.MaxAssetArchiveBytes+1)
		closeErr := gz.Close()
		if copyErr != nil && copyErr != io.EOF {
			return fmt.Errorf("decompress asset archive: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close compressed asset archive: %w", closeErr)
		}
		if n > bundle.MaxAssetArchiveBytes {
			return fmt.Errorf("asset archive exceeds %d-byte limit", bundle.MaxAssetArchiveBytes)
		}
		if compressed.Len() != 0 {
			return errors.New("compressed asset archive has trailing data")
		}
		tarBytes = plain.Bytes()
	default:
		return fmt.Errorf("unsupported compression %q", m.Payload.Compression)
	}
	return assets.ReadTar(bytes.NewReader(tarBytes), m.Assets, dest)
}

func materializeAssets(m *bundle.Manifest, dir string, key []byte, assetsDir string) (string, error) {
	if strings.TrimSpace(assetsDir) == "" || !filepath.IsAbs(assetsDir) {
		return "", errors.New("--assets-dir must be an absolute path")
	}
	root := filepath.Clean(assetsDir)
	if root == string(filepath.Separator) {
		return "", errors.New("--assets-dir cannot be the filesystem root")
	}
	createdRoot := false
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0700); err != nil {
			return "", fmt.Errorf("create assets root: %w", err)
		}
		createdRoot = true
	} else if err != nil {
		return "", fmt.Errorf("inspect assets root: %w", err)
	}
	if createdRoot {
		if err := os.Chmod(root, 0700); err != nil {
			return "", fmt.Errorf("set assets root permissions: %w", err)
		}
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0700 {
		return "", errors.New("assets root must be a non-symlink directory with mode 0700")
	}

	var digest [32]byte
	if m.Payload.AssetsArtifact == "" {
		if m.AssetsBaseManifest == "" {
			return "", errors.New("bundle has assets but no asset artifact or base reference")
		}
		decoded, err := hex.DecodeString(strings.TrimPrefix(m.AssetsBaseManifest, "sha256:"))
		if err != nil || len(decoded) != len(digest) {
			return "", errors.New("invalid base asset manifest digest")
		}
		copy(digest[:], decoded)
	} else {
		var err error
		digest, err = bundle.ManifestDigest(m)
		if err != nil {
			return "", err
		}
	}
	finalName := assetDirectoryName(m.BundleID, digest)
	final := filepath.Join(root, finalName)
	if info, err := os.Lstat(final); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("existing asset directory must not be a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if m.Payload.AssetsArtifact == "" {
		if info, err := os.Lstat(final); err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("base asset directory %s is unavailable", final)
			}
			return "", err
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("base asset directory must be a non-symlink directory")
		}
		if err := assets.ValidateDir(final, m.Assets); err != nil {
			return "", fmt.Errorf("base asset directory conflicts with bundle: %w", err)
		}
		return final, nil
	}

	archive, err := decryptAssets(m, dir, key)
	if err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(root, "."+finalName+".tmp-")
	if err != nil {
		return "", fmt.Errorf("create asset staging directory: %w", err)
	}
	if err := os.Chmod(stage, 0700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := readAssetsPayload(m, archive, stage); err != nil {
		return "", err
	}
	if err := assets.ValidateDir(stage, m.Assets); err != nil {
		return "", fmt.Errorf("validate staged assets: %w", err)
	}

	if _, err := os.Lstat(final); err == nil {
		if err := assets.ValidateDir(final, m.Assets); err != nil {
			return "", fmt.Errorf("existing asset directory conflicts with bundle: %w", err)
		}
		return final, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(stage, final); err != nil {
		return "", fmt.Errorf("publish asset directory: %w", err)
	}
	removeStage = false
	return final, nil
}

func assetDirectoryName(bundleID string, digest [32]byte) string {
	return sanitizeAssetName(bundleID) + "-" + hex.EncodeToString(digest[:16])
}

func sanitizeAssetName(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		if b.Len() == 48 {
			break
		}
	}
	name := strings.Trim(b.String(), "-._")
	if name == "" {
		return "bundle"
	}
	return name
}
