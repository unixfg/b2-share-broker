package broker

import (
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := map[string]string{
		"Screenshot 1.png":           "Screenshot_1.png",
		"../../secret.txt":           "secret.txt",
		" spaces and / slashes .jpg": "slashes_.jpg",
		"☃":                          "upload",
		"":                           "upload",
	}
	for input, want := range tests {
		if got := SanitizeFilename(input); got != want {
			t.Fatalf("SanitizeFilename(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGenerateObjectKey(t *testing.T) {
	key := GenerateObjectKey("s", strings.Repeat("a", 64), ".txt")
	if key != "s/"+strings.Repeat("a", 64)+".txt" {
		t.Fatalf("unexpected key: %s", key)
	}
}

func TestNormalizeSHA256(t *testing.T) {
	normalized, raw, err := NormalizeSHA256(strings.Repeat("A", 64))
	if err != nil {
		t.Fatal(err)
	}
	if normalized != strings.Repeat("a", 64) || len(raw) != 32 {
		t.Fatalf("normalized = %q, raw len = %d", normalized, len(raw))
	}
	if _, _, err := NormalizeSHA256("nope"); err == nil {
		t.Fatal("expected invalid sha256 to fail")
	}
}

func TestGenerateAliasSlug(t *testing.T) {
	_, raw, err := NormalizeSHA256(strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	slug := GenerateAliasSlug([]byte("alias-key-012345678901234567890123"), raw, ".PNG")
	if !strings.HasSuffix(slug, ".png") || len(strings.TrimSuffix(slug, ".png")) != 26 {
		t.Fatalf("slug = %q", slug)
	}
}

func TestNormalizeManualAlias(t *testing.T) {
	tests := map[string]string{
		"Latest Screenshot": "latest-screenshot.png",
		"/s/Report.PDF":     "report.pdf",
		"../bad name":       "bad-name.txt",
	}
	for input, want := range tests {
		ext := ".png"
		if strings.Contains(input, "bad") {
			ext = ".txt"
		}
		if got := NormalizeManualAlias(input, ext); got != want {
			t.Fatalf("NormalizeManualAlias(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExtensionFor(t *testing.T) {
	if got := ExtensionFor("photo.HEIC", "application/octet-stream"); got != ".heic" {
		t.Fatalf("extension = %q", got)
	}
	if got := ExtensionFor("upload", "image/jpeg"); got != ".jpg" {
		t.Fatalf("extension = %q", got)
	}
	if got := ExtensionFor("upload", ""); got != ".bin" {
		t.Fatalf("extension = %q", got)
	}
}
