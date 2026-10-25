package supervisor

import (
	"errors"
	"testing"
)

const initStatusWithKill = `Name:	tini
Uid:	0	0	0	0
CapEff:	00000000a80425fb
`

const initStatusWithoutKill = `Name:	tini
Uid:	0	0	0	0
CapEff:	00000000a80425db
`

func TestStopSignalWarningDetectsTheMissingKillCapability(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		uid     int
		wantErr bool
	}{
		{"init keeps CAP_KILL", initStatusWithKill, 10000, false},
		{"init lacks CAP_KILL", initStatusWithoutKill, 10000, true},
		{"init shares the runtime uid", initStatusWithoutKill, 0, false},
		{"no capability line", "Name:\ttini\nUid:\t0\t0\t0\t0\n", 10000, true},
		{"garbage", "nonsense\n", 10000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := stopSignalWarning([]byte(tt.status), tt.uid)
			if tt.wantErr {
				if !errors.Is(err, ErrStopSignalUnreachable) {
					t.Fatalf("err = %v, want ErrStopSignalUnreachable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

func TestParseInitStatus(t *testing.T) {
	uid, caps, ok := parseInitStatus([]byte(initStatusWithKill))
	if !ok || uid != 0 || caps&(1<<capKill) == 0 {
		t.Fatalf("parse = %d %#x %v", uid, caps, ok)
	}
	if _, _, ok := parseInitStatus([]byte("Uid:\tx\t0\n")); ok {
		t.Fatal("a non-numeric uid must not parse")
	}
}
