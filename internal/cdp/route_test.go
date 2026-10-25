package cdp

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRoute(t *testing.T) {
	id64 := strings.Repeat("a", 64)
	tests := []struct {
		name, method, path, query string
		want                      Route
		err                       error
	}{
		{"version", "GET", "/json/version", "", Route{Kind: RouteVersion}, nil},
		{"version trailing slash", "GET", "/json/version/", "", Route{Kind: RouteVersion}, nil},
		{"json list", "GET", "/json", "", Route{Kind: RouteList}, nil},
		{"json list trailing slash", "GET", "/json/", "", Route{Kind: RouteList}, nil},
		{"json/list", "GET", "/json/list", "", Route{Kind: RouteList}, nil},
		{"protocol", "GET", "/json/protocol", "", Route{Kind: RouteProtocol}, nil},
		{"new keeps query", "PUT", "/json/new", "https://example.com/?a=b", Route{Kind: RouteNew, RawQuery: "https://example.com/?a=b"}, nil},
		{"new trailing slash", "PUT", "/json/new/", "", Route{Kind: RouteNew}, nil},
		{"activate", "GET", "/json/activate/AB-12", "", Route{Kind: RouteActivate, ID: "AB-12"}, nil},
		{"close", "GET", "/json/close/ab12", "", Route{Kind: RouteClose, ID: "ab12"}, nil},
		{"ws browser", "GET", "/devtools/browser/0f-9A", "", Route{Kind: RouteWSBrowser, ID: "0f-9A"}, nil},
		{"ws page", "GET", "/devtools/page/" + id64, "", Route{Kind: RouteWSPage, ID: id64}, nil},
		{"query ignored outside new", "GET", "/json/version", "x=1", Route{Kind: RouteVersion}, nil},

		{"new via GET", "GET", "/json/new", "", Route{}, ErrMethodNotAllowed},
		{"new via POST", "POST", "/json/new", "", Route{}, ErrMethodNotAllowed},
		{"version via PUT", "PUT", "/json/version", "", Route{}, ErrMethodNotAllowed},
		{"list via DELETE", "DELETE", "/json/list", "", Route{}, ErrMethodNotAllowed},
		{"close via PUT", "PUT", "/json/close/ab", "", Route{}, ErrMethodNotAllowed},
		{"ws via POST", "POST", "/devtools/page/ab", "", Route{}, ErrMethodNotAllowed},
		{"lowercase method", "get", "/json/version", "", Route{}, ErrMethodNotAllowed},
		{"new query too long", "PUT", "/json/new", strings.Repeat("a", maxNewQuery+1), Route{}, ErrQueryTooLong},

		{"id too long", "GET", "/devtools/page/" + id64 + "a", "", Route{}, ErrRouteNotFound},
		{"empty id", "GET", "/devtools/page/", "", Route{}, ErrRouteNotFound},
		{"id with slash", "GET", "/devtools/page/a/b", "", Route{}, ErrRouteNotFound},
		{"id with dots", "GET", "/devtools/page/..", "", Route{}, ErrRouteNotFound},
		{"id with space", "GET", "/json/close/a b", "", Route{}, ErrRouteNotFound},
		{"id with underscore", "GET", "/json/activate/a_b", "", Route{}, ErrRouteNotFound},
		{"id non-ascii", "GET", "/json/activate/é", "", Route{}, ErrRouteNotFound},
		{"two trailing slashes", "GET", "/json/version//", "", Route{}, ErrRouteNotFound},
		{"double leading slash", "GET", "//json/version", "", Route{}, ErrRouteNotFound},
		{"extra segment", "GET", "/json/version/extra", "", Route{}, ErrRouteNotFound},
		{"dot segment", "GET", "/json/../json/version", "", Route{}, ErrRouteNotFound},
		{"unknown json path", "GET", "/json/foo", "", Route{}, ErrRouteNotFound},
		{"inspector page", "GET", "/devtools/inspector.html", "", Route{}, ErrRouteNotFound},
		{"case differs", "GET", "/JSON/version", "", Route{}, ErrRouteNotFound},
		{"root", "GET", "/", "", Route{}, ErrRouteNotFound},
		{"empty path", "GET", "", "", Route{}, ErrRouteNotFound},
		{"no leading slash", "GET", "json/version", "", Route{}, ErrRouteNotFound},
		{"devtools remote debugger", "GET", "/devtools/remote/serve_file/@x/inspector.html", "", Route{}, ErrRouteNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRoute(tt.method, tt.path, tt.query)
			if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("route = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRoutePathIsRebuiltFromParts(t *testing.T) {
	for _, tt := range []struct{ method, path, want string }{
		{"GET", "/json/version/", "/json/version"},
		{"GET", "/json", "/json/list"},
		{"PUT", "/json/new/", "/json/new"},
		{"GET", "/json/activate/AB/", "/json/activate/AB"},
		{"GET", "/devtools/page/AB/", "/devtools/page/AB"},
		{"GET", "/devtools/browser/x-1", "/devtools/browser/x-1"},
	} {
		r, err := ParseRoute(tt.method, tt.path, "")
		if err != nil {
			t.Fatalf("%s %s: %v", tt.method, tt.path, err)
		}
		if got := r.Path(); got != tt.want {
			t.Errorf("Path(%s) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
