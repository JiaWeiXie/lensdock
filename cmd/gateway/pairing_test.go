package main

import "testing"

func TestPairingEnvironmentOverridesConfiguration(t *testing.T) {
	for _, tc := range []struct {
		configured bool
		env        string
		want       bool
		wantErr    bool
	}{
		{true, "", true, false},
		{true, "0", false, false},
		{true, "false", false, false},
		{false, "1", true, false},
		{false, "true", true, false},
		{true, "invalid", false, true},
	} {
		got, err := pairingRequired(tc.configured, tc.env)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Fatalf("configured=%v env=%q: got %v, %v", tc.configured, tc.env, got, err)
		}
	}
}
