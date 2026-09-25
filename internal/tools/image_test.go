package tools

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

func TestImageMediaType(t *testing.T) {
	cases := map[string]string{
		"\x89PNG\r\n\x1a\nxxxx":        "image/png",
		"\xff\xd8\xff\xe0xxxx":         "image/jpeg",
		"GIF89axxxx":                   "image/gif",
		"RIFF\x00\x00\x00\x00WEBPVP8 ": "image/webp",
		"%PDF-1.4":                     "",
		"package main":                 "",
		"RIFF\x00\x00\x00\x00WAVEfmt ": "",
	}
	for in, want := range cases {
		if got := imageMediaType([]byte(in)); got != want {
			t.Errorf("imageMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadImageAndIsImageFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot") // no extension: detection is by content
	if err := os.WriteFile(p, tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsImageFile(p) {
		t.Error("IsImageFile false for a PNG")
	}
	img, desc, err := LoadImageFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if img.MediaType != "image/png" || img.Data != base64.StdEncoding.EncodeToString(tinyPNG) {
		t.Errorf("image = %+v", img)
	}
	if desc != "image/png, 1x1, 70 B" {
		t.Errorf("desc = %q", desc)
	}
	txt := filepath.Join(dir, "a.txt")
	os.WriteFile(txt, []byte("hello"), 0o644)
	if IsImageFile(txt) {
		t.Error("IsImageFile true for text")
	}
	if _, _, err := LoadImageFile(txt); err == nil {
		t.Error("LoadImage should reject non-images")
	}
	big := append([]byte{}, tinyPNG...)
	big = append(big, make([]byte, maxImageBytes)...)
	if _, _, err := LoadImage("big.png", big); err == nil {
		t.Error("oversized image should be rejected")
	}
}

func TestExtractDocumentRejectsImageWithHint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.png")
	os.WriteFile(p, tinyPNG, 0o644)
	_, err := ExtractDocument(p)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("err = %v", err)
	}
}
