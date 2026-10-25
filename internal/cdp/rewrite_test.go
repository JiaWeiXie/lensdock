package cdp

import (
	"encoding/json"
	"strings"
	"testing"
)

func testWSURL(p string) string { return "ws://127.0.0.1:41000/pfx" + p }

func TestRewriteDiscoveryVersion(t *testing.T) {
	in := `{"Browser":"Chrome/200","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/browser/GUID-1"}`
	out, err := RewriteDiscovery(RouteVersion, []byte(in), testWSURL)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Browser":              "Chrome/200",
		"Protocol-Version":     "1.3",
		"webSocketDebuggerUrl": "ws://127.0.0.1:41000/pfx/devtools/browser/GUID-1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if strings.Contains(string(out), "9223") {
		t.Errorf("output still mentions 9223: %s", out)
	}
}

func TestRewriteDiscoveryListDropsFrontendURLsAndKeepsFields(t *testing.T) {
	in := `[{"id":"A1","type":"page","title":"x <b> & y","url":"https://e.com/?q=1&r=2","big":12345678901234567890,"nested":{"k":[1,2.50,null]},` +
		`"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/A1",` +
		`"devtoolsFrontendUrl":"/devtools/inspector.html?ws=127.0.0.1:9223/devtools/page/A1",` +
		`"devtoolsFrontendUrlCompat":"/devtools/inspector.html?ws=127.0.0.1:9223/devtools/page/A1"},` +
		`{"id":"B2","type":"background_page","webSocketDebuggerUrl":"ws://localhost:9223/devtools/page/B2"}]`
	out, err := RewriteDiscovery(RouteList, []byte(in), testWSURL)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"9223", "devtoolsFrontendUrl", "inspector.html", "localhost"} {
		if strings.Contains(s, bad) {
			t.Errorf("output contains %q: %s", bad, s)
		}
	}
	for _, keep := range []string{`12345678901234567890`, `2.50`, `x <b> & y`, `"url":"https://e.com/?q=1&r=2"`,
		`ws://127.0.0.1:41000/pfx/devtools/page/A1`, `ws://127.0.0.1:41000/pfx/devtools/page/B2`} {
		if !strings.Contains(s, keep) {
			t.Errorf("output lost %q: %s", keep, s)
		}
	}
}

func TestRewriteDiscoveryNewAndEmptyList(t *testing.T) {
	out, err := RewriteDiscovery(RouteNew, []byte(`{"id":"N","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/N","devtoolsFrontendUrl":"/x"}`), testWSURL)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "9223") || strings.Contains(s, "Frontend") || !strings.Contains(s, "/pfx/devtools/page/N") {
		t.Errorf("new target not rewritten: %s", s)
	}
	out, err = RewriteDiscovery(RouteList, []byte(`[]`), testWSURL)
	if err != nil || string(out) != `[]` {
		t.Errorf("empty list = %q, %v", out, err)
	}
}

func TestRewriteDiscoveryPassesThroughNonDiscoveryKinds(t *testing.T) {
	for _, k := range []RouteKind{RouteProtocol, RouteActivate, RouteClose} {
		in := []byte("Target activated")
		out, err := RewriteDiscovery(k, in, testWSURL)
		if err != nil || string(out) != string(in) {
			t.Errorf("kind %d: out %q err %v", k, out, err)
		}
	}
}

func TestRewriteDiscoveryRejectsUnsafeDocuments(t *testing.T) {
	tests := []struct{ name, in string }{
		{"not json", `nope`},
		{"trailing data", `{"a":1} {"b":2}`},
		{"list where object expected", `[]`},
		{"null document", `null`},
		{"null element", `[null]`},
		{"non-string ws url", `{"webSocketDebuggerUrl":5}`},
		{"http scheme", `{"webSocketDebuggerUrl":"http://127.0.0.1:9223/devtools/browser/x"}`},
		{"path outside allowlist", `{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/inspector.html"}`},
		{"traversal path", `{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/../x"}`},
		{"non-websocket allowlisted path", `{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/json/version"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RewriteDiscovery(RouteVersion, []byte(tt.in), testWSURL); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := RewriteDiscovery(RouteList, []byte(`[{"webSocketDebuggerUrl":"ws://h/evil"}]`), testWSURL); err == nil {
		t.Error("list entry with foreign path was accepted")
	}
}

func TestRewriteDiscoveryEnforcesSizeCap(t *testing.T) {
	big := []byte(`{"x":"` + strings.Repeat("a", maxDiscoveryBody) + `"}`)
	if _, err := RewriteDiscovery(RouteVersion, big, testWSURL); err == nil {
		t.Fatal("oversized body accepted")
	}
	if _, err := RewriteDiscovery(RouteProtocol, big, testWSURL); err == nil {
		t.Fatal("oversized pass-through body accepted")
	}
}

func TestRewriteDiscoveryUnderMountPath(t *testing.T) {
	in := []byte(`{"webSocketDebuggerUrl":"ws://api.example:8080/api/v1/workspaces/default/cdp/devtools/browser/G-1"}`)
	out, err := rewriteDiscovery(RouteVersion, in, "/api/v1/workspaces/default/cdp", testWSURL)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"webSocketDebuggerUrl":"ws://127.0.0.1:41000/pfx/devtools/browser/G-1"}`; string(out) != want {
		t.Errorf("out = %s, want %s", out, want)
	}
	if _, err := rewriteDiscovery(RouteVersion, in, "/other/mount", testWSURL); err == nil {
		t.Error("URL outside the mount path was accepted")
	}
}
