package broker

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"
)

var repeatedDash = regexp.MustCompile(`[-_]{2,}`)

func GenerateObjectKey(now time.Time, entropy io.Reader, prefix, filename string) (string, error) {
	var entropyBytes [10]byte
	if _, err := io.ReadFull(entropy, entropyBytes[:]); err != nil {
		return "", err
	}
	id, err := ulid.New(ulid.Timestamp(now), bytes.NewReader(entropyBytes[:]))
	if err != nil {
		return "", err
	}
	name := SanitizeFilename(filename)
	return fmt.Sprintf("%s/%04d/%02d/%02d/%s/%s", strings.Trim(prefix, "/"), now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), id.String(), name), nil
}

func SanitizeFilename(filename string) string {
	name := strings.TrimSpace(path.Base(strings.ReplaceAll(filename, "\\", "/")))
	if name == "." || name == "/" || name == "" {
		return "upload"
	}

	var builder strings.Builder
	lastWasSeparator := false
	for _, r := range name {
		switch {
		case r == '.' || r == '-':
			builder.WriteRune(r)
			lastWasSeparator = false
		case r == '_' || unicode.IsSpace(r):
			if !lastWasSeparator {
				builder.WriteByte('_')
				lastWasSeparator = true
			}
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			builder.WriteRune(r)
			lastWasSeparator = false
		}
	}

	cleaned := strings.Trim(builder.String(), "._-")
	cleaned = repeatedDash.ReplaceAllString(cleaned, "_")
	if cleaned == "" {
		cleaned = "upload"
	}
	if len(cleaned) > 160 {
		ext := path.Ext(cleaned)
		base := strings.TrimSuffix(cleaned, ext)
		if len(ext) > 32 {
			ext = ""
		}
		limit := 160 - len(ext)
		if limit < 1 {
			limit = 160
			ext = ""
		}
		cleaned = strings.Trim(base[:limit], "._-") + ext
		if cleaned == "" {
			cleaned = "upload"
		}
	}
	return cleaned
}
