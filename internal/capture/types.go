// Package capture takes explicit-target, page-viewport PNG captures through
// the runtime's own trusted CDP channel (never the lease-gated agent proxy),
// records the page state before and after, classifies the consistency of the
// result, and lists the page targets of the browser.
//
// The wire shapes in this file are the runtime's private contract with the
// Gateway; see local://runtime-wire.md.
package capture

import (
	"encoding/json"
	"time"
)

// Consistency values, identical to the openapi CaptureState.consistency enum.
const (
	// ConsistencyStable: same target, document and navigation entry before
	// and after the screenshot, and identical viewport and scroll.
	ConsistencyStable = "stable-navigation"
	// ConsistencyBestEffort: same document, but the viewport, scale or scroll
	// changed during the capture, so metadata and pixels may disagree.
	ConsistencyBestEffort = "best-effort"
	// ConsistencyUnknown: the target navigated, was replaced or could not be
	// observed, even after the automatic retry.
	ConsistencyUnknown = "unknown"
)

// Source is the only capture source the runtime produces; desktop captures
// come from the user's RFB client and never reach the runtime.
const Source = "page-viewport"

// Viewport is the visible area of the page in CSS pixels.
type Viewport struct {
	CSSWidth  float64 `json:"cssWidth"`
	CSSHeight float64 `json:"cssHeight"`
	Scale     float64 `json:"scale"`
	Zoom      float64 `json:"zoom"`
}

// Scroll is the layout viewport offset in CSS pixels.
type Scroll struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// State is what was observed about a target at one moment.
type State struct {
	TargetID          string   `json:"targetId"`
	URL               string   `json:"url"`
	Title             string   `json:"title"`
	NavigationEntryID int      `json:"navigationEntryId"`
	LoaderID          string   `json:"loaderId"`
	DevicePixelRatio  float64  `json:"devicePixelRatio"`
	Viewport          Viewport `json:"viewport"`
	Scroll            Scroll   `json:"scroll"`
}

// Image describes the captured PNG.
type Image struct {
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"contentType"`
}

// Scale converts image pixels to the viewport's CSS pixels:
// pixelsPerCssX = image width / viewport CSS width. It is not the DPR.
type Scale struct {
	PixelsPerCSSX float64 `json:"pixelsPerCssX"`
	PixelsPerCSSY float64 `json:"pixelsPerCssY"`
}

// LeaseRef names a lease without its capability token.
type LeaseRef struct {
	LeaseID  string `json:"leaseId"`
	HolderID string `json:"holderId"`
	Mode     string `json:"mode"`
}

// Control says what the capture did to the control lease.
type Control struct {
	// PriorLease is the lease the capture fenced, or null.
	PriorLease *LeaseRef `json:"priorLease"`
	// RestoredLease is the prior holder's new lease (same token, new lease ID
	// and epoch) as an apiwire.Lease, or null when none was restored.
	RestoredLease json.RawMessage `json:"restoredLease"`
	// FinalState is the workspace control state after the capture.
	FinalState string `json:"finalState"`
}

// Result is the metadata of one capture.
type Result struct {
	CaptureID      string    `json:"captureId"`
	Source         string    `json:"source"`
	TargetID       string    `json:"targetId"`
	NavigationID   string    `json:"navigationId"`
	Consistency    string    `json:"consistency"`
	Attempts       int       `json:"attempts"`
	CapturedAt     time.Time `json:"capturedAt"`
	BrowserVersion string    `json:"browserVersion"`
	Image          Image     `json:"image"`
	Scale          Scale     `json:"scale"`
	Pre            State     `json:"pre"`
	Post           State     `json:"post"`
	Control        Control   `json:"control"`
}

// Target is one page target of the browser.
type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Visible is document.visibilityState == "visible"; nil when the probe
	// failed.
	Visible *bool `json:"visible"`
}
