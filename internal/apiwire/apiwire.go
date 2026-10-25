// Package apiwire defines the JSON wire types of the runtime API. The structs
// mirror the Error, Workspace, AcquireLease, Lease, LeaseGrant and RenewLease
// schemas in contracts/openapi.yaml.
package apiwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// Error is the body of every non-2xx response.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

// Workspace is the runtime view of one workspace. HolderID is null while no
// lease is held.
type Workspace struct {
	ID                string  `json:"id"`
	RuntimeGeneration string  `json:"runtimeGeneration"`
	ControlState      string  `json:"controlState"`
	HolderID          *string `json:"holderId"`
	Ready             bool    `json:"ready"`
	// AgentControl is the usage policy (mode and pause). It is always set
	// by the runtime; consumers treat a missing value as fail-closed.
	AgentControl AgentControl `json:"agentControl"`
}

// AcquireLease is the body of a lease acquisition request. A nil TTLSeconds
// selects the server default. FeedbackID, when present, is the feedback the
// caller is claiming; the runtime validates it but does not store it, because
// delivery state belongs to the Gateway.
type AcquireLease struct {
	Mode             string `json:"mode"`
	TTLSeconds       *int   `json:"ttlSeconds,omitempty"`
	FeedbackID       string `json:"feedbackId,omitempty"`
	FeedbackRevision int    `json:"feedbackRevision,omitempty"`
}

// Lease describes a granted lease.
type Lease struct {
	LeaseID           string    `json:"leaseId"`
	WorkspaceID       string    `json:"workspaceId"`
	HolderID          string    `json:"holderId"`
	Mode              string    `json:"mode"`
	Epoch             uint64    `json:"epoch"`
	RuntimeGeneration string    `json:"runtimeGeneration"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// LeaseGrant is the response to a successful acquisition.
type LeaseGrant struct {
	Lease           Lease  `json:"lease"`
	CapabilityToken string `json:"capabilityToken"`
}

// RenewLease is the body of a lease renewal request.
type RenewLease struct {
	Epoch             uint64 `json:"epoch"`
	RuntimeGeneration string `json:"runtimeGeneration"`
}

// Error codes carried in Error.Code.
const (
	CodeInvalidRequest         = "INVALID_REQUEST"
	CodeInvalidUTF8            = "INVALID_UTF8"
	CodeUnauthenticated        = "UNAUTHENTICATED"
	CodeWrongLeaseMode         = "WRONG_LEASE_MODE"
	CodeOriginNotAllowed       = "ORIGIN_NOT_ALLOWED"
	CodeNotFound               = "NOT_FOUND"
	CodeMethodNotAllowed       = "METHOD_NOT_ALLOWED"
	CodeLeaseConflict          = "LEASE_CONFLICT"
	CodeLeaseEnded             = "LEASE_ENDED"
	CodeRuntimeNotReady        = "RUNTIME_NOT_READY"
	CodeIdempotencyReplay      = "IDEMPOTENCY_REPLAY"
	CodeEpochMismatch          = "EPOCH_MISMATCH"
	CodeGenerationMismatch     = "GENERATION_MISMATCH"
	CodeBodyTooLarge           = "BODY_TOO_LARGE"
	CodeTooManyConnections     = "TOO_MANY_CONNECTIONS"
	CodeUpstreamUnavailable    = "UPSTREAM_UNAVAILABLE"
	CodeSandboxPreflightFailed = "SANDBOX_PREFLIGHT_FAILED"
	CodeInternal               = "INTERNAL"
)

// Errors returned by Decode, so callers can map them to wire codes.
var (
	// ErrBodyTooLarge reports a body larger than the size cap (BODY_TOO_LARGE).
	ErrBodyTooLarge = errors.New("apiwire: body too large")
	// ErrInvalidUTF8 reports a body that is not valid UTF-8 (INVALID_UTF8).
	ErrInvalidUTF8 = errors.New("apiwire: body is not valid UTF-8")
	// ErrInvalidJSON reports malformed JSON, unknown fields, wrong types or
	// trailing data (INVALID_REQUEST).
	ErrInvalidJSON = errors.New("apiwire: invalid JSON body")
)

// Decode reads at most max bytes from r and decodes exactly one JSON value
// into v. Unknown fields, invalid UTF-8, trailing data and bodies larger than
// max are rejected. The returned error wraps ErrBodyTooLarge, ErrInvalidUTF8
// or ErrInvalidJSON, except for read failures of r which are returned wrapped
// as-is. Decoder detail is deliberately omitted from the error so request
// content cannot leak through error messages.
func Decode(r io.Reader, max int64, v any) error {
	if max < 0 {
		return errors.New("apiwire: negative size limit")
	}
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return fmt.Errorf("apiwire: read body: %w", err)
	}
	if int64(len(body)) > max {
		return ErrBodyTooLarge
	}
	if !utf8.Valid(body) {
		return ErrInvalidUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return ErrInvalidJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidJSON
	}
	return nil
}

// ReadError decodes the runtime's Error body from r, reading at most max
// bytes. ok is false when the body is unreadable, larger than max, not JSON
// or lacks a code, so clients can substitute their own generic error. Unlike
// Decode it tolerates unknown fields, so a newer server's error stays
// readable.
func ReadError(r io.Reader, max int64) (e Error, ok bool) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil || int64(len(data)) > max {
		return Error{}, false
	}
	if json.Unmarshal(data, &e) != nil || e.Code == "" {
		return Error{}, false
	}
	return e, true
}
