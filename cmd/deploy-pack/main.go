package main

import (
	"bytes"
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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/deploybundle/internal/assets"
	"github.com/openai/deploybundle/internal/binding"
	"github.com/openai/deploybundle/internal/compose"
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
  create    加密 Compose 和可选资产，生成独立签名 Bundle

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
  --bundle-id NAME          稳定的 Bundle 标识；同一服务栈更新时保持不变
  --release VERSION         发布版本，默认 dev
  --profile NAME            运行时 profile，默认 host-integrated-v1
  --platform OS/ARCH        目标平台，默认当前运行平台
  --compression NAME        资产归档压缩方式：gzip 或 none，默认 gzip
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
	fs.StringVar(&o.compression, "compression", "gzip", "asset archive compression: gzip or none")
	fs.IntVar(&o.chunkSize, "chunk-size", bundle.DefaultChunkSize, "encrypted stream chunk size")
	fs.StringVar(&o.bindMACs, "bind-mac", "", "optional comma-separated deployment MAC addresses")
	fs.BoolVar(&o.packageAssets, "package-assets", false, "package relative Compose config files into an encrypted asset artifact")
	fs.Var(&o.composeProfiles, "compose-profile", "Compose profile to include in the packaged release model (repeatable)")
	removedOptions := map[string]bool{"base-bundle": true, "base-manifest": true, "reuse-images-from": true}
	for name := range removedOptions {
		fs.String(name, "", "removed: new bundles contain Compose and optional assets only")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %q", fs.Args())
	}
	removed := ""
	fs.Visit(func(f *flag.Flag) {
		if removedOptions[f.Name] && removed == "" {
			removed = f.Name
		}
	})
	if removed != "" {
		return fmt.Errorf("--%s has been removed: create now encrypts standalone Compose and optional assets; supply images.tar separately when needed", removed)
	}
	for name, value := range map[string]string{"--compose": o.composePath, "--out": o.outDir, "--key-file": o.keyFile, "--signing-key": o.signingKey} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
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
	for name, value := range services {
		service, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("service %q has an invalid object", name)
		}
		ref, ok := service["image"].(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("service %q has no image; build-only services are not supported", name)
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
	assetsName := ""
	if len(sources) > 0 {
		assetsName = "assets.tar." + o.compression + ".enc"
		if err := saveAssets(filepath.Join(stage, assetsName), sources, o.compression, key, o.chunkSize); err != nil {
			return err
		}
	}
	composeHash, composeBytes, err := files.SHA256(composeArtifact)
	if err != nil {
		return err
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
		ComposeArtifact: "compose.enc",
		Compression:     o.compression, Encryption: bundle.EncryptionName, ChunkSize: o.chunkSize,
	}
	if assetsName != "" {
		payload.AssetsArtifact = assetsName
		payload.AssetsSHA256 = assetHash
		payload.AssetsBytes = assetBytes
	}
	// Artifact hashes are included in the manifest so a runner can reject a
	// replaced file before spending time decrypting or invoking Docker.
	manifest := &bundle.Manifest{
		SchemaVersion: 1, BundleType: "full", UpdateKind: "config-only", BundleID: o.bundleID, ReleaseVersion: o.release,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Profile: o.profile,
		TargetPlatform: platform,
		Compose:        bundle.ComposeArtifact{SourceSHA256: sourceHash, PackagedSHA256: packagedHash, Bytes: int64(len(canonicalCompose)), Artifact: "compose.enc", EncryptedSHA256: composeHash, EncryptedBytes: composeBytes},
		Images:         []bundle.ImageArtifact{}, Assets: assetManifest, Capabilities: capabilities, Binding: macBinding,
		Payload: payload,
		Signing: bundle.SigningSpec{Algorithm: "Ed25519", KeyID: keyID(privateKey)},
	}
	if assetsName != "" {
		manifest.SchemaVersion = 2
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
	fmt.Printf("created DeployBundle %s (config-only)\n", o.outDir)
	fmt.Printf("  manifest: %s\n  compose: %s (%d bytes)\n", filepath.Join(o.outDir, "manifest.json"), filepath.Join(o.outDir, "compose.enc"), composeBytes)
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
