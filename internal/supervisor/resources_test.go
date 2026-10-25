package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCgroup(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadResourcesParsesCgroupV2AndDisk(t *testing.T) {
	cg, vol := t.TempDir(), t.TempDir()
	writeCgroup(t, cg, map[string]string{
		"cpu.stat":       "usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n",
		"cpu.max":        "400000 100000\n",
		"memory.current": "123456789\n",
		"memory.max":     "4294967296\n",
	})
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("x", 3600))
	r := ReadResources(now, cg, []NamedPath{{"profile", vol}, {"missing", filepath.Join(vol, "nope")}})

	if !r.SampledAt.Equal(now) || r.SampledAt.Location() != time.UTC {
		t.Fatalf("sampledAt = %v", r.SampledAt)
	}
	if r.CPU.UsageNanos == nil || *r.CPU.UsageNanos != 2_500_000_000 {
		t.Fatalf("cpu usage = %v, want 2.5e9 ns", r.CPU.UsageNanos)
	}
	if r.CPU.Cores == nil || *r.CPU.Cores != 4 {
		t.Fatalf("cores = %v", r.CPU.Cores)
	}
	if r.Memory.CurrentBytes == nil || *r.Memory.CurrentBytes != 123456789 || r.Memory.MaxBytes == nil || *r.Memory.MaxBytes != 4<<30 {
		t.Fatalf("memory = %+v", r.Memory)
	}
	if len(r.Disk) != 2 || r.Disk[0].Name != "profile" || r.Disk[0].TotalBytes == nil || r.Disk[0].AvailableBytes == nil || *r.Disk[0].TotalBytes == 0 {
		t.Fatalf("disk = %+v", r.Disk)
	}
	if *r.Disk[0].AvailableBytes > *r.Disk[0].TotalBytes {
		t.Fatalf("available %d exceeds total %d", *r.Disk[0].AvailableBytes, *r.Disk[0].TotalBytes)
	}
	if d := r.Disk[1]; d.Name != "missing" || d.TotalBytes != nil || d.AvailableBytes != nil {
		t.Fatalf("an unreadable volume must report nulls, got %+v", d)
	}
}

func TestReadResourcesReportsUnknownAsNullNotZero(t *testing.T) {
	cg := t.TempDir() // no cgroup files at all
	r := ReadResources(time.Now(), cg, nil)
	if r.CPU.UsageNanos != nil || r.CPU.Cores != nil || r.Memory.CurrentBytes != nil || r.Memory.MaxBytes != nil {
		t.Fatalf("resources = %+v, want every field null", r)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"usageNanos":null`, `"cores":null`, `"currentBytes":null`, `"maxBytes":null`, `"disk":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("json %s lacks %s", raw, want)
		}
	}
}

func TestReadResourcesTreatsUnlimitedAndGarbageAsNull(t *testing.T) {
	cg := t.TempDir()
	writeCgroup(t, cg, map[string]string{
		"cpu.max":        "max 100000\n",
		"memory.max":     "max\n",
		"memory.current": "lots\n",
		"cpu.stat":       "nr_periods 3\n",
	})
	r := ReadResources(time.Now(), cg, nil)
	if r.CPU.Cores != nil || r.Memory.MaxBytes != nil || r.Memory.CurrentBytes != nil || r.CPU.UsageNanos != nil {
		t.Fatalf("resources = %+v", r)
	}
	writeCgroup(t, cg, map[string]string{"cpu.max": "0 100000\n"})
	if r := ReadResources(time.Now(), cg, nil); r.CPU.Cores != nil {
		t.Fatalf("a zero quota must be unknown, got %v", *r.CPU.Cores)
	}
}
