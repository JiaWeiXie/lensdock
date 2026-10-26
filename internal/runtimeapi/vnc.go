package runtimeapi

import (
	"context"
	"net/http"

	"github.com/coder/websocket"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/lease"
	"github.com/JiaWeiXie/lensdock/internal/vnc"
)

// rejectOrigin refuses a request that carries an Origin header. The runtime
// is reached only through the Gateway, which dials without one; a browser
// reaching the runtime directly would send it.
func rejectOrigin(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := r.Header["Origin"]; ok {
		writeError(w, http.StatusForbidden, apiwire.CodeOriginNotAllowed, "requests with an Origin header are not allowed")
		return true
	}
	return false
}

// vncView proxies the view-only x11vnc (port 5901). Writing is impossible by
// construction: that instance is started with -viewonly -nosel, so key,
// pointer and clipboard messages a viewer sends are ignored by the server.
func (a *api) vncView(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) || rejectOrigin(w, r) {
		return
	}
	if a.VNCView == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "VNC view unavailable")
		return
	}
	a.VNCView.Serve(w, r, vnc.Binding{Key: "view"})
}

// vncControl proxies the control x11vnc (port 5900) for the holder of a HUMAN
// or AGENT lease: the whole desktop, including Chromium's own toolbar,
// menus and native dialogs, not only page content. The socket is bound to
// the lease ticket: when the lease ends for any reason (release, expiry,
// Agent pause or mode change, takeover, capture, generation change) the
// upstream connection is closed first, then the WebSocket (4001, or 4002
// for a generation change). Serve returns only after nothing of the
// connection can still write, and only then is the in-flight use released, so
// a takeover never grants a new lease while this socket can still type.
func (a *api) vncControl(w http.ResponseWriter, r *http.Request) {
	if rejectOrigin(w, r) {
		return
	}
	ticket, release, ok := a.authorizeLeaseRoute(w, r, lease.ModeHuman, lease.ModeAgent)
	if !ok {
		return
	}
	defer release()
	if a.VNCControl == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "VNC control unavailable")
		return
	}
	a.VNCControl.Serve(w, r, vnc.Binding{
		Key:  ticket.Lease().ID,
		Done: ticket.Done(),
		Reason: func() (websocket.StatusCode, string) {
			if ticket.Reason() == lease.EndGeneration {
				return vnc.CloseGenerationChanged, "browser generation changed"
			}
			return vnc.CloseLeaseEnded, "lease ended"
		},
	})
}

// resolutionBody is the body of PUT /resolution.
type resolutionBody struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (a *api) getResolution(w http.ResponseWriter, r *http.Request) {
	if !a.operatorRoute(w, r) {
		return
	}
	if a.Resizer == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "display control unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), bodyReadTimeout)
	defer cancel()
	d, err := a.Resizer.Query(ctx)
	if err != nil {
		status, code, msg := resizeErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	a.display.Store(&vnc.Size{Width: d.Current.Width, Height: d.Current.Height})
	if d.Supported == nil {
		d.Supported = []vnc.Size{}
	}
	writeJSON(w, http.StatusOK, d)
}

// setResolution changes the display size for the HUMAN lease holder. Only
// sizes of vnc.AllowedSizes that the X server advertises are applied; the
// response is the size read back from the server.
func (a *api) setResolution(w http.ResponseWriter, r *http.Request) {
	_, release, ok := a.authorizeLeaseRoute(w, r, lease.ModeHuman)
	if !ok {
		return
	}
	defer release()
	limitReads(w)
	var body resolutionBody
	if err := apiwire.Decode(r.Body, jsonBodyLimit, &body); err != nil {
		status, code, msg := bodyErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	if body.Width <= 0 || body.Height <= 0 {
		writeError(w, http.StatusBadRequest, apiwire.CodeInvalidRequest, "width and height must be positive")
		return
	}
	if a.Resizer == nil {
		writeError(w, http.StatusBadGateway, apiwire.CodeUpstreamUnavailable, "display control unavailable")
		return
	}
	got, err := a.Resizer.Set(r.Context(), vnc.Size{Width: body.Width, Height: body.Height})
	if got.Width > 0 && got.Height > 0 {
		a.display.Store(&got)
	}
	if err != nil {
		status, code, msg := resizeErrorStatus(err)
		writeError(w, status, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, resolutionBody{Width: got.Width, Height: got.Height})
}
