// Package hostmanager coordinates explicitly authorized local LensDock projects.
// Its private socket is an accidental-misrouting boundary, not same-UID isolation.
package hostmanager

import (
	"context"
	"os"
	"time"
)

// Resources is a pair of optional quantities. Zero means no configured limit
// for that dimension, independently for memory and CPU.
type Resources struct {
	MemoryBytes int64 `json:"memoryBytes"`
	NanoCPUs    int64 `json:"nanoCpus"`
}

// Limits are the optional caps of one project's Runtime and Gateway.
type Limits struct {
	Runtime Resources `json:"runtime"`
	Gateway Resources `json:"gateway"`
}

// Policy is the manager's optional resource and access policy. Every resource
// quantity is zero (unlimited) unless a human opted in.
type Policy struct {
	Budget                     Resources `json:"budget"`
	Helpers                    Resources `json:"helpers"`
	Headroom                   Resources `json:"headroom"`
	UnboundedExternalAllowance Resources `json:"unboundedExternalAllowance"`
	AllowUnboundedExternal     bool      `json:"allowUnboundedExternal"`
	ProjectLimits              Limits    `json:"projectLimits"`
	// HelperPidsLimit caps the processes of each lifecycle helper container.
	// Zero means unlimited.
	HelperPidsLimit int64 `json:"helperPidsLimit"`
	// RequireProjectAuthorization and RequireExplicitStart are opt-in access
	// restrictions owned by the access-policy code; resource code only
	// preserves them.
	RequireProjectAuthorization bool `json:"requireProjectAuthorization"`
	RequireExplicitStart        bool `json:"requireExplicitStart"`
	RequireStopConfirmation     bool `json:"requireStopConfirmation"`
}

// Config records human-selected executable, bundle and engine identities.
// Requests cannot override these values.
type Config struct {
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Root          string `json:"root"`
	Executable    string `json:"executable"`
	DockerBinary  string `json:"dockerBinary"`
	DockerContext string `json:"dockerContext"`
	DockerHost    string `json:"dockerHost"`
	DockerConfig  string `json:"dockerConfig"`
	EngineID      string `json:"engineId"`
	BundleDir     string `json:"bundleDir"`
	RuntimeImage  string `json:"runtimeImage"`
	GatewayImage  string `json:"gatewayImage"`
	PullPolicy    string `json:"pullPolicy"`
	PortFirst     int    `json:"portFirst"`
	PortLast      int    `json:"portLast"`
	Policy        Policy `json:"policy"`
	// RequirePairing records the opt-in pairing requirement chosen at
	// initialization; the access-policy code interprets it.
	RequirePairing bool `json:"requirePairing"`
}

type DirectoryIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type Project struct {
	ID             string            `json:"id"`
	Root           string            `json:"root"`
	Directory      DirectoryIdentity `json:"directory"`
	Revoked        bool              `json:"revoked"`
	StateDir       string            `json:"stateDir"`
	ComposeProject string            `json:"composeProject"`
	Workspace      string            `json:"workspace"`
	Origin         string            `json:"origin"`
	Port           int               `json:"port"`
	TokenFile      string            `json:"tokenFile"`
	Limits         Limits            `json:"limits"`
	State          string            `json:"state"`
	Blocker        string            `json:"blocker,omitempty"`
	OperationID    string            `json:"operationId,omitempty"`
	Reserved       bool              `json:"reserved"`
	// Reservation is the capacity the manager currently accounts to the project:
	// the maximum of its declared reservation, the enforced caps reported by the
	// backend and, while the project is not positively stopped, one project pair.
	Reservation Resources `json:"reservation"`
	// Usage is the last measured usage. It is meaningful only when UsageAvailable.
	Usage          Resources `json:"usage"`
	UsageAvailable bool      `json:"usageAvailable"`
	// QueuePosition is 1 for the head of the FIFO queue and 0 when not queued.
	QueuePosition int `json:"queuePosition,omitempty"`
	Waiters       int `json:"waiters,omitempty"`
}

// Connection is pinned once before creating a client. It contains a credential
// file location, never a credential. Identity and port ownership precede use.
type Connection struct {
	ProjectID string `json:"projectId"`
	API       string `json:"api"`
	Workspace string `json:"workspace"`
	TokenFile string `json:"tokenFile"`
}

type Waiter struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	OperationID string    `json:"operationId"`
	Deadline    time.Time `json:"deadline"`
	LastSeen    time.Time `json:"lastSeen"`
}

// UpdateAction exposes only the existing typed update operations. The admin
// capability is required even when a project capability can read its status.
type UpdateAction struct {
	Kind                 string `json:"kind"`
	PlanFile             string `json:"planFile,omitempty"`
	UpdateID             string `json:"updateId,omitempty"`
	CheckpointID         string `json:"checkpointId,omitempty"`
	Volume               string `json:"volume,omitempty"`
	Confirmed            bool   `json:"confirmed,omitempty"`
	DryRun               bool   `json:"dryRun,omitempty"`
	RevokeLeases         bool   `json:"revokeLeases,omitempty"`
	ApproveSwitch        bool   `json:"approveSwitch,omitempty"`
	AcceptCrashSnapshot  bool   `json:"acceptCrashSnapshot,omitempty"`
	AllowCrashCheckpoint bool   `json:"allowCrashCheckpoint,omitempty"`
	KeepCandidate        bool   `json:"keepCandidate,omitempty"`

	// Scopes and AfterDays parameterize UpdateExpire: the scopes to clean
	// (empty means all) and an age in days that replaces the deployment's
	// expireAfterDays for this run only (nil keeps it; 0 disables).
	Scopes    []string `json:"scopes,omitempty"`
	AfterDays *int     `json:"afterDays,omitempty"`

	// Automatic marks scheduler-created housekeeping (UpdateExpire with the
	// deployment's own setting). Requests cannot set it.
	Automatic bool `json:"automatic,omitempty"`
}

type Operation struct {
	ID                 string        `json:"id"`
	ProjectID          string        `json:"projectId"`
	Kind               string        `json:"kind"`
	Phase              string        `json:"phase"`
	CreatedAt          time.Time     `json:"createdAt"`
	Error              string        `json:"error,omitempty"`
	DiscardPageState   bool          `json:"discardPageState,omitempty"`
	Force              bool          `json:"force,omitempty"`
	ExpectedGeneration string        `json:"expectedGeneration,omitempty"`
	Update             *UpdateAction `json:"update,omitempty"`
}

type Observation struct {
	State     string    `json:"state"`
	Blocker   string    `json:"blocker,omitempty"`
	PortOwned bool      `json:"portOwned"`
	Usage     Resources `json:"usage"`
	Enforced  Resources `json:"enforced"`
	// UsageAvailable is false when the backend could not measure Usage. A zero
	// Usage must then not be presented as observed.
	UsageAvailable bool `json:"usageAvailable"`
}

type Inventory struct {
	EngineID string    `json:"engineId"`
	Capacity Resources `json:"capacity"`
	External Resources `json:"external"`
	// UnboundedMemory and UnboundedCPU name running external containers
	// without a cap in that dimension.
	UnboundedMemory []string `json:"unboundedMemory,omitempty"`
	UnboundedCPU    []string `json:"unboundedCpu,omitempty"`
	// MeasuredAt is when the backend produced this inventory.
	MeasuredAt time.Time              `json:"measuredAt,omitempty"`
	Projects   map[string]Observation `json:"projects"`
}

// Backend performs Docker work outside registry locks. Execute runs in a
// detached worker holding project and helper locks. Every child must inherit
// those descriptors so a killed worker cannot leave an unfenced child.
type Backend interface {
	Inspect(context.Context, Config, []Project) (Inventory, error)
	Execute(context.Context, Config, Project, Operation, []*os.File) error
	VerifyConnection(context.Context, Config, Project) (Connection, error)
}

type Request struct {
	Action             string        `json:"action"`
	ProjectID          string        `json:"projectId,omitempty"`
	Path               string        `json:"path,omitempty"`
	WaiterID           string        `json:"waiterId,omitempty"`
	OperationID        string        `json:"operationId,omitempty"`
	Deadline           time.Time     `json:"deadline,omitempty"`
	DiscardPageState   bool          `json:"discardPageState,omitempty"`
	Force              bool          `json:"force,omitempty"`
	ExpectedGeneration string        `json:"expectedGeneration,omitempty"`
	Update             *UpdateAction `json:"update,omitempty"`
	Policy             *Policy       `json:"policy,omitempty"`
	// Limits reconfigures the Runtime and Gateway caps of an existing stopped
	// project (ActionLimits). Zero removes the cap of that dimension.
	Limits *Limits `json:"limits,omitempty"`
	// ExpectedLimits, when set, must equal the project's current limits or the
	// request is refused. It makes a read-modify-write of Limits safe against
	// concurrent reconfiguration.
	ExpectedLimits *Limits `json:"expectedLimits,omitempty"`
}

type Response struct {
	Error         string      `json:"error,omitempty"`
	Project       *Project    `json:"project,omitempty"`
	Projects      []Project   `json:"projects,omitempty"`
	Operation     *Operation  `json:"operation,omitempty"`
	Waiter        *Waiter     `json:"waiter,omitempty"`
	QueuePosition int         `json:"queuePosition,omitempty"`
	Connection    *Connection `json:"connection,omitempty"`
	Inventory     *Inventory  `json:"inventory,omitempty"`
	Policy        *Policy     `json:"policy,omitempty"`
	Warnings      []string    `json:"warnings,omitempty"`
}
