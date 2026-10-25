package apiwire

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		body string
		max  int64
		want error
	}{
		{"valid", `{"mode":"AGENT","ttlSeconds":30}`, 1024, nil},
		{"valid with trailing whitespace", `{"mode":"AGENT"}` + " \n", 1024, nil},
		{"exactly at limit", `{"mode":"AGENT"}`, int64(len(`{"mode":"AGENT"}`)), nil},
		{"one over limit", `{"mode":"AGENT"}`, int64(len(`{"mode":"AGENT"}`)) - 1, ErrBodyTooLarge},
		{"unknown field", `{"mode":"AGENT","extra":1}`, 1024, ErrInvalidJSON},
		{"wrong type", `{"mode":1}`, 1024, ErrInvalidJSON},
		{"empty body", ``, 1024, ErrInvalidJSON},
		{"truncated", `{"mode":"AGENT"`, 1024, ErrInvalidJSON},
		{"two values", `{"mode":"AGENT"}{"mode":"HUMAN"}`, 1024, ErrInvalidJSON},
		{"trailing garbage", `{"mode":"AGENT"} x`, 1024, ErrInvalidJSON},
		{"invalid utf-8", "{\"mode\":\"\xff\"}", 1024, ErrInvalidUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v AcquireLease
			err := Decode(strings.NewReader(tt.body), tt.max, &v)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && v.Mode != "AGENT" {
				t.Fatalf("decoded mode = %q", v.Mode)
			}
		})
	}
}

func TestDecodeSizeCapDoesNotReadUnboundedInput(t *testing.T) {
	r := io.LimitReader(zeroReader{}, 1<<30)
	var v AcquireLease
	if err := Decode(r, 16, &v); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

func TestDecodeReadErrorIsNotAClientError(t *testing.T) {
	var v AcquireLease
	boom := errors.New("boom")
	err := Decode(iotest.ErrReader(boom), 1024, &v)
	if !errors.Is(err, boom) || errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("err = %v, want wrapped read error only", err)
	}
}

func TestWorkspaceHolderIDIsExplicitNull(t *testing.T) {
	b, err := json.Marshal(Workspace{ID: "default", RuntimeGeneration: "g", ControlState: "IDLE", Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"holderId":null`) {
		t.Fatalf("holderId must be present as null: %s", b)
	}
	holder := "operator"
	b, err = json.Marshal(Workspace{HolderID: &holder})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"holderId":"operator"`) {
		t.Fatalf("holderId missing: %s", b)
	}
}

func TestLeaseWireFormat(t *testing.T) {
	l := Lease{
		LeaseID: "lse_1", WorkspaceID: "default", HolderID: "operator", Mode: "AGENT",
		Epoch: 7, RuntimeGeneration: "g", ExpiresAt: time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC),
	}
	b, err := json.Marshal(LeaseGrant{Lease: l, CapabilityToken: "ldl_x"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"lease":{"leaseId":"lse_1","workspaceId":"default","holderId":"operator","mode":"AGENT","epoch":7,"runtimeGeneration":"g","expiresAt":"2026-10-24T01:02:03Z"},"capabilityToken":"ldl_x"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	var back LeaseGrant
	if err := Decode(strings.NewReader(want), 4096, &back); err != nil || back != (LeaseGrant{Lease: l, CapabilityToken: "ldl_x"}) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

func TestAcquireLeaseOmitsAbsentTTL(t *testing.T) {
	b, err := json.Marshal(AcquireLease{Mode: "HUMAN"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"mode":"HUMAN"}` {
		t.Fatalf("got %s", b)
	}
}
