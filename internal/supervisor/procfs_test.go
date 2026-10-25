package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseProcStatusRenderer(t *testing.T) {
	st, err := parseProcStatus(readFixture(t, "status_renderer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Seccomp != 2 {
		t.Errorf("Seccomp = %d, want 2", st.Seccomp)
	}
	if !reflect.DeepEqual(st.NSpid, []int{4300, 7}) {
		t.Errorf("NSpid = %v, want [4300 7]", st.NSpid)
	}
	if st.NoNewPrivs != 1 {
		t.Errorf("NoNewPrivs = %d, want 1", st.NoNewPrivs)
	}
	if !reflect.DeepEqual(st.UIDs, []uint32{10001, 10001, 10001, 10001}) {
		t.Errorf("UIDs = %v", st.UIDs)
	}
	if len(st.Groups) != 0 {
		t.Errorf("Groups = %v, want none", st.Groups)
	}
	if st.CapEff != 0 || st.CapBnd != 0 {
		t.Errorf("caps = %x/%x, want 0", st.CapEff, st.CapBnd)
	}
}

func TestParseProcStatusCapabilitiesAndMissingSeccomp(t *testing.T) {
	st, err := parseProcStatus(readFixture(t, "status_supervisor.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.CapEff != 0xc0 || st.CapPrm != 0xc0 || st.CapBnd != 0xc0 {
		t.Errorf("caps eff/prm/bnd = %x/%x/%x, want c0", st.CapEff, st.CapPrm, st.CapBnd)
	}

	st, err = parseProcStatus([]byte("Name:\tx\nUid:\t1\t1\t1\t1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Seccomp != -1 {
		t.Errorf("absent Seccomp = %d, want -1", st.Seccomp)
	}
}

func TestParseProcStatusRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"bad cap":      "CapEff:\tzz\n",
		"bad seccomp":  "Seccomp:\tx\n",
		"bad nspid":    "NSpid:\t1\tq\n",
		"no key value": "garbage without colon",
	} {
		if _, err := parseProcStatus([]byte(in)); err == nil {
			t.Errorf("%s: parseProcStatus succeeded", name)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	st, err := parseProcStat(readFixture(t, "stat_chromium.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := procStat{PID: 4242, Comm: "Chromium (sandbox) x", PPID: 4100, PGRP: 4243, UTime: 150, STime: 75, RSSPages: 25000}
	if st != want {
		t.Fatalf("parseProcStat = %+v, want %+v", st, want)
	}
}

func TestParseProcStatRejectsTruncated(t *testing.T) {
	for name, in := range map[string]string{
		"no parens":    "12 comm S 1 1",
		"short fields": "12 (x) S 1 2 3 0 -1",
		"bad pid":      "x (x) S 1 2 3 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 1 1 1",
	} {
		if _, err := parseProcStat([]byte(in)); err == nil {
			t.Errorf("%s: parseProcStat succeeded", name)
		}
	}
}

func TestResourceUsageSumsGroupAndDescendants(t *testing.T) {
	table := []procStat{
		{PID: 10, PPID: 1, PGRP: 10, UTime: 100, STime: 50, RSSPages: 10},
		{PID: 11, PPID: 10, PGRP: 10, UTime: 10, STime: 5, RSSPages: 20},
		{PID: 12, PPID: 11, PGRP: 12, UTime: 1, STime: 1, RSSPages: 30}, // left the group
		{PID: 99, PPID: 1, PGRP: 99, UTime: 7777, STime: 7777, RSSPages: 7777},
	}
	ticks, rss := resourceUsage(table, 10, 4096)
	if ticks != 167 {
		t.Errorf("cpu ticks = %d, want 167", ticks)
	}
	if rss != 60*4096 {
		t.Errorf("rss = %d, want %d", rss, 60*4096)
	}
}

type fakeProc struct {
	pid, ppid, pgrp int
	args            []string
	seccomp         int
	filters         int
	nspid           []int
	proctitle       bool // argv rewritten by setproctitle: space-joined, one NUL
}

func writeFakeProc(t *testing.T, root string, procs []fakeProc) {
	t.Helper()
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (chrome) S %d %d %d 0 -1 0 0 0 0 0 10 5 0 0 20 0 1 0 100 1000 40", p.pid, p.ppid, p.pgrp, p.pgrp)
		nspid := make([]string, len(p.nspid))
		for i, n := range p.nspid {
			nspid[i] = fmt.Sprint(n)
		}
		status := fmt.Sprintf("Name:\tchrome\nNSpid:\t%s\nNoNewPrivs:\t1\nCapEff:\t0000000000000000\nSeccomp:\t%d\nSeccomp_filters:\t%d\n", strings.Join(nspid, "\t"), p.seccomp, p.filters)
		sep := "\x00"
		if p.proctitle {
			sep = " "
		}
		cmdline := strings.Join(append([]string{"/usr/lib/chromium/chromium"}, p.args...), sep) + "\x00"
		for name, content := range map[string]string{"stat": stat, "status": status, "cmdline": cmdline} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestInspectSandboxVerifiesRenderers(t *testing.T) {
	root := t.TempDir()
	// Inside a container every process carries the runtime's filter (Seccomp 2,
	// one filter); sandboxed renderers carry Chromium's filter on top.
	writeFakeProc(t, root, []fakeProc{
		{pid: 100, ppid: 1, pgrp: 100, seccomp: 2, filters: 1, nspid: []int{100}},
		{pid: 101, ppid: 100, pgrp: 100, args: []string{"--type=zygote"}, seccomp: 2, filters: 1, nspid: []int{101, 1}},
		{pid: 102, ppid: 101, pgrp: 100, args: []string{"--type=renderer", "--lang=zh-TW"}, seccomp: 2, filters: 2, nspid: []int{102, 5}, proctitle: true},
		{pid: 103, ppid: 101, pgrp: 103, args: []string{"--type=renderer"}, seccomp: 2, filters: 2, nspid: []int{103, 6}},
		{pid: 200, ppid: 1, pgrp: 200, args: []string{"--type=renderer"}, seccomp: 0, nspid: []int{200}}, // unrelated
	})
	report, err := InspectSandbox(root, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatal("report not verified")
	}
	var pids []int
	types := map[int]string{}
	for _, p := range report.Processes {
		pids = append(pids, p.PID)
		types[p.PID] = p.Type
	}
	if !reflect.DeepEqual(pids, []int{100, 101, 102, 103}) {
		t.Fatalf("pids = %v, want [100 101 102 103]", pids)
	}
	if types[100] != "browser" || types[101] != "zygote" || types[102] != "renderer" {
		t.Fatalf("types = %v", types)
	}
}

func TestInspectSandboxRejections(t *testing.T) {
	tests := []struct {
		name    string
		procs   []fakeProc
		pgid    int
		wantErr string
	}{
		{
			name: "renderer without seccomp",
			procs: []fakeProc{
				{pid: 100, ppid: 1, pgrp: 100, nspid: []int{100}},
				{pid: 102, ppid: 100, pgrp: 100, args: []string{"--type=renderer"}, seccomp: 0, nspid: []int{102, 5}},
			},
			pgid: 100, wantErr: "seccomp",
		},
		{
			name: "renderer only under the container runtime filter",
			procs: []fakeProc{
				{pid: 100, ppid: 1, pgrp: 100, seccomp: 2, filters: 1, nspid: []int{100}},
				{pid: 102, ppid: 100, pgrp: 100, args: []string{"--type=renderer"}, seccomp: 2, filters: 1, nspid: []int{102, 5}},
			},
			pgid: 100, wantErr: "no Chromium seccomp filter",
		},
		{
			name: "renderer in the host PID namespace",
			procs: []fakeProc{
				{pid: 100, ppid: 1, pgrp: 100, nspid: []int{100}},
				{pid: 102, ppid: 100, pgrp: 100, args: []string{"--type=renderer"}, seccomp: 2, nspid: []int{102}},
			},
			pgid: 100, wantErr: "PID namespace",
		},
		{
			name: "no renderer yet",
			procs: []fakeProc{
				{pid: 100, ppid: 1, pgrp: 100, nspid: []int{100}},
			},
			pgid: 100, wantErr: "no renderer",
		},
		{
			name:    "unknown group",
			procs:   []fakeProc{{pid: 100, ppid: 1, pgrp: 100, nspid: []int{100}}},
			pgid:    555,
			wantErr: "no processes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFakeProc(t, root, tt.procs)
			report, err := InspectSandbox(root, tt.pgid)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if report.Verified {
				t.Fatal("failed inspection reported Verified")
			}
		})
	}
}

func TestVerifyDropped(t *testing.T) {
	good, err := parseProcStatus(readFixture(t, "status_supervisor.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyDropped(good); err != nil {
		t.Fatalf("good status rejected: %v", err)
	}

	mutations := map[string]func(*procStatus){
		"root uid":             func(s *procStatus) { s.UIDs = []uint32{0, 10000, 10000, 10000} },
		"supplementary group":  func(s *procStatus) { s.Groups = []uint32{0} },
		"extra effective cap":  func(s *procStatus) { s.CapEff |= 1 << 1 },
		"ambient cap":          func(s *procStatus) { s.CapAmb = 1 << capSetuid },
		"inheritable cap":      func(s *procStatus) { s.CapInh = 1 << capSetuid },
		"wide bounding set":    func(s *procStatus) { s.CapBnd |= 1 << 21 }, // CAP_SYS_ADMIN
		"missing no_new_privs": func(s *procStatus) { s.NoNewPrivs = 0 },
	}
	for name, mutate := range mutations {
		st := good
		mutate(&st)
		if err := verifyDropped(st); err == nil {
			t.Errorf("%s: verifyDropped accepted", name)
		}
	}
}
