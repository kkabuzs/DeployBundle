package binding

import "testing"

func TestNormalizeMAC(t *testing.T) {
	for _, value := range []string{"AA:bb:cc:dd:ee:ff", "aa-bb-cc-dd-ee-ff", "aabb.ccdd.eeff"} {
		got, err := NormalizeMAC(value)
		if err != nil || got != "aabbccddeeff" {
			t.Fatalf("NormalizeMAC(%q) = %q, %v", value, got, err)
		}
	}
	if _, err := NormalizeMAC("not-a-mac"); err == nil {
		t.Fatal("invalid MAC accepted")
	}
}

func TestMakeBinding(t *testing.T) {
	b, err := MakeBinding([]string{"AA:bb:cc:dd:ee:ff"}, []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != "mac-sha256" || len(b.Values) != 1 || b.Salt == "" {
		t.Fatalf("unexpected binding: %+v", b)
	}
}
