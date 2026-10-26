package feedback

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func raws(t *testing.T, items ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(items))
	for i, s := range items {
		out[i] = json.RawMessage(s)
	}
	return out
}

func TestParseAnnotationsAcceptsEveryKind(t *testing.T) {
	in := raws(t,
		`{"id":"r","kind":"rectangle","geometry":{"x":0.1,"y":0.1,"width":0.5,"height":0.5},"comment":"","style":{"stroke":"#FF0000","fill":"#00ff00","strokeWidthPx":2,"fontSizePx":12}}`,
		`{"id":"a","kind":"arrow","geometry":{"start":{"x":0,"y":0},"end":{"x":1,"y":1}}}`,
		`{"id":"f","kind":"freehand","geometry":{"points":[{"x":0,"y":0},{"x":0.5,"y":0.5}]}}`,
		`{"id":"t","kind":"text","geometry":{"position":{"x":0.2,"y":0.2},"text":"家 👨‍👩‍👧 Ünï"},"comment":"line1\nline2"}`,
		`{"id":"p","kind":"pin","geometry":{"position":{"x":0.3,"y":0.3},"number":1},"comment":"step 1"}`,
		`{"id":"x","kind":"redact","mode":"solid","geometry":{"x":0.5,"y":0.5,"width":0.5,"height":0.5}}`,
	)
	canon, reds, err := ParseAnnotations(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(canon) != 6 || len(reds) != 1 {
		t.Fatalf("canon=%d reds=%d", len(canon), len(reds))
	}
	// Text is preserved byte for byte (no normalization, no HTML escaping).
	if !strings.Contains(string(canon[3]), "家 👨‍👩‍👧 Ünï") {
		t.Fatalf("text altered: %s", canon[3])
	}
	// Canonical output re-parses to itself.
	again, _, err := ParseAnnotations(canon)
	if err != nil {
		t.Fatal(err)
	}
	for i := range canon {
		if string(canon[i]) != string(again[i]) {
			t.Fatalf("canonical form unstable: %s vs %s", canon[i], again[i])
		}
	}
	// Exactly touching the border is allowed.
	if _, _, err := ParseAnnotations(raws(t, `{"id":"e","kind":"rectangle","geometry":{"x":0.5,"y":0.5,"width":0.5,"height":0.5}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestParseAnnotationsRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"x+w > 1":             `{"id":"a","kind":"rectangle","geometry":{"x":0.6,"y":0,"width":0.5,"height":0.1}}`,
		"y+h > 1":             `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0.9,"width":0.1,"height":0.2}}`,
		"zero width":          `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0,"height":0.2}}`,
		"negative x":          `{"id":"a","kind":"rectangle","geometry":{"x":-0.1,"y":0,"width":0.1,"height":0.2}}`,
		"x above 1":           `{"id":"a","kind":"arrow","geometry":{"start":{"x":1.5,"y":0},"end":{"x":0,"y":0}}}`,
		"missing field":       `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1}}`,
		"unknown kind":        `{"id":"a","kind":"svg","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"extra field":         `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"svg":"<script>"}`,
		"extra geometry key":  `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1,"rx":1}}`,
		"empty id":            `{"id":"","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"missing id":          `{"kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"long id":             `{"id":"` + strings.Repeat("a", 129) + `","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"bad color":           `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"style":{"stroke":"red"}}`,
		"stroke too wide":     `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"style":{"strokeWidthPx":33}}`,
		"font too small":      `{"id":"a","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"x"},"style":{"fontSizePx":4}}`,
		"one freehand point":  `{"id":"a","kind":"freehand","geometry":{"points":[{"x":0,"y":0}]}}`,
		"empty text":          `{"id":"a","kind":"text","geometry":{"position":{"x":0,"y":0},"text":""}}`,
		"control char text":   `{"id":"a","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"a\u0000b"}}`,
		"pin number 0":        `{"id":"a","kind":"pin","geometry":{"position":{"x":0,"y":0},"number":0}}`,
		"redact without mode": `{"id":"a","kind":"redact","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"redact blur":         `{"id":"a","kind":"redact","mode":"blur","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"redact with style":   `{"id":"a","kind":"redact","mode":"solid","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"style":{"fill":"#000000"}}`,
		"mode on rectangle":   `{"id":"a","kind":"rectangle","mode":"solid","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`,
		"not an object":       `[]`,
		"null geometry":       `{"id":"a","kind":"rectangle","geometry":null}`,
		"trailing data":       `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}} {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseAnnotations(raws(t, raw))
			var fe *Error
			if err == nil {
				t.Fatalf("accepted: %s", raw)
			}
			if e, ok := err.(*Error); ok {
				fe = e
			}
			if fe == nil || fe.Code != CodeAnnotationInvalid || fe.Status != 400 {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestParseAnnotationsCrossAnnotationRules(t *testing.T) {
	dup := `{"id":"a","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`
	if _, _, err := ParseAnnotations(raws(t, dup, dup)); err == nil {
		t.Fatal("duplicate ids accepted")
	}
	pin := func(id string, n int) string {
		return fmt.Sprintf(`{"id":%q,"kind":"pin","geometry":{"position":{"x":0,"y":0},"number":%d}}`, id, n)
	}
	if _, _, err := ParseAnnotations(raws(t, pin("a", 1), pin("b", 1))); err == nil {
		t.Fatal("duplicate pin numbers accepted")
	}
	// Annotation, freehand point, text and comment counts are not capped:
	// 101 annotations, a 10001-point stroke, a 5000-character text and a
	// 9000-character comment are all accepted.
	many := make([]json.RawMessage, 101)
	for i := range many {
		many[i] = json.RawMessage(fmt.Sprintf(`{"id":"r%d","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1}}`, i))
	}
	if _, _, err := ParseAnnotations(many); err != nil {
		t.Fatalf("101 annotations refused: %v", err)
	}
	pts := func(n int) string {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"x":0.5,"y":0.5}`)
		}
		return `{"id":"f","kind":"freehand","geometry":{"points":[` + b.String() + `]}}`
	}
	if _, _, err := ParseAnnotations(raws(t, pts(10001))); err != nil {
		t.Fatalf("10001 points refused: %v", err)
	}
	longText := `{"id":"t","kind":"text","geometry":{"position":{"x":0,"y":0},"text":"` + strings.Repeat("a", 5000) + `"}}`
	longComment := `{"id":"c","kind":"rectangle","geometry":{"x":0,"y":0,"width":0.1,"height":0.1},"comment":"` + strings.Repeat("a", 9000) + `"}`
	if _, _, err := ParseAnnotations(raws(t, longText, longComment)); err != nil {
		t.Fatalf("long text or comment refused: %v", err)
	}
	hugePin := `{"id":"p","kind":"pin","geometry":{"position":{"x":0,"y":0},"number":101}}`
	if _, _, err := ParseAnnotations(raws(t, hugePin)); err != nil {
		t.Fatalf("pin number above 100 refused: %v", err)
	}
}

func TestValidateTask(t *testing.T) {
	ok := Task{Kind: "bug", Intent: "reproduce", Title: "t", Description: "d", ExpectedResult: "e", ActualResult: "a", ReproductionSteps: []string{"one"}}
	if err := prepareTaskForPublish(&ok); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Task){
		"kind":          func(x *Task) { x.Kind = "epic" },
		"intent":        func(x *Task) { x.Intent = "" },
		"title newline": func(x *Task) { x.Title = "a\nb" },
		"control":       func(x *Task) { x.Description = "a\x07b" },
		"empty step":    func(x *Task) { x.ReproductionSteps = []string{""} },
	}
	for name, mut := range cases {
		x := ok
		x.ReproductionSteps = append([]string{}, ok.ReproductionSteps...)
		mut(&x)
		if err := validateTask(&x); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Long CJK descriptions remain valid.
	long := ok
	long.Description = strings.Repeat("字", 12000)
	if err := validateTask(&long); err != nil {
		t.Fatalf("12000 code points rejected: %v", err)
	}
	// Prose is optional at publication; a blank title gets the fixed fallback
	// and nothing else is invented.
	for name, mut := range map[string]func(*Task){
		"no title":        func(x *Task) { x.Title = "" },
		"blank title":     func(x *Task) { x.Title = " \t " },
		"no description":  func(x *Task) { x.Description = "" },
		"bug no expected": func(x *Task) { x.ExpectedResult = "" },
		"vr no actual":    func(x *Task) { x.Kind = "visual-regression"; x.ActualResult = "" },
		"no prose": func(x *Task) {
			x.Title, x.Description, x.ExpectedResult, x.ActualResult, x.ReproductionSteps = "", "", "", "", nil
		},
	} {
		x := ok
		mut(&x)
		before := x
		if err := prepareTaskForPublish(&x); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantTitle := before.Title
		if strings.TrimSpace(wantTitle) == "" {
			wantTitle = DefaultTitle
		}
		if x.Title != wantTitle || x.Description != before.Description || x.ExpectedResult != before.ExpectedResult || x.ActualResult != before.ActualResult {
			t.Fatalf("%s changed the task: %+v", name, x)
		}
		if x.ReproductionSteps == nil {
			t.Fatalf("%s: reproduction steps must be an array", name)
		}
	}
	explicit := ok
	explicit.Title = "  keep me  "
	if err := prepareTaskForPublish(&explicit); err != nil || explicit.Title != "  keep me  " {
		t.Fatalf("supplied title = %q, %v", explicit.Title, err)
	}
	// Prose length is not capped at publication either.
	long = ok
	long.ExpectedResult = strings.Repeat("e", 20000)
	if err := prepareTaskForPublish(&long); err != nil {
		t.Fatalf("long expected result refused: %v", err)
	}
	// Character checks still apply.
	control := ok
	control.Title = "a\x07"
	if err := prepareTaskForPublish(&control); err == nil {
		t.Fatal("control character in title published")
	}

	// Schema 0.1.0 readers need the prose; everything without it is 0.2.0.
	for name, c := range map[string]struct {
		mut  func(*Task)
		want bool
	}{
		"complete bug":    {func(*Task) {}, false},
		"no description":  {func(x *Task) { x.Description = "" }, true},
		"bug no expected": {func(x *Task) { x.ExpectedResult = "" }, true},
		"vr no actual":    {func(x *Task) { x.Kind = "visual-regression"; x.ActualResult = "" }, true},
		"question bare":   {func(x *Task) { x.Kind = "question"; x.ExpectedResult, x.ActualResult = "", "" }, false},
		"task bare":       {func(x *Task) { x.Kind = "task"; x.ExpectedResult, x.ActualResult = "", "" }, false},
	} {
		x := ok
		c.mut(&x)
		if got := taskNeedsProseFreeSchema(x); got != c.want {
			t.Errorf("%s: taskNeedsProseFreeSchema = %v, want %v", name, got, c.want)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"https://user:pw@App.test:8443/a/b?token=1#frag": "https://App.test:8443/a/b",
		"http://x.test/?q=secret":                        "http://x.test/",
		"chrome://settings/passwords":                    "chrome:",
		"javascript:alert(1)":                            "javascript:",
		"not a url":                                      "about:blank",
		"":                                               "about:blank",
	}
	for in, want := range cases {
		if got := sanitizeURL(in); got != want {
			t.Errorf("sanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrincipalScopes(t *testing.T) {
	if !adminP.InWorkspace("anything") || humanP.InWorkspace("ws2") || !humanP.InWorkspace("ws1") {
		t.Fatal("workspace scoping is wrong")
	}
	if (Principal{ID: "a", Role: RoleAdmin}).Valid() != true || (Principal{ID: "a", Role: RoleHuman}).Valid() {
		t.Fatal("only admins may be unscoped")
	}
	if !humanP.IsHuman() || agentP.IsHuman() || viewerP.IsHuman() || !adminP.IsHuman() {
		t.Fatal("IsHuman is wrong")
	}
}
