// Package cdp implements the LensDock Chrome DevTools Protocol edge: the
// runtime-side Proxy that exposes an allowlisted subset of Chromium's
// discovery and WebSocket endpoints to an AGENT lease holder, and the
// host-side Bridge that gives local CDP clients a loopback endpoint backed by
// that Proxy.
package cdp

import (
	"errors"
	"strings"
)

// RouteKind identifies one allowlisted CDP endpoint.
type RouteKind int

// The allowlisted routes. The zero value is not a valid route.
const (
	RouteVersion RouteKind = iota + 1
	RouteList
	RouteProtocol
	RouteNew
	RouteActivate
	RouteClose
	RouteWSBrowser
	RouteWSPage
)

// maxNewQuery bounds the query forwarded for /json/new, which carries the
// URL to open.
const maxNewQuery = 8192

// Route is a parsed, allowlisted request target.
type Route struct {
	Kind RouteKind
	// ID is the target or session id for activate, close and WebSocket routes.
	ID string
	// RawQuery is only set for RouteNew, where Chromium takes the whole query
	// as the URL to open. Queries on every other route are ignored.
	RawQuery string
}

var (
	// ErrRouteNotFound reports a path outside the allowlist.
	ErrRouteNotFound = errors.New("cdp: route not allowed")
	// ErrMethodNotAllowed reports an allowlisted path requested with the wrong method.
	ErrMethodNotAllowed = errors.New("cdp: method not allowed for route")
	// ErrQueryTooLong reports an oversized /json/new query.
	ErrQueryTooLong = errors.New("cdp: query too long")
)

// ParseRoute maps a request to the allowlist. path is the already-decoded
// request path relative to the CDP mount point. Exactly one trailing slash is
// tolerated. Ids must match ^[A-Za-z0-9-]{1,64}$. WebSocket routes are
// reported for GET; the caller still has to verify the Upgrade handshake.
func ParseRoute(method, path, rawQuery string) (Route, error) {
	if !strings.HasPrefix(path, "/") {
		return Route{}, ErrRouteNotFound
	}
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}

	want := func(m string, r Route) (Route, error) {
		if method != m {
			return Route{}, ErrMethodNotAllowed
		}
		return r, nil
	}

	switch path {
	case "/json/version":
		return want("GET", Route{Kind: RouteVersion})
	case "/json", "/json/list":
		return want("GET", Route{Kind: RouteList})
	case "/json/protocol":
		return want("GET", Route{Kind: RouteProtocol})
	case "/json/new":
		if method != "PUT" {
			return Route{}, ErrMethodNotAllowed
		}
		if len(rawQuery) > maxNewQuery {
			return Route{}, ErrQueryTooLong
		}
		return Route{Kind: RouteNew, RawQuery: rawQuery}, nil
	}

	for _, p := range []struct {
		prefix string
		kind   RouteKind
	}{
		{"/json/activate/", RouteActivate},
		{"/json/close/", RouteClose},
		{"/devtools/browser/", RouteWSBrowser},
		{"/devtools/page/", RouteWSPage},
	} {
		id, ok := strings.CutPrefix(path, p.prefix)
		if !ok {
			continue
		}
		if !validID(id) {
			return Route{}, ErrRouteNotFound
		}
		return want("GET", Route{Kind: p.kind, ID: id})
	}
	return Route{}, ErrRouteNotFound
}

// Path returns the canonical Chromium path for the route. It is rebuilt from
// parsed components, never copied from the request.
func (r Route) Path() string {
	switch r.Kind {
	case RouteVersion:
		return "/json/version"
	case RouteList:
		return "/json/list"
	case RouteProtocol:
		return "/json/protocol"
	case RouteNew:
		return "/json/new"
	case RouteActivate:
		return "/json/activate/" + r.ID
	case RouteClose:
		return "/json/close/" + r.ID
	case RouteWSBrowser:
		return "/devtools/browser/" + r.ID
	case RouteWSPage:
		return "/devtools/page/" + r.ID
	}
	return ""
}

// Method returns the HTTP method Chromium expects for the route.
func (r Route) Method() string {
	if r.Kind == RouteNew {
		return "PUT"
	}
	return "GET"
}

// IsWebSocket reports whether the route is a WebSocket endpoint.
func (r Route) IsWebSocket() bool {
	return r.Kind == RouteWSBrowser || r.Kind == RouteWSPage
}

func validID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
