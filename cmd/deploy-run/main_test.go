package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openai/deploybundle/pkg/bundle"
)

func TestCheckHostCapabilitiesAcceptsExistingResources(t *testing.T) {
	root := t.TempDir()
	hostPath := filepath.Join(root, "data")
	if err := os.Mkdir(hostPath, 0700); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(root, "device")
	if err := os.WriteFile(devicePath, []byte("placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &bundle.Manifest{Capabilities: bundle.Capabilities{
		Devices:   []bundle.DeviceCapability{{Service: "camera", Host: devicePath, Container: "/dev/camera", Permissions: "r"}},
		HostPaths: []bundle.HostPathCapability{{Service: "web", Source: hostPath, Target: "/data", Mode: "ro"}},
	}}
	if err := checkHostCapabilities(m); err != nil {
		t.Fatalf("existing resources rejected: %v", err)
	}
}

func TestCheckHostCapabilitiesReportsMissingResource(t *testing.T) {
	m := &bundle.Manifest{Capabilities: bundle.Capabilities{
		Devices: []bundle.DeviceCapability{{Service: "camera", Host: "/definitely/missing/device", Container: "/dev/camera", Permissions: "r"}},
	}}
	err := checkHostCapabilities(m)
	if err == nil {
		t.Fatal("missing device accepted")
	}
	if !strings.Contains(err.Error(), "camera") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing device error = %v", err)
	}

	m = &bundle.Manifest{Capabilities: bundle.Capabilities{
		HostPaths: []bundle.HostPathCapability{{Service: "web", Source: "/definitely/missing/path", Target: "/data", Mode: "rw"}},
	}}
	err = checkHostCapabilities(m)
	if err == nil {
		t.Fatal("missing host path accepted")
	}
	if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing host path error = %v", err)
	}
}

func TestArtifactPathRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../compose.enc", `..\\compose.enc`, "nested/compose.enc"} {
		if _, err := artifactPath(t.TempDir(), name); err == nil {
			t.Fatalf("artifact path %q accepted", name)
		}
	}
	if path, err := artifactPath("/tmp/bundle", "compose.enc"); err != nil || path != "/tmp/bundle/compose.enc" {
		t.Fatalf("valid artifact path = %q, %v", path, err)
	}
}

func TestSanitizeProjectKeepsBundleIDsDistinct(t *testing.T) {
	left := sanitizeProject("foo/bar")
	right := sanitizeProject("foo-bar")
	if left == right {
		t.Fatalf("sanitized project names collide: %q", left)
	}
	if !strings.HasPrefix(left, "foo-bar-") || !strings.HasPrefix(right, "foo-bar-") {
		t.Fatalf("unexpected project names: %q, %q", left, right)
	}
}
