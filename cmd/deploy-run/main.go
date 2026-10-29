package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/openai/deploybundle/internal/binding"
	"github.com/openai/deploybundle/internal/files"
	"github.com/openai/deploybundle/internal/keys"
	"github.com/openai/deploybundle/pkg/bundle"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "inspect":
		err = runInspect(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "help", "-h", "--help":
		printRootUsage(os.Stdout)
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy-run:", err)
		os.Exit(1)
	}
}

func usage() {
	printRootUsage(os.Stderr)
}

func printRootUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-run inspect [参数]
  deploy-run verify [参数]
  deploy-run apply [参数]
  deploy-run --help

命令：
  inspect   读取并显示 Bundle 清单，不解密镜像
  verify    验证签名、哈希、加密流和主机预检
  apply     验证 Bundle、加载镜像并启动 Compose

使用“deploy-run <命令> --help”查看具体参数说明。`)
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printInspectUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-run inspect --bundle bundle-dir --verify-key signing.pub

参数：
  --bundle PATH             Bundle 目录
  --verify-key PATH         Ed25519 验签公钥
  --allow-platform-mismatch 允许 Bundle 目标平台与当前主机不同
  --skip-host-preflight     跳过 GPU、设备和宿主机路径预检
  --docker PATH             Docker CLI 路径，默认 docker
  -h, --help                显示此帮助

说明：inspect 只读取 Bundle，不启动服务。`)
}

func printVerifyUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-run verify --bundle bundle-dir --key-file key.bin --verify-key signing.pub

参数：
  --bundle PATH             Bundle 目录
  --key-file PATH           32 字节对称解密密钥
  --verify-key PATH         Ed25519 验签公钥
  --allow-platform-mismatch 允许 Bundle 目标平台与当前主机不同
  --skip-host-preflight     跳过 GPU、设备和宿主机路径预检
  --docker PATH             Docker CLI 路径，默认 docker
  -h, --help                显示此帮助

说明：verify 会验证 manifest、artifact 哈希、加密流、Compose 和主机能力，但不会启动 Compose。`)
}

func printApplyUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-run apply --bundle bundle-dir --key-file key.bin --verify-key signing.pub [参数]

参数：
  --bundle PATH             Bundle 目录
  --key-file PATH           32 字节对称解密密钥
  --verify-key PATH         Ed25519 验签公钥
  --assets-dir PATH         持久化 Compose 资产根目录，默认 /var/lib/deploybundle/assets
  --service NAME            只启动指定 Compose 服务，可重复指定
  --allow-platform-mismatch 允许 Bundle 目标平台与当前主机不同
  --skip-host-preflight     跳过 GPU、设备和宿主机路径预检
  --docker PATH             Docker CLI 路径，默认 docker
  -h, --help                显示此帮助

说明：apply 会先完整验证并解密 Bundle，再加载镜像并启动 Compose；不会比较客户机已有镜像 ID；失败时不会自动删除已加载镜像或回滚持久化数据。`)
}

type runOptions struct {
	bundleDir     string
	keyFile       string
	verifyKey     string
	dockerBin     string
	assetsDir     string
	services      stringList
	allowPlatform bool
	skipHostCheck bool
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") {
		return fmt.Errorf("invalid service name %q", value)
	}
	for _, existing := range *values {
		if existing == value {
			return fmt.Errorf("duplicate service %q", value)
		}
	}
	*values = append(*values, value)
	return nil
}

func parseRunOptions(args []string, command string, needSecret bool) (runOptions, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := runOptions{}
	fs.StringVar(&o.bundleDir, "bundle", "", "bundle directory")
	fs.StringVar(&o.keyFile, "key-file", "", "32-byte symmetric key")
	fs.StringVar(&o.verifyKey, "verify-key", "", "Ed25519 public key")
	fs.StringVar(&o.dockerBin, "docker", "docker", "Docker CLI executable")
	if command == "apply" {
		fs.StringVar(&o.assetsDir, "assets-dir", "/var/lib/deploybundle/assets", "persistent root for packaged Compose assets")
		fs.Var(&o.services, "service", "Compose service to start (repeatable)")
	}
	fs.BoolVar(&o.allowPlatform, "allow-platform-mismatch", false, "allow manifest target platform to differ from runtime")
	fs.BoolVar(&o.skipHostCheck, "skip-host-preflight", false, "skip GPU/device/path preflight")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected positional arguments: %q", fs.Args())
	}
	if o.bundleDir == "" || o.verifyKey == "" {
		return o, errors.New("--bundle and --verify-key are required")
	}
	if needSecret && o.keyFile == "" {
		return o, errors.New("--key-file is required")
	}
	return o, nil
}

func runInspect(args []string) error {
	if helpRequested(args) {
		printInspectUsage(os.Stdout)
		return nil
	}
	o, err := parseRunOptions(args, "inspect", false)
	if err != nil {
		return err
	}
	m, err := loadAndVerifyManifest(o)
	if err != nil {
		return err
	}
	fmt.Printf("bundleId: %s\nrelease: %s\nbundleType: %s\nupdateKind: %s\nprofile: %s\ntarget: %s/%s\nimages: %d\n", m.BundleID, m.ReleaseVersion, m.BundleType, m.UpdateKind, m.Profile, m.TargetPlatform.OS, m.TargetPlatform.Architecture, len(m.Images))
	if m.BundleType == "delta" {
		fmt.Printf("base: %s/%s\nrequired images: %d\n", m.BaseBundleID, m.BaseRelease, len(m.RequiredImages))
	}
	for _, image := range m.Images {
		fmt.Printf("  %s: %s -> %s\n", image.Service, image.OriginalRef, image.LocalTag)
	}
	fmt.Printf("gpu requirements: %d, devices: %d, host paths: %d\n", len(m.Capabilities.GPU), len(m.Capabilities.Devices), len(m.Capabilities.HostPaths))
	if m.SchemaVersion == 2 || len(m.Assets) > 0 {
		var assetBytes int64
		for _, asset := range m.Assets {
			assetBytes += asset.Bytes
		}
		if m.Payload.AssetsArtifact == "" {
			fmt.Printf("assets: %d\nasset bytes: %d\nasset artifact: reused from %s\n", len(m.Assets), assetBytes, m.AssetsBaseManifest)
		} else {
			fmt.Printf("schemaVersion: %d\nassets: %d\nasset bytes: %d\nasset artifact bytes: %d\n", m.SchemaVersion, len(m.Assets), assetBytes, m.Payload.AssetsBytes)
		}
	}
	return nil
}

func runVerify(args []string) error {
	if helpRequested(args) {
		printVerifyUsage(os.Stdout)
		return nil
	}
	o, err := parseRunOptions(args, "verify", true)
	if err != nil {
		return err
	}
	key, err := keys.ReadSymmetric(o.keyFile)
	if err != nil {
		return err
	}
	m, err := loadAndVerifyManifest(o)
	if err != nil {
		return err
	}
	if err := verifyArtifacts(m, o.bundleDir, key); err != nil {
		return err
	}
	if err := checkPlatform(m, o.allowPlatform); err != nil {
		return err
	}
	if err := binding.Verify(m.Binding); err != nil {
		return err
	}
	if !o.skipHostCheck {
		if err := checkHostCapabilities(m); err != nil {
			return err
		}
	}
	fmt.Println("bundle signature, artifact hashes, encrypted streams, and host preflight verified")
	return nil
}

func runApply(args []string) error {
	if helpRequested(args) {
		printApplyUsage(os.Stdout)
		return nil
	}
	o, err := parseRunOptions(args, "apply", true)
	if err != nil {
		return err
	}
	key, err := keys.ReadSymmetric(o.keyFile)
	if err != nil {
		return err
	}
	m, err := loadAndVerifyManifest(o)
	if err != nil {
		return err
	}
	if err := verifyArtifactHashes(m, o.bundleDir); err != nil {
		return err
	}
	composeBytes, err := decryptCompose(m, o.bundleDir, key)
	if err != nil {
		return fmt.Errorf("decrypt Compose preflight: %w", err)
	}
	if bundle.SHA256Hex(composeBytes) != m.Compose.PackagedSHA256 || int64(len(composeBytes)) != m.Compose.Bytes {
		return errors.New("decrypted Compose hash or length does not match manifest")
	}
	if err := validateSelectedServices(composeBytes, o.services); err != nil {
		return err
	}
	var imageArchive *os.File
	if len(m.Images) > 0 {
		imageArchive, err = materializeImages(m, o.bundleDir, key)
		if err != nil {
			return fmt.Errorf("decrypt image archive preflight: %w", err)
		}
		defer func() {
			name := imageArchive.Name()
			_ = imageArchive.Close()
			_ = os.Remove(name)
		}()
	}
	workingDir := ""
	if len(m.Assets) > 0 {
		workingDir, err = materializeAssets(m, o.bundleDir, key, o.assetsDir)
		if err != nil {
			return fmt.Errorf("materialize Compose assets: %w", err)
		}
	}
	if err := checkPlatform(m, o.allowPlatform); err != nil {
		return err
	}
	if err := binding.Verify(m.Binding); err != nil {
		return err
	}
	if !o.skipHostCheck {
		if err := checkHostCapabilities(m); err != nil {
			return err
		}
	}
	if imageArchive != nil {
		if err := loadImages(context.Background(), imageArchive, o.dockerBin); err != nil {
			return err
		}
	}
	if err := startCompose(context.Background(), m, composeBytes, o.dockerBin, workingDir, o.services); err != nil {
		return err
	}
	fmt.Printf("bundle %s applied\n", m.BundleID)
	return nil
}

func validateSelectedServices(composeBytes []byte, selected []string) error {
	var model struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(composeBytes, &model); err != nil {
		return fmt.Errorf("parse verified Compose model: %w", err)
	}
	if len(model.Services) == 0 {
		return errors.New("verified Compose model has no services")
	}
	for _, service := range selected {
		if _, ok := model.Services[service]; !ok {
			return fmt.Errorf("service %q is not present in the packaged Compose model", service)
		}
	}
	return nil
}

func loadAndVerifyManifest(o runOptions) (*bundle.Manifest, error) {
	manifestPath := filepath.Join(o.bundleDir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	m, err := bundle.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	signature, err := os.ReadFile(filepath.Join(o.bundleDir, "manifest.sig"))
	if err != nil {
		return nil, err
	}
	pub, err := keys.ReadPublic(o.verifyKey)
	if err != nil {
		return nil, err
	}
	keyDigest := sha256.Sum256(pub)
	wantKeyID := "sha256:" + hex.EncodeToString(keyDigest[:8])
	if m.Signing.KeyID != wantKeyID {
		return nil, fmt.Errorf("manifest signing keyId %q does not match verify key", m.Signing.KeyID)
	}
	if err := bundle.VerifyManifest(m, signature, pub); err != nil {
		return nil, err
	}
	return m, nil
}

func checkPlatform(m *bundle.Manifest, allow bool) error {
	if allow {
		return nil
	}
	if m.TargetPlatform.OS != runtime.GOOS || m.TargetPlatform.Architecture != runtime.GOARCH {
		return fmt.Errorf("bundle targets %s/%s, current runtime is %s/%s (use --allow-platform-mismatch only when Docker target is known to match)", m.TargetPlatform.OS, m.TargetPlatform.Architecture, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func checkHostCapabilities(m *bundle.Manifest) error {
	for _, device := range m.Capabilities.Devices {
		if _, err := os.Stat(device.Host); err != nil {
			return fmt.Errorf("required host device %s for service %s is unavailable: %w", device.Host, device.Service, err)
		}
	}
	for _, path := range m.Capabilities.HostPaths {
		if path.Source == "" || !filepath.IsAbs(path.Source) {
			continue
		}
		if _, err := os.Stat(path.Source); err != nil {
			return fmt.Errorf("required host path %s for service %s is unavailable: %w", path.Source, path.Service, err)
		}
	}
	return nil
}

func verifyArtifacts(m *bundle.Manifest, dir string, key []byte) error {
	if err := verifyArtifactHashes(m, dir); err != nil {
		return err
	}
	composeBytes, err := decryptCompose(m, dir, key)
	if err != nil {
		return err
	}
	if bundle.SHA256Hex(composeBytes) != m.Compose.PackagedSHA256 || int64(len(composeBytes)) != m.Compose.Bytes {
		return errors.New("decrypted Compose hash or length does not match manifest")
	}
	if m.Payload.ImagesArtifact != "" {
		if err := verifyImagesStream(m, dir, key); err != nil {
			return err
		}
	}
	if m.Payload.AssetsArtifact != "" {
		if err := verifyAssetsStream(m, dir, key); err != nil {
			return err
		}
	}
	return nil
}

func verifyArtifactHashes(m *bundle.Manifest, dir string) error {
	composePath, err := artifactPath(dir, m.Payload.ComposeArtifact)
	if err != nil {
		return err
	}
	composeHash, composeBytes, err := files.SHA256(composePath)
	if err != nil {
		return err
	}
	if composeHash != m.Compose.EncryptedSHA256 || composeBytes != m.Compose.EncryptedBytes {
		return errors.New("encrypted Compose artifact hash or length does not match manifest")
	}
	if m.Payload.ImagesArtifact != "" {
		imagesPath, err := artifactPath(dir, m.Payload.ImagesArtifact)
		if err != nil {
			return err
		}
		imageHash, imageBytes, err := files.SHA256(imagesPath)
		if err != nil {
			return err
		}
		if imageHash != m.Payload.ImagesSHA256 || imageBytes != m.Payload.ImagesBytes {
			return errors.New("encrypted image artifact hash or length does not match manifest")
		}
	}
	if m.Payload.AssetsArtifact != "" {
		assetsPath, err := artifactPath(dir, m.Payload.AssetsArtifact)
		if err != nil {
			return err
		}
		assetHash, assetBytes, err := files.SHA256(assetsPath)
		if err != nil {
			return err
		}
		if assetHash != m.Payload.AssetsSHA256 || assetBytes != m.Payload.AssetsBytes {
			return errors.New("encrypted assets artifact hash or length does not match manifest")
		}
	}
	return nil
}

func artifactPath(dir, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.Contains(name, `\`) || strings.Contains(name, "/") || name == "." || name == ".." {
		return "", fmt.Errorf("invalid artifact path %q", name)
	}
	return filepath.Join(dir, name), nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func decryptCompose(m *bundle.Manifest, dir string, key []byte) ([]byte, error) {
	path, err := artifactPath(dir, m.Payload.ComposeArtifact)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out bytes.Buffer
	h := sha256.New()
	counted := &countingWriter{w: h}
	info, err := bundle.DecryptWithInfo(&out, io.TeeReader(f, counted), key)
	if err != nil {
		return nil, err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != m.Compose.EncryptedSHA256 || counted.n != m.Compose.EncryptedBytes {
		return nil, errors.New("encrypted Compose artifact changed during decryption")
	}
	if err := validateStreamInfo(m, info); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func validateStreamInfo(m *bundle.Manifest, info bundle.StreamInfo) error {
	if info.Version != 2 {
		return fmt.Errorf("unsupported encrypted stream version %d", info.Version)
	}
	if info.ChunkSize != m.Payload.ChunkSize {
		return fmt.Errorf("encrypted stream chunk size %d does not match manifest %d", info.ChunkSize, m.Payload.ChunkSize)
	}
	return nil
}

func verifyImagesStream(m *bundle.Manifest, dir string, key []byte) error {
	path, err := artifactPath(dir, m.Payload.ImagesArtifact)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	counted := &countingWriter{w: h}
	pr, pw := io.Pipe()
	type decryptResult struct {
		info bundle.StreamInfo
		err  error
	}
	decryptDone := make(chan decryptResult, 1)
	go func() {
		info, err := bundle.DecryptWithInfo(pw, io.TeeReader(f, counted), key)
		_ = pw.CloseWithError(err)
		decryptDone <- decryptResult{info: info, err: err}
	}()
	var reader io.Reader = pr
	var gz *gzip.Reader
	if m.Payload.Compression == "gzip" {
		gz, err = gzip.NewReader(pr)
		if err != nil {
			_ = pr.CloseWithError(err)
			<-decryptDone
			return err
		}
		reader = gz
	} else if m.Payload.Compression != "none" {
		err = fmt.Errorf("unsupported compression %q", m.Payload.Compression)
		_ = pr.CloseWithError(err)
		<-decryptDone
		return err
	}
	_, copyErr := io.Copy(io.Discard, reader)
	if copyErr != nil {
		_ = pr.CloseWithError(copyErr)
	}
	if gz != nil {
		if err := gz.Close(); copyErr == nil {
			copyErr = err
		}
	}
	if result := <-decryptDone; copyErr == nil {
		copyErr = result.err
		if copyErr == nil {
			if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != m.Payload.ImagesSHA256 || counted.n != m.Payload.ImagesBytes {
				copyErr = errors.New("encrypted image artifact changed during verification")
			} else {
				copyErr = validateStreamInfo(m, result.info)
			}
		}
	}
	return copyErr
}

// materializeImages fully authenticates and decompresses the image stream
// before Docker is invoked. The returned file is a private, seekable snapshot;
// this prevents a late AEAD or gzip failure from causing a partial docker load.
func materializeImages(m *bundle.Manifest, dir string, key []byte) (*os.File, error) {
	path, err := artifactPath(dir, m.Payload.ImagesArtifact)
	if err != nil {
		return nil, err
	}
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp("", "deploybundle-images-*.tar")
	if err != nil {
		return nil, err
	}
	if err := tmp.Chmod(0600); err != nil {
		name := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(name)
		return nil, err
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	h := sha256.New()
	counted := &countingWriter{w: h}
	pr, pw := io.Pipe()
	type decryptResult struct {
		info bundle.StreamInfo
		err  error
	}
	decryptDone := make(chan decryptResult, 1)
	go func() {
		info, err := bundle.DecryptWithInfo(pw, io.TeeReader(src, counted), key)
		_ = pw.CloseWithError(err)
		decryptDone <- decryptResult{info: info, err: err}
	}()
	var reader io.Reader = pr
	var gz *gzip.Reader
	if m.Payload.Compression == "gzip" {
		gz, err = gzip.NewReader(pr)
		if err != nil {
			_ = pr.CloseWithError(err)
			<-decryptDone
			return nil, err
		}
		reader = gz
	} else if m.Payload.Compression != "none" {
		err = fmt.Errorf("unsupported compression %q", m.Payload.Compression)
		_ = pr.CloseWithError(err)
		<-decryptDone
		return nil, err
	}
	_, copyErr := io.Copy(tmp, reader)
	if copyErr != nil {
		_ = pr.CloseWithError(copyErr)
	}
	if gz != nil {
		if err := gz.Close(); copyErr == nil {
			copyErr = err
		}
	}
	if result := <-decryptDone; copyErr == nil {
		copyErr = result.err
		if copyErr == nil {
			if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != m.Payload.ImagesSHA256 || counted.n != m.Payload.ImagesBytes {
				copyErr = errors.New("encrypted image artifact changed during decryption")
			} else {
				copyErr = validateStreamInfo(m, result.info)
			}
		}
	}
	if copyErr != nil {
		return nil, copyErr
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	removeOnError = false
	return tmp, nil
}

func loadImages(ctx context.Context, archive *os.File, dockerBin string) error {
	cmd := exec.CommandContext(ctx, dockerBin, "load")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start docker load: %w", err)
	}
	_, copyErr := io.Copy(stdin, archive)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if copyErr != nil {
		return fmt.Errorf("stream image archive: %w", copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("docker load failed: %w: %s", waitErr, stderr.String())
	}
	fmt.Print(stdout.String())
	return nil
}

func startCompose(ctx context.Context, m *bundle.Manifest, composeBytes []byte, dockerBin, projectDirectory string, services []string) error {
	project := sanitizeProject(m.BundleID)
	args := []string{"compose", "--project-name", project}
	if projectDirectory != "" {
		args = append(args, "--project-directory", projectDirectory)
	}
	args = append(args, "-f", "-", "up", "-d", "--pull", "never")
	args = append(args, services...)
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start docker compose: %w", err)
	}
	h := sha256.New()
	_, writeErr := io.Copy(io.MultiWriter(stdin, h), bytes.NewReader(composeBytes))
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != m.Compose.PackagedSHA256 {
		return errors.New("decrypted Compose hash changed before apply")
	}
	if waitErr != nil {
		return fmt.Errorf("docker compose failed: %w", waitErr)
	}
	return nil
}

func sanitizeProject(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	base := b.String()
	if base == "" {
		base = "deploybundle"
	}
	// Keep projects from distinct bundle IDs separate even when sanitization
	// would otherwise collapse names such as "foo/bar" and "foo-bar".
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-_")
		if base == "" {
			base = "deploybundle"
		}
	}
	sum := sha256.Sum256([]byte(s))
	return base + "-" + hex.EncodeToString(sum[:4])
}
