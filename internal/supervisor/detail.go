package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	procRoot        = "/proc"
	fcMatchTimeout  = 5 * time.Second
	maxManifestSize = 4 << 20
)

// UnitDetail is the per-unit runtime snapshot. CPU ticks and RSS are summed
// over the unit's process group and its descendants.
type UnitDetail struct {
	Name     string `json:"name"`
	PID      int    `json:"pid"`
	State    string `json:"state"`
	Restarts int    `json:"restarts"`
	CPUTicks uint64 `json:"cpuTicks"`
	RSSBytes uint64 `json:"rssBytes"`
}

// Detail is the diagnostic snapshot exposed by /readyz?detail=1.
type Detail struct {
	Units           []UnitDetail      `json:"units"`
	ChromiumVersion string            `json:"chromiumVersion"`
	Sandbox         SandboxReport     `json:"sandbox"`
	FcMatch         map[string]string `json:"fcMatch"`
	Packages        map[string]string `json:"packages"`
}

// fontQueries maps the reported key to the fontconfig pattern. The family
// returned for each proves the CJK and emoji fonts are resolvable.
var fontQueries = []string{
	"sans-serif:lang=zh-tw",
	"sans-serif:lang=zh-cn",
	"sans-serif:lang=ja",
	"sans-serif:lang=ko",
	"emoji",
}

// resourceUsage sums CPU ticks and resident bytes over the group led by pid.
func resourceUsage(table []procStat, pid, pageSize int) (cpuTicks, rssBytes uint64) {
	for _, p := range groupMembers(table, pid) {
		cpuTicks += p.UTime + p.STime
		if p.RSSPages > 0 {
			rssBytes += uint64(p.RSSPages) * uint64(pageSize)
		}
	}
	return cpuTicks, rssBytes
}

// parseManifest parses dpkg-query output (package, version, architecture,
// tab separated) into package -> version. Malformed lines are skipped.
func parseManifest(data []byte) map[string]string {
	out := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 2 || fields[0] == "" || fields[1] == "" {
			continue
		}
		out[fields[0]] = fields[1]
	}
	return out
}

func readManifest() (map[string]string, error) {
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifestSize))
	if err != nil {
		return nil, err
	}
	return parseManifest(data), nil
}

// matchFonts runs fc-match for every font query. The boolean is true only if
// every query succeeded.
func matchFonts() (map[string]string, bool) {
	out := make(map[string]string, len(fontQueries))
	complete := true
	for _, pattern := range fontQueries {
		ctx, cancel := context.WithTimeout(context.Background(), fcMatchTimeout)
		cmd := exec.CommandContext(ctx, "/usr/bin/fc-match", "-f", "%{family[0]}", pattern)
		cmd.Env = childEnv()
		b, err := cmd.Output()
		cancel()
		family := strings.TrimSpace(string(b))
		if err != nil || family == "" {
			complete = false
			family = ""
		}
		out[pattern] = family
	}
	return out, complete
}
