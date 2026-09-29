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
		usage()
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
	fmt.Fprintln(os.Stderr, `Usage:
  deploy-pack keygen --key-out key.bin --signing-private-out signing.seed --signing-public-out signing.pub
  deploy-pack create --compose compose.yaml --out bundle-dir --key-file key.bin --signing-key signing.seed

create reads image references from Compose using Docker Compose, tags them with
bundle-local names, exports all images in one docker archive, and writes signed
encrypted artifacts. The input Compose is never overwritten.`)
}

func runKeygen(args []string) error {
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
	composePath string
	outDir      string
	keyFile     string
	signingKey  string
	dockerBin   string
	project     string
	bundleID    string
	release     string
	profile     string
	platform    string
	compression string
	chunkSize   int
	bindMACs    string
}

func runCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := createOptions{}
	fs.StringVar(&o.composePath, "compose", "", "input Compose file")
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	for name, value := range map[string]string{"--compose": o.composePath, "--out": o.outDir, "--key-file": o.keyFile, "--signing-key": o.signingKey} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
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
	config, err := compose.Config(ctx, o.dockerBin, o.composePath, o.project)
	if err != nil {
		return err
	}
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return errors.New("Compose contains no services")
	}
	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)
	images := make([]bundle.ImageArtifact, 0, len(services))
	localTags := make([]string, 0, len(services))
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
		localTag := makeLocalTag(o.bundleID, service, inspect.ID)
		if err := docker.Tag(ctx, o.dockerBin, ref, localTag); err != nil {
			return err
		}
		serviceValue["image"] = localTag
		serviceValue["pull_policy"] = "never"
		sourceDigest := "local:" + inspect.ID
		if len(inspect.RepoDigests) > 0 {
			sourceDigest = inspect.RepoDigests[0]
		}
		images = append(images, bundle.ImageArtifact{
			Service: service, OriginalRef: ref, SourceRepoDigest: sourceDigest,
			LocalImageID: inspect.ID, RootfsDiffIDs: append([]string(nil), inspect.RootFS.Layers...), LocalTag: localTag,
		})
		localTags = append(localTags, localTag)
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
	imagesName := "images.tar." + o.compression + ".enc"
	imagesArtifact := filepath.Join(stage, imagesName)
	if err := saveImages(ctx, o.dockerBin, localTags, imagesArtifact, o.compression, key, o.chunkSize); err != nil {
		return err
	}
	composeHash, composeBytes, err := files.SHA256(composeArtifact)
	if err != nil {
		return err
	}
	imageHash, imageBytes, err := files.SHA256(imagesArtifact)
	if err != nil {
		return err
	}
	// Artifact hashes are included in the manifest so a runner can reject a
	// replaced file before spending time decrypting or invoking Docker.
	manifest := &bundle.Manifest{
		SchemaVersion: 1, BundleID: o.bundleID, ReleaseVersion: o.release,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Profile: o.profile,
		TargetPlatform: platform,
		Compose:        bundle.ComposeArtifact{SourceSHA256: sourceHash, PackagedSHA256: packagedHash, Bytes: int64(len(canonicalCompose)), Artifact: "compose.enc", EncryptedSHA256: composeHash, EncryptedBytes: composeBytes},
		Images:         images, Capabilities: capabilities, Binding: macBinding,
		Payload: bundle.PayloadSpec{ComposeArtifact: "compose.enc", ImagesArtifact: imagesName, ImagesSHA256: imageHash, ImagesBytes: imageBytes, Compression: o.compression, Encryption: bundle.EncryptionName, ChunkSize: o.chunkSize},
		Signing: bundle.SigningSpec{Algorithm: "Ed25519", KeyID: keyID(privateKey)},
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
	fmt.Printf("created DeployBundle %s (%d images)\n", o.outDir, len(images))
	fmt.Printf("  manifest: %s\n  compose: %s (%d bytes)\n  images: %s\n", filepath.Join(o.outDir, "manifest.json"), filepath.Join(o.outDir, "compose.enc"), composeBytes, filepath.Join(o.outDir, imagesName))
	return nil
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
	defer f.Close()
	return bundle.Encrypt(f, bytes.NewReader(data), key, bundle.WithChunkSize(chunkSize))
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

func makeLocalTag(bundleID, service, imageID string) string {
	sanitize := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
		v := strings.Trim(b.String(), "-._")
		if v == "" {
			return "bundle"
		}
		return v
	}
	id := strings.TrimPrefix(imageID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return "bundle.local/" + sanitize(bundleID) + "/" + sanitize(service) + ":" + id
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
