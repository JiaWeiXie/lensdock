package update

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

// Probe operations. A probe is one request/response exchange with the runtime
// API. It runs either in the host process (the runtime API is reachable from
// the host) or in a throwaway sidecar that shares the runtime container's
// network namespace; both execute RunProbe, so the code path is identical.
const (
	ProbeStatus           = "status"
	ProbeMaintenanceEnter = "maintenance-enter"
	ProbeMaintenanceExit  = "maintenance-exit"
	ProbeRevokeLease      = "revoke-lease"
	ProbeVerifyCandidate  = "verify-candidate"
)

// Probe response codes.
const (
	CodeUnsupported = "unsupported"
	CodeUnreachable = "unreachable"
)

const maxProbeBody = 8 << 20

// ProbeTLS is a client certificate bundle for an mTLS runtime. It travels on
// stdin only.
type ProbeTLS struct {
	CAPEM      string `json:"caPem"`
	CertPEM    string `json:"certPem"`
	KeyPEM     string `json:"keyPem"`
	ServerName string `json:"serverName,omitempty"`
}

// ProbeRequest is the stdin document of `lensdock update __probe`. It carries
// credentials and is never logged.
type ProbeRequest struct {
	Op        string      `json:"op"`
	BaseURL   string      `json:"baseUrl"`
	Workspace string      `json:"workspace"`
	Token     string      `json:"token,omitempty"`
	TLS       *ProbeTLS   `json:"tls,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	LeaseID   string      `json:"leaseId,omitempty"`
	Verify    *VerifySpec `json:"verify,omitempty"`
	// IfIdle makes maintenance-enter conditional: the runtime refuses instead
	// of fencing a live lease. It requires ExpectedGeneration, the runtime
	// generation the caller observed.
	IfIdle             bool   `json:"ifIdle,omitempty"`
	ExpectedGeneration string `json:"expectedGeneration,omitempty"`
}

// RuntimeStatus is the operator view of a runtime (GET /readyz?detail=1).
type RuntimeStatus struct {
	Ready             bool   `json:"ready"`
	ControlState      string `json:"controlState"`
	HolderID          string `json:"holderId,omitempty"`
	LeaseID           string `json:"leaseId,omitempty"`
	LeaseMode         string `json:"leaseMode,omitempty"`
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
	Locked            bool   `json:"locked"`
	LockCode          string `json:"lockCode,omitempty"`
	ChromiumVersion   string `json:"chromiumVersion,omitempty"`
}

// LeaseHeld reports whether a control lease is active.
func (s RuntimeStatus) LeaseHeld() bool { return s.LeaseID != "" }

// MaintenanceResult is the answer of POST /maintenance.
type MaintenanceResult struct {
	State             string `json:"state"`
	Drained           bool   `json:"drained"`
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
}

// ProbeResponse is the stdout document of a probe. Failures of the operation
// are reported in Error (the process still exits 0), so a transport failure of
// the sidecar itself is distinguishable from a refusal by the runtime.
type ProbeResponse struct {
	Status      *RuntimeStatus     `json:"status,omitempty"`
	Maintenance *MaintenanceResult `json:"maintenance,omitempty"`
	Verify      *VerifyReport      `json:"verify,omitempty"`
	Error       string             `json:"error,omitempty"`
	Code        string             `json:"code,omitempty"`
}

// RunProbe reads one ProbeRequest from in, performs it and writes one
// ProbeResponse to out.
func RunProbe(ctx context.Context, in io.Reader, out io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(in, 1<<20+1))
	if err != nil {
		return fmt.Errorf("read probe request: %w", err)
	}
	if len(data) > 1<<20 {
		return errors.New("probe request is larger than 1 MiB")
	}
	var req ProbeRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Errorf("decode probe request: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return fmt.Errorf("decode probe request: %w", err)
	}
	resp := HandleProbe(ctx, req)
	return json.NewEncoder(out).Encode(resp)
}

// HandleProbe executes a probe request.
func HandleProbe(ctx context.Context, req ProbeRequest) ProbeResponse {
	c, err := newRuntimeClient(req)
	if err != nil {
		return ProbeResponse{Error: err.Error()}
	}
	switch req.Op {
	case ProbeStatus:
		st, _, err := c.status(ctx)
		if err != nil {
			return failure(err)
		}
		return ProbeResponse{Status: &st}
	case ProbeMaintenanceEnter:
		reason := req.Reason
		if reason == "" {
			reason = "lensdock update"
		}
		if err := validateConditionalMaintenance(req); err != nil {
			return ProbeResponse{Error: err.Error()}
		}
		res, err := c.enterMaintenance(ctx, reason, req.IfIdle, req.ExpectedGeneration)
		if err != nil {
			return failure(err)
		}
		return ProbeResponse{Maintenance: &res}
	case ProbeMaintenanceExit:
		if err := c.exitMaintenance(ctx); err != nil {
			return failure(err)
		}
		return ProbeResponse{}
	case ProbeRevokeLease:
		if err := c.revokeLease(ctx, req.LeaseID); err != nil {
			return failure(err)
		}
		return ProbeResponse{}
	case ProbeVerifyCandidate:
		if req.Verify == nil {
			return ProbeResponse{Error: "verify-candidate needs a verify spec"}
		}
		report := verifyCandidate(ctx, c, *req.Verify)
		return ProbeResponse{Verify: &report}
	}
	return ProbeResponse{Error: fmt.Sprintf("unknown probe operation %q", req.Op)}
}

// validateConditionalMaintenance rejects ambiguous or unbounded conditional
// maintenance input before anything is sent: ifIdle needs a bounded
// expectedGeneration and expectedGeneration needs ifIdle. Other operations
// accept neither field.
func validateConditionalMaintenance(req ProbeRequest) error {
	if !req.IfIdle {
		if req.ExpectedGeneration != "" {
			return errors.New("expectedGeneration requires ifIdle")
		}
		return nil
	}
	if n := len(req.ExpectedGeneration); n < 1 || n > 128 {
		return errors.New("ifIdle requires an expectedGeneration of 1 to 128 characters")
	}
	return nil
}

type probeError struct {
	code string
	msg  string
}

func (e *probeError) Error() string { return e.msg }

func failure(err error) ProbeResponse {
	var pe *probeError
	if errors.As(err, &pe) {
		return ProbeResponse{Error: pe.msg, Code: pe.code}
	}
	return ProbeResponse{Error: err.Error()}
}

// runtimeClient is an HTTP client of one runtime API endpoint.
type runtimeClient struct {
	base      *url.URL
	hc        *http.Client
	wsClient  *http.Client
	token     string
	workspace string
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newRuntimeClient(req ProbeRequest) (*runtimeClient, error) {
	u, err := url.Parse(req.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" && u.Path != "/" {
		return nil, errors.New("probe baseUrl must be an http(s) URL without a path")
	}
	u.Path = ""
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("plain http probes are only allowed for loopback addresses")
	}
	if !workspaceIDPattern.MatchString(req.Workspace) {
		return nil, errors.New("invalid workspace id")
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if req.TLS != nil {
		cfg, err := clientTLSConfig(*req.TLS)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = cfg
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &runtimeClient{
		base:      u,
		hc:        &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: noRedirect},
		wsClient:  &http.Client{Transport: tr, CheckRedirect: noRedirect},
		token:     req.Token,
		workspace: req.Workspace,
	}, nil
}

func clientTLSConfig(b ProbeTLS) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(b.CAPEM)) {
		return nil, errors.New("probe TLS: no CA certificate in caPem")
	}
	cert, err := tls.X509KeyPair([]byte(b.CertPEM), []byte(b.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("probe TLS: client key pair: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: b.ServerName}, nil
}

func (c *runtimeClient) wsPath(rest string) string {
	return "/api/v1/workspaces/" + url.PathEscape(c.workspace) + rest
}

// call performs one request. A transport failure returns a probeError with
// CodeUnreachable; any HTTP status is returned to the caller.
func (c *runtimeClient) call(ctx context.Context, method, path string, headers map[string]string, token string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		msg := "request failed"
		if errors.As(err, &ue) {
			msg = ue.Err.Error()
		}
		return 0, nil, &probeError{code: CodeUnreachable, msg: fmt.Sprintf("runtime unreachable: %s %s: %s", method, path, msg)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil {
		return 0, nil, &probeError{code: CodeUnreachable, msg: "read response: " + err.Error()}
	}
	if len(data) > maxProbeBody {
		return 0, nil, errors.New("response too large")
	}
	return resp.StatusCode, data, nil
}

// apiFailure turns a non-2xx answer into an error with the runtime's error
// code.
func apiFailure(op string, status int, body []byte) error {
	if e, ok := apiwire.ReadError(bytes.NewReader(body), 4096); ok {
		return &probeError{code: fmt.Sprintf("http-%d", status), msg: fmt.Sprintf("%s: %d %s: %s", op, status, e.Code, e.Message)}
	}
	return &probeError{code: fmt.Sprintf("http-%d", status), msg: fmt.Sprintf("%s: HTTP %d", op, status)}
}

type readyDoc struct {
	Ready     bool `json:"ready"`
	Workspace struct {
		ID                string  `json:"id"`
		RuntimeGeneration string  `json:"runtimeGeneration"`
		ControlState      string  `json:"controlState"`
		HolderID          *string `json:"holderId"`
	} `json:"workspace"`
	Locked   bool   `json:"locked"`
	LockCode string `json:"lockCode"`
	Lease    *struct {
		LeaseID  string `json:"leaseId"`
		HolderID string `json:"holderId"`
		Mode     string `json:"mode"`
	} `json:"lease"`
	Detail struct {
		ChromiumVersion string `json:"chromiumVersion"`
		Sandbox         struct {
			Verified bool `json:"verified"`
		} `json:"sandbox"`
		FcMatch  map[string]string `json:"fcMatch"`
		Packages map[string]string `json:"packages"`
		Units    []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"units"`
	} `json:"detail"`
}

// status reads GET /readyz?detail=1. The runtime answers 503 with the same
// body while it is not ready, which is a status, not an error.
func (c *runtimeClient) status(ctx context.Context) (RuntimeStatus, readyDoc, error) {
	code, body, err := c.call(ctx, http.MethodGet, "/readyz?detail=1", nil, c.token, nil)
	if err != nil {
		return RuntimeStatus{}, readyDoc{}, err
	}
	if code != http.StatusOK && code != http.StatusServiceUnavailable {
		return RuntimeStatus{}, readyDoc{}, apiFailure("GET /readyz?detail=1", code, body)
	}
	var doc readyDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return RuntimeStatus{}, readyDoc{}, errors.New("malformed /readyz?detail=1 response")
	}
	st := RuntimeStatus{
		Ready: doc.Ready, ControlState: doc.Workspace.ControlState, RuntimeGeneration: doc.Workspace.RuntimeGeneration,
		Locked: doc.Locked, LockCode: doc.LockCode, ChromiumVersion: doc.Detail.ChromiumVersion,
	}
	if doc.Workspace.HolderID != nil {
		st.HolderID = *doc.Workspace.HolderID
	}
	if doc.Lease != nil {
		st.LeaseID, st.LeaseMode = doc.Lease.LeaseID, doc.Lease.Mode
		if st.HolderID == "" {
			st.HolderID = doc.Lease.HolderID
		}
	}
	return st, doc, nil
}

func (c *runtimeClient) enterMaintenance(ctx context.Context, reason string, ifIdle bool, expectedGeneration string) (MaintenanceResult, error) {
	payload := map[string]any{"reason": reason}
	if ifIdle {
		payload["ifIdle"] = true
		payload["expectedGeneration"] = expectedGeneration
	}
	body, _ := json.Marshal(payload)
	code, data, err := c.call(ctx, http.MethodPost, c.wsPath("/maintenance"), nil, c.token, body)
	if err != nil {
		return MaintenanceResult{}, err
	}
	switch code {
	case http.StatusOK:
		var res MaintenanceResult
		if err := json.Unmarshal(data, &res); err != nil {
			return MaintenanceResult{}, errors.New("malformed maintenance response")
		}
		return res, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return MaintenanceResult{}, &probeError{code: CodeUnsupported, msg: "this runtime has no maintenance endpoint"}
	}
	return MaintenanceResult{}, apiFailure("POST maintenance", code, data)
}

func (c *runtimeClient) exitMaintenance(ctx context.Context) error {
	code, data, err := c.call(ctx, http.MethodDelete, c.wsPath("/maintenance"), nil, c.token, nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return &probeError{code: CodeUnsupported, msg: "this runtime has no maintenance endpoint"}
	}
	return apiFailure("DELETE maintenance", code, data)
}

func (c *runtimeClient) revokeLease(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return errors.New("invalid lease id")
	}
	code, data, err := c.call(ctx, http.MethodDelete, c.wsPath("/leases/"+url.PathEscape(id)), nil, c.token, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNoContent || code == http.StatusOK || code == http.StatusNotFound {
		return nil
	}
	return apiFailure("DELETE lease", code, data)
}

// acquireAgent obtains an AGENT lease with the operator token and returns its
// id and capability token.
func (c *runtimeClient) acquireAgent(ctx context.Context) (leaseID, capability string, err error) {
	idem, err := auth.RandomHex(16)
	if err != nil {
		return "", "", err
	}
	body := []byte(`{"mode":"AGENT"}`)
	code, data, err := c.call(ctx, http.MethodPost, c.wsPath("/leases"), map[string]string{
		"Idempotency-Key": idem, "X-LensDock-Holder-ID": "lensdock-update-verify",
	}, c.token, body)
	if err != nil {
		return "", "", err
	}
	if code != http.StatusCreated && code != http.StatusOK {
		return "", "", apiFailure("POST leases", code, data)
	}
	var grant apiwire.LeaseGrant
	if err := json.Unmarshal(data, &grant); err != nil || grant.CapabilityToken == "" {
		return "", "", errors.New("malformed lease grant")
	}
	return grant.Lease.LeaseID, grant.CapabilityToken, nil
}
