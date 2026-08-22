package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 1×1 PNG; decoded length is well above minDecodedFileBytes.
const testPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func testPNG() (raw []byte, b64, sha string) {
	raw, err := base64.StdEncoding.DecodeString(testPNGB64)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return raw, testPNGB64, hex.EncodeToString(sum[:])
}

func TestExtractFilesOpenAIDataURL(t *testing.T) {
	raw, b64, sha := testPNG()
	in := []byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	require.Equal(t, "image/png", files[0].MimeType)
	require.Equal(t, raw, files[0].Data)
	require.Contains(t, string(out), filePlaceholder(sha))
	require.NotContains(t, string(out), b64)
}

func TestExtractFilesAnthropicSource(t *testing.T) {
	_, b64, sha := testPNG()
	in := []byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}`)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	require.Equal(t, "image/png", files[0].MimeType)
	require.Contains(t, string(out), `"data":"`+filePlaceholder(sha)+`"`)
	require.NotContains(t, string(out), b64)
}

func TestExtractFilesInputAudio(t *testing.T) {
	raw, b64, sha := testPNG()
	in := []byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + b64 + `","format":"wav"}}]}]}`)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	require.Equal(t, "audio/wav", files[0].MimeType)
	require.Equal(t, raw, files[0].Data)
	require.Contains(t, string(out), filePlaceholder(sha))
	require.NotContains(t, string(out), b64)
}

func TestExtractFilesB64JSON(t *testing.T) {
	raw, b64, sha := testPNG()
	in := []byte(`{"created":1,"data":[{"b64_json":"` + b64 + `"}]}`)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	require.Equal(t, raw, files[0].Data)
	require.True(t, strings.HasPrefix(files[0].MimeType, "image/"))
	require.Contains(t, string(out), `"b64_json":"`+filePlaceholder(sha)+`"`)
	require.NotContains(t, string(out), b64)
}

func TestExtractFilesSSEJSONString(t *testing.T) {
	_, b64, sha := testPNG()
	sse := "data: {\"b64_json\":\"" + b64 + "\"}\n\n"
	in, err := json.Marshal(sse)
	require.NoError(t, err)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	var text string
	require.NoError(t, json.Unmarshal(out, &text))
	require.Contains(t, text, filePlaceholder(sha))
	require.NotContains(t, text, b64)
}

func TestExtractFilesHTTPURLUnchanged(t *testing.T) {
	in := []byte(`{"model":"fast","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`)
	out, files := extractFiles(in)
	require.Empty(t, files)
	require.Equal(t, in, out)
}

func TestExtractFilesShortAndInvalid(t *testing.T) {
	short := []byte(`{"url":"data:image/png;base64,QQ=="}`)
	out, files := extractFiles(short)
	require.Empty(t, files)
	require.Equal(t, short, out)

	invalid := []byte(`{"source":{"type":"base64","media_type":"image/png","data":"!!!!not-base64!!!!not-base64!!!!not-base64!!!!"}}`)
	out, files = extractFiles(invalid)
	require.Empty(t, files)
	require.Equal(t, invalid, out)
}

func TestExtractFilesDedup(t *testing.T) {
	_, b64, sha := testPNG()
	in := []byte(`{"a":"data:image/png;base64,` + b64 + `","b":"data:image/png;base64,` + b64 + `"}`)
	out, files := extractFiles(in)
	require.Len(t, files, 1)
	require.Equal(t, sha, files[0].SHA256)
	require.Equal(t, 2, strings.Count(string(out), filePlaceholder(sha)))
}

func TestExtractFilesNoHitSameBytes(t *testing.T) {
	in := []byte(`{"model":"fast","n":1}`)
	out, files := extractFiles(in)
	require.Empty(t, files)
	require.Equal(t, in, out)
}

func TestMergeExtracted(t *testing.T) {
	_, _, sha := testPNG()
	a := []extractedFile{{SHA256: sha, MimeType: "image/png"}}
	b := []extractedFile{{SHA256: sha, MimeType: "image/png"}, {SHA256: strings.Repeat("b", 64), MimeType: "image/jpeg"}}
	got := mergeExtracted(a, b)
	require.Len(t, got, 2)
	require.Equal(t, sha, got[0].SHA256)
	require.Equal(t, strings.Repeat("b", 64), got[1].SHA256)
}

func TestValidFileSHA256(t *testing.T) {
	_, _, sha := testPNG()
	require.True(t, validFileSHA256(sha))
	require.False(t, validFileSHA256(""))
	require.False(t, validFileSHA256(strings.Repeat("G", 64)))
	require.False(t, validFileSHA256(sha[:63]))
}

func TestFileKind(t *testing.T) {
	require.Equal(t, "image", fileKind("image/png"))
	require.Equal(t, "video", fileKind("video/mp4"))
	require.Equal(t, "audio", fileKind("audio/wav"))
	require.Equal(t, "file", fileKind("application/pdf"))
}

func TestPruneFileScope(t *testing.T) {
	before, all := pruneFileScope(0, 99)
	require.True(t, all)
	require.Equal(t, uint64(0), before)
	before, all = pruneFileScope(1000, 42)
	require.False(t, all)
	require.Equal(t, uint64(42), before)
}

func TestFilePlaceholder(t *testing.T) {
	require.Equal(t, "<file:abc>", filePlaceholder("abc"))
}
