package update

import (
	"encoding/json"
	"testing"
)

func TestSandboxCheckSeparatesDisabledFromUnverified(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name     string
		enabled  *bool
		verified bool
		status   string
		blocks   bool
	}{
		{"enabled and verified", &on, true, CheckPass, false},
		{"enabled but unverified fails closed", &on, false, CheckFail, true},
		{"older report, verified", nil, true, CheckPass, false},
		{"older report, unverified fails closed", nil, false, CheckFail, true},
		{"explicitly disabled", &off, false, CheckSkip, false},
		{"disabled but claims verified is inconsistent", &off, true, CheckFail, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sandboxCheck(tc.enabled, tc.verified)
			if c.Status != tc.status {
				t.Fatalf("status = %q (%s), want %q", c.Status, c.Detail, tc.status)
			}
			if c.blocks() != tc.blocks {
				t.Fatalf("blocks = %t, want %t", c.blocks(), tc.blocks)
			}
		})
	}
}

func TestOnlyTheDisabledSandboxSkipIsAccepted(t *testing.T) {
	for _, c := range []Check{
		{ID: "sandbox", Status: CheckSkip, Detail: "runtime is not ready"},
		{ID: "fonts", Status: CheckSkip, Reason: sandboxDisabledReason},
		{ID: "cdp", Status: CheckSkip, Detail: "runtime is not ready"},
		{ID: "sandbox", Status: CheckFail, Reason: sandboxDisabledReason},
		{ID: "sandbox", Status: CheckSkip, Detail: "Chromium sandbox not enabled (LENSDOCK_CHROMIUM_SANDBOX is off)"},
		{ID: "sandbox", Status: CheckSkip, Reason: "unknown"},
	} {
		if !c.blocks() {
			t.Errorf("%+v must block the candidate", c)
		}
	}
	var rep VerifyReport
	rep.add("ready", "ready", CheckPass, "")
	off := false
	rep.Checks = append(rep.Checks, sandboxCheck(&off, false))
	rep.finish()
	if !rep.OK {
		t.Fatal("a report whose only non-pass check is the disabled sandbox must be OK")
	}
	rep = VerifyReport{}
	rep.add("sandbox", "renderer sandbox verified", CheckSkip, "runtime is not ready")
	rep.finish()
	if rep.OK {
		t.Fatal("a skipped sandbox of a runtime that is not ready must not be OK")
	}
}

func TestReadyDocSandboxEnabledIsOptional(t *testing.T) {
	var missing, disabled, enabled readyDoc
	for doc, body := range map[*readyDoc]string{
		&missing:  `{"detail":{"sandbox":{"verified":false}}}`,
		&disabled: `{"detail":{"sandbox":{"enabled":false,"verified":false}}}`,
		&enabled:  `{"detail":{"sandbox":{"enabled":true,"verified":true}}}`,
	} {
		if err := json.Unmarshal([]byte(body), doc); err != nil {
			t.Fatal(err)
		}
	}
	if missing.Detail.Sandbox.Enabled != nil {
		t.Error("a report without enabled must decode to nil, not false")
	}
	if e := disabled.Detail.Sandbox.Enabled; e == nil || *e {
		t.Error("enabled=false must decode to an explicit false")
	}
	if e := enabled.Detail.Sandbox.Enabled; e == nil || !*e {
		t.Error("enabled=true must decode to an explicit true")
	}
}

func TestDisabledSandboxAdmissionDoesNotDependOnDiagnosticText(t *testing.T) {
	off := false
	report := VerifyReport{Checks: []Check{sandboxCheck(&off, false)}}
	report.Checks[0].Detail = "Chromium sandbox disabled by configuration"
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var received VerifyReport
	if err := json.Unmarshal(data, &received); err != nil {
		t.Fatal(err)
	}
	received.finish()
	if !received.OK || received.Checks[0].blocks() {
		t.Fatal("diagnostic wording changed whether the candidate can proceed")
	}
}
