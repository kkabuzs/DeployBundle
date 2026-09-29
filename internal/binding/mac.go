package binding

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/openai/deploybundle/pkg/bundle"
)

func NormalizeMAC(value string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r == ':' || r == '-' || r == '.' || r == ' ' {
			continue
		}
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
			continue
		}
		return "", fmt.Errorf("invalid MAC address %q", value)
	}
	if b.Len() != 12 {
		return "", fmt.Errorf("MAC address must contain 12 hex digits: %q", value)
	}
	return b.String(), nil
}

func HashMAC(normalized, salt []byte) string {
	h := sha256.New()
	h.Write(salt)
	h.Write(normalized)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func MakeBinding(macs []string, salt []byte) (bundle.Binding, error) {
	if len(macs) == 0 {
		return bundle.Binding{Mode: "none", Values: []string{}}, nil
	}
	values := make([]string, 0, len(macs))
	seen := map[string]bool{}
	for _, raw := range macs {
		mac, err := NormalizeMAC(raw)
		if err != nil {
			return bundle.Binding{}, err
		}
		if !seen[mac] {
			seen[mac] = true
			values = append(values, HashMAC([]byte(mac), salt))
		}
	}
	sort.Strings(values)
	return bundle.Binding{Mode: "mac-sha256", Salt: base64.StdEncoding.EncodeToString(salt), Values: values}, nil
}

func Verify(m bundle.Binding) error {
	if m.Mode == "none" {
		return nil
	}
	if m.Mode != "mac-sha256" {
		return fmt.Errorf("unsupported binding mode %q", m.Mode)
	}
	salt, err := base64.StdEncoding.DecodeString(m.Salt)
	if err != nil || len(salt) == 0 {
		return errors.New("invalid MAC binding salt")
	}
	if len(m.Values) == 0 {
		return errors.New("MAC binding has no values")
	}
	local, err := LocalMACs()
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(m.Values))
	for _, value := range m.Values {
		allowed[value] = true
	}
	for _, mac := range local {
		if allowed[HashMAC([]byte(mac), salt)] {
			return nil
		}
	}
	return errors.New("no local MAC address matches bundle binding")
}

func LocalMACs() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) == 0 {
			continue
		}
		mac, err := NormalizeMAC(iface.HardwareAddr.String())
		if err == nil {
			values = append(values, mac)
		}
	}
	sort.Strings(values)
	return values, nil
}
