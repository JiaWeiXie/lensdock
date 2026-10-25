package cdp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// extensionsDomainPrefix denies every method of the Extensions domain, which
// can load, uninstall and read the storage of browser extensions.
const extensionsDomainPrefix = "Extensions."

// MethodPolicy is the set of CDP methods a proxied client may not call.
type MethodPolicy struct {
	deny map[string]struct{}
}

// DefaultPolicy denies the methods that could end the browser or expose the
// protocol beyond the proxy, and the whole Extensions domain.
func DefaultPolicy() MethodPolicy {
	deny := map[string]struct{}{}
	for _, m := range []string{
		"Browser.close",
		"Browser.crash",
		"Browser.crashGpuProcess",
		"Target.exposeDevToolsProtocol",
	} {
		deny[m] = struct{}{}
	}
	return MethodPolicy{deny: deny}
}

// Denied reports whether method is denied. Methods are matched exactly, as
// Chromium does, except that the Extensions domain is denied by its
// "Extensions." prefix.
func (p MethodPolicy) Denied(method string) bool {
	if strings.HasPrefix(method, extensionsDomainPrefix) {
		return true
	}
	_, ok := p.deny[method]
	return ok
}

// verdict is the outcome of inspecting one client message.
type verdict int

const (
	verdictAllow verdict = iota
	verdictDeny
	verdictMalformed
)

// request carries the fields echoed in synthetic replies.
type request struct {
	id        json.RawMessage
	sessionID json.RawMessage
}

// inspect classifies one client text message. The message is scanned with a
// token decoder so keys keep their exact case (encoding/json struct decoding
// is case-insensitive and would disagree with Chromium) and duplicate
// method/id/sessionId keys are rejected rather than resolved. Anything that
// cannot be parsed unambiguously fails closed: it is never forwarded.
func (p MethodPolicy) inspect(msg []byte) (verdict, request) {
	var req request
	if !utf8.Valid(msg) {
		return verdictMalformed, req
	}
	dec := json.NewDecoder(bytes.NewReader(msg))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return verdictMalformed, req
	}

	var method string
	var haveMethod, haveID, haveSession bool
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return verdictMalformed, req
		}
		key, ok := keyTok.(string)
		if !ok {
			return verdictMalformed, req
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return verdictMalformed, req
		}
		switch key {
		case "method":
			if haveMethod {
				return verdictMalformed, req
			}
			haveMethod = true
			// A non-string method is rejected by Chromium itself.
			if err := json.Unmarshal(val, &method); err != nil {
				method = ""
			}
		case "id":
			if haveID {
				return verdictMalformed, req
			}
			haveID = true
			req.id = val
		case "sessionId":
			if haveSession {
				return verdictMalformed, req
			}
			haveSession = true
			req.sessionID = val
		}
	}
	if end, err := dec.Token(); err != nil {
		return verdictMalformed, req
	} else if d, ok := end.(json.Delim); !ok || d != '}' {
		return verdictMalformed, req
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return verdictMalformed, req
	}
	if p.Denied(method) {
		return verdictDeny, req
	}
	return verdictAllow, req
}

// reply builds a synthetic CDP error response. The id is echoed verbatim
// when it is a JSON number or string.
func (r request) reply(code int, message string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	if isScalarID(r.id) {
		buf.WriteString(`"id":`)
		buf.Write(r.id)
		buf.WriteByte(',')
	}
	if len(r.sessionID) > 0 && r.sessionID[0] == '"' {
		buf.WriteString(`"sessionId":`)
		buf.Write(r.sessionID)
		buf.WriteByte(',')
	}
	msg, _ := json.Marshal(message)
	buf.WriteString(`"error":{"code":`)
	buf.WriteString(strconv.Itoa(code))
	buf.WriteString(`,"message":`)
	buf.Write(msg)
	buf.WriteString(`}}`)
	return buf.Bytes()
}

func isScalarID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	c := raw[0]
	return c == '"' || c == '-' || (c >= '0' && c <= '9')
}
