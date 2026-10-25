package apiwire

// Error codes added with the Gateway-facing runtime routes (takeover,
// maintenance, capture, VNC proxies, file transfer, display resize and the
// profile lock). They share the Error envelope with the codes in apiwire.go.
const (
	// CodeMaintenance means the workspace is in MAINTENANCE: no lease can be
	// granted until the operator leaves it.
	CodeMaintenance = "MAINTENANCE"
	// CodeProfileLocked is the lock code of a runtime whose profile is held
	// by another runtime (RT-05).
	CodeProfileLocked = "PROFILE_LOCKED"
	// CodeTargetNotFound means an explicit CDP target ID does not name a page.
	CodeTargetNotFound = "TARGET_NOT_FOUND"
	// CodeCaptureFailed means the CDP viewport capture could not complete.
	CodeCaptureFailed = "CAPTURE_FAILED"
	// CodeLeaseMismatch means the lease a caller expected is not the one held.
	CodeLeaseMismatch = "LEASE_MISMATCH"
	// CodeFileExists means an upload would overwrite an existing file.
	CodeFileExists = "FILE_EXISTS"
	// CodeInvalidName means a file name is not a safe single path segment.
	CodeInvalidName = "INVALID_NAME"
	// CodeQuotaExceeded means a transfer would exceed a size, count or quota limit.
	CodeQuotaExceeded = "QUOTA_EXCEEDED"
	// CodeResizeUnsupported means the requested display size is not offered
	// by the X server or by the allowed size list.
	CodeResizeUnsupported = "RESIZE_UNSUPPORTED"
)
