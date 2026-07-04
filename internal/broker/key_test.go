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
	key := GenerateObjectKey(strings.Repeat("a", 64), ".txt")
	if key != "aa/"+strings.Repeat("a", 64)+".txt" {
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

func TestGenerateRandomAliasSlugUsesFilenameAndFinalExtension(t *testing.T) {
	slug, err := GenerateRandomAliasSlug("Clip MOV.mov", ".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(slug, "-clip_mov.mp4") {
		t.Fatalf("slug = %q", slug)
	}
	if len(strings.TrimSuffix(slug, "-clip_mov.mp4")) != 16 {
		t.Fatalf("slug = %q", slug)
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
