package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"  // register decoders for dimension sniffing
	_ "image/jpeg" //
	_ "image/png"  //
	"os"

	"cubcoder/internal/provider"
)

// maxImageBytes is the largest image passed to the model inline. It matches
// the Anthropic per-image limit; local vision models are bound by the same
// order of magnitude through the gateway's request size.
const maxImageBytes = 5 << 20

// imageMediaType sniffs the image format from the file's magic bytes. It
// returns "" for anything that is not a supported raster image. Content is
// checked rather than the extension so a screenshot saved as "shot" or a
// mislabeled .jpg still works.
func imageMediaType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// isImage reports whether data is a supported image.
func isImage(data []byte) bool { return imageMediaType(data) != "" }

// LoadImage turns an image file's bytes into a provider.Image plus a short
// human/model-readable description (format, dimensions, size). name is used
// in the description and in errors.
func LoadImage(name string, data []byte) (provider.Image, string, error) {
	mt := imageMediaType(data)
	if mt == "" {
		return provider.Image{}, "", fmt.Errorf("%s is not a supported image (png, jpeg, gif, webp)", name)
	}
	if len(data) > maxImageBytes {
		return provider.Image{}, "", fmt.Errorf("%s is %.1f MiB — images are limited to %d MiB; resize or re-encode it first", name, float64(len(data))/(1<<20), maxImageBytes>>20)
	}
	desc := fmt.Sprintf("%s, %s", mt, humanBytes(len(data)))
	// Dimensions come from the header only (DecodeConfig); webp has no stdlib
	// decoder, so it reports without dimensions.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		desc = fmt.Sprintf("%s, %dx%d, %s", mt, cfg.Width, cfg.Height, humanBytes(len(data)))
	}
	return provider.Image{MediaType: mt, Data: base64.StdEncoding.EncodeToString(data)}, desc, nil
}

// LoadImageFile is LoadImage over a path.
func LoadImageFile(path string) (provider.Image, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return provider.Image{}, "", err
	}
	return LoadImage(path, data)
}

// IsImageFile reports whether the file at path is a supported image, by
// content. Errors read as "not an image".
func IsImageFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := f.Read(head)
	return isImage(head[:n])
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
