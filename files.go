package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

const (
	filePlaceholderPrefix = "<file:"
	filePlaceholderSuffix = ">"
	minDecodedFileBytes   = 32
)

type extractedFile struct {
	SHA256   string
	MimeType string
	Data     []byte
}

var (
	reDataURL = regexp.MustCompile(
		`data:(?:image|video|audio)/[A-Za-z0-9.+-]+(?:;[A-Za-z0-9.=+-]+)*;base64,[A-Za-z0-9+/=]+` +
			`|data:application/pdf(?:;[A-Za-z0-9.=+-]+)*;base64,[A-Za-z0-9+/=]+`,
	)
	reB64JSONField = regexp.MustCompile(`("b64_json"\s*:\s*")([A-Za-z0-9+/=]+)(")`)
)

var audioFormats = map[string]string{
	"wav":   "audio/wav",
	"mp3":   "audio/mpeg",
	"mpeg":  "audio/mpeg",
	"pcm16": "audio/pcm",
	"ogg":   "audio/ogg",
	"flac":  "audio/flac",
	"webm":  "audio/webm",
}

func filePlaceholder(sha string) string {
	return filePlaceholderPrefix + sha + filePlaceholderSuffix
}

func validFileSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func fileKind(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case mime == "image/svg+xml":
		// SVG can carry script; never render it inline.
		return "file"
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	default:
		return "file"
	}
}

// inlineFileMime reports whether a stored file may be served inline to the
// admin browser. Stored MIME types come from client-controlled request
// fields, so anything scriptable (text/html, image/svg+xml, ...) is forced
// to download as octet-stream instead.
func inlineFileMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(mime))
	if strings.HasPrefix(mime, "audio/") {
		return true
	}
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif",
		"video/mp4", "video/webm", "video/ogg",
		"application/pdf":
		return true
	}
	return false
}

func extractFiles(body []byte) ([]byte, []extractedFile) {
	if len(body) == 0 {
		return body, nil
	}
	var files []extractedFile
	seen := make(map[string]struct{})
	out, changed := rewriteJSON(body, &files, seen)
	if !changed {
		return body, nil
	}
	return out, files
}

func mergeExtracted(a, b []extractedFile) []extractedFile {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]extractedFile, 0, len(a)+len(b))
	for _, src := range [][]extractedFile{a, b} {
		for _, f := range src {
			if _, ok := seen[f.SHA256]; ok {
				continue
			}
			seen[f.SHA256] = struct{}{}
			out = append(out, f)
		}
	}
	return out
}

func addExtracted(files *[]extractedFile, seen map[string]struct{}, f extractedFile) {
	if _, ok := seen[f.SHA256]; ok {
		return
	}
	seen[f.SHA256] = struct{}{}
	*files = append(*files, f)
}

func rewriteJSON(raw []byte, files *[]extractedFile, seen map[string]struct{}) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false
	}
	switch trimmed[0] {
	case '"':
		return rewriteJSONString(raw, trimmed, files, seen)
	case '[':
		return rewriteJSONArray(raw, trimmed, files, seen)
	case '{':
		return rewriteJSONObject(raw, trimmed, files, seen)
	default:
		return raw, false
	}
}

func rewriteJSONString(raw, trimmed []byte, files *[]extractedFile, seen map[string]struct{}) ([]byte, bool) {
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return raw, false
	}
	ns, changed := rewriteText(s, files, seen)
	if !changed {
		return raw, false
	}
	out, err := encodeJSONValue(ns)
	if err != nil {
		return raw, false
	}
	return out, true
}

func rewriteJSONArray(raw, trimmed []byte, files *[]extractedFile, seen map[string]struct{}) ([]byte, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return raw, false
	}
	changed := false
	for i, v := range arr {
		nv, c := rewriteJSON(v, files, seen)
		if c {
			arr[i] = nv
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := encodeJSONValue(arr)
	if err != nil {
		return raw, false
	}
	return out, true
}

func rewriteJSONObject(raw, trimmed []byte, files *[]extractedFile, seen map[string]struct{}) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return raw, false
	}
	changed := false
	skip := make(map[string]bool)

	if typ, _ := rawJSONString(obj["type"]); typ == "base64" {
		if data, err := rawJSONString(obj["data"]); err == nil && data != "" {
			mime, _ := rawJSONString(obj["media_type"])
			if f, ok := fileFromBase64(data, mime); ok {
				p, err := encodeJSONValue(filePlaceholder(f.SHA256))
				if err == nil {
					addExtracted(files, seen, f)
					obj["data"] = p
					skip["data"] = true
					changed = true
				}
			}
		}
	}

	if b64, err := rawJSONString(obj["b64_json"]); err == nil && b64 != "" {
		if f, ok := fileFromBase64(b64, ""); ok {
			p, err := encodeJSONValue(filePlaceholder(f.SHA256))
			if err == nil {
				addExtracted(files, seen, f)
				obj["b64_json"] = p
				skip["b64_json"] = true
				changed = true
			}
		}
	}

	if format, _ := rawJSONString(obj["format"]); audioFormats[strings.ToLower(format)] != "" {
		if data, err := rawJSONString(obj["data"]); err == nil && data != "" && !strings.HasPrefix(data, "data:") {
			if f, ok := fileFromBase64(data, audioFormats[strings.ToLower(format)]); ok {
				p, err := encodeJSONValue(filePlaceholder(f.SHA256))
				if err == nil {
					addExtracted(files, seen, f)
					obj["data"] = p
					skip["data"] = true
					changed = true
				}
			}
		}
	}

	for k, v := range obj {
		if skip[k] {
			continue
		}
		nv, c := rewriteJSON(v, files, seen)
		if c {
			obj[k] = nv
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := encodeJSONValue(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}

func rewriteText(s string, files *[]extractedFile, seen map[string]struct{}) (string, bool) {
	if !strings.Contains(s, "data:") && !strings.Contains(s, "b64_json") {
		return s, false
	}
	changed := false
	s = reDataURL.ReplaceAllStringFunc(s, func(m string) string {
		mime, payload, ok := parseDataURL(m)
		if !ok {
			return m
		}
		f, ok := fileFromBase64(payload, mime)
		if !ok {
			return m
		}
		addExtracted(files, seen, f)
		changed = true
		return filePlaceholder(f.SHA256)
	})
	s = reB64JSONField.ReplaceAllStringFunc(s, func(m string) string {
		sub := reB64JSONField.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		f, ok := fileFromBase64(sub[2], "")
		if !ok {
			return m
		}
		addExtracted(files, seen, f)
		changed = true
		return sub[1] + filePlaceholder(f.SHA256) + sub[3]
	})
	return s, changed
}

func parseDataURL(s string) (mime, payload string, ok bool) {
	const marker = ";base64,"
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := s[len("data:"):]
	i := strings.Index(rest, marker)
	if i < 0 {
		return "", "", false
	}
	meta := rest[:i]
	payload = rest[i+len(marker):]
	mime = meta
	if j := strings.IndexByte(meta, ';'); j >= 0 {
		mime = meta[:j]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if !allowedDataURLMime(mime) {
		return "", "", false
	}
	return mime, payload, true
}

func allowedDataURLMime(mime string) bool {
	typ, _, found := strings.Cut(mime, "/")
	if !found || typ == "" {
		return false
	}
	switch typ {
	case "image", "video", "audio":
		return true
	}
	return mime == "application/pdf"
}

func fileFromBase64(payload, mime string) (extractedFile, bool) {
	data, ok := decodeBase64Payload(payload)
	if !ok {
		return extractedFile{}, false
	}
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	sum := sha256.Sum256(data)
	return extractedFile{
		SHA256:   hex.EncodeToString(sum[:]),
		MimeType: mime,
		Data:     data,
	}, true
}

func decodeBase64Payload(s string) ([]byte, bool) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		default:
			return r
		}
	}, s)
	if s == "" {
		return nil, false
	}
	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	data, err := enc.DecodeString(s)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil, false
		}
	}
	if len(data) < minDecodedFileBytes || len(data) > maxMediumBlob {
		return nil, false
	}
	return data, true
}
