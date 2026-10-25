package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "db", "lensdock.db"), PrivateDir: filepath.Join(dir, "private"), ArtifactDir: filepath.Join(dir, "artifacts")}
	st, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, cfg
}

func TestOpenCreatesPrivateLayoutAndReopens(t *testing.T) {
	st, cfg := openStore(t)
	for _, p := range []string{cfg.PrivateDir, cfg.ArtifactDir, filepath.Dir(cfg.DBPath)} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v mode %v", p, err, info)
		}
	}
	info, err := os.Stat(cfg.DBPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode: %v %v", err, info)
	}
	ctx := context.Background()
	if _, err := st.DB.SQL().ExecContext(ctx, `CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.SQL().ExecContext(ctx, `INSERT INTO t VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var v string
	if err := st2.DB.SQL().QueryRowContext(ctx, `SELECT v FROM t`).Scan(&v); err != nil || v != "kept" {
		t.Fatalf("persisted value = %q, %v", v, err)
	}
	versions, err := SchemaVersions(ctx, st2.DB)
	if err != nil || versions["storage"] != 1 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
}

func TestMigrateIsIdempotentAndRefusesNewerSchema(t *testing.T) {
	st, _ := openStore(t)
	ctx := context.Background()
	ms := []Migration{
		{Version: 1, Name: "a", Statements: []string{`CREATE TABLE m1 (id INTEGER)`}},
		{Version: 2, Name: "b", Statements: []string{`CREATE TABLE m2 (id INTEGER)`}},
	}
	if err := st.DB.Migrate(ctx, "demo", ms); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Migrate(ctx, "demo", ms); err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}
	if err := st.DB.Migrate(ctx, "demo", ms[:1]); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("an older binary must refuse a newer schema, got %v", err)
	}
	// A failing migration rolls back completely.
	bad := []Migration{{Version: 1, Name: "bad", Statements: []string{`CREATE TABLE ok1 (id INTEGER)`, `CREATE TABLE ok1 (id INTEGER)`}}}
	if err := st.DB.Migrate(ctx, "broken", bad); err == nil {
		t.Fatal("broken migration succeeded")
	}
	if _, err := st.DB.SQL().ExecContext(ctx, `SELECT * FROM ok1`); err == nil {
		t.Fatal("failed migration was not rolled back")
	}
}

func TestBackupProducesIndependentCopy(t *testing.T) {
	st, cfg := openStore(t)
	ctx := context.Background()
	dest := filepath.Join(filepath.Dir(cfg.DBPath), "backup.db")
	if err := st.DB.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Backup(ctx, dest); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	copyDB, err := OpenDB(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	versions, err := SchemaVersions(ctx, copyDB)
	if err != nil || versions["storage"] != 1 {
		t.Fatalf("backup versions = %v, %v", versions, err)
	}
	if err := st.DB.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEventsAreAppendOnlyAndScoped(t *testing.T) {
	st, _ := openStore(t)
	ctx := context.Background()
	now := time.Now()
	add := func(e Event) {
		t.Helper()
		e.WorkspaceID, e.ActorRole, e.Category, e.Result, e.SubjectType, e.SubjectID = "ws1", "human", "feedback", "ok", "feedback", "fb1"
		if e.ActorID == "" {
			e.ActorID = "u1"
		}
		if _, err := st.Events.Append(ctx, st.DB.SQL(), e, now); err != nil {
			t.Fatal(err)
		}
	}
	add(Event{Action: "a.private", Visibility: VisPrivate, ActorID: "u1"})
	add(Event{Action: "b.workspace", Visibility: VisWorkspace})
	add(Event{Action: "c.assigned", Visibility: VisAssigned, AgentID: "agent-1"})
	add(Event{Action: "d.admin", Visibility: VisAdmin})
	add(Event{Action: "e.private-other", Visibility: VisPrivate, ActorID: "u2"})

	actions := func(q EventQuery) []string {
		evs, err := st.Events.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			out = append(out, e.Action)
		}
		return out
	}
	if got := actions(EventQuery{WorkspaceID: "ws1", Scope: ScopeAll}); len(got) != 5 {
		t.Fatalf("admin = %v", got)
	}
	if got := strings.Join(actions(EventQuery{WorkspaceID: "ws1", Scope: ScopeMember, PrincipalID: "u1"}), ","); got != "a.private,b.workspace,c.assigned" {
		t.Fatalf("member = %s", got)
	}
	if got := strings.Join(actions(EventQuery{WorkspaceID: "ws1", Scope: ScopeAssigned, PrincipalID: "agent-1"}), ","); got != "c.assigned" {
		t.Fatalf("agent-1 = %s", got)
	}
	if got := actions(EventQuery{WorkspaceID: "ws1", Scope: ScopeAssigned, PrincipalID: "agent-2"}); len(got) != 0 {
		t.Fatalf("agent-2 = %v", got)
	}
	if got := actions(EventQuery{WorkspaceID: "other", Scope: ScopeAll}); len(got) != 0 {
		t.Fatalf("other workspace = %v", got)
	}
	if got := actions(EventQuery{WorkspaceID: "ws1", Scope: ScopeAll, AfterSeq: 4}); len(got) != 1 {
		t.Fatalf("after = %v", got)
	}
	if _, err := st.Events.Query(ctx, EventQuery{Scope: "bogus"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	// Rows cannot be rewritten or removed.
	if _, err := st.DB.SQL().ExecContext(ctx, `UPDATE events SET action = 'x'`); err == nil {
		t.Fatal("event rewritten")
	}
	if _, err := st.DB.SQL().ExecContext(ctx, `DELETE FROM events`); err == nil {
		t.Fatal("event deleted")
	}
	// Invalid or secret-looking structure is refused; detail is capped and cleaned.
	if _, err := st.Events.Append(ctx, st.DB.SQL(), Event{WorkspaceID: "ws1", ActorID: "has space", ActorRole: "human", Category: "x", Action: "y", Result: "ok", SubjectType: "s", SubjectID: "i", Visibility: VisAdmin}, now); err == nil {
		t.Fatal("invalid actor accepted")
	}
	if _, err := st.Events.Append(ctx, st.DB.SQL(), Event{WorkspaceID: "ws1", ActorID: "a", ActorRole: "human", Category: "x", Action: "y", Result: "ok", SubjectType: "s", SubjectID: "i", Visibility: VisAssigned}, now); err == nil {
		t.Fatal("assigned event without agent accepted")
	}
	if _, err := st.Events.Append(ctx, st.DB.SQL(), Event{WorkspaceID: "ws1", ActorID: "a", ActorRole: "human", Category: "x", Action: "y", Result: "ok", SubjectType: "s", SubjectID: "i", Visibility: VisAdmin,
		Detail: strings.Repeat("d\n", 500)}, now); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Events.Query(ctx, EventQuery{WorkspaceID: "ws1", Scope: ScopeAll, AfterSeq: 5})
	if len(evs) != 1 || len(evs[0].Detail) != maxDetail || strings.ContainsRune(evs[0].Detail, '\n') {
		t.Fatalf("detail = %q", evs[0].Detail)
	}
}

func TestEventWait(t *testing.T) {
	st, _ := openStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- st.Events.Wait(ctx, 0) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := st.Events.Append(ctx, st.DB.SQL(), Event{WorkspaceID: "w", ActorID: "a", ActorRole: "human", Category: "c", Action: "x", Result: "ok", SubjectType: "s", SubjectID: "i", Visibility: VisWorkspace}, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Events.Wake()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Wake")
	}
	// A cancelled context ends the wait.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if err := st.Events.Wait(cctx, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v", err)
	}
}

func TestIdempotencyRecords(t *testing.T) {
	st, _ := openStore(t)
	ctx := context.Background()
	now := time.Now()
	if rec, err := IdemLookup(ctx, st.DB.SQL(), "s", "k"); err != nil || rec != nil {
		t.Fatalf("lookup = %v, %v", rec, err)
	}
	if err := IdemStore(ctx, st.DB.SQL(), "s", "k", "fp", []byte(`{"a":1}`), now); err != nil {
		t.Fatal(err)
	}
	if err := IdemStore(ctx, st.DB.SQL(), "s", "k", "fp", []byte(`{}`), now); err == nil {
		t.Fatal("duplicate idempotency key stored twice")
	}
	rec, err := IdemLookup(ctx, st.DB.SQL(), "s", "k")
	if err != nil || rec == nil || rec.Fingerprint != "fp" || string(rec.Response) != `{"a":1}` {
		t.Fatalf("lookup = %+v, %v", rec, err)
	}
	if n, err := IdemPurge(ctx, st.DB.SQL(), now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
}

func TestObjectsRejectUnsafeKeysAndAreAtomic(t *testing.T) {
	st, cfg := openStore(t)
	for _, k := range []string{"", "../x", "a/b", "A", ".hidden", "x y", strings.Repeat("a", 129), "..", `a\b`} {
		if _, err := st.Private.Put(k, []byte("x")); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Put(%q) = %v", k, err)
		}
		if _, err := st.Private.Read(k, 10); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Read(%q) = %v", k, err)
		}
		if err := st.Private.Delete(k); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Delete(%q) = %v", k, err)
		}
	}
	sum, err := st.Private.Put("obj-1", []byte("hello"))
	if err != nil || sum != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("Put = %q, %v", sum, err)
	}
	info, _ := os.Stat(filepath.Join(cfg.PrivateDir, "obj-1"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("object mode = %v", info.Mode())
	}
	if _, err := st.Private.Read("obj-1", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read over limit = %v", err)
	}
	if _, err := st.Private.Read("missing", 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read missing = %v", err)
	}
	// A symlink planted in the directory is never followed.
	secret := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(cfg.PrivateDir, "obj-link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := st.Private.Read("obj-link", 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("symlink read = %v", err)
	}
	// No temporary files are left behind and listing ignores them.
	objs, err := st.Private.List()
	if err != nil || len(objs) != 1 || objs[0].Key != "obj-1" {
		t.Fatalf("List = %+v, %v", objs, err)
	}
	if err := st.Private.Delete("obj-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Private.Delete("obj-1"); err != nil {
		t.Fatalf("deleting a missing object must not fail: %v", err)
	}
}

func TestArtifactsAreImmutableAndNamesChecked(t *testing.T) {
	st, cfg := openStore(t)
	files := []File{{Name: "feedback.json", Data: []byte("{}")}, {Name: "screenshot.redacted.png", Data: []byte("png")}}
	if err := st.Artifacts.Publish("fb_1", 1, files); err != nil {
		t.Fatal(err)
	}
	if err := st.Artifacts.Publish("fb_1", 1, files); !errors.Is(err, ErrExists) {
		t.Fatalf("second publish = %v", err)
	}
	data, err := st.Artifacts.Read("fb_1", 1, "feedback.json", 100)
	if err != nil || string(data) != "{}" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	info, _ := os.Stat(filepath.Join(cfg.ArtifactDir, "fb_1", "r1", "feedback.json"))
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("artifact mode = %v", info.Mode())
	}
	for _, name := range []string{"../feedback.json", "a/b", ".x", "", "..", "a..b", "A.json"} {
		if err := st.Artifacts.Publish("fb_2", 1, []File{{Name: name, Data: []byte("x")}}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Publish(%q) = %v", name, err)
		}
		if _, err := st.Artifacts.Read("fb_1", 1, name, 10); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Read(%q) = %v", name, err)
		}
	}
	if err := st.Artifacts.Publish("fb_3", 1, []File{{Name: "a.json", Data: nil}, {Name: "a.json", Data: nil}}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("duplicate names = %v", err)
	}
	for _, id := range []string{"../fb", "", "a/b", "FB"} {
		if err := st.Artifacts.Publish(id, 1, files); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Publish id %q = %v", id, err)
		}
	}
	if err := st.Artifacts.Publish("fb_4", 0, files); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("revision 0 = %v", err)
	}
	// A failed publish leaves no staging directory.
	dirs, err := st.Artifacts.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if d.Staging {
			t.Fatalf("staging directory left behind: %+v", d)
		}
	}
	if len(dirs) != 1 || dirs[0].FeedbackID != "fb_1" || dirs[0].Revision != 1 {
		t.Fatalf("List = %+v", dirs)
	}
	if err := st.Artifacts.Remove("fb_1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Artifacts.Read("fb_1", 1, "feedback.json", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read after remove = %v", err)
	}
}
