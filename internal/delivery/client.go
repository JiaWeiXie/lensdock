package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiclient"
	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	jsonTimeout     = 30 * time.Second
	artifactTimeout = 2 * time.Minute
	maxErrorBody    = 64 << 10
	maxListPages    = 20
	userAgent       = "lensdock-feedback/1"
)

// TokenSource returns the bearer credential for one request. Reading it per
// request lets a rotated token file take effect without restarting a
// long-running MCP server.
type TokenSource func() (auth.Secret, error)

// FileTokenSource reads the credential from a private (0600, regular,
// non-symlink, owned by the caller) token file on every call.
func FileTokenSource(path string) TokenSource {
	return func() (auth.Secret, error) {
		if path == "" {
			return "", errors.New("no token file configured")
		}
		data, err := auth.ReadPrivateFile(path, 4096)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", errors.New("token file is empty")
		}
		return auth.Secret(tok), nil
	}
}

// Client talks to the Gateway feedback API with a bearer token. It never
// follows redirects, so the token cannot be forwarded to another origin.
type Client struct {
	base   string
	hc     *http.Client
	tokens TokenSource
}

// New validates base (https, or http on a loopback host with an explicit
// port) and returns a Client. hc may be nil.
func New(base string, tokens TokenSource, hc *http.Client) (*Client, error) {
	if tokens == nil {
		return nil, errors.New("delivery: token source is required")
	}
	v, err := apiclient.New(base, hc)
	if err != nil {
		return nil, err
	}
	var client http.Client
	if hc != nil {
		client = *hc
	} else {
		client.Transport = &http.Transport{
			Proxy:                 nil,
			DisableCompression:    true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: v.BaseURL(), hc: &client, tokens: tokens}, nil
}

// BaseURL returns the validated base URL.
func (c *Client) BaseURL() string { return c.base }

type call struct {
	method  string
	path    string
	query   url.Values
	idemKey string
	body    any
	timeout time.Duration
	// wantType is the required response media type.
	wantType string
	// maxBytes bounds the response body; exact makes any other length an error.
	maxBytes int64
	exact    bool
}

func (c *Client) fetch(parent context.Context, r call) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()

	tok, err := c.tokens()
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if r.body != nil {
		data, err := json.Marshal(r.body)
		if err != nil {
			return nil, errors.New("delivery: encode request")
		}
		body = bytes.NewReader(data)
	}
	target := c.base + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target, body)
	if err != nil {
		return nil, errors.New("delivery: build request")
	}
	req.Header.Set("Authorization", "Bearer "+tok.Reveal())
	req.Header.Set("Accept", r.wantType)
	req.Header.Set("User-Agent", userAgent)
	if r.idemKey != "" {
		req.Header.Set("Idempotency-Key", r.idemKey)
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.transportError(parent, ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeAPIError(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feedback API: unexpected HTTP %d", resp.StatusCode)
	}
	if mt, _, perr := mime.ParseMediaType(resp.Header.Get("Content-Type")); perr != nil || mt != r.wantType {
		return nil, fmt.Errorf("feedback API answered content type %q, want %q", resp.Header.Get("Content-Type"), r.wantType)
	}
	if resp.ContentLength > r.maxBytes {
		return nil, fmt.Errorf("feedback API response of %d bytes exceeds the %d byte limit", resp.ContentLength, r.maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.maxBytes+1))
	if err != nil {
		return nil, c.transportError(parent, ctx, err)
	}
	if int64(len(data)) > r.maxBytes {
		return nil, fmt.Errorf("feedback API response exceeds the %d byte limit", r.maxBytes)
	}
	if r.exact && int64(len(data)) != r.maxBytes {
		return nil, fmt.Errorf("feedback API sent %d bytes, expected exactly %d", len(data), r.maxBytes)
	}
	return data, nil
}

// transportError separates caller cancellation from an unreachable Gateway.
func (c *Client) transportError(parent, ctx context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: request timed out", apiclient.ErrUnreachable)
	}
	// url.Error carries method and URL only, never headers or bodies.
	return fmt.Errorf("%w: %w", apiclient.ErrUnreachable, err)
}

func decodeAPIError(resp *http.Response) error {
	e := &apiclient.APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
	if w, ok := apiwire.ReadError(resp.Body, maxErrorBody); ok {
		e.Code, e.Message = w.Code, w.Message
		if w.RequestID != "" {
			e.RequestID = w.RequestID
		}
	}
	return e
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values) ([]byte, error) {
	return c.fetch(ctx, call{
		method: http.MethodGet, path: path, query: q, timeout: jsonTimeout,
		wantType: mimeJSON, maxBytes: MaxJSONBody,
	})
}

func feedbackPath(id string, rest ...string) string {
	p := "/api/v1/feedback/" + url.PathEscape(id)
	for _, s := range rest {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// ListQuery selects a page of the authorized feedback list.
type ListQuery struct {
	WorkspaceID string
	State       string
	Cursor      string
}

// List returns one page of the revisions the token is authorized to read.
func (c *Client) List(ctx context.Context, q ListQuery) (List, error) {
	if err := ValidateID("workspace ID", q.WorkspaceID); err != nil {
		return List{}, err
	}
	v := url.Values{"workspaceId": {q.WorkspaceID}}
	if q.State != "" {
		if err := ValidateState(q.State); err != nil {
			return List{}, err
		}
		v.Set("state", q.State)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	data, err := c.getJSON(ctx, "/api/v1/feedback", v)
	if err != nil {
		return List{}, err
	}
	l, err := decodeList(data)
	if err != nil {
		return List{}, fmt.Errorf("feedback list: %w", err)
	}
	for _, it := range l.Items {
		if it.WorkspaceID != q.WorkspaceID {
			return List{}, fmt.Errorf("feedback list: server returned an item of workspace %q for workspace %q", it.WorkspaceID, q.WorkspaceID)
		}
	}
	return l, nil
}

// AssignedRevision returns the revision of feedback id that the server lists
// for this token. The server decides which revision an Agent may read; the
// client never guesses "latest". It fails when the list holds none or more
// than one revision of id.
func (c *Client) AssignedRevision(ctx context.Context, workspaceID, id string) (int, error) {
	if err := ValidateID("feedback ID", id); err != nil {
		return 0, err
	}
	var found []int
	cursor := ""
	seen := map[string]bool{}
	for range maxListPages {
		l, err := c.List(ctx, ListQuery{WorkspaceID: workspaceID, Cursor: cursor})
		if err != nil {
			return 0, err
		}
		for _, it := range l.Items {
			if it.FeedbackID == id {
				found = append(found, it.Revision)
			}
		}
		if l.NextCursor == nil || *l.NextCursor == "" {
			switch len(found) {
			case 0:
				return 0, fmt.Errorf("feedback %q is not in the authorized list of workspace %q", id, workspaceID)
			case 1:
				return found[0], nil
			}
			return 0, fmt.Errorf("feedback %q is listed with several revisions %v; pass an explicit revision", id, found)
		}
		if seen[*l.NextCursor] {
			return 0, errors.New("feedback list: server repeated a pagination cursor")
		}
		seen[*l.NextCursor] = true
		cursor = *l.NextCursor
	}
	return 0, fmt.Errorf("feedback %q not found within %d list pages", id, maxListPages)
}

// Get reads one authorized published revision.
func (c *Client) Get(ctx context.Context, id string, revision int) (*Feedback, error) {
	if err := ValidateID("feedback ID", id); err != nil {
		return nil, err
	}
	if revision < 1 {
		return nil, errors.New("revision must be at least 1")
	}
	data, err := c.getJSON(ctx, feedbackPath(id), url.Values{"revision": {strconv.Itoa(revision)}})
	if err != nil {
		return nil, err
	}
	f, err := decodeFeedback(data, id, revision)
	if err != nil {
		return nil, fmt.Errorf("feedback %s revision %d: %w", id, revision, err)
	}
	return f, nil
}

// Artifact downloads one artifact listed by f and verifies its exact size,
// SHA-256, media type and content shape before returning the bytes.
func (c *Client) Artifact(ctx context.Context, f *Feedback, name string) ([]byte, error) {
	a, ok := f.Artifact(name)
	if !ok {
		return nil, fmt.Errorf("feedback %s revision %d does not share artifact %q", f.FeedbackID, f.Revision, name)
	}
	data, err := c.fetch(ctx, call{
		method: http.MethodGet, path: feedbackPath(f.FeedbackID, "artifacts", a.Name),
		query:   url.Values{"revision": {strconv.Itoa(f.Revision)}},
		timeout: artifactTimeout, wantType: a.MimeType, maxBytes: a.SizeBytes, exact: true,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", name, err)
	}
	if err := verifyArtifact(a, data); err != nil {
		return nil, fmt.Errorf("artifact %s: %w", name, err)
	}
	return data, nil
}

func verifyArtifact(a Artifact, data []byte) error {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != a.SHA256 {
		return errors.New("sha256 does not match the published digest")
	}
	switch a.MimeType {
	case mimePNG:
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return errors.New("not a decodable PNG")
		}
		if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxDimension || cfg.Height > maxDimension || cfg.Width*cfg.Height > maxPixels {
			return fmt.Errorf("PNG dimensions %dx%d are outside the accepted range", cfg.Width, cfg.Height)
		}
	case mimeJSON:
		if !json.Valid(data) {
			return errors.New("not valid JSON")
		}
	}
	return nil
}

// Ack records that this Agent received and read revision of feedback id. The
// server records the acknowledgement for the authenticated agent only and
// authorizes the revision; it is never implied by reading.
func (c *Client) Ack(ctx context.Context, id string, revision int) (Delivery, error) {
	if err := checkTarget(id, revision); err != nil {
		return Delivery{}, err
	}
	d, err := c.transition(ctx, id, revision, "ack", struct {
		Revision int `json:"revision"`
	}{revision}, idemKey("ack", id, revision, ""))
	if err != nil {
		return Delivery{}, err
	}
	switch d.State {
	case StateAcknowledged, StateInProgress, StateResolved, StateVerified:
		return d, nil
	}
	return Delivery{}, fmt.Errorf("server did not record the acknowledgement: delivery state is %s", d.State)
}

// Resolve submits the Agent's result for human verification.
func (c *Client) Resolve(ctx context.Context, id string, revision int, r Resolution) (Delivery, error) {
	if err := checkTarget(id, revision); err != nil {
		return Delivery{}, err
	}
	if err := r.Validate(); err != nil {
		return Delivery{}, err
	}
	body := struct {
		Revision          int      `json:"revision"`
		Summary           string   `json:"summary"`
		Verification      string   `json:"verification"`
		ResultFeedbackIDs []string `json:"resultFeedbackIds,omitempty"`
	}{revision, r.Summary, r.Verification, r.ResultFeedbackIDs}
	payload, _ := json.Marshal(body)
	d, err := c.transition(ctx, id, revision, "resolve", body, idemKey("resolve", id, revision, string(payload)))
	if err != nil {
		return Delivery{}, err
	}
	if d.State != StateResolved && d.State != StateVerified {
		return Delivery{}, fmt.Errorf("server did not record the resolution: delivery state is %s", d.State)
	}
	return d, nil
}

func (c *Client) transition(ctx context.Context, id string, revision int, action string, body any, key string) (Delivery, error) {
	data, err := c.fetch(ctx, call{
		method: http.MethodPost, path: feedbackPath(id, action), idemKey: key, body: body,
		timeout: jsonTimeout, wantType: mimeJSON, maxBytes: MaxJSONBody,
	})
	if err != nil {
		return Delivery{}, err
	}
	d, err := decodeDelivery(data)
	if err != nil {
		return Delivery{}, fmt.Errorf("feedback %s: %w", action, err)
	}
	if d.FeedbackID != id || d.Revision != revision {
		return Delivery{}, fmt.Errorf("feedback %s: server answered feedback %q revision %d, asked for %q revision %d",
			action, d.FeedbackID, d.Revision, id, revision)
	}
	return d, nil
}

func checkTarget(id string, revision int) error {
	if err := ValidateID("feedback ID", id); err != nil {
		return err
	}
	if revision < 1 {
		return errors.New("revision must be at least 1")
	}
	return nil
}

// idemKey is deterministic, so a retried command replays instead of
// duplicating: the same acknowledgement or the same result text always maps
// to one server operation.
func idemKey(action, id string, revision int, payload string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + strconv.Itoa(revision) + "\x00" + payload))
	return "lensdock-" + action + "-" + hex.EncodeToString(sum[:])
}
