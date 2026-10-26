package hostmanager

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	protocolVersion = 1
	maxRequestBody  = 1 << 20

	// DefaultHeartbeatTTL is how long a waiter stays alive without a heartbeat.
	DefaultHeartbeatTTL = 15 * time.Second
)

// options are the daemon timings and seams. Production uses defaultOptions.
type options struct {
	// interval is the scheduler period; zero disables the loop so tests drive
	// passes by hand.
	interval        time.Duration
	heartbeatTTL    time.Duration
	inventoryMaxAge time.Duration
	relaunchGrace   time.Duration
	maxAttempts     int
	refreshTimeout  time.Duration
	now             func() time.Time
	launch          func(ctx context.Context, cfg Config, opID string) error
}

func defaultOptions() options {
	return options{
		interval:        2 * time.Second,
		heartbeatTTL:    DefaultHeartbeatTTL,
		inventoryMaxAge: 10 * time.Second,
		relaunchGrace:   15 * time.Second,
		maxAttempts:     5,
		refreshTimeout:  45 * time.Second,
		now:             time.Now,
		launch:          spawnWorker,
	}
}

type principal struct {
	admin     bool
	projectID string
}

type server struct {
	root    string
	backend Backend
	opts    options
	log     *slog.Logger

	adminHash string

	passMu    sync.Mutex // serializes scheduler passes
	refreshMu sync.Mutex // serializes inventory refreshes

	mu      sync.Mutex // guards everything below; never held across Docker or network calls
	cfg     Config
	reg     *registry
	seen    map[string]time.Time
	reasons map[string]string
	usage   map[string]Resources
	usageOK map[string]bool
	inv     *Inventory
	invAt   time.Time
	invErr  error
	// invTried is the last inspection attempt; refreshedRev is the registry
	// revision at which the last full inspection began.
	invTried     time.Time
	refreshedRev uint64

	kickCh     chan struct{}
	shutdownCh chan struct{}
}

func newServer(root string, backend Backend, opts options, logger *slog.Logger) (*server, error) {
	cfg, err := LoadConfig(root)
	if err != nil {
		return nil, err
	}
	reg, err := loadRegistry(cfg.Root)
	if err != nil {
		return nil, err
	}
	token, err := readPrivateFile(AdminTokenFile(cfg.Root), 4096)
	if err != nil {
		return nil, fmt.Errorf("admin token: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s := &server{
		root:       cfg.Root,
		backend:    backend,
		opts:       opts,
		log:        logger,
		adminHash:  hashToken(strings.TrimSpace(string(token))),
		cfg:        cfg,
		reg:        reg,
		seen:       map[string]time.Time{},
		reasons:    map[string]string{},
		usage:      map[string]Resources{},
		usageOK:    map[string]bool{},
		kickCh:     make(chan struct{}, 1),
		shutdownCh: make(chan struct{}),
	}
	// Waiters survive a restart with a fresh heartbeat so a client that kept
	// polling through the outage is not dropped.
	now := opts.now()
	for id := range reg.Waiters {
		s.seen[id] = now
	}
	return s, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *server) now() time.Time { return s.opts.now() }

func (s *server) kick() {
	select {
	case s.kickCh <- struct{}{}:
	default:
	}
}

// commitLocked applies fn to a copy of the registry and persists it before it
// becomes visible. The caller holds s.mu. The copy already carries the new
// revision so fn can stamp it.
func (s *server) commitLocked(fn func(r *registry) error) error { return s.commit(fn, false) }

func (s *server) extraLocked() *viewExtra {
	return &viewExtra{reasons: s.reasons, usage: s.usage, usageOK: s.usageOK}
}

func (s *server) projectLocked(id string) (Project, bool) {
	rec := s.reg.Projects[id]
	if rec == nil {
		return Project{}, false
	}
	return buildProject(s.root, s.reg, rec, s.extraLocked()), true
}

// Serve runs the singleton manager on a private Unix socket until ctx ends.
// Stopping it never touches containers or workers.
func Serve(ctx context.Context, root string, backend Backend) error {
	return serve(ctx, root, backend, defaultOptions())
}

func serve(ctx context.Context, root string, backend Backend, opts options) error {
	cfg, err := LoadConfig(root)
	if err != nil {
		return err
	}
	lock, busy, err := tryFlock(filepath.Join(cfg.Root, serveLockName))
	if err != nil {
		return err
	}
	if busy {
		return errors.New("another manager already serves this root")
	}
	defer lock.Close()
	_ = lock.Truncate(0)
	_, _ = fmt.Fprintf(lock, "pid=%d\n", os.Getpid())

	s, err := newServer(cfg.Root, backend, opts, nil)
	if err != nil {
		return err
	}
	runDir := filepath.Join(cfg.Root, "run")
	if err := ensurePrivateDir(runDir); err != nil {
		return err
	}
	sock := socketPath(cfg.Root)
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 || !ownedByUs(fi) {
			return fmt.Errorf("%s exists and is not a manager socket", sock)
		}
		// The singleton lock proves no other manager owns this socket.
		if err := os.Remove(sock); err != nil {
			return err
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", sock)
	if err != nil {
		return fmt.Errorf("listen on manager socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	srv := &http.Server{
		Handler:           s.handler(),
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	loopCtx, stopLoop := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.loop(loopCtx)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	s.log.Info("manager ready", "id", cfg.ID, "pid", os.Getpid())

	var result error
	select {
	case <-ctx.Done():
	case <-s.shutdownCh:
	case result = <-serveErr:
	}
	stopLoop()
	cancelRequests()
	// The singleton lock must outlive every handler: callers may remove the
	// manager directory after Shutdown returns.
	_ = srv.Shutdown(context.Background())
	<-loopDone
	_ = os.Remove(sock)
	if errors.Is(result, http.ErrServerClosed) {
		result = nil
	}
	return result
}

type identityResponse struct {
	Protocol  int    `json:"protocol"`
	ManagerID string `json:"managerId"`
	Root      string `json:"root"`
	PID       int    `json:"pid"`
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/identity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		id := s.cfg.ID
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, identityResponse{Protocol: protocolVersion, ManagerID: id, Root: s.root, PID: os.Getpid()})
	})
	mux.HandleFunc("/v1/request", s.handleRequest)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rpcError is a refusal with an HTTP status and an optional partial response
// (for example the operation ID of a conflicting operation).
type rpcError struct {
	status int
	msg    string
	resp   Response
}

func (e *rpcError) Error() string { return e.msg }

func rpcErrf(status int, format string, args ...any) *rpcError {
	return &rpcError{status: status, msg: fmt.Sprintf(format, args...)}
}

func (s *server) authenticate(r *http.Request) (principal, bool) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" || len(token) > 512 {
		return principal{}, false
	}
	hash := hashToken(token)
	if tokensEqual(hash, s.adminHash) {
		return principal{admin: true}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.reg.Projects {
		if !p.Revoked && p.TokenHash != "" && tokensEqual(hash, p.TokenHash) {
			return principal{projectID: p.ID}, true
		}
	}
	return principal{}, false
}

func (s *server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, ok := s.authenticate(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, Response{Error: "invalid or missing credential"})
		return
	}
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, Response{Error: "invalid request body"})
		return
	}
	resp, err := s.dispatch(r.Context(), p, req)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			out := re.resp
			out.Error = re.msg
			writeJSON(w, re.status, out)
			return
		}
		s.log.Error("request failed", "action", req.Action, "error", err)
		writeJSON(w, http.StatusInternalServerError, Response{Error: "internal error: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) dispatch(ctx context.Context, p principal, req Request) (Response, error) {
	switch req.Action {
	case ActionAuthorize, ActionEnroll, ActionList, ActionRelocate, ActionRevoke, ActionPolicy, ActionLimits, ActionRecover, ActionUpdate, ActionShutdown:
		if !p.admin {
			return Response{}, rpcErrf(http.StatusForbidden, "%s requires the Host Admin credential", req.Action)
		}
	case ActionEnsure, ActionStatus, ActionCancel, ActionHeartbeat, ActionStop, ActionConnect:
		switch {
		case p.admin:
		case req.ProjectID == "" || req.ProjectID == p.projectID:
			req.ProjectID = p.projectID
		default:
			return Response{}, rpcErrf(http.StatusForbidden, "this credential is scoped to another project")
		}
	default:
		return Response{}, rpcErrf(http.StatusBadRequest, "unknown action %q", req.Action)
	}
	select {
	case <-s.shutdownCh:
		if req.Action == ActionShutdown {
			return Response{}, nil
		}
		return Response{}, rpcErrf(http.StatusServiceUnavailable, "manager is shutting down")
	default:
	}
	needsProject := req.Action != ActionAuthorize && req.Action != ActionEnroll && req.Action != ActionList && req.Action != ActionPolicy && req.Action != ActionShutdown
	if needsProject && !projectPattern.MatchString(req.ProjectID) {
		return Response{}, rpcErrf(http.StatusBadRequest, "a valid projectId is required")
	}
	switch req.Action {
	case ActionShutdown:
		return s.requestShutdown()
	case ActionAuthorize, ActionEnroll:
		return s.authorize(req.Path, req.Action == ActionEnroll)
	case ActionList:
		return s.list(), nil
	case ActionRelocate:
		return s.relocate(req.ProjectID, req.Path)
	case ActionRevoke:
		return s.revoke(req.ProjectID)
	case ActionPolicy:
		return s.setPolicy(req.Policy)
	case ActionLimits:
		return s.limits(ctx, req.ProjectID, req.Limits, req.ExpectedLimits)
	case ActionRecover:
		return s.recoverOp(req.ProjectID, req.OperationID)
	case ActionEnsure:
		return s.ensure(ctx, req.ProjectID, req.Deadline)
	case ActionStatus:
		return s.status(req.ProjectID, req.OperationID, p)
	case ActionCancel:
		return s.cancelWaiter(req.ProjectID, req.WaiterID)
	case ActionHeartbeat:
		return s.heartbeat(req.ProjectID, req.WaiterID)
	case ActionStop:
		return s.stop(ctx, p, req)
	case ActionUpdate:
		return s.update(ctx, req)
	case ActionConnect:
		return s.connect(ctx, req.ProjectID)
	}
	return Response{}, rpcErrf(http.StatusBadRequest, "unknown action %q", req.Action)
}

// loop runs scheduler passes on a timer and whenever a request kicks it.
func (s *server) loop(ctx context.Context) {
	if s.opts.interval <= 0 {
		return
	}
	ticker := time.NewTicker(s.opts.interval)
	defer ticker.Stop()
	for {
		s.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.kickCh:
		}
	}
}
