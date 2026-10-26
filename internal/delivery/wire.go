// Package delivery is the Agent side of LensDock feedback delivery: an
// authenticated Gateway client for the published-feedback API and the
// hash-verified local bundle that `lensdock feedback pull` writes.
//
// Everything a published feedback revision carries (title, description,
// screenshots, diagnostics) is untrusted data. This package validates its
// shape and integrity, never its intent.
package delivery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
	"unicode/utf8"
)

const (
	// SchemaVersion is the schemas/feedback.schema.json version that adds the
	// "drawing" capture source, the ellipse, diamond and line annotation kinds
	// and visual-only tasks.
	SchemaVersion = "0.2.0"
	// SchemaVersionAdvanced adds the highlight, spotlight, blur and pixelate
	// annotation kinds, curved-arrow control points and the opacity,
	// effectSizePx, arrowStyle, textStyle and smoothing styles. It is the
	// newest version this client understands; 0.1.0 and 0.2.0 documents must
	// not use any of those features.
	SchemaVersionAdvanced = "0.3.0"
	// SchemaVersionLegacy is the original schema version; its documents stay
	// readable unchanged.
	SchemaVersionLegacy = "0.1.0"

	// maxDeclaredBytes is the largest integer a JSON document represents
	// exactly (2^53-1). It is a structural bound on a declared size, not a
	// usage limit: it keeps size sums far from int64 overflow.
	maxDeclaredBytes = 1<<53 - 1
)

// Limits are optional restrictions on what this client accepts. The zero value
// of every field means no limit: the exact size, SHA-256, media type and PNG
// shape of every artifact are verified regardless.
type Limits struct {
	// MaxArtifactBytes bounds one artifact.
	MaxArtifactBytes int64
	// MaxBundleBytes bounds the sum of all artifacts of one revision.
	MaxBundleBytes int64
	// MaxJSONBytes bounds one JSON API response and a bundle's feedback.json.
	MaxJSONBytes int64
}

func (l Limits) validate() error {
	if l.MaxArtifactBytes < 0 || l.MaxBundleBytes < 0 || l.MaxJSONBytes < 0 {
		return errors.New("delivery: limits must not be negative (use 0 for no limit)")
	}
	return nil
}

// Fixed bundle file names. Nothing outside this set is ever written to or
// accepted in a bundle directory.
const (
	FileFeedback    = "feedback.json"
	FileTask        = "task.md"
	FileAnnotations = "annotations.json"
	FileRedacted    = "screenshot.redacted.png"
	FileAnnotated   = "screenshot.annotated.png"
	FileEnvironment = "environment.json"
	FileDiagnostics = "diagnostics.redacted.json"
	FileManifest    = "manifest.json"
)

const (
	mimePNG      = "image/png"
	mimeJSON     = "application/json"
	mimeMarkdown = "text/markdown"
)

// artifactMIME lists the server-supplied artifacts and their only valid MIME
// type (schemas/feedback.schema.json $defs/artifact).
var artifactMIME = map[string]string{
	FileRedacted:    mimePNG,
	FileAnnotated:   mimePNG,
	FileEnvironment: mimeJSON,
	FileDiagnostics: mimeJSON,
}

// bundleMIME lists every file a bundle may contain except the manifest.
var bundleMIME = map[string]string{
	FileFeedback:    mimeJSON,
	FileTask:        mimeMarkdown,
	FileAnnotations: mimeJSON,
	FileRedacted:    mimePNG,
	FileAnnotated:   mimePNG,
	FileEnvironment: mimeJSON,
	FileDiagnostics: mimeJSON,
}

// bundleOrder fixes the manifest order.
var bundleOrder = []string{
	FileFeedback, FileTask, FileAnnotations, FileRedacted, FileAnnotated, FileEnvironment, FileDiagnostics,
}

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~:-]{0,127}$`)
	sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// ValidateID reports whether s is usable as a feedback or workspace ID in a
// URL path and an Idempotency-Key.
func ValidateID(kind, s string) error {
	if !idPattern.MatchString(s) {
		return fmt.Errorf("invalid %s %q: want 1-128 characters of A-Z a-z 0-9 . _ ~ : -", kind, s)
	}
	return nil
}

// Delivery states of the feedback state machine.
const (
	StateReady        = "READY"
	StateQueued       = "QUEUED"
	StateAcknowledged = "ACKNOWLEDGED"
	StateInProgress   = "IN_PROGRESS"
	StateResolved     = "RESOLVED"
	StateVerified     = "VERIFIED"
	StateCancelled    = "CANCELLED"
)

var listStates = map[string]bool{
	StateReady: true, StateQueued: true, StateAcknowledged: true, StateInProgress: true,
	StateResolved: true, StateVerified: true, StateCancelled: true,
}

// ValidateState reports whether s is a state accepted by the list filter.
func ValidateState(s string) error {
	if !listStates[s] {
		return fmt.Errorf("invalid state %q: want READY, QUEUED, ACKNOWLEDGED, IN_PROGRESS, RESOLVED, VERIFIED or CANCELLED", s)
	}
	return nil
}

// Summary is one entry of the feedback list (openapi FeedbackSummary).
type Summary struct {
	FeedbackID    string `json:"feedbackId"`
	Revision      int    `json:"revision"`
	WorkspaceID   string `json:"workspaceId"`
	Title         string `json:"title"`
	DeliveryState string `json:"deliveryState"`
	AgentID       string `json:"agentId,omitempty"`
	PublishedAt   string `json:"publishedAt,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

// List is one page of authorized immutable feedback revisions.
type List struct {
	Items      []Summary `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}

// Artifact is one shared file of a published revision.
type Artifact struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Task holds the human-authored task fields. All of it is untrusted text.
type Task struct {
	Kind              string   `json:"kind"`
	Intent            string   `json:"intent"`
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	ExpectedResult    string   `json:"expectedResult"`
	ActualResult      string   `json:"actualResult"`
	ReproductionSteps []string `json:"reproductionSteps"`
}

// SharePolicy records what the human approved for sharing.
type SharePolicy struct {
	HumanApproved       bool `json:"humanApproved"`
	RedactionsApplied   bool `json:"redactionsApplied"`
	IncludesRawImage    bool `json:"includesRawImage"`
	DiagnosticsIncluded bool `json:"diagnosticsIncluded"`
}

// Feedback is an immutable published revision (schemas/feedback.schema.json).
// Raw holds the exact response body it was decoded from.
type Feedback struct {
	SchemaVersion string          `json:"schemaVersion"`
	FeedbackID    string          `json:"feedbackId"`
	Revision      int             `json:"revision"`
	WorkspaceID   string          `json:"workspaceId"`
	PublishedAt   string          `json:"publishedAt"`
	Capture       json.RawMessage `json:"capture"`
	Task          Task            `json:"task"`
	Annotations   json.RawMessage `json:"annotations"`
	Artifacts     []Artifact      `json:"artifacts"`
	SharePolicy   SharePolicy     `json:"sharePolicy"`

	Raw []byte `json:"-"`
}

// Artifact returns the listed artifact called name.
func (f *Feedback) Artifact(name string) (Artifact, bool) {
	for _, a := range f.Artifacts {
		if a.Name == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// Delivery is the server's answer to an acknowledgement or resolution.
type Delivery struct {
	FeedbackID string `json:"feedbackId"`
	Revision   int    `json:"revision"`
	AgentID    string `json:"agentId"`
	State      string `json:"state"`
}

// Resolution is the Agent's result report for human verification.
type Resolution struct {
	Summary           string
	Verification      string
	ResultFeedbackIDs []string
}

// Validate checks the resolve request: both texts are required and valid
// UTF-8, and result feedback IDs must be well-formed. Neither length nor the
// number of result IDs is capped.
func (r Resolution) Validate() error {
	if err := checkText("summary", r.Summary, 1); err != nil {
		return err
	}
	if err := checkText("verification", r.Verification, 1); err != nil {
		return err
	}
	for _, id := range r.ResultFeedbackIDs {
		if err := ValidateID("result feedback ID", id); err != nil {
			return err
		}
	}
	return nil
}

// checkText requires valid UTF-8 and at least min code points.
func checkText(name, s string, min int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if utf8.RuneCountInString(s) < min {
		return fmt.Errorf("%s must not be empty", name)
	}
	return nil
}

// decodeStrict decodes exactly one JSON value from data, rejecting unknown
// fields, invalid UTF-8 and trailing data.
func decodeStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("malformed body: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("malformed body: trailing data")
	}
	return nil
}

// decodeLenient is decodeStrict for envelopes of which only a few known
// fields are read, so a server that adds a field stays usable.
func decodeLenient(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("body is not valid UTF-8")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("malformed body: %w", err)
	}
	return nil
}

func decodeList(data []byte) (List, error) {
	var l List
	if err := decodeLenient(data, &l); err != nil {
		return List{}, err
	}
	if l.Items == nil {
		return List{}, errors.New("malformed body: missing items")
	}
	for _, it := range l.Items {
		if err := ValidateID("feedback ID", it.FeedbackID); err != nil {
			return List{}, err
		}
		if it.Revision < 1 {
			return List{}, errors.New("malformed body: revision below 1")
		}
		if !listStates[it.DeliveryState] {
			return List{}, fmt.Errorf("malformed body: unknown deliveryState %q", it.DeliveryState)
		}
	}
	return l, nil
}

func decodeDelivery(data []byte) (Delivery, error) {
	var d Delivery
	if err := decodeLenient(data, &d); err != nil {
		return Delivery{}, err
	}
	if err := ValidateID("feedback ID", d.FeedbackID); err != nil {
		return Delivery{}, err
	}
	if d.Revision < 1 || d.AgentID == "" || !listStates[d.State] || d.State == StateReady {
		return Delivery{}, errors.New("malformed body: incomplete delivery")
	}
	return d, nil
}

// decodeFeedback decodes and validates a published revision against the
// request it answers.
func decodeFeedback(data []byte, wantID string, wantRevision int, lim Limits) (*Feedback, error) {
	var f Feedback
	if err := decodeStrict(data, &f); err != nil {
		return nil, err
	}
	f.Raw = bytes.Clone(data)
	if !supportedSchemaVersion(f.SchemaVersion) {
		return nil, fmt.Errorf("unsupported feedback schemaVersion %q (this client understands %s, %s and %s)", f.SchemaVersion, SchemaVersionLegacy, SchemaVersion, SchemaVersionAdvanced)
	}
	if f.FeedbackID != wantID || f.Revision != wantRevision {
		return nil, fmt.Errorf("server answered feedback %q revision %d, asked for %q revision %d",
			f.FeedbackID, f.Revision, wantID, wantRevision)
	}
	if err := ValidateID("workspace ID", f.WorkspaceID); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, f.PublishedAt); err != nil {
		return nil, errors.New("malformed body: publishedAt is not RFC 3339")
	}
	if !jsonIs(f.Capture, '{') {
		return nil, errors.New("malformed body: capture must be an object")
	}
	if !jsonIs(f.Annotations, '[') {
		return nil, errors.New("malformed body: annotations must be an array")
	}
	if err := f.Task.validate(f.SchemaVersion); err != nil {
		return nil, err
	}
	if err := validateAnnotationsFor(f.SchemaVersion, f.Annotations); err != nil {
		return nil, err
	}
	p := f.SharePolicy
	if !p.HumanApproved || !p.RedactionsApplied || p.IncludesRawImage {
		return nil, errors.New("refusing feedback: share policy lacks human approval or redaction, or includes a raw image")
	}
	if err := f.validateArtifacts(lim); err != nil {
		return nil, err
	}
	return &f, nil
}

// supportedSchemaVersion reports whether this client reads documents of
// version v.
func supportedSchemaVersion(v string) bool {
	return v == SchemaVersionAdvanced || v == SchemaVersion || v == SchemaVersionLegacy
}

// validateAnnotationsFor refuses annotation data newer than the document's
// schemaVersion: the 0.2.0 kinds in a 0.1.0 document, and every 0.3.0 kind,
// control point and style field in a 0.1.0 or 0.2.0 document. A version
// label that understates its content would otherwise let an older reader's
// guarantees be silently broken.
func validateAnnotationsFor(version string, raw json.RawMessage) error {
	if version == SchemaVersionAdvanced {
		return nil
	}
	var list []struct {
		Kind     string `json:"kind"`
		Geometry struct {
			Control json.RawMessage `json:"control"`
		} `json:"geometry"`
		Style map[string]json.RawMessage `json:"style"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("malformed body: annotations: %w", err)
	}
	for i, a := range list {
		switch a.Kind {
		case "highlight", "spotlight", "blur", "pixelate":
			return fmt.Errorf("malformed body: annotation %d is a %s, which needs schemaVersion %s, not %s", i, a.Kind, SchemaVersionAdvanced, version)
		case "ellipse", "diamond", "line":
			if version == SchemaVersionLegacy {
				return fmt.Errorf("malformed body: annotation %d is a %s, which needs schemaVersion %s or later, not %s", i, a.Kind, SchemaVersion, version)
			}
		}
		if len(a.Geometry.Control) > 0 {
			return fmt.Errorf("malformed body: annotation %d has a control point, which needs schemaVersion %s, not %s", i, SchemaVersionAdvanced, version)
		}
		for _, field := range []string{"opacity", "effectSizePx", "arrowStyle", "textStyle", "smoothing"} {
			if _, ok := a.Style[field]; ok {
				return fmt.Errorf("malformed body: annotation %d uses style %s, which needs schemaVersion %s, not %s", i, field, SchemaVersionAdvanced, version)
			}
		}
	}
	return nil
}

func jsonIs(raw json.RawMessage, first byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == first
}

// validate checks the task against the schema of the given version. Schema
// 0.1.0 requires a description, and expected and actual results for bug and
// visual-regression; from 0.2.0 on all of them may be empty (a visual-only
// revision). The title is required in both.
func (t Task) validate(schemaVersion string) error {
	switch t.Kind {
	case "bug", "task", "question", "visual-regression":
	default:
		return fmt.Errorf("malformed body: unknown task kind %q", t.Kind)
	}
	switch t.Intent {
	case "analyze-only", "reproduce", "modify-and-verify":
	default:
		return fmt.Errorf("malformed body: unknown task intent %q", t.Intent)
	}
	legacy := schemaVersion == SchemaVersionLegacy
	minDescription, minResult := 0, 0
	if legacy {
		minDescription = 1
		if t.Kind == "bug" || t.Kind == "visual-regression" {
			minResult = 1
		}
	}
	if err := checkText("task title", t.Title, 1); err != nil {
		return err
	}
	if err := checkText("task description", t.Description, minDescription); err != nil {
		return err
	}
	if err := checkText("expected result", t.ExpectedResult, minResult); err != nil {
		return err
	}
	if err := checkText("actual result", t.ActualResult, minResult); err != nil {
		return err
	}
	if t.ReproductionSteps == nil {
		return errors.New("malformed body: reproductionSteps must be an array")
	}
	for _, s := range t.ReproductionSteps {
		if err := checkText("reproduction step", s, 1); err != nil {
			return err
		}
	}
	return nil
}

func (f *Feedback) validateArtifacts(lim Limits) error {
	if n := len(f.Artifacts); n < 2 || n > 4 {
		return fmt.Errorf("malformed body: %d artifacts, want 2 to 4", n)
	}
	seen := make(map[string]bool, len(f.Artifacts))
	var total int64
	for _, a := range f.Artifacts {
		want, ok := artifactMIME[a.Name]
		switch {
		case !ok:
			return fmt.Errorf("refusing artifact %q: not a fixed share artifact name", a.Name)
		case seen[a.Name]:
			return fmt.Errorf("refusing duplicate artifact %q", a.Name)
		case a.MimeType != want:
			return fmt.Errorf("refusing artifact %q: MIME type %q, want %q", a.Name, a.MimeType, want)
		case !sha256Pattern.MatchString(a.SHA256):
			return fmt.Errorf("refusing artifact %q: malformed sha256", a.Name)
		case a.SizeBytes < 1 || a.SizeBytes > maxDeclaredBytes:
			return fmt.Errorf("refusing artifact %q: size %d outside 1..%d", a.Name, a.SizeBytes, int64(maxDeclaredBytes))
		case lim.MaxArtifactBytes > 0 && a.SizeBytes > lim.MaxArtifactBytes:
			return fmt.Errorf("refusing artifact %q: size %d is above the configured per-artifact limit of %d bytes", a.Name, a.SizeBytes, lim.MaxArtifactBytes)
		}
		seen[a.Name] = true
		total += a.SizeBytes
	}
	if !seen[FileRedacted] || !seen[FileAnnotated] {
		return errors.New("refusing feedback: both screenshot artifacts must be shared")
	}
	if lim.MaxBundleBytes > 0 && total > lim.MaxBundleBytes {
		return fmt.Errorf("refusing feedback: artifacts total %d bytes, the configured limit is %d", total, lim.MaxBundleBytes)
	}
	return nil
}
