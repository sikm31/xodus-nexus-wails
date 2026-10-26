// Package imageutil scales untrusted images the way Electron's nativeImage
// resize did for shortcut icons, Steam artwork and avatars.
package imageutil

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"strings"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxPixels rejects decompression bombs before decoding.
const MaxPixels = 16_777_216

// Fit decodes an image and returns a PNG no larger than limit on its longest
// side. It returns nil for unreadable, empty or oversized input.
func Fit(data []byte, limit int) []byte {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width*config.Height > MaxPixels {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	width, height := config.Width, config.Height
	if longest := max(width, height); longest > limit {
		width = max(1, (width*limit+longest/2)/longest)
		height = max(1, (height*limit+longest/2)/longest)
	}
	var out image.Image = src
	if width != config.Width || height != config.Height {
		dst := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		out = dst
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, out); err != nil {
		return nil
	}
	return buffer.Bytes()
}

// FitDataURL scales a base64 image data URL and returns a PNG data URL, or "".
func FitDataURL(value string, limit int) string {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	fitted := Fit(data, limit)
	if fitted == nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(fitted)
}
