package assets

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openai/deploybundle/pkg/bundle"
)

type Source struct {
	Manifest bundle.AssetArtifact
	Data     []byte
}

// Collect snapshots relative file mounts, configs.file and secrets.file.
// Absolute mounts are host resources; named volumes never enter the archive.
func Collect(config map[string]any, composePath string) ([]Source, error) {
	root, err := filepath.Abs(filepath.Dir(composePath))
	if err != nil {
		return nil, err
	}
	seen := map[string]Source{}
	var total int64
	add := func(value string, allowAbsolute bool) (string, error) {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("asset path is not valid UTF-8 or contains NUL: %q", value)
		}
		if filepath.IsAbs(value) {
			if allowAbsolute {
				return value, nil
			}
			return "", fmt.Errorf("absolute config or secret asset is unsupported: %q", value)
		}
		if strings.Contains(value, `\`) {
			return "", fmt.Errorf("asset path contains a backslash: %q", value)
		}
		for _, part := range strings.Split(filepath.ToSlash(value), "/") {
			if part == ".." {
				return "", fmt.Errorf("asset path contains traversal: %q", value)
			}
		}
		name := path.Clean(filepath.ToSlash(value))
		if !SafePath(name) {
			return "", fmt.Errorf("unsafe asset path %q", value)
		}
		if _, exists := seen[name]; exists {
			return "./" + name, nil
		}

		full := filepath.Join(root, filepath.FromSlash(name))
		current := root
		for _, part := range strings.Split(name, "/") {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil {
				return "", fmt.Errorf("asset %q: %w", value, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("asset symlink is not allowed: %q", value)
			}
		}
		f, err := os.Open(full)
		if err != nil {
			return "", fmt.Errorf("open asset %q: %w", value, err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return "", fmt.Errorf("stat asset %q: %w", value, err)
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return "", fmt.Errorf("asset %q must be a regular file; use a named volume or absolute host path for directories", value)
		}
		if linkCount(info) > 1 {
			_ = f.Close()
			return "", fmt.Errorf("asset hardlink is not allowed: %q", value)
		}
		if len(seen) >= bundle.MaxAssetFiles || info.Size() > bundle.MaxAssetFileBytes || info.Size() > bundle.MaxAssetBytes-total {
			_ = f.Close()
			return "", fmt.Errorf("assets exceed limit (%d files, 64 MiB)", bundle.MaxAssetFiles)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, bundle.MaxAssetBytes-total+1))
		post, statErr := f.Stat()
		closeErr := f.Close()
		if readErr != nil {
			return "", fmt.Errorf("read asset %q: %w", value, readErr)
		}
		if statErr != nil {
			return "", fmt.Errorf("restat asset %q: %w", value, statErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close asset %q: %w", value, closeErr)
		}
		if !os.SameFile(info, post) || info.Size() != post.Size() || info.Mode() != post.Mode() {
			return "", fmt.Errorf("asset changed while reading: %q", value)
		}
		if int64(len(data)) != info.Size() || int64(len(data)) > bundle.MaxAssetBytes-total {
			return "", fmt.Errorf("asset changed while reading or exceeds 64 MiB limit: %q", value)
		}
		total += int64(len(data))
		seen[name] = Source{Manifest: bundle.AssetArtifact{
			Path: name, SHA256: bundle.SHA256Hex(data), Bytes: int64(len(data)), Mode: uint32(info.Mode().Perm()),
		}, Data: data}
		return "./" + name, nil
	}

	services, _ := config["services"].(map[string]any)
	for service, raw := range services {
		svc, _ := raw.(map[string]any)
		volumes, _ := svc["volumes"].([]any)
		for i, raw := range volumes {
			switch v := raw.(type) {
			case string:
				parts := strings.Split(v, ":")
				if len(parts) < 2 || !strings.HasPrefix(parts[0], ".") {
					continue
				}
				parts[0], err = add(parts[0], true)
				if err != nil {
					return nil, fmt.Errorf("service %q: %w", service, err)
				}
				volumes[i] = strings.Join(parts, ":")
			case map[string]any:
				typ, _ := v["type"].(string)
				if typ != "bind" {
					continue
				}
				src, _ := v["source"].(string)
				v["source"], err = add(src, true)
				if err != nil {
					return nil, fmt.Errorf("service %q: %w", service, err)
				}
			}
		}
	}
	for _, section := range []string{"configs", "secrets"} {
		objects, _ := config[section].(map[string]any)
		for name, raw := range objects {
			object, _ := raw.(map[string]any)
			file, ok := object["file"].(string)
			if !ok {
				continue
			}
			object["file"], err = add(file, false)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", section, name, err)
			}
		}
	}
	result := make([]Source, 0, len(seen))
	for _, source := range seen {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Manifest.Path < result[j].Manifest.Path })
	return result, nil
}

func WriteTar(dst io.Writer, sources []Source) error {
	tw := tar.NewWriter(dst)
	for _, source := range sources {
		m := source.Manifest
		if err := tw.WriteHeader(&tar.Header{Name: m.Path, Mode: int64(m.Mode), Size: m.Bytes, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(source.Data); err != nil {
			return err
		}
	}
	return tw.Close()
}

func SafePath(name string) bool {
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(name) > 4096 {
		return false
	}
	if name == "" || name == "." || name == ".." || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, `\`) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." || len(component) > 255 {
			return false
		}
	}
	return true
}

// ReadTar validates the exact signed file set. dest must be a new, private
// directory; an empty dest verifies without persisting plaintext.
func ReadTar(src io.Reader, entries []bundle.AssetArtifact, dest string) error {
	want := make(map[string]bundle.AssetArtifact, len(entries))
	for _, entry := range entries {
		if !SafePath(entry.Path) {
			return fmt.Errorf("unsafe asset path %q", entry.Path)
		}
		if _, exists := want[entry.Path]; exists {
			return fmt.Errorf("duplicate asset path %q", entry.Path)
		}
		want[entry.Path] = entry
	}
	if len(want) > bundle.MaxAssetFiles {
		return fmt.Errorf("asset archive exceeds %d-file limit", bundle.MaxAssetFiles)
	}

	seen := map[string]bool{}
	var total int64
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read asset archive: %w", err)
		}
		if !SafePath(h.Name) {
			return fmt.Errorf("unsafe asset archive path %q", h.Name)
		}
		entry, ok := want[h.Name]
		if !ok || seen[h.Name] {
			return fmt.Errorf("unexpected or duplicate asset %q", h.Name)
		}
		if h.Typeflag != tar.TypeReg || h.Linkname != "" {
			return fmt.Errorf("asset %q is not a regular file", h.Name)
		}
		if h.Size < 0 || h.Size > bundle.MaxAssetFileBytes || h.Size != entry.Bytes || h.Mode < 0 || uint32(h.Mode) != entry.Mode {
			return fmt.Errorf("asset metadata mismatch: %q", h.Name)
		}
		if total > bundle.MaxAssetBytes-h.Size {
			return fmt.Errorf("asset archive exceeds 64 MiB limit")
		}
		total += h.Size

		hash := sha256.New()
		var output io.Writer = hash
		var f *os.File
		if dest != "" {
			name, err := destinationPath(dest, entry.Path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return fmt.Errorf("create asset parent for %q: %w", entry.Path, err)
			}
			if err := chmodParents(dest, filepath.Dir(name)); err != nil {
				return err
			}
			f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return fmt.Errorf("create asset %q: %w", entry.Path, err)
			}
			output = io.MultiWriter(hash, f)
		}
		n, copyErr := io.CopyN(output, tr, h.Size)
		if copyErr == nil && n != h.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if f != nil {
			if copyErr == nil {
				copyErr = f.Chmod(os.FileMode(entry.Mode))
			}
			closeErr := f.Close()
			if copyErr == nil {
				copyErr = closeErr
			}
		}
		if copyErr != nil {
			return fmt.Errorf("write asset %q: %w", h.Name, copyErr)
		}
		gotHash := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if n != entry.Bytes || gotHash != entry.SHA256 {
			return fmt.Errorf("asset hash or length mismatch: %q", h.Name)
		}
		seen[h.Name] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("asset archive has %d files, expected %d", len(seen), len(want))
	}
	var extra [1]byte
	if n, err := src.Read(extra[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("unexpected data after asset archive")
	}
	return nil
}

func ValidateDir(root string, entries []bundle.AssetArtifact) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0700 {
		return fmt.Errorf("asset directory is not a private regular directory")
	}

	wantFiles := make(map[string]bundle.AssetArtifact, len(entries))
	wantDirs := map[string]bool{".": true}
	for _, entry := range entries {
		if !SafePath(entry.Path) {
			return fmt.Errorf("unsafe asset path %q", entry.Path)
		}
		if _, exists := wantFiles[entry.Path]; exists {
			return fmt.Errorf("duplicate asset path %q", entry.Path)
		}
		wantFiles[entry.Path] = entry
		for parent := path.Dir(entry.Path); parent != "."; parent = path.Dir(parent) {
			wantDirs[parent] = true
		}
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(name string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("asset symlink is not allowed: %q", rel)
		}
		if d.IsDir() {
			if !wantDirs[rel] || info.Mode().Perm() != 0700 {
				return fmt.Errorf("unexpected or non-private asset directory %q", rel)
			}
			return nil
		}
		entry, ok := wantFiles[rel]
		if !ok {
			return fmt.Errorf("unexpected asset file %q", rel)
		}
		if !info.Mode().IsRegular() || linkCount(info) > 1 || info.Size() != entry.Bytes || uint32(info.Mode().Perm()) != entry.Mode {
			return fmt.Errorf("asset metadata changed: %q", rel)
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, io.LimitReader(f, bundle.MaxAssetFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("asset content changed: %q", rel)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(wantFiles) {
		return fmt.Errorf("asset directory has %d files, expected %d", len(seen), len(wantFiles))
	}
	return nil
}

func destinationPath(root, name string) (string, error) {
	if !SafePath(name) {
		return "", fmt.Errorf("unsafe asset path %q", name)
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("asset path escapes destination: %q", name)
	}
	return full, nil
}

func chmodParents(root, parent string) error {
	for parent != root {
		if err := os.Chmod(parent, 0700); err != nil {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return fmt.Errorf("asset parent escaped destination")
		}
		parent = next
	}
	return nil
}

// linkCount uses the platform stat value without tying this package to one
// syscall.Stat_t definition. Linux and Darwin both expose Nlink.
func linkCount(info os.FileInfo) uint64 {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 1
	}
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 1
	}
	field := value.FieldByName("Nlink")
	if !field.IsValid() {
		return 1
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if field.Int() > 0 {
			return uint64(field.Int())
		}
	}
	return 1
}
