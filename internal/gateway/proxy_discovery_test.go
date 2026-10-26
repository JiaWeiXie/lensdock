package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestGatewayDiscoveryResponseRewrite(t *testing.T) {
	const prefix = "/api/v1/workspaces/default/cdp/devtools/"
	for _, tt := range []struct {
		name   string
		scheme string
		body   string
		want   string
	}{
		{
			name: "browser uses public websocket origin", scheme: "http",
			body: `{"webSocketDebuggerUrl":"ws://runtime:9223` + prefix + `browser/id","devtoolsFrontendUrl":"/private","devtoolsFrontendUrlCompat":"/private-compat"}`,
			want: `{"webSocketDebuggerUrl":"ws://public.example:8080` + prefix + `browser/id"}`,
		},
		{
			name: "nested arrays preserve values and remove userinfo", scheme: "https",
			body: `[[{"webSocketDebuggerUrl":"ws://user@runtime:9223` + prefix + `page/id"}],12345678901234567890,null,true]`,
			want: `[[{"webSocketDebuggerUrl":"wss://public.example:8080` + prefix + `page/id"}],12345678901234567890,null,true]`,
		},
		{
			name: "protocol objects do not rewrite nested properties", scheme: "https",
			body: `{"devtoolsFrontendUrl":"/private","nested":{"webSocketDebuggerUrl":"schema-value","devtoolsFrontendUrl":"schema-field"}}`,
			want: `{"nested":{"webSocketDebuggerUrl":"schema-value","devtoolsFrontendUrl":"schema-field"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := &runtimeEdge{config: RuntimeConfig{WorkspaceID: "default"}, publicOrigin: &url.URL{Scheme: tt.scheme, Host: "public.example:8080"}}
			resp := discoveryResponse(tt.body)
			if err := e.rewriteDiscovery(resp); err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			decode := func(s string) any {
				t.Helper()
				dec := json.NewDecoder(strings.NewReader(s))
				dec.UseNumber()
				var v any
				if err := dec.Decode(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if !reflect.DeepEqual(decode(string(got)), decode(tt.want)) {
				t.Fatalf("rewritten discovery = %s, want %s", got, tt.want)
			}
			if resp.ContentLength != int64(len(got)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
				t.Fatal("response length does not match rewritten bytes")
			}
		})
	}
}

func TestGatewayDiscoveryRejectsInvalidURLs(t *testing.T) {
	const valid = "ws://runtime/api/v1/workspaces/default/cdp/devtools/page/id"
	for _, body := range []string{
		`{"webSocketDebuggerUrl":null}`,
		`{"webSocketDebuggerUrl":5}`,
		`{"webSocketDebuggerUrl":"ws://runtime/devtools/page/id"}`,
		`{"webSocketDebuggerUrl":"` + valid + `?secret=1"}`,
		`{"webSocketDebuggerUrl":"` + valid + `#fragment"}`,
		`{"webSocketDebuggerUrl":"%invalid"}`,
		`[{"webSocketDebuggerUrl":"` + valid + `"},[{"webSocketDebuggerUrl":null}]]`,
	} {
		t.Run(body, func(t *testing.T) {
			e := &runtimeEdge{config: RuntimeConfig{WorkspaceID: "default"}, publicOrigin: &url.URL{Scheme: "https", Host: "public.example"}}
			resp := discoveryResponse(body)
			defer resp.Body.Close()
			if err := e.rewriteDiscovery(resp); err == nil {
				t.Fatal("invalid discovery URL accepted")
			}
		})
	}
}

func discoveryResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{URL: &url.URL{Path: "/api/v1/workspaces/default/cdp/json/version"}},
	}
}
