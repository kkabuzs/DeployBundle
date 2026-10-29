package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/deploybundle/internal/assets"
	"github.com/openai/deploybundle/internal/binding"
	"github.com/openai/deploybundle/internal/compose"
	"github.com/openai/deploybundle/internal/docker"
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
	case "keygen":
		err = runKeygen(os.Args[2:])
	case "create":
		err = runCreate(os.Args[2:])
	case "help", "-h", "--help":
		printRootUsage(os.Stdout)
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy-pack:", err)
		os.Exit(1)
	}
}

func usage() {
	printRootUsage(os.Stderr)
}

func printRootUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-pack keygen [参数]
  deploy-pack create [参数]
  deploy-pack --help

命令：
  keygen    生成加密密钥和 Ed25519 签名密钥
  create    从 Compose 读取镜像，生成签名加密 Bundle

使用“deploy-pack <命令> --help”查看具体参数说明。`)
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printKeygenUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-pack keygen --key-out key.bin --signing-private-out signing.seed --signing-public-out signing.pub

参数：
  --key-out                输出 32 字节 Bundle 加密密钥（敏感文件）
  --signing-private-out    输出 Ed25519 签名私钥种子（敏感文件）
  --signing-public-out     输出 Ed25519 验签公钥
  -h, --help               显示此帮助

说明：同名文件会被覆盖；请将私钥和加密密钥保存在安全目录，不要提交到 Git。`)
}

func printCreateUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：
  deploy-pack create --compose compose.yaml --out bundle-dir --key-file key.bin --signing-key signing.seed [参数]

必填参数：
  --compose PATH            输入 Compose 文件，服务必须已有 image
  --out PATH                新 Bundle 输出目录，目录必须不存在
  --key-file PATH           32 字节对称加密密钥
  --signing-key PATH        Ed25519 签名私钥种子

常用参数：
  --env-file PATH           可选，Compose 变量插值用的环境文件
  --package-assets          将 Compose 引用的项目内相对文件一并加密打包
  --compose-profile NAME    启用 Compose profile，可重复指定
  --base-bundle PATH        基于旧 Bundle 生成只携带变化镜像的增量 Bundle
  --base-manifest PATH      直接使用旧 Bundle 的 manifest.json 作为增量基线
  --reuse-images-from PATH  复用完整 Bundle 的加密镜像归档，生成独立完整 Bundle（与增量基线互斥）
  --bundle-id NAME          稳定的 Bundle 标识；使用增量 Bundle 时必填
  --release VERSION         发布版本，默认 dev
  --profile NAME            运行时 profile，默认 host-integrated-v1
  --platform OS/ARCH        目标平台，默认当前运行平台
  --compression NAME        镜像归档压缩方式：gzip 或 none，默认 gzip
  --chunk-size BYTES        加密分块大小，默认 1 MiB
  --bind-mac ADDRESSES      可选，逗号分隔的部署主机 MAC 地址
  --project-name NAME       打包阶段使用的 Compose project 名
  --docker PATH              Docker CLI 路径，默认 docker
  -h, --help                 显示此帮助`)
}

func runKeygen(args []string) error {
	if helpRequested(args) {
		printKeygenUsage(os.Stdout)
		return nil
	}
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keyOut := fs.String("key-out", "", "32-byte symmetric encryption key output")
	privOut := fs.String("signing-private-out", "", "Ed25519 seed output")
	pubOut := fs.String("signing-public-out", "", "Ed25519 public key output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyOut == "" || *privOut == "" || *pubOut == "" {
		return errors.New("--key-out, --signing-private-out and --signing-public-out are required")
	}
	if err := keys.WriteSymmetric(*keyOut); err != nil {
		return err
	}
	if err := keys.GenerateSigningFiles(*privOut, *pubOut); err != nil {
		return err
	}
	fmt.Printf("generated encryption key: %s\nEd25519 private seed: %s\nEd25519 public key: %s\n", *keyOut, *privOut, *pubOut)
	return nil
}

type createOptions struct {
	composePath     string
	envFile         string
	outDir          string
	keyFile         string
	signingKey      string
	dockerBin       string
	project         string
	bundleID        string
	release         string
	profile         string
	platform        string
	compression     string
	chunkSize       int
	bindMACs        string
	packageAssets   bool
	composeProfiles stringList
	baseBundle      string
	baseManifest    string
	reuseImagesFrom string
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("invalid value %q", value)
	}
	for _, existing := range *values {
		if existing == value {
			return fmt.Errorf("duplicate value %q", value)
		}
	}
	*values = append(*values, value)
	return nil
}

func runCreate(args []string) error {
	if helpRequested(args) {
		printCreateUsage(os.Stdout)
		return nil
	}
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := createOptions{}
	fs.StringVar(&o.composePath, "compose", "", "input Compose file")
	fs.StringVar(&o.envFile, "env-file", "", "optional Compose interpolation environment file")
	fs.StringVar(&o.outDir, "out", "", "output bundle directory")
	fs.StringVar(&o.keyFile, "key-file", "", "32-byte symmetric key")
	fs.StringVar(&o.signingKey, "signing-key", "", "Ed25519 seed/private key")
	fs.StringVar(&o.dockerBin, "docker", "docker", "Docker CLI executable")
	fs.StringVar(&o.project, "project-name", "", "Compose project name")
	fs.StringVar(&o.bundleID, "bundle-id", "", "stable bundle identifier")
	fs.StringVar(&o.release, "release", "dev", "release version")
	fs.StringVar(&o.profile, "profile", "host-integrated-v1", "runtime profile")
	fs.StringVar(&o.platform, "platform", runtime.GOOS+"/"+runtime.GOARCH, "target OS/architecture")
	fs.StringVar(&o.compression, "compression", "gzip", "image archive compression: gzip or none")
	fs.IntVar(&o.chunkSize, "chunk-size", bundle.DefaultChunkSize, "encrypted stream chunk size")
	fs.StringVar(&o.bindMACs, "bind-mac", "", "optional comma-separated deployment MAC addresses")
	fs.BoolVar(&o.packageAssets, "package-assets", false, "package relative Compose config files into an encrypted asset artifact")
	fs.Var(&o.composeProfiles, "compose-profile", "Compose profile to include in the packaged release model (repeatable)")
	fs.StringVar(&o.baseBundle, "base-bundle", "", "base bundle directory; create a delta bundle with only changed images")
	fs.StringVar(&o.baseManifest, "base-manifest", "", "base manifest.json; create a delta bundle without retaining the full base bundle")
	fs.StringVar(&o.reuseImagesFrom, "reuse-images-from", "", "reuse the encrypted image archive from a complete bundle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", fs.Args())
	}
	for name, value := range map[string]string{"--compose": o.composePath, "--out": o.outDir, "--key-file": o.keyFile, "--signing-key": o.signingKey} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if o.baseBundle != "" && o.bundleID == "" {
		return errors.New("--bundle-id is required when --base-bundle is used")
	}
	if o.baseManifest != "" && o.bundleID == "" {
		return errors.New("--bundle-id is required when --base-manifest is used")
	}
	if o.baseBundle != "" && o.baseManifest != "" {
		return errors.New("--base-bundle and --base-manifest are mutually exclusive")
	}
	if (o.baseBundle != "" || o.baseManifest != "") && o.reuseImagesFrom != "" {
		return errors.New("--base-bundle/--base-manifest and --reuse-images-from are mutually exclusive")
	}
	if o.baseManifest != "" {
		info, err := os.Stat(o.baseManifest)
		if err != nil {
			return fmt.Errorf("--base-manifest: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("--base-manifest must be a file: %s", o.baseManifest)
		}
	}
	if o.reuseImagesFrom != "" {
		info, err := os.Stat(o.reuseImagesFrom)
		if err != nil {
			return fmt.Errorf("--reuse-images-from: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("--reuse-images-from must be a bundle directory: %s", o.reuseImagesFrom)
		}
	}
	if o.envFile != "" {
		info, err := os.Stat(o.envFile)
		if err != nil {
			return fmt.Errorf("--env-file: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("--env-file must be a file: %s", o.envFile)
		}
	}
	if o.compression != "gzip" && o.compression != "none" {
		return fmt.Errorf("unsupported compression %q (use gzip or none)", o.compression)
	}
	key, err := keys.ReadSymmetric(o.keyFile)
	if err != nil {
		return err
	}
	privateKey, err := keys.ReadPrivate(o.signingKey)
	if err != nil {
		return err
	}
	platform, err := parsePlatform(o.platform)
	if err != nil {
		return err
	}
	if o.bundleID == "" {
		o.bundleID = "bundle-" + time.Now().UTC().Format("20060102T150405Z")
	}
	return createBundle(context.Background(), o, key, privateKey, platform)
}

func createBundle(ctx context.Context, o createOptions, key []byte, privateKey ed25519.PrivateKey, platform bundle.TargetPlatform) error {
	var base *bundle.Manifest
	var reused *bundle.Manifest
	baseDigest := ""
	basePath := o.baseBundle
	if o.baseManifest != "" {
		basePath = o.baseManifest
	}
	if basePath != "" {
		var err error
		base, err = readBundleManifest(basePath)
		if err != nil {
			return fmt.Errorf("read base manifest: %w", err)
		}
		if base.BundleID != o.bundleID {
			return fmt.Errorf("base bundleId %q does not match requested bundleId %q", base.BundleID, o.bundleID)
		}
		if base.TargetPlatform != platform {
			return fmt.Errorf("base bundle platform %s/%s does not match requested platform %s/%s", base.TargetPlatform.OS, base.TargetPlatform.Architecture, platform.OS, platform.Architecture)
		}
		digest, err := bundle.ManifestDigest(base)
		if err != nil {
			return fmt.Errorf("digest base manifest: %w", err)
		}
		baseDigest = "sha256:" + hex.EncodeToString(digest[:])
	}
	if o.reuseImagesFrom != "" {
		var err error
		reused, err = readBundleManifest(o.reuseImagesFrom)
		if err != nil {
			return fmt.Errorf("read image reuse bundle: %w", err)
		}
		if reused.BundleType != "" && reused.BundleType != "full" {
			return fmt.Errorf("image reuse bundle must be complete, got bundleType %q", reused.BundleType)
		}
		if reused.TargetPlatform != platform {
			return fmt.Errorf("image reuse bundle platform %s/%s does not match requested platform %s/%s", reused.TargetPlatform.OS, reused.TargetPlatform.Architecture, platform.OS, platform.Architecture)
		}
		if reused.Payload.Compression != o.compression {
			return fmt.Errorf("image reuse bundle compression %q does not match requested compression %q", reused.Payload.Compression, o.compression)
		}
		if reused.Payload.ChunkSize != o.chunkSize {
			return fmt.Errorf("image reuse bundle chunk size %d does not match requested chunk size %d", reused.Payload.ChunkSize, o.chunkSize)
		}
		if strings.TrimSpace(reused.Payload.ImagesArtifact) == "" {
			return errors.New("image reuse bundle has no encrypted image artifact")
		}
	}
	// A new Bundle uses the original Compose image reference as its runtime
	// tag. When extending a legacy Bundle that used bundle.local/<id> tags,
	// preserve the previous tag for unchanged images so configuration-only
	// updates remain compatible with images already loaded on the target host.
	previousImages := make(map[string]bundle.ImageArtifact)
	if base != nil {
		baseImages := base.Images
		if base.BundleType == "delta" && len(base.RequiredImages) > 0 {
			baseImages = base.RequiredImages
		}
		for _, image := range baseImages {
			previousImages[image.Service] = image
		}
	} else if reused != nil {
		for _, image := range reused.Images {
			previousImages[image.Service] = image
		}
	}
	sourceCompose, err := os.ReadFile(o.composePath)
	if err != nil {
		return err
	}
	sourceHash := bundle.SHA256Hex(sourceCompose)
	config, err := compose.Config(ctx, o.dockerBin, o.composePath, o.project, o.envFile, o.composeProfiles, o.packageAssets)
	if err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return errors.New("Compose contains no services")
	}
	var sources []assets.Source
	if o.packageAssets {
		sources, err = assets.Collect(config, o.composePath)
		if err != nil {
			return err
		}
	}
	assetManifest := assetManifests(sources)
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	images := make([]bundle.ImageArtifact, 0, len(services))
	for _, service := range serviceNames {
		serviceValue, ok := services[service].(map[string]any)
		if !ok {
			return fmt.Errorf("service %q has an invalid object", service)
		}
		ref, ok := serviceValue["image"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("service %q has no image; build-only services are not supported by this MVP", service)
		}
		inspect, err := docker.Inspect(ctx, o.dockerBin, ref)
		if err != nil {
			return err
		}
		if inspect.OS != "" && inspect.OS != platform.OS {
			return fmt.Errorf("service %q image %q targets OS %s, but bundle platform is %s", service, ref, inspect.OS, platform.OS)
		}
		if inspect.Architecture != "" && inspect.Architecture != platform.Architecture {
			return fmt.Errorf("service %q image %q targets architecture %s, but bundle platform is %s", service, ref, inspect.Architecture, platform.Architecture)
		}
		runtimeTag := ref
		if previous, ok := previousImages[service]; ok &&
			previous.LocalImageID == inspect.ID && previous.OriginalRef == ref &&
			strings.TrimSpace(previous.LocalTag) != "" {
			runtimeTag = previous.LocalTag
		}
		serviceValue["image"] = runtimeTag
		serviceValue["pull_policy"] = "never"
		sourceDigest := "local:" + inspect.ID
		if len(inspect.RepoDigests) > 0 {
			sourceDigest = inspect.RepoDigests[0]
		}
		images = append(images, bundle.ImageArtifact{
			Service: service, OriginalRef: ref, SourceRepoDigest: sourceDigest,
			LocalImageID: inspect.ID, RootfsDiffIDs: append([]string(nil), inspect.RootFS.Layers...), LocalTag: runtimeTag,
		})
	}
	includedImages := images
	if base != nil {
		baseImages := base.Images
		if base.BundleType == "delta" && len(base.RequiredImages) > 0 {
			baseImages = base.RequiredImages
		}
		baseByService := make(map[string]bundle.ImageArtifact, len(baseImages))
		for _, image := range baseImages {
			baseByService[image.Service] = image
		}
		includedImages = make([]bundle.ImageArtifact, 0, len(images))
		for _, image := range images {
			previous, ok := baseByService[image.Service]
			if !ok || previous.LocalImageID != image.LocalImageID || previous.LocalTag != image.LocalTag {
				includedImages = append(includedImages, image)
			}
		}
	}
	if reused != nil {
		if err := validateReusableImages(images, reused.Images); err != nil {
			return err
		}
	}
	assetsBaseManifest := ""
	if base != nil && len(assetManifest) > 0 && sameAssetManifests(assetManifest, base.Assets) {
		if base.Payload.AssetsArtifact != "" {
			assetsBaseManifest = baseDigest
		} else {
			assetsBaseManifest = base.AssetsBaseManifest
		}
	}
	canonicalCompose, err := bundle.MarshalCanonical(config)
	if err != nil {
		return err
	}
	packagedHash := bundle.SHA256Hex(canonicalCompose)
	capabilities, err := summarizeCapabilities(config)
	if err != nil {
		return err
	}
	macBinding, err := makeMACBinding(o.bindMACs)
	if err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Clean(o.outDir))
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(o.outDir); err == nil {
		return fmt.Errorf("output directory already exists: %s", o.outDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".deploybundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	composeArtifact := filepath.Join(stage, "compose.enc")
	if err := encryptBytes(composeArtifact, canonicalCompose, key, o.chunkSize); err != nil {
		return err
	}
	imagesName := ""
	if len(includedImages) > 0 {
		if reused != nil {
			imagesName = reused.Payload.ImagesArtifact
			reusedImagesPath := filepath.Join(o.reuseImagesFrom, imagesName)
			reusedHash, reusedBytes, err := files.SHA256(reusedImagesPath)
			if err != nil {
				return fmt.Errorf("hash reused encrypted image artifact: %w", err)
			}
			if reusedHash != reused.Payload.ImagesSHA256 || reusedBytes != reused.Payload.ImagesBytes {
				return fmt.Errorf("image reuse artifact hash or size does not match its manifest")
			}
			if err := copyFile(reusedImagesPath, filepath.Join(stage, imagesName)); err != nil {
				return fmt.Errorf("reuse encrypted image artifact: %w", err)
			}
		} else {
			imagesName = "images.tar." + o.compression + ".enc"
			imagesArtifact := filepath.Join(stage, imagesName)
			includedTags := make([]string, 0, len(includedImages))
			for _, image := range includedImages {
				includedTags = append(includedTags, image.LocalTag)
			}
			if err := saveImages(ctx, o.dockerBin, includedTags, imagesArtifact, o.compression, key, o.chunkSize); err != nil {
				return err
			}
		}
	}
	assetsName := ""
	if len(sources) > 0 && assetsBaseManifest == "" {
		assetsName = "assets.tar." + o.compression + ".enc"
		if err := saveAssets(filepath.Join(stage, assetsName), sources, o.compression, key, o.chunkSize); err != nil {
			return err
		}
	}
	composeHash, composeBytes, err := files.SHA256(composeArtifact)
	if err != nil {
		return err
	}
	imageHash := ""
	var imageBytes int64
	if imagesName != "" {
		imageHash, imageBytes, err = files.SHA256(filepath.Join(stage, imagesName))
		if err != nil {
			return err
		}
	}
	assetHash := ""
	var assetBytes int64
	if assetsName != "" {
		assetHash, assetBytes, err = files.SHA256(filepath.Join(stage, assetsName))
		if err != nil {
			return err
		}
	}
	payload := bundle.PayloadSpec{
		ComposeArtifact: "compose.enc", ImagesArtifact: imagesName, ImagesSHA256: imageHash, ImagesBytes: imageBytes,
		Compression: o.compression, Encryption: bundle.EncryptionName, ChunkSize: o.chunkSize,
	}
	if assetsName != "" {
		payload.AssetsArtifact = assetsName
		payload.AssetsSHA256 = assetHash
		payload.AssetsBytes = assetBytes
	}
	// Artifact hashes are included in the manifest so a runner can reject a
	// replaced file before spending time decrypting or invoking Docker.
	manifest := &bundle.Manifest{
		SchemaVersion: 1, BundleType: "full", BundleID: o.bundleID, ReleaseVersion: o.release,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Profile: o.profile,
		TargetPlatform: platform,
		Compose:        bundle.ComposeArtifact{SourceSHA256: sourceHash, PackagedSHA256: packagedHash, Bytes: int64(len(canonicalCompose)), Artifact: "compose.enc", EncryptedSHA256: composeHash, EncryptedBytes: composeBytes},
		Images:         includedImages, Assets: assetManifest, Capabilities: capabilities, Binding: macBinding,
		AssetsBaseManifest: assetsBaseManifest,
		Payload:            payload,
		Signing:            bundle.SigningSpec{Algorithm: "Ed25519", KeyID: keyID(privateKey)},
	}
	if base != nil {
		manifest.SchemaVersion = 3
		manifest.BundleType = "delta"
		manifest.BaseBundleID = base.BundleID
		manifest.BaseRelease = base.ReleaseVersion
		manifest.BaseManifest = baseDigest
		manifest.RequiredImages = images
	}
	// Schema 2 is used for full bundles with assets. Delta bundles stay on
	// schema 3 even when they carry an encrypted asset artifact.
	if assetsName != "" && base == nil {
		manifest.SchemaVersion = 2
	}
	if base == nil {
		manifest.UpdateKind = "full"
	} else if len(includedImages) == 0 {
		manifest.UpdateKind = "config-only"
	} else {
		manifest.UpdateKind = "mixed"
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	manifestBytes, err := bundle.MarshalManifest(manifest)
	if err != nil {
		return err
	}
	signature, err := bundle.SignManifest(manifest, privateKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), manifestBytes, 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.sig"), signature, 0644); err != nil {
		return err
	}
	if err := os.Rename(stage, o.outDir); err != nil {
		return fmt.Errorf("publish bundle: %w", err)
	}
	fmt.Printf("created DeployBundle %s (%s, %d images, %d carried)\n", o.outDir, manifest.UpdateKind, len(images), len(includedImages))
	if imagesName != "" {
		fmt.Printf("  images: %s\n", filepath.Join(o.outDir, imagesName))
	}
	if assetsBaseManifest != "" {
		fmt.Printf("  assets: reused from %s\n", assetsBaseManifest)
	}
	fmt.Printf("  manifest: %s\n  compose: %s (%d bytes)\n", filepath.Join(o.outDir, "manifest.json"), filepath.Join(o.outDir, "compose.enc"), composeBytes)
	return nil
}

func validateReusableImages(current, reusable []bundle.ImageArtifact) error {
	if len(current) != len(reusable) {
		return fmt.Errorf("image reuse bundle has %d images, current Compose has %d", len(reusable), len(current))
	}
	byService := make(map[string]bundle.ImageArtifact, len(reusable))
	for _, image := range reusable {
		byService[image.Service] = image
	}
	for _, image := range current {
		previous, ok := byService[image.Service]
		if !ok {
			return fmt.Errorf("image reuse bundle is missing service %q", image.Service)
		}
		if previous.OriginalRef != image.OriginalRef || previous.SourceRepoDigest != image.SourceRepoDigest ||
			previous.LocalImageID != image.LocalImageID || previous.LocalTag != image.LocalTag ||
			!stringSlicesEqual(previous.RootfsDiffIDs, image.RootfsDiffIDs) {
			return fmt.Errorf("image metadata for service %q differs from reuse bundle; use the matching full Bundle", image.Service)
		}
	}
	return nil
}

func assetManifests(sources []assets.Source) []bundle.AssetArtifact {
	if len(sources) == 0 {
		return nil
	}
	result := make([]bundle.AssetArtifact, 0, len(sources))
	for _, source := range sources {
		result = append(result, source.Manifest)
	}
	return result
}

func sameAssetManifests(a, b []bundle.AssetArtifact) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func readBundleManifest(dir string) (*bundle.Manifest, error) {
	path := dir
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		path = filepath.Join(dir, "manifest.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := bundle.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func makeMACBinding(value string) (bundle.Binding, error) {
	if strings.TrimSpace(value) == "" {
		return bundle.Binding{Mode: "none", Values: []string{}}, nil
	}
	parts := strings.Split(value, ",")
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return bundle.Binding{}, fmt.Errorf("generate MAC binding salt: %w", err)
	}
	return binding.MakeBinding(parts, salt)
}

func encryptBytes(path string, data, key []byte, chunkSize int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encryptErr := bundle.Encrypt(f, bytes.NewReader(data), key, bundle.WithChunkSize(chunkSize))
	closeErr := f.Close()
	if encryptErr != nil {
		return encryptErr
	}
	return closeErr
}

func saveImages(ctx context.Context, dockerBin string, tags []string, output, compression string, key []byte, chunkSize int) error {
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	args := append([]string{"save"}, tags...)
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = f.Close()
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return fmt.Errorf("start docker save: %w", err)
	}
	pr, pw := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		var streamErr error
		if compression == "gzip" {
			gz := gzip.NewWriter(pw)
			_, streamErr = io.Copy(gz, stdout)
			if closeErr := gz.Close(); streamErr == nil {
				streamErr = closeErr
			}
		} else {
			_, streamErr = io.Copy(pw, stdout)
		}
		pw.CloseWithError(streamErr)
		streamDone <- streamErr
	}()
	encryptErr := bundle.Encrypt(f, pr, key, bundle.WithChunkSize(chunkSize))
	if encryptErr != nil {
		// Unblock the producer before waiting for it. Without closing the read
		// side, a producer can remain blocked on the pipe after an output error.
		_ = pr.CloseWithError(encryptErr)
		_ = cmd.Process.Kill()
	}
	closeErr := f.Close()
	waitErr := cmd.Wait()
	streamErr := <-streamDone
	if encryptErr != nil {
		return fmt.Errorf("encrypt image archive: %w", encryptErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if streamErr != nil {
		return fmt.Errorf("read docker save stream: %w", streamErr)
	}
	if waitErr != nil {
		return fmt.Errorf("docker save failed: %w: %s", waitErr, stderr.String())
	}
	return nil
}

func parsePlatform(value string) (bundle.TargetPlatform, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return bundle.TargetPlatform{}, fmt.Errorf("platform must be os/architecture, got %q", value)
	}
	return bundle.TargetPlatform{OS: parts[0], Architecture: parts[1]}, nil
}

func keyID(privateKey ed25519.PrivateKey) string {
	sum := sha256.Sum256(privateKey.Public().(ed25519.PublicKey))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func summarizeCapabilities(config map[string]any) (bundle.Capabilities, error) {
	result := bundle.Capabilities{GPU: []bundle.GPUCapability{}, Devices: []bundle.DeviceCapability{}, HostPaths: []bundle.HostPathCapability{}, Privileged: []string{}, HostNetwork: []string{}, HostPID: []string{}}
	services, ok := config["services"].(map[string]any)
	if !ok {
		return result, errors.New("compose config has no services")
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, service := range names {
		svc, ok := services[service].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := svc["privileged"].(bool); ok && v {
			result.Privileged = append(result.Privileged, service)
		}
		if v, ok := svc["network_mode"].(string); ok && v == "host" {
			result.HostNetwork = append(result.HostNetwork, service)
		}
		if v, ok := svc["pid"].(string); ok && v == "host" {
			result.HostPID = append(result.HostPID, service)
		}
		if devices, ok := svc["devices"].([]any); ok {
			for _, raw := range devices {
				if value, ok := raw.(string); ok {
					parts := strings.Split(value, ":")
					if len(parts) >= 2 {
						perm := "rwm"
						if len(parts) >= 3 && parts[2] != "" {
							perm = parts[2]
						}
						result.Devices = append(result.Devices, bundle.DeviceCapability{Service: service, Host: parts[0], Container: parts[1], Permissions: perm})
					}
				}
			}
		}
		if volumes, ok := svc["volumes"].([]any); ok {
			for _, raw := range volumes {
				switch value := raw.(type) {
				case string:
					parts := strings.Split(value, ":")
					if len(parts) >= 2 && strings.HasPrefix(parts[0], "/") {
						mode := "rw"
						if len(parts) >= 3 && parts[2] != "" {
							mode = parts[2]
						}
						result.HostPaths = append(result.HostPaths, bundle.HostPathCapability{Service: service, Source: parts[0], Target: parts[1], Mode: mode})
					}
				case map[string]any:
					if typ, _ := value["type"].(string); typ == "bind" {
						source, _ := value["source"].(string)
						target, _ := value["target"].(string)
						if strings.HasPrefix(source, "/") {
							result.HostPaths = append(result.HostPaths, bundle.HostPathCapability{Service: service, Source: source, Target: target, Mode: "rw"})
						}
					}
				}
			}
		}
		if deploy, ok := svc["deploy"].(map[string]any); ok {
			if resources, ok := deploy["resources"].(map[string]any); ok {
				if reservations, ok := resources["reservations"].(map[string]any); ok {
					if gpus, ok := reservations["devices"].([]any); ok {
						for _, raw := range gpus {
							device, ok := raw.(map[string]any)
							if !ok {
								continue
							}
							driver, _ := device["driver"].(string)
							count := ""
							if v, ok := device["count"].(string); ok {
								count = v
							} else if v, ok := device["count"].(float64); ok {
								count = fmt.Sprintf("%g", v)
							}
							caps := []string{}
							if values, ok := device["capabilities"].([]any); ok {
								for _, v := range values {
									if s, ok := v.(string); ok {
										caps = append(caps, s)
									}
								}
							}
							ids := []string{}
							if values, ok := device["device_ids"].([]any); ok {
								for _, v := range values {
									if s, ok := v.(string); ok {
										ids = append(ids, s)
									}
								}
							}
							result.GPU = append(result.GPU, bundle.GPUCapability{Service: service, Driver: driver, Count: count, Capabilities: caps, DeviceIDs: ids})
						}
					}
				}
			}
		}
	}
	return result, nil
}
