package main

import "testing"

func TestDemoNoAuthIsOffUnlessExplicit(t *testing.T) {
	for _, tc := range []struct {
		flag    bool
		env     string
		want    bool
		wantErr bool
	}{
		{false, "", false, false},
		{false, "0", false, false},
		{true, "", true, false},
		{true, "0", true, false},
		{false, "1", true, false},
		{false, "true", false, true},
		{false, "yes", false, true},
	} {
		got, err := demoNoAuth(tc.flag, tc.env)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Fatalf("flag=%v env=%q: got %v, %v", tc.flag, tc.env, got, err)
		}
	}
}
