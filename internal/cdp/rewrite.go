package cdp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// maxDiscoveryBody caps discovery bodies read from Chromium and rewritten.
const maxDiscoveryBody = 16 << 20

var errRewrite = errors.New("cdp: malformed discovery document")

// RewriteDiscovery rewrites a Chromium discovery response for the given route
// kind. Every webSocketDebuggerUrl is rebuilt from its parsed /devtools/...
// path via wsURL, so no upstream host or port survives; devtoolsFrontendUrl
// and devtoolsFrontendUrlCompat are dropped. All other fields are preserved
// (numbers verbatim). Kinds that carry no discovery data are returned
// unchanged. Malformed documents and unexpected WebSocket paths are errors.
func RewriteDiscovery(k RouteKind, body []byte, wsURL func(devtoolsPath string) string) ([]byte, error) {
	return rewriteDiscovery(k, body, "", wsURL)
}

// rewriteDiscovery is RewriteDiscovery for documents whose WebSocket URLs sit
// under a mount path (the runtime proxy's public prefix), which is stripped
// before the /devtools/... path is validated.
func rewriteDiscovery(k RouteKind, body []byte, mount string, wsURL func(devtoolsPath string) string) ([]byte, error) {
	if len(body) > maxDiscoveryBody {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", errRewrite, maxDiscoveryBody)
	}
	switch k {
	case RouteVersion, RouteNew:
		var obj map[string]json.RawMessage
		if err := decodeStrict(body, &obj); err != nil || obj == nil {
			return nil, errRewrite
		}
		if err := rewriteTarget(obj, mount, wsURL); err != nil {
			return nil, err
		}
		return encode(obj)
	case RouteList:
		var list []map[string]json.RawMessage
		if err := decodeStrict(body, &list); err != nil {
			return nil, errRewrite
		}
		for _, obj := range list {
			if obj == nil {
				return nil, errRewrite
			}
			if err := rewriteTarget(obj, mount, wsURL); err != nil {
				return nil, err
			}
		}
		if list == nil {
			list = []map[string]json.RawMessage{}
		}
		return encode(list)
	default:
		return body, nil
	}
}

func rewriteTarget(obj map[string]json.RawMessage, mount string, wsURL func(string) string) error {
	delete(obj, "devtoolsFrontendUrl")
	delete(obj, "devtoolsFrontendUrlCompat")

	raw, ok := obj["webSocketDebuggerUrl"]
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return errRewrite
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return errRewrite
	}
	path, ok := strings.CutPrefix(u.Path, mount)
	if !ok {
		return errRewrite
	}
	route, err := ParseRoute("GET", path, "")
	if err != nil || !route.IsWebSocket() {
		return errRewrite
	}
	out, err := json.Marshal(wsURL(route.Path()))
	if err != nil {
		return errRewrite
	}
	obj["webSocketDebuggerUrl"] = out
	return nil
}

// decodeStrict decodes exactly one JSON value, preserving numbers verbatim.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errRewrite
	}
	if _, err := dec.Token(); err == nil {
		return errRewrite
	}
	return nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: encode", errRewrite)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
