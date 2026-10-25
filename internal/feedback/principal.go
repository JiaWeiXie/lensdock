package feedback

import "context"

// Roles of an authenticated principal.
const (
	RoleViewer = "viewer"
	RoleHuman  = "human"
	RoleAgent  = "agent"
	RoleAdmin  = "admin"
)

// AllWorkspaces is the workspace scope of an administrator that may act on
// every workspace. An empty WorkspaceID means the same for admins only.
const AllWorkspaces = "*"

// Principal is the authenticated caller. The Gateway authenticates the
// request and passes the principal explicitly through WithPrincipal; this
// package never authenticates and fails closed without one.
type Principal struct {
	ID          string
	Role        string
	WorkspaceID string
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored by WithPrincipal.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Valid reports whether p has an identity, a known role and a workspace
// scope.
func (p Principal) Valid() bool {
	if p.ID == "" || len(p.ID) > 128 {
		return false
	}
	switch p.Role {
	case RoleViewer, RoleHuman, RoleAgent:
		return p.WorkspaceID != "" && p.WorkspaceID != AllWorkspaces
	case RoleAdmin:
		return true
	}
	return false
}

// InWorkspace reports whether p is scoped to workspaceID.
func (p Principal) InWorkspace(workspaceID string) bool {
	if p.Role == RoleAdmin && (p.WorkspaceID == "" || p.WorkspaceID == AllWorkspaces) {
		return workspaceID != ""
	}
	return p.WorkspaceID != "" && p.WorkspaceID == workspaceID
}

// IsHuman reports whether p may perform human operator actions.
func (p Principal) IsHuman() bool { return p.Role == RoleHuman || p.Role == RoleAdmin }

// canReadPublished reports whether p's role reads published revisions of its
// workspace without a delivery assignment.
func (p Principal) canReadPublished() bool {
	return p.Role == RoleViewer || p.Role == RoleHuman || p.Role == RoleAdmin
}

func requirePrincipal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok || !p.Valid() {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}
