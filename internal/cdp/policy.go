package cdp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MethodPolicy is the set of CDP methods a proxied client may not call.
type MethodPolicy struct {
	deny    map[string]struct{}
	domains map[string]struct{} // whole domains such as "Storage"
}

// denyPresets name groups of CDP methods an operator can add to the default
// policy (LENSDOCK_CDP_DENY). An entry ending in ".*" denies a whole domain.
//
//   - cookies: reading or changing the browser's cookies, including HttpOnly
//     ones that page scripts cannot see.
//   - storage: site storage (Storage, IndexedDB, CacheStorage, DOMStorage).
//   - browser-control: ending the browser, changing where downloads go, and the
//     Extensions domain.
//
// A denied method does not make a lease safe for a profile with valuable
// logins: the holder still drives the browser as the signed-in user.
var denyPresets = map[string][]string{
	"cookies": {
		"Network.getAllCookies", "Network.getCookies", "Network.setCookie", "Network.setCookies",
		"Network.deleteCookies", "Network.clearBrowserCookies",
		"Storage.getCookies", "Storage.setCookies", "Storage.clearCookies",
		"Page.getCookies", "Page.deleteCookie",
	},
	"storage":         {"Storage.*", "IndexedDB.*", "CacheStorage.*", "DOMStorage.*"},
	"browser-control": {"Browser.close", "Browser.setDownloadBehavior", "Extensions.*"},
}

// DenyPresetNames lists the preset names in a stable order.
func DenyPresetNames() []string { return []string{"browser-control", "cookies", "storage"} }

var methodPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*\.([A-Za-z][A-Za-z0-9]*|\*)$`)

// PolicyWithDeny returns the default policy plus the given entries. An entry is a
// preset name, a method such as "Network.getAllCookies", or a domain wildcard such
// as "Storage.*". The default denials cannot be removed. Entries are case-sensitive
// like Chromium's method names; an unknown or malformed entry is an error.
func PolicyWithDeny(entries []string) (MethodPolicy, error) {
	p := DefaultPolicy()
	p.domains = map[string]struct{}{}
	add := func(entry string) {
		if domain, ok := strings.CutSuffix(entry, ".*"); ok {
			p.domains[domain] = struct{}{}
			return
		}
		p.deny[entry] = struct{}{}
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if methods, ok := denyPresets[entry]; ok {
			for _, m := range methods {
				add(m)
			}
			continue
		}
		if !methodPattern.MatchString(entry) {
			return MethodPolicy{}, fmt.Errorf("cdp: unknown deny entry %q: use a preset (%s), a method such as Network.getAllCookies, or a domain such as Storage.*", entry, strings.Join(DenyPresetNames(), ", "))
		}
		add(entry)
	}
	return p, nil
}

// DefaultPolicy denies only what would leave the lease fence or fault the
// browser on purpose. An AGENT lease otherwise controls the whole browser,
// including Browser.close (the runtime restarts Chromium, which ends every
// lease with a new generation) and the Extensions domain.
//
//   - Target.exposeDevToolsProtocol hands a page a binding to the browser
//     protocol that is not tied to the lease ticket: it would stay usable
//     after revoke, pause or a generation change.
//   - Target.sendMessageToTarget carries a nested protocol message the proxy
//     does not inspect; it would deliver the denied methods above. Flattened
//     sessions (sessionId on each message, what every current client uses)
//     are inspected like any other message.
//   - Browser.crash and Browser.crashGpuProcess only fault the browser.
func DefaultPolicy() MethodPolicy {
	deny := map[string]struct{}{}
	for _, m := range []string{
		"Browser.crash",
		"Browser.crashGpuProcess",
		"Target.exposeDevToolsProtocol",
		"Target.sendMessageToTarget",
	} {
		deny[m] = struct{}{}
	}
	return MethodPolicy{deny: deny}
}

// Denied reports whether method is denied. Methods are matched exactly, as
// Chromium does.
func (p MethodPolicy) Denied(method string) bool {
	if _, ok := p.deny[method]; ok {
		return true
	}
	domain, _, found := strings.Cut(method, ".")
	if !found {
		return false
	}
	_, ok := p.domains[domain]
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
