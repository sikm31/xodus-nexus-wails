package steam

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"

	// Decoders for game covers.
	_ "image/jpeg"

	_ "golang.org/x/image/webp"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"xodus-nexus-wails/assets"
)

// Local branding only: no network, account data or page-supplied content.
// These replace the original offscreen HTML rendering with direct drawing.

var (
	textColor   = color.RGBA{0xf4, 0xf6, 0xf5, 0xff}
	mutedColor  = color.RGBA{0xae, 0xbc, 0xb6, 0xff}
	accentColor = color.RGBA{0x67, 0xb4, 0x3c, 0xff}
)

// background draws radial-gradient(ellipse at 50% 30%, #173c32, #141c24 65%).
func background(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	inner, outer := [3]float64{0x17, 0x3c, 0x32}, [3]float64{0x14, 0x1c, 0x24}
	cx, cy := float64(width)/2, float64(height)*0.3
	rx, ry := math.Max(cx, float64(width)-cx), math.Max(cy, float64(height)-cy)
	for y := range height {
		for x := range width {
			dx, dy := (float64(x)-cx)/rx, (float64(y)-cy)/ry
			t := math.Min(1, math.Sqrt(dx*dx+dy*dy)/0.65)
			img.SetRGBA(x, y, color.RGBA{uint8(inner[0] + (outer[0]-inner[0])*t), uint8(inner[1] + (outer[1]-inner[1])*t), uint8(inner[2] + (outer[2]-inner[2])*t), 0xff})
		}
	}
	return img
}

func face(size float64) (font.Face, error) {
	data, err := assets.Fonts.ReadFile("fonts/DejaVuSans.ttf")
	if err != nil {
		return nil, err
	}
	parsed, err := opentype.Parse(data)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(parsed, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

func drawCentered(img *image.RGBA, text string, size float64, baseline int, tint color.Color, tracking float64) error {
	f, err := face(size)
	if err != nil {
		return err
	}
	defer f.Close()
	drawer := &font.Drawer{Dst: img, Src: image.NewUniform(tint), Face: f}
	width := drawer.MeasureString(text) + fixed.Int26_6(tracking*64*float64(len([]rune(text))-1))
	x := fixed.I(img.Bounds().Dx()/2) - width/2
	for _, r := range text {
		drawer.Dot = fixed.Point26_6{X: x, Y: fixed.I(baseline)}
		drawer.DrawString(string(r))
		advance, _ := f.GlyphAdvance(r)
		x += advance + fixed.Int26_6(tracking*64)
	}
	return nil
}

// contain scales src to fit within rect, preserving its proportions.
func contain(dst *image.RGBA, src image.Image, rect image.Rectangle) {
	bounds := src.Bounds()
	scale := math.Min(float64(rect.Dx())/float64(bounds.Dx()), float64(rect.Dy())/float64(bounds.Dy()))
	w, h := int(float64(bounds.Dx())*scale), int(float64(bounds.Dy())*scale)
	x, y := rect.Min.X+(rect.Dx()-w)/2, rect.Min.Y+(rect.Dy()-h)/2
	draw.CatmullRom.Scale(dst, image.Rect(x, y, x+w, y+h), src, bounds, draw.Over, nil)
}

func encode(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func nexusCard(icon image.Image, width, height int) ([]byte, error) {
	img := background(width, height)
	iconSize, title := 280, 52.0
	if width > height {
		iconSize, title = 220, 64
	}
	total := iconSize + 32 + int(title) + 32 + 4 + 24 + 21
	top := (height - total) / 2
	contain(img, icon, image.Rect((width-iconSize)/2, top, (width+iconSize)/2, top+iconSize))
	baseline := top + iconSize + 32 + int(title*0.8)
	if err := drawCentered(img, "XODUS NEXUS", title, baseline, textColor, 3); err != nil {
		return nil, err
	}
	line := baseline + 32
	draw.Draw(img, image.Rect(width/2-42, line, width/2+42, line+4), image.NewUniform(accentColor), image.Point{}, draw.Src)
	if err := drawCentered(img, "YOUR PC GAMES, TOGETHER.", 21, line+4+24+17, mutedColor, 2); err != nil {
		return nil, err
	}
	return encode(img)
}

// NexusArtwork renders the launcher's own portrait and wide Steam artwork.
func NexusArtwork() (Artwork, error) {
	icon, _, err := image.Decode(bytes.NewReader(assets.Icon))
	if err != nil {
		return Artwork{}, err
	}
	cover, err := nexusCard(icon, 600, 900)
	if err != nil {
		return Artwork{}, err
	}
	wide, err := nexusCard(icon, 1280, 720)
	if err != nil {
		return Artwork{}, err
	}
	return Artwork{Cover: cover, Wide: wide}, nil
}

// blurred approximates a CSS blur by scaling down and back up.
func blurred(src image.Image, width, height int) *image.RGBA {
	small := image.NewRGBA(image.Rect(0, 0, max(1, width/24), max(1, height/24)))
	draw.BiLinear.Scale(small, small.Bounds(), src, src.Bounds(), draw.Src, nil)
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.BiLinear.Scale(out, out.Bounds(), small, small.Bounds(), draw.Src, nil)
	return out
}

// GamePortrait fits a game cover into Steam's portrait slot over a blurred,
// dimmed copy of itself, keeping the original image's proportions.
func GamePortrait(cover []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(cover))
	if err != nil {
		return nil, err
	}
	const width, height = 600, 900
	img := background(width, height)
	// background-size: cover, then 30% opacity.
	bounds := src.Bounds()
	scale := math.Max(float64(width+80)/float64(bounds.Dx()), float64(height+80)/float64(bounds.Dy()))
	coverW, coverH := int(float64(bounds.Dx())*scale), int(float64(bounds.Dy())*scale)
	backdrop := blurred(src, coverW, coverH)
	offset := image.Pt((width-coverW)/2, (height-coverH)/2)
	draw.DrawMask(img, image.Rect(offset.X, offset.Y, offset.X+coverW, offset.Y+coverH), backdrop, image.Point{},
		image.NewUniform(color.Alpha{0x4d}), image.Point{}, draw.Over)
	contain(img, src, img.Bounds())
	return encode(img)
}

// Normalize converts a PNG, JPEG or WebP image into PNG, or nil when undecodable.
func Normalize(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	out, err := encode(img)
	if err != nil {
		return nil
	}
	return out
}
