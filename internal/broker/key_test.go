package broker

import (
	"strings"
	"testing"
	"time"
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
	key, err := GenerateObjectKey(
		time.Date(2026, 6, 28, 1, 2, 3, 0, time.UTC),
		strings.NewReader("abcdefghij"),
		"share-broker",
		"hello world.txt",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "share-broker/2026/06/28/") || !strings.HasSuffix(key, "/hello_world.txt") {
		t.Fatalf("unexpected key: %s", key)
	}
}
