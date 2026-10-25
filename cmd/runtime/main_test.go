package main

import (
	"io"
	"testing"
)

func TestHealthURLFollowsListenAddress(t *testing.T) {
	tests := []struct {
		name   string
		listen string
		want   string
	}{
		{"unset", "", "http://127.0.0.1:8080/readyz"},
		{"default listen", ":8080", "http://127.0.0.1:8080/readyz"},
		{"custom port on all interfaces", "0.0.0.0:9090", "http://127.0.0.1:9090/readyz"},
		{"custom port on IPv6 wildcard", "[::]:9090", "http://127.0.0.1:9090/readyz"},
		{"loopback host kept", "127.0.0.1:9000", "http://127.0.0.1:9000/readyz"},
		{"IPv6 loopback kept", "[::1]:9000", "http://[::1]:9000/readyz"},
		{"non-loopback host is never probed", "192.0.2.10:9000", "http://127.0.0.1:9000/readyz"},
		{"hostname is never probed", "example.com:9000", "http://127.0.0.1:9000/readyz"},
		{"missing port falls back", "localhost", "http://127.0.0.1:8080/readyz"},
		{"port zero falls back", ":0", "http://127.0.0.1:8080/readyz"},
		{"non-numeric port falls back", ":http", "http://127.0.0.1:8080/readyz"},
		{"port out of range falls back", ":70000", "http://127.0.0.1:8080/readyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := healthURL(func(k string) string {
				if k == "LENSDOCK_LISTEN" {
					return tt.listen
				}
				return ""
			})
			if got != tt.want {
				t.Fatalf("healthURL(%q) = %s, want %s", tt.listen, got, tt.want)
			}
		})
	}
}

// TestSuperviseRejectsBadArguments covers the argument handling that runs
// before any privileged startup work.
func TestSuperviseRejectsBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":  {"supervise", "--nope"},
		"positional":    {"supervise", "extra"},
		"missing value": {"supervise", "--mode"},
	} {
		if code := run(args, io.Discard); code != 2 {
			t.Errorf("%s: exit code %d, want 2", name, code)
		}
	}
}
