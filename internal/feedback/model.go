package feedback

import (
	"context"
	"encoding/json"
	"time"
)

// SchemaVersion is the schemas/feedback.schema.json version of a published
// revision whose capture source is "drawing", that uses the annotation kinds
// "ellipse", "diamond" or "line", or whose task carries no description (or no
// expected and actual result for a bug or visual-regression task). Every
// other revision keeps SchemaVersionLegacy, so clients that only understand
// 0.1.0 keep reading all feedback that does not use the new features, and
// fail with an explicit schemaVersion error on the rest.
const SchemaVersion = "0.2.0"

// SchemaVersionAdvanced is the schemas/feedback.schema.json version of a
// published revision that uses any 0.3.0 annotation feature: the highlight,
// spotlight, blur or pixelate kinds, an arrow control point, or one of the
// style fields opacity, effectSizePx, arrowStyle, textStyle and smoothing.
// It supersedes SchemaVersion only for those revisions; 0.1.0 and 0.2.0
// documents reject all of them.
const SchemaVersionAdvanced = "0.3.0"

// SchemaVersionLegacy is the schemas/feedback.schema.json version of every
// revision published before standalone drawings and the ellipse, diamond and
// line annotation kinds existed. Published documents are immutable and keep it.
const SchemaVersionLegacy = "0.1.0"

// Capture sources, provenances and consistency values.
const (
	SourceDesktop      = "desktop"
	SourceViewport     = "page-viewport"
	SourcePageFull     = "page-full"
	SourceDrawing      = "drawing"
	ProvClientFB       = "client-framebuffer"
	ProvRuntimeCDP     = "runtime-cdp"
	ProvGenerated      = "generated-canvas"
	ConsStable         = "stable-navigation"
	ConsBestEffort     = "best-effort"
	ConsUnknown        = "unknown"
	StatusAwaiting     = "awaiting-upload"
	StatusCapturing    = "capturing"
	StatusReady        = "ready"
	StatusFailed       = "failed"
	TransformCrop      = "crop"
	TransformRevision  = "revision-base"
	DraftState         = "DRAFT"
	revisionDraft      = "DRAFT"
	revisionPublished  = "PUBLISHED"
	StateReady         = "READY"
	StateQueued        = "QUEUED"
	StateAcknowledged  = "ACKNOWLEDGED"
	StateInProgress    = "IN_PROGRESS"
	StateResolved      = "RESOLVED"
	StateVerified      = "VERIFIED"
	StateCancelled     = "CANCELLED"
	ArtifactRedacted   = "screenshot.redacted.png"
	ArtifactAnnotated  = "screenshot.annotated.png"
	ArtifactEnv        = "environment.json"
	ArtifactFeedback   = "feedback.json"
	ArtifactTask       = "task.md"
	ArtifactAnnots     = "annotations.json"
	ArtifactManifest   = "manifest.json"
	mimePNG            = "image/png"
	mimeJSON           = "application/json"
	mimeMarkdown       = "text/markdown; charset=utf-8"
	transitionDispatch = "dispatch"
)

// PageInfo is the sanitized page context of a page-viewport capture.
type PageInfo struct {
	URLSanitized      string  `json:"urlSanitized"`
	TitleSanitized    string  `json:"titleSanitized"`
	ViewportWidthCSS  float64 `json:"viewportWidthCss"`
	ViewportHeightCSS float64 `json:"viewportHeightCss"`
	PixelsPerCSSX     float64 `json:"pixelsPerCssX"`
	PixelsPerCSSY     float64 `json:"pixelsPerCssY"`
	ScrollXCSS        float64 `json:"scrollXCss"`
	ScrollYCSS        float64 `json:"scrollYCss"`
	BrowserVersion    string  `json:"browserVersion"`
}

// ParentTransform records how a capture derives from another one, so
// annotations of the parent can be related to it and old data never drifts.
type ParentTransform struct {
	Kind            string `json:"kind"`
	ParentCaptureID string `json:"parentCaptureId"`
	ParentWidthPx   int    `json:"parentWidthPx"`
	ParentHeightPx  int    `json:"parentHeightPx"`
	CropXPx         int    `json:"cropXPx"`
	CropYPx         int    `json:"cropYPx"`
	CropWidthPx     int    `json:"cropWidthPx"`
	CropHeightPx    int    `json:"cropHeightPx"`
	FromFeedbackID  string `json:"fromFeedbackId,omitempty"`
	FromRevision    int    `json:"fromRevision,omitempty"`
}

// CaptureState is the private capture status (a superset of the OpenAPI
// CaptureState).
type CaptureState struct {
	CaptureID         string           `json:"captureId"`
	WorkspaceID       string           `json:"workspaceId"`
	Status            string           `json:"status"`
	Consistency       string           `json:"consistency"`
	Source            string           `json:"source"`
	Provenance        string           `json:"provenance"`
	ImageWidthPx      int              `json:"imageWidthPx,omitempty"`
	ImageHeightPx     int              `json:"imageHeightPx,omitempty"`
	CapturedAt        *time.Time       `json:"capturedAt,omitempty"`
	ServerReceivedAt  *time.Time       `json:"serverReceivedAt,omitempty"`
	RuntimeGeneration string           `json:"runtimeGeneration"`
	TargetID          *string          `json:"targetId"`
	NavigationID      *string          `json:"navigationId"`
	Page              *PageInfo        `json:"page,omitempty"`
	ParentTransform   *ParentTransform `json:"parentTransform,omitempty"`
	FeedbackID        string           `json:"feedbackId,omitempty"`
	ExpiresAt         *time.Time       `json:"expiresAt,omitempty"` // absent: never expires
}

// Task is the human-written part of a feedback revision.
type Task struct {
	Kind              string   `json:"kind"`
	Intent            string   `json:"intent"`
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	ExpectedResult    string   `json:"expectedResult"`
	ActualResult      string   `json:"actualResult"`
	ReproductionSteps []string `json:"reproductionSteps"`
}

// Draft is the response of draft creation and update.
type Draft struct {
	FeedbackID string `json:"feedbackId"`
	Revision   int    `json:"revision"`
	ETag       string `json:"etag"`
	State      string `json:"state"`
}

// DraftDetail is a private draft as read by its owner.
type DraftDetail struct {
	Draft
	CaptureID      string            `json:"captureId"`
	Task           Task              `json:"task"`
	Annotations    []json.RawMessage `json:"annotations"`
	PreviewReady   bool              `json:"previewReady"`
	ExpiresAt      *time.Time        `json:"expiresAt,omitempty"` // absent: never expires
	RedactionCount int               `json:"redactionCount"`
	Warnings       []string          `json:"warnings"`
}

// Draft warnings.
const (
	WarnNoRedaction = "NO_REDACTION_REGIONS"
	WarnExpiring    = "DRAFT_EXPIRING"
)

// ArtifactRef is one entry of PublishedFeedback.Artifacts.
type ArtifactRef struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// PublishedCapture is the capture section of a published revision.
type PublishedCapture struct {
	ID                  string    `json:"id"`
	Source              string    `json:"source"`
	Provenance          string    `json:"provenance"`
	CapturedAt          time.Time `json:"capturedAt"`
	ServerReceivedAt    time.Time `json:"serverReceivedAt"`
	RuntimeGeneration   string    `json:"runtimeGeneration"`
	TargetID            *string   `json:"targetId"`
	NavigationID        *string   `json:"navigationId"`
	Consistency         string    `json:"consistency"`
	ImageWidthPx        int       `json:"imageWidthPx"`
	ImageHeightPx       int       `json:"imageHeightPx"`
	RedactedImageSHA256 string    `json:"redactedImageSha256"`
	Page                *PageInfo `json:"page,omitempty"`
}

// SharePolicy is the share policy of a published revision.
type SharePolicy struct {
	HumanApproved       bool `json:"humanApproved"`
	RedactionsApplied   bool `json:"redactionsApplied"`
	IncludesRawImage    bool `json:"includesRawImage"`
	DiagnosticsIncluded bool `json:"diagnosticsIncluded"`
}

// PublishedFeedback is schemas/feedback.schema.json.
type PublishedFeedback struct {
	SchemaVersion string            `json:"schemaVersion"`
	FeedbackID    string            `json:"feedbackId"`
	Revision      int               `json:"revision"`
	WorkspaceID   string            `json:"workspaceId"`
	PublishedAt   time.Time         `json:"publishedAt"`
	Capture       PublishedCapture  `json:"capture"`
	Task          Task              `json:"task"`
	Annotations   []json.RawMessage `json:"annotations"`
	Artifacts     []ArtifactRef     `json:"artifacts"`
	SharePolicy   SharePolicy       `json:"sharePolicy"`
}

// FileEntry is one file of manifest.json.
type FileEntry struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Manifest is manifest.json: every file of the bundle with hash and size.
type Manifest struct {
	SchemaVersion string      `json:"schemaVersion"`
	FeedbackID    string      `json:"feedbackId"`
	Revision      int         `json:"revision"`
	WorkspaceID   string      `json:"workspaceId"`
	PublishedAt   time.Time   `json:"publishedAt"`
	Files         []FileEntry `json:"files"`
}

// Delivery is the contract Delivery.
type Delivery struct {
	FeedbackID string `json:"feedbackId"`
	Revision   int    `json:"revision"`
	AgentID    string `json:"agentId"`
	State      string `json:"state"`
}

// ResolveResult is what an agent submitted on resolve.
type ResolveResult struct {
	Summary           string   `json:"summary"`
	Verification      string   `json:"verification"`
	ResultFeedbackIDs []string `json:"resultFeedbackIds"`
}

// Transition is one entry of the delivery history.
type Transition struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	ActorID   string    `json:"actorId"`
	ActorRole string    `json:"actorRole"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason,omitempty"`
}

// DeliveryDetail is the full delivery record of one revision.
type DeliveryDetail struct {
	FeedbackID     string         `json:"feedbackId"`
	Revision       int            `json:"revision"`
	WorkspaceID    string         `json:"workspaceId"`
	State          string         `json:"state"`
	AgentID        string         `json:"agentId,omitempty"`
	QueuedAt       *time.Time     `json:"queuedAt,omitempty"`
	AcknowledgedAt *time.Time     `json:"acknowledgedAt,omitempty"`
	InProgressAt   *time.Time     `json:"inProgressAt,omitempty"`
	ResolvedAt     *time.Time     `json:"resolvedAt,omitempty"`
	VerifiedAt     *time.Time     `json:"verifiedAt,omitempty"`
	CancelledAt    *time.Time     `json:"cancelledAt,omitempty"`
	LeaseID        string         `json:"leaseId,omitempty"`
	Result         *ResolveResult `json:"result,omitempty"`
	LastReason     string         `json:"lastReason,omitempty"`
	RejectionCount int            `json:"rejectionCount"`
	History        []Transition   `json:"history"`
}

// FeedbackSummary is one row of the published list (one per revision).
type FeedbackSummary struct {
	FeedbackID    string    `json:"feedbackId"`
	Revision      int       `json:"revision"`
	WorkspaceID   string    `json:"workspaceId"`
	Title         string    `json:"title"`
	DeliveryState string    `json:"deliveryState"`
	AgentID       string    `json:"agentId,omitempty"`
	PublishedAt   time.Time `json:"publishedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// FeedbackList is a page of FeedbackSummary.
type FeedbackList struct {
	Items      []FeedbackSummary `json:"items"`
	NextCursor *string           `json:"nextCursor"`
}

// DraftList is the list of the caller's open drafts.
type DraftList struct {
	Items []DraftDetail `json:"items"`
}

// HistoryVisible is the only value of the list filter "history": it omits
// revisions a human cleared from the published history. Without the filter
// every revision is listed (Agent queue, CLI, evidence lookups).
const HistoryVisible = "visible"

// ListQuery filters ListPublished.
type ListQuery struct {
	WorkspaceID string
	State       string
	History     string
	Cursor      string
	Limit       int
}

// ClearHistoryResult is the response of both history-clear operations: the
// number of revisions newly hidden by the call.
type ClearHistoryResult struct {
	Cleared int `json:"cleared"`
}

// ViewportIngest is a page-viewport capture obtained from the runtime.
type ViewportIngest struct {
	PNG               []byte
	ExpectedSHA256    string
	TargetID          string
	NavigationID      string
	Consistency       string
	RuntimeGeneration string
	LeaseID           string
	PriorLeaseID      string
	CapturedAt        time.Time
	Page              PageInfo
}

// ViewportRequest is what the Gateway receives to capture a viewport.
type ViewportRequest struct {
	Principal      Principal
	WorkspaceID    string
	TargetID       string
	LeaseID        string
	AllowInterrupt bool
}

// ViewportCapturer performs a page-viewport capture through the runtime. The
// implementation enforces lease rules (own HUMAN lease, IDLE, or explicit
// AllowInterrupt).
type ViewportCapturer func(ctx context.Context, req ViewportRequest) (ViewportIngest, error)

// CreateCaptureInput is the body of POST /workspaces/{id}/captures. The image
// dimensions are only valid (and required) for source "drawing".
type CreateCaptureInput struct {
	Source         string `json:"source"`
	TargetID       string `json:"targetId,omitempty"`
	LeaseID        string `json:"leaseId,omitempty"`
	AllowInterrupt bool   `json:"allowInterrupt,omitempty"`
	ImageWidthPx   int    `json:"imageWidthPx,omitempty"`
	ImageHeightPx  int    `json:"imageHeightPx,omitempty"`
}

// CreateDraftInput is the body of POST /feedback.
type CreateDraftInput struct {
	WorkspaceID string `json:"workspaceId"`
	CaptureID   string `json:"captureId"`
	Task        Task   `json:"task"`
}

// PatchDraftInput is the body of PATCH /feedback/{id}.
type PatchDraftInput struct {
	Task        *Task              `json:"task,omitempty"`
	Annotations *[]json.RawMessage `json:"annotations,omitempty"`
}

// CropInput selects a pixel rectangle of a capture.
type CropInput struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// NewRevisionInput is the optional body of POST /feedback/{id}/revisions.
type NewRevisionInput struct {
	CaptureID string `json:"captureId,omitempty"`
}

// PublishInput is the body of POST /feedback/{id}/publish. Dispatch is the
// opt-in to queue the new revision for the workspace's configured agent in
// the same transaction as the publication; without it the revision stays
// READY for CLI and result workflows.
type PublishInput struct {
	HumanApproved bool
	Dispatch      bool
}

// DispatchInput, AckInput, ResolveInput, VerifyInput and CancelInput are the
// bodies of the delivery transitions. An omitted AgentID is resolved by the
// server to the workspace's only configured agent or its configured default.
type DispatchInput struct {
	Revision int    `json:"revision"`
	AgentID  string `json:"agentId,omitempty"`
}

type AckInput struct {
	Revision int `json:"revision"`
}

type ResolveInput struct {
	Revision          int      `json:"revision"`
	Summary           string   `json:"summary"`
	Verification      string   `json:"verification"`
	ResultFeedbackIDs []string `json:"resultFeedbackIds,omitempty"`
}

type VerifyInput struct {
	Revision int    `json:"revision"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
}

type CancelInput struct {
	Revision int    `json:"revision"`
	Reason   string `json:"reason,omitempty"`
}

// AuditInput lets the Gateway record its own events (lease, takeover...).
type AuditInput struct {
	WorkspaceID string
	Category    string // "lease", "runtime", "auth" or "update"
	Action      string
	Result      string // "ok", "denied" or "error"
	SubjectType string
	SubjectID   string
	Detail      string
}
