package feedback

import (
	"encoding/json"
	"errors"
	"image"
	"math"
)

// maxEffectPixels bounds the summed area of all blur and pixelate rectangles
// one render processes. It is about four full-size images at the 24 MP
// capture limit; real feedback blurs a few small regions.
const maxEffectPixels = 100_000_000

var errEffectBudget = badRequest("blur and pixelate annotations cover too many pixels")

// effectPixelRect converts a normalized effect rectangle to source pixels.
// The renderers of the web editor use the same rule, so SVG, PNG and the
// shared image agree: floor of the origin, ceil of the far edge, clamped.
func effectPixelRect(w, h int, r normRect) image.Rectangle {
	fw, fh := float64(w), float64(h)
	x0 := int(math.Floor(float64(r.X * fw)))
	y0 := int(math.Floor(float64(r.Y * fh)))
	x1 := int(math.Ceil(float64(float64(r.X+r.W) * fw)))
	y1 := int(math.Ceil(float64(float64(r.Y+r.H) * fh)))
	return image.Rect(x0, y0, x1, y1).Intersect(image.Rect(0, 0, w, h))
}

// applyEffects runs the blur and pixelate operations on img in annotation
// order, each reading the result of the previous one. Every alpha channel it
// touches ends at 255. Buffers are bounded by the affected rectangle and the
// work is linear in its area, independent of the radius.
func applyEffects(img *image.RGBA, ops []effectOp) error {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	var total int64
	rects := make([]image.Rectangle, len(ops))
	for i, op := range ops {
		rects[i] = effectPixelRect(w, h, op.Rect)
		total += int64(rects[i].Dx()) * int64(rects[i].Dy())
	}
	if total > maxEffectPixels {
		return errEffectBudget
	}
	for i, op := range ops {
		r := rects[i]
		if r.Empty() {
			continue
		}
		switch op.Kind {
		case "pixelate":
			pixelateRect(img, r, op.SizePx)
		case "blur":
			blurRect(img, r, op.SizePx)
		default:
			return errors.New("feedback: unknown effect kind")
		}
	}
	return nil
}

// roundedMean is the nearest-integer mean of n values summing to sum, with
// halves rounded up. It uses integer arithmetic only.
func roundedMean(sum, n int) uint8 {
	return uint8((2*sum + n) / (2 * n))
}

// pixelateRect replaces every block of size x size pixels, anchored at the
// top-left of r and cut by r's edges, with the mean RGB of its pixels.
func pixelateRect(img *image.RGBA, r image.Rectangle, size int) {
	for by := r.Min.Y; by < r.Max.Y; by += size {
		bh := min(size, r.Max.Y-by)
		for bx := r.Min.X; bx < r.Max.X; bx += size {
			bw := min(size, r.Max.X-bx)
			var sr, sg, sb int
			for y := by; y < by+bh; y++ {
				row := img.Pix[y*img.Stride+bx*4 : y*img.Stride+(bx+bw)*4]
				for i := 0; i < len(row); i += 4 {
					sr += int(row[i])
					sg += int(row[i+1])
					sb += int(row[i+2])
				}
			}
			n := bw * bh
			mr, mg, mb := roundedMean(sr, n), roundedMean(sg, n), roundedMean(sb, n)
			for y := by; y < by+bh; y++ {
				row := img.Pix[y*img.Stride+bx*4 : y*img.Stride+(bx+bw)*4]
				for i := 0; i < len(row); i += 4 {
					row[i], row[i+1], row[i+2], row[i+3] = mr, mg, mb, 255
				}
			}
		}
	}
}

// boxMean is the rounded mean of the 2*radius+1 values centred on index i of
// a line of n values, where indices outside 0..n-1 repeat the nearest end
// value (first and last). prefix[k] is the sum of the first k values.
func boxMean(prefix []int, i, n, radius, first, last int) uint8 {
	lo, hi := i-radius, i+radius
	sum := 0
	if lo < 0 {
		sum += -lo * first
		lo = 0
	}
	if hi > n-1 {
		sum += (hi - (n - 1)) * last
		hi = n - 1
	}
	sum += prefix[hi+1] - prefix[lo]
	return roundedMean(sum, 2*radius+1)
}

// blurRect applies a two-pass separable box blur of the given radius to the
// RGB channels of r: horizontal first, then vertical, each pass clamped to
// the rectangle's edges and rounded to the nearest integer.
func blurRect(img *image.RGBA, r image.Rectangle, radius int) {
	rw, rh := r.Dx(), r.Dy()
	tmp := make([]uint8, rw*rh*3)
	prefix := make([]int, max(rw, rh)+1)
	for y := range rh {
		off := (r.Min.Y+y)*img.Stride + r.Min.X*4
		for c := range 3 {
			for x := range rw {
				prefix[x+1] = prefix[x] + int(img.Pix[off+x*4+c])
			}
			first, last := int(img.Pix[off+c]), int(img.Pix[off+(rw-1)*4+c])
			for x := range rw {
				tmp[(y*rw+x)*3+c] = boxMean(prefix, x, rw, radius, first, last)
			}
		}
	}
	for x := range rw {
		for c := range 3 {
			for y := range rh {
				prefix[y+1] = prefix[y] + int(tmp[(y*rw+x)*3+c])
			}
			first, last := int(tmp[x*3+c]), int(tmp[((rh-1)*rw+x)*3+c])
			for y := range rh {
				img.Pix[(r.Min.Y+y)*img.Stride+(r.Min.X+x)*4+c] = boxMean(prefix, y, rh, radius, first, last)
			}
		}
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Pix[y*img.Stride+x*4+3] = 255
		}
	}
}

// editorRedactionRect is the rectangle the web editor fills black under the
// effects: floor of the origin, then ceil(width*w) plus one pixel of padding.
// The server pre-redacts it in addition to its own outward-rounded rectangle,
// so both renderers blur the same neighbourhood of a redaction.
func editorRedactionRect(w, h int, r normRect) image.Rectangle {
	fw, fh := float64(w), float64(h)
	x0 := int(math.Floor(float64(r.X * fw)))
	y0 := int(math.Floor(float64(r.Y * fh)))
	x1 := min(w, x0+int(math.Ceil(float64(r.W*fw)))+1)
	y1 := min(h, y0+int(math.Ceil(float64(r.H*fh)))+1)
	return image.Rect(x0, y0, x1, y1).Intersect(image.Rect(0, 0, w, h))
}

// fillBlack overwrites the rectangles with opaque black.
func fillBlack(img *image.RGBA, rects []image.Rectangle) {
	for _, r := range rects {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := img.Pix[y*img.Stride+r.Min.X*4 : y*img.Stride+r.Max.X*4]
			for i := 0; i < len(row); i += 4 {
				row[i], row[i+1], row[i+2], row[i+3] = 0, 0, 0, 255
			}
		}
	}
}

// shareBase prepares a raw base image for sharing. Solid redactions are
// applied first, so blur and pixelate can never sample (and smear outward) a
// secret; the cosmetic effects then run in annotation order; the redactions
// are applied again last and verified, so overlapping effects cannot lighten
// a redacted pixel. Blur and pixelate are not redaction.
func shareBase(img *image.RGBA, reds []normRect, effects []effectOp) error {
	if len(effects) > 0 {
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		applyRedactions(img, reds)
		pad := make([]image.Rectangle, 0, len(reds))
		for _, r := range reds {
			pad = append(pad, editorRedactionRect(w, h, r))
		}
		fillBlack(img, pad)
		if err := applyEffects(img, effects); err != nil {
			return err
		}
	}
	px := applyRedactions(img, reds)
	if !verifyRedactions(img, px) {
		return errors.New("feedback: redaction verification failed")
	}
	return nil
}

// withoutBakedEffects removes the blur and pixelate annotations from a stored
// annotation list. A revision that starts from a published redacted image
// inherits those effects in its pixels, and applying them again would filter
// the image twice. All other annotations keep their stored bytes.
func withoutBakedEffects(annotationsJSON string) (string, error) {
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(annotationsJSON), &list); err != nil {
		return "", err
	}
	kept := make([]json.RawMessage, 0, len(list))
	for _, raw := range list {
		var a struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		if a.Kind == "blur" || a.Kind == "pixelate" {
			continue
		}
		kept = append(kept, raw)
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
