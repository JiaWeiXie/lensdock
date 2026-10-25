package gateway

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
)

type storageUsage struct {
	Bytes    int64 `json:"bytes"`
	Files    int64 `json:"files"`
	Complete bool  `json:"complete"`
}

func usage(root string) storageUsage {
	result := storageUsage{Complete: true}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() {
			result.Files++
			result.Bytes += st.Size()
		}
		return nil
	})
	if err != nil {
		result.Complete = false
	}
	return result
}
func (g *Gateway) system(w http.ResponseWriter, r *http.Request, p Identity) {
	if !human(p) {
		fail(w, 403, "FORBIDDEN", "human diagnostics access required")
		return
	}
	revision := "unknown"
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
			if s.Key == "vcs.modified" {
				dirty = s.Value == "true"
			}
		}
	}
	status := map[string]any{"release": map[string]any{"id": "development", "verified": false}, "update": map[string]any{"phase": "not-configured", "lastBackupAt": nil}}
	if g.config.StatusFile != "" {
		f, err := os.Open(g.config.StatusFile)
		if err == nil {
			b, e := io.ReadAll(io.LimitReader(f, 64<<10))
			f.Close()
			if e == nil && len(b) < 64<<10 {
				var persisted map[string]json.RawMessage
				if json.Unmarshal(b, &persisted) == nil {
					for _, key := range []string{"release", "update"} {
						if v, ok := persisted[key]; ok {
							status[key] = v
						}
					}
				}
			}
		}
	}
	status["gateway"] = map[string]any{"version": "development", "revision": revision, "dirty": dirty, "goVersion": runtime.Version(), "schema": g.schema}
	status["storage"] = map[string]storageUsage{"database": usage(filepath.Join(g.config.DataDir, "db")), "private": usage(filepath.Join(g.config.DataDir, "private")), "artifacts": usage(filepath.Join(g.config.DataDir, "artifacts"))}
	if g.integrations.FeedbackStats != nil {
		stats, err := g.integrations.FeedbackStats(r.Context(), p)
		if err != nil {
			fail(w, 503, "UNAVAILABLE", "feedback statistics unavailable")
			return
		}
		status["feedback"] = stats
	}
	jsonReply(w, 200, status)
}
