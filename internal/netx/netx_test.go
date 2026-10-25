package netx

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host       string
		wantHost   bool
		wantIPOnly bool
	}{
		{"127.0.0.1", true, true},
		{"127.0.0.2", true, true},
		{"::1", true, true},
		{"localhost", true, false},
		{"LocalHost", true, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"192.0.2.1", false, false},
		{"127.0.0.1.evil.test", false, false},
		{"localhost.evil.test", false, false},
		{"[::1]", false, false}, // brackets are not part of Hostname()
		{"", false, false},
	}
	for _, tt := range tests {
		if got := IsLoopbackHost(tt.host); got != tt.wantHost {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", tt.host, got, tt.wantHost)
		}
		if got := IsLoopbackIP(tt.host); got != tt.wantIPOnly {
			t.Errorf("IsLoopbackIP(%q) = %v, want %v", tt.host, got, tt.wantIPOnly)
		}
	}
}
