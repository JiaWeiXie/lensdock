package feedback

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// Image limits from SPEC §18 and schemas/feedback.schema.json.
const (
	defaultMaxImageBytes = 32 << 20
	defaultMaxPixels     = 24_000_000
	defaultMaxDimension  = 32768
)

// Standalone drawing canvas bounds per side, in pixels. They apply on top of
// the shared dimension, pixel and encoded-size limits (Limits).
const (
	minDrawingDimension = 64
	maxDrawingDimension = 4096
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// decodePNG decodes a PNG with resource limits checked before any pixel
// allocation, and returns it flattened onto opaque white so no hidden colour
// data survives under transparent pixels. 16-bit images are refused (they
// would multiply decode memory); metadata chunks are dropped by design.
func decodePNG(data []byte, l Limits) (*image.RGBA, error) {
	if int64(len(data)) > l.MaxImageBytes {
		return nil, ErrImageTooLarge
	}
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, ErrInvalidImage
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalidImage
	}
	if cfg.Width < 1 || cfg.Height < 1 {
		return nil, ErrInvalidImage
	}
	if cfg.Width > l.MaxDimension || cfg.Height > l.MaxDimension || int64(cfg.Width)*int64(cfg.Height) > int64(l.MaxPixels) {
		return nil, ErrImageTooLarge
	}
	switch cfg.ColorModel {
	case color.Gray16Model, color.RGBA64Model, color.NRGBA64Model:
		return nil, ErrInvalidImage
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrInvalidImage
	}
	b := src.Bounds()
	if b.Dx() != cfg.Width || b.Dy() != cfg.Height {
		return nil, ErrInvalidImage
	}
	if rgba, ok := src.(*image.RGBA); ok && rgba.Opaque() && b.Min == (image.Point{}) {
		return rgba, nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst, nil
}

// encodePNG re-encodes img as an opaque RGB PNG without metadata and refuses
// output larger than max bytes.
func encodePNG(img *image.RGBA, max int64) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, ErrImageTooLarge
	}
	return buf.Bytes(), nil
}

// pixelRects converts normalized redaction rectangles to pixel rectangles,
// rounding outward so a redaction never under-covers its region.
func pixelRects(w, h int, rects []normRect) []image.Rectangle {
	out := make([]image.Rectangle, 0, len(rects))
	for _, r := range rects {
		x0 := int(math.Floor(r.X*float64(w) - 1e-6))
		y0 := int(math.Floor(r.Y*float64(h) - 1e-6))
		x1 := int(math.Ceil((r.X+r.W)*float64(w) + 1e-6))
		y1 := int(math.Ceil((r.Y+r.H)*float64(h) + 1e-6))
		out = append(out, image.Rect(x0, y0, x1, y1).Intersect(image.Rect(0, 0, w, h)))
	}
	return out
}

// applyRedactions overwrites every rectangle with opaque black pixels. This
// is real pixel replacement: nothing remains of the original content.
func applyRedactions(img *image.RGBA, rects []normRect) []image.Rectangle {
	px := pixelRects(img.Bounds().Dx(), img.Bounds().Dy(), rects)
	for _, r := range px {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := img.Pix[y*img.Stride+r.Min.X*4 : y*img.Stride+r.Max.X*4]
			for i := 0; i < len(row); i += 4 {
				row[i], row[i+1], row[i+2], row[i+3] = 0, 0, 0, 255
			}
		}
	}
	return px
}

// verifyRedactions re-reads every redacted pixel; it guards the publish
// pipeline against a redaction that silently did not apply.
func verifyRedactions(img *image.RGBA, px []image.Rectangle) bool {
	for _, r := range px {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := img.Pix[y*img.Stride+r.Min.X*4 : y*img.Stride+r.Max.X*4]
			for i := 0; i < len(row); i += 4 {
				if row[i] != 0 || row[i+1] != 0 || row[i+2] != 0 || row[i+3] != 255 {
					return false
				}
			}
		}
	}
	return true
}

var errBadCrop = errors.New("feedback: crop rectangle outside image")

// cropRGBA returns a copy of the rectangle r of img.
func cropRGBA(img *image.RGBA, r image.Rectangle) (*image.RGBA, error) {
	if r.Empty() || !r.In(img.Bounds()) {
		return nil, errBadCrop
	}
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		so := (r.Min.Y+y)*img.Stride + r.Min.X*4
		copy(dst.Pix[y*dst.Stride:y*dst.Stride+r.Dx()*4], img.Pix[so:so+r.Dx()*4])
	}
	return dst, nil
}
