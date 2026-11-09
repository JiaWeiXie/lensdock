package cdp

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
)

func TestSelectorUsesStableElementAttributes(t *testing.T) {
	tests := []struct {
		name       string
		node       string
		attributes []string
		want       string
	}{
		{name: "id", node: "BUTTON", attributes: []string{"id", `run"next`}, want: `[id="run\"next"]`},
		{name: "name", node: "INPUT", attributes: []string{"name", "search"}, want: `input[name="search"]`},
		{name: "tag", node: "A", want: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selector(tt.node, tt.attributes); got != tt.want {
				t.Fatalf("selector() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFocusHTTPErrorClassifiesOnlyTerminalWireCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		terminal bool
	}{
		{name: "lease ended", status: http.StatusConflict, body: `{"code":"` + apiwire.CodeLeaseEnded + `"}`, terminal: true},
		{name: "generation changed", status: http.StatusPreconditionFailed, body: `{"code":"` + apiwire.CodeGenerationMismatch + `"}`, terminal: true},
		{name: "lease conflict", status: http.StatusConflict, body: `{"code":"` + apiwire.CodeLeaseConflict + `"}`},
		{name: "malformed", status: http.StatusConflict, body: `not-json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(tt.body))}
			err := focusHTTPError(resp, "focus test")
			if got := errors.Is(err, ErrLeaseLost); got != tt.terminal {
				t.Fatalf("ErrLeaseLost = %v, want %v: %v", got, tt.terminal, err)
			}
			if strings.Contains(err.Error(), tt.body) {
				t.Fatalf("error exposed the wire body: %q", err)
			}
		})
	}
}
