package feedback

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Annotation limits from schemas/feedback.schema.json.
const (
	maxAnnotations    = 100
	maxFreehandPts    = 10000
	maxAnnotationID   = 128
	maxComment        = 4000
	maxAnnotText      = 2000
	maxPinNumber      = 100
	coordinateEpsilon = 1e-9

	// Schema 0.3.0 style parameter bounds.
	minHighlightOpacity = 0.1
	maxHighlightOpacity = 0.8
	minEffectSizePx     = 2
	maxEffectSizePx     = 64
	// Default effect sizes in source-image pixels when a blur or pixelate
	// annotation carries no effectSizePx. The renderers use the same values.
	defaultBlurSizePx     = 12
	defaultPixelateSizePx = 16
)

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type point struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}

type rectGeometry struct {
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
	Width  *float64 `json:"width"`
	Height *float64 `json:"height"`
}

type arrowGeometry struct {
	Start *point `json:"start"`
	End   *point `json:"end"`
	// Control is the quadratic Bezier control point of a curved arrow
	// (schema 0.3.0). It is only valid for the arrow kind.
	Control *point `json:"control"`
}

type freehandGeometry struct {
	Points []point `json:"points"`
}

type textGeometry struct {
	Position *point  `json:"position"`
	Text     *string `json:"text"`
}

type pinGeometry struct {
	Position *point `json:"position"`
	Number   *int   `json:"number"`
}

// annotationStyle carries the style of one annotation. The first four
// fields exist since schema 0.1.0; every later field is schema 0.3.0 only and
// valid for exactly one annotation kind (see validStyle).
type annotationStyle struct {
	Stroke        *string  `json:"stroke,omitempty"`
	Fill          *string  `json:"fill,omitempty"`
	StrokeWidthPx *float64 `json:"strokeWidthPx,omitempty"`
	FontSizePx    *float64 `json:"fontSizePx,omitempty"`
	// Opacity is the highlight's alpha, 0.1 to 0.8.
	Opacity *float64 `json:"opacity,omitempty"`
	// EffectSizePx is the blur radius or pixelate block edge in source-image
	// pixels, an integer from 2 to 64.
	EffectSizePx *float64 `json:"effectSizePx,omitempty"`
	ArrowStyle   *string  `json:"arrowStyle,omitempty"`
	TextStyle    *string  `json:"textStyle,omitempty"`
	Smoothing    *bool    `json:"smoothing,omitempty"`
}

type rawAnnotation struct {
	ID       *string          `json:"id"`
	Kind     *string          `json:"kind"`
	Geometry json.RawMessage  `json:"geometry"`
	Comment  *string          `json:"comment"`
	Style    *annotationStyle `json:"style"`
	Mode     *string          `json:"mode"`
}

// outAnnotation is the canonical output form.
type outAnnotation struct {
	ID       string           `json:"id"`
	Kind     string           `json:"kind"`
	Geometry any              `json:"geometry"`
	Comment  *string          `json:"comment,omitempty"`
	Style    *annotationStyle `json:"style,omitempty"`
	Mode     string           `json:"mode,omitempty"`
}

type outPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type outRect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type outArrow struct {
	Start   outPoint  `json:"start"`
	End     outPoint  `json:"end"`
	Control *outPoint `json:"control,omitempty"`
}

type outFreehand struct {
	Points []outPoint `json:"points"`
}

type outText struct {
	Position outPoint `json:"position"`
	Text     string   `json:"text"`
}

type outPin struct {
	Position outPoint `json:"position"`
	Number   int      `json:"number"`
}

// normRect is a validated rectangle in normalized coordinates.
type normRect struct{ X, Y, W, H float64 }

// effectOp is one cosmetic base-image effect (blur or pixelate) in
// annotation order. It is NOT a redaction: it never hides content securely.
type effectOp struct {
	Kind   string
	Rect   normRect
	SizePx int
}

// annotationSet is the validated form of an annotation list.
type annotationSet struct {
	Canonical  []json.RawMessage
	Redactions []normRect
	Effects    []effectOp
}

func strictUnmarshal(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func unit(p *float64) (float64, bool) {
	if p == nil || !finite(*p) || *p < 0 || *p > 1 {
		return 0, false
	}
	return *p, true
}

func toPoint(p *point) (outPoint, bool) {
	if p == nil {
		return outPoint{}, false
	}
	x, ok1 := unit(p.X)
	y, ok2 := unit(p.Y)
	return outPoint{X: x, Y: y}, ok1 && ok2
}

func toRect(g *rectGeometry) (outRect, bool) {
	x, ok1 := unit(g.X)
	y, ok2 := unit(g.Y)
	if !ok1 || !ok2 || g.Width == nil || g.Height == nil {
		return outRect{}, false
	}
	w, h := *g.Width, *g.Height
	if !finite(w) || !finite(h) || w <= 0 || h <= 0 || w > 1 || h > 1 {
		return outRect{}, false
	}
	// Cross-field rule the JSON Schema cannot express.
	if x+w > 1+coordinateEpsilon || y+h > 1+coordinateEpsilon {
		return outRect{}, false
	}
	return outRect{X: x, Y: y, Width: w, Height: h}, true
}

func validText(s string, max int, multiline bool) bool {
	if utf8.RuneCountInString(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\t' {
			continue
		}
		if (r == '\n' || r == '\r') && multiline {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validStyle checks a style against the annotation kind. The four schema
// 0.1.0 fields keep their meaning for every styled kind; each schema 0.3.0
// field is valid for exactly one kind and is refused everywhere else.
func validStyle(kind string, s *annotationStyle) bool {
	if s == nil {
		return true
	}
	if s.Stroke != nil && !colorPattern.MatchString(*s.Stroke) {
		return false
	}
	if s.Fill != nil && !colorPattern.MatchString(*s.Fill) {
		return false
	}
	if s.StrokeWidthPx != nil && (!finite(*s.StrokeWidthPx) || *s.StrokeWidthPx < 0.5 || *s.StrokeWidthPx > 32) {
		return false
	}
	if s.FontSizePx != nil && (!finite(*s.FontSizePx) || *s.FontSizePx < 8 || *s.FontSizePx > 128) {
		return false
	}
	if s.Opacity != nil && (kind != "highlight" || !finite(*s.Opacity) || *s.Opacity < minHighlightOpacity || *s.Opacity > maxHighlightOpacity) {
		return false
	}
	if s.EffectSizePx != nil {
		v := *s.EffectSizePx
		if (kind != "blur" && kind != "pixelate") || !finite(v) || v != math.Trunc(v) || v < minEffectSizePx || v > maxEffectSizePx {
			return false
		}
	}
	if s.ArrowStyle != nil {
		if kind != "arrow" {
			return false
		}
		switch *s.ArrowStyle {
		case "filled", "open", "double", "curved":
		default:
			return false
		}
	}
	if s.TextStyle != nil {
		if kind != "text" {
			return false
		}
		switch *s.TextStyle {
		case "plain", "outline", "label", "shadow", "mono", "bold", "underline":
		default:
			return false
		}
	}
	if s.Smoothing != nil && kind != "freehand" {
		return false
	}
	return true
}

// ParseAnnotations validates annotations against the schema and the
// cross-field rules, and returns them in canonical JSON form together with
// the solid redaction rectangles. Unknown fields, arbitrary SVG or script
// are rejected: annotations are plain data.
func ParseAnnotations(in []json.RawMessage) (canonical []json.RawMessage, redactions []normRect, err error) {
	set, err := parseAnnotationSet(in)
	if err != nil {
		return nil, nil, err
	}
	return set.Canonical, set.Redactions, nil
}

// parseAnnotationSet is ParseAnnotations that also returns the cosmetic
// blur and pixelate effects in annotation order.
func parseAnnotationSet(in []json.RawMessage) (*annotationSet, error) {
	if len(in) > maxAnnotations {
		return nil, annotationInvalid("too many annotations")
	}
	set := &annotationSet{Canonical: make([]json.RawMessage, 0, len(in))}
	ids := map[string]bool{}
	pins := map[int]bool{}
	for i, raw := range in {
		var a rawAnnotation
		if !strictUnmarshal(raw, &a) {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d is malformed", i))
		}
		if a.ID == nil || *a.ID == "" || utf8.RuneCountInString(*a.ID) > maxAnnotationID || !validText(*a.ID, maxAnnotationID, false) {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d has an invalid id", i))
		}
		if ids[*a.ID] {
			return nil, annotationInvalid("annotation ids must be unique")
		}
		ids[*a.ID] = true
		if a.Kind == nil || len(a.Geometry) == 0 {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d needs kind and geometry", i))
		}
		if a.Comment != nil && !validText(*a.Comment, maxComment, true) {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d has an invalid comment", i))
		}
		if !validStyle(*a.Kind, a.Style) {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d has an invalid style", i))
		}
		out := outAnnotation{ID: *a.ID, Kind: *a.Kind, Comment: a.Comment, Style: a.Style}
		bad := annotationInvalid(fmt.Sprintf("annotation %d has invalid geometry", i))
		switch *a.Kind {
		case "rectangle", "redact", "ellipse", "diamond", "highlight", "spotlight", "blur", "pixelate":
			var g rectGeometry
			if !strictUnmarshal(a.Geometry, &g) {
				return nil, bad
			}
			r, ok := toRect(&g)
			if !ok {
				return nil, bad
			}
			out.Geometry = r
			nr := normRect{X: r.X, Y: r.Y, W: r.Width, H: r.Height}
			switch *a.Kind {
			case "redact":
				if a.Mode == nil || *a.Mode != "solid" || a.Style != nil {
					return nil, annotationInvalid(fmt.Sprintf("annotation %d: redact requires mode \"solid\" and no style", i))
				}
				out.Mode = "solid"
				set.Redactions = append(set.Redactions, nr)
			case "blur", "pixelate":
				size := defaultBlurSizePx
				if *a.Kind == "pixelate" {
					size = defaultPixelateSizePx
				}
				if a.Style != nil && a.Style.EffectSizePx != nil {
					size = int(*a.Style.EffectSizePx)
				}
				set.Effects = append(set.Effects, effectOp{Kind: *a.Kind, Rect: nr, SizePx: size})
			}
		case "arrow", "line":
			var g arrowGeometry
			if !strictUnmarshal(a.Geometry, &g) {
				return nil, bad
			}
			s, ok1 := toPoint(g.Start)
			e, ok2 := toPoint(g.End)
			if !ok1 || !ok2 {
				return nil, bad
			}
			oa := outArrow{Start: s, End: e}
			if g.Control != nil {
				c, ok := toPoint(g.Control)
				if !ok || *a.Kind != "arrow" {
					return nil, bad
				}
				oa.Control = &c
			}
			out.Geometry = oa
		case "freehand":
			var g freehandGeometry
			if !strictUnmarshal(a.Geometry, &g) || len(g.Points) < 2 || len(g.Points) > maxFreehandPts {
				return nil, bad
			}
			pts := make([]outPoint, 0, len(g.Points))
			for j := range g.Points {
				p, ok := toPoint(&g.Points[j])
				if !ok {
					return nil, bad
				}
				pts = append(pts, p)
			}
			out.Geometry = outFreehand{Points: pts}
		case "text":
			var g textGeometry
			if !strictUnmarshal(a.Geometry, &g) || g.Text == nil {
				return nil, bad
			}
			p, ok := toPoint(g.Position)
			if !ok || utf8.RuneCountInString(*g.Text) < 1 || !validText(*g.Text, maxAnnotText, true) {
				return nil, bad
			}
			out.Geometry = outText{Position: p, Text: *g.Text}
		case "pin":
			var g pinGeometry
			if !strictUnmarshal(a.Geometry, &g) || g.Number == nil {
				return nil, bad
			}
			p, ok := toPoint(g.Position)
			if !ok || *g.Number < 1 || *g.Number > maxPinNumber || pins[*g.Number] {
				return nil, bad
			}
			pins[*g.Number] = true
			out.Geometry = outPin{Position: p, Number: *g.Number}
		default:
			return nil, annotationInvalid(fmt.Sprintf("annotation %d has an unknown kind", i))
		}
		if a.Mode != nil && *a.Kind != "redact" {
			return nil, annotationInvalid(fmt.Sprintf("annotation %d: mode is only valid for redact", i))
		}
		b, merr := marshalCompact(out)
		if merr != nil {
			return nil, ErrInternal
		}
		set.Canonical = append(set.Canonical, json.RawMessage(b))
	}
	return set, nil
}

// needsAdvancedSchema reports whether one canonical annotation uses a schema
// 0.3.0 feature: a rectangle-effect kind, a curve control point or any of the
// 0.3.0 style fields. Absent features keep the annotation valid for 0.1.0 and
// 0.2.0 readers.
func needsAdvancedSchema(raw json.RawMessage) bool {
	var a struct {
		Kind     string `json:"kind"`
		Geometry struct {
			Control *outPoint `json:"control"`
		} `json:"geometry"`
		Style *annotationStyle `json:"style"`
	}
	if json.Unmarshal(raw, &a) != nil {
		return false
	}
	switch a.Kind {
	case "highlight", "spotlight", "blur", "pixelate":
		return true
	}
	if a.Geometry.Control != nil {
		return true
	}
	s := a.Style
	return s != nil && (s.Opacity != nil || s.EffectSizePx != nil || s.ArrowStyle != nil || s.TextStyle != nil || s.Smoothing != nil)
}
