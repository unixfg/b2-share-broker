package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode"
)

var repeatedDash = regexp.MustCompile(`[-_]{2,}`)

func NormalizeSHA256(value string) (string, []byte, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", nil, fmt.Errorf("sha256 must be %d lowercase hex characters", sha256.Size*2)
	}
	sum, err := hex.DecodeString(value)
	if err != nil {
		return "", nil, fmt.Errorf("sha256 must be hex: %w", err)
	}
	return value, sum, nil
}

func GenerateObjectKey(sha256Hex, extension string) string {
	extension = normalizeExtension(extension)
	sha256Hex = strings.ToLower(strings.TrimSpace(sha256Hex))
	if len(sha256Hex) < 2 {
		return sha256Hex + extension
	}
	return sha256Hex[:2] + "/" + sha256Hex + extension
}

func GenerateRandomAliasSlug(filename, extension string) (string, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	suffix := hex.EncodeToString(data[:])
	extension = normalizeExtension(extension)
	name := strings.ToLower(SanitizeFilename(filename))
	nameExt := path.Ext(name)
	base := strings.TrimSuffix(name, nameExt)
	base = strings.Trim(base, ".-_")
	if base == "" {
		base = "share"
	}
	if len(base) > 120 {
		base = strings.Trim(base[:120], ".-_")
	}
	if base == "" {
		base = "share"
	}
	if extension == "" {
		extension = normalizeExtension(nameExt)
	}
	return base + "-" + suffix + extension, nil
}

func ExtensionFor(filename, contentType string) string {
	name := SanitizeFilename(filename)
	if ext := normalizeExtension(path.Ext(name)); ext != "" {
		return ext
	}
	contentType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	if contentType != "" {
		if extensions, err := mime.ExtensionsByType(contentType); err == nil && len(extensions) > 0 {
			if ext := normalizeExtension(extensions[0]); ext != "" {
				return ext
			}
		}
	}
	return ".bin"
}

func normalizeExtension(extension string) string {
	extension = strings.ToLower(strings.TrimSpace(extension))
	if extension == "" {
		return ""
	}
	if !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	var builder strings.Builder
	builder.WriteByte('.')
	for _, r := range strings.TrimPrefix(extension, ".") {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			builder.WriteRune(r)
		}
	}
	cleaned := builder.String()
	if cleaned == "." {
		return ""
	}
	if len(cleaned) > 17 {
		return cleaned[:17]
	}
	return cleaned
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
