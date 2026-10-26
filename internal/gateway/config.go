package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/feedback"
)

// RuntimeConfig binds one workspace to a fixed trusted runtime. URLs are never
// accepted from HTTP request parameters.
type RuntimeConfig struct {
	WorkspaceID       string `json:"workspaceId"`
	URL               string `json:"url"`
	OperatorTokenFile string `json:"operatorTokenFile"`
	CAFile            string `json:"caFile,omitempty"`
	ClientCertFile    string `json:"clientCertFile,omitempty"`
	ClientKeyFile     string `json:"clientKeyFile,omitempty"`
	// DefaultAgentID optionally names the agent identity of this workspace that
	// receives feedback when several agents are configured. With exactly one
	// agent it is optional; it must be an agent identity of this workspace.
	DefaultAgentID string `json:"defaultAgentId,omitempty"`
	// TransferTimeoutSeconds optionally bounds how long this runtime may take
	// to answer a user data transfer (file upload or download, viewport
	// screenshot). Zero, the default, sets no deadline; control calls keep
	// their short fixed deadlines.
	TransferTimeoutSeconds int `json:"transferTimeoutSeconds,omitempty"`
}

// Config is the Gateway configuration. DeploymentID is a non-secret identity
// that managed deployments expose at GET /api/v1/deployment and mix into the
// session cookie names; empty means a standalone deployment.
type Config struct {
	Listen       string          `json:"listen"`
	Origin       string          `json:"origin"`
	DeploymentID string          `json:"deploymentId,omitempty"`
	DataDir      string          `json:"dataDir"`
	WebDir       string          `json:"webDir"`
	StatusFile   string          `json:"statusFile,omitempty"`
	TLSCertFile  string          `json:"tlsCertFile,omitempty"`
	TLSKeyFile   string          `json:"tlsKeyFile,omitempty"`
	Identities   []Identity      `json:"identities"`
	Runtimes     []RuntimeConfig `json:"runtimes"`
	// FeedbackLimits are optional feedback restrictions; every zero value
	// means no limit. Environment variables LENSDOCK_FEEDBACK_* override them.
	FeedbackLimits feedback.Limits `json:"feedbackLimits,omitzero"`
	// RequirePairing disables automatic access for same-origin local browsers.
	RequirePairing bool          `json:"requirePairing,omitempty"`
	SessionLimits  SessionLimits `json:"sessionLimits,omitzero"`
}

// ValidateDeploymentID accepts the empty standalone ID or 1-64 ASCII letters,
// digits, underscores and hyphens that start with a letter or digit.
func ValidateDeploymentID(id string) error {
	if len(id) > 64 {
		return errors.New("gateway: deployment ID must be 1-64 characters")
	}
	for i := range len(id) {
		ch := id[i]
		alnum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || (ch != '_' && ch != '-')) {
			return errors.New("gateway: deployment ID must start with a letter or digit and contain only letters, digits, underscores and hyphens")
		}
	}
	return nil
}

// EnvRuntimeTransferTimeoutSeconds overrides every runtime's
// TransferTimeoutSeconds; 0 explicitly removes the deadline.
const EnvRuntimeTransferTimeoutSeconds = "LENSDOCK_RUNTIME_TRANSFER_TIMEOUT_SECONDS"

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := auth.ReadPrivateFile(path, 1<<20)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("gateway: invalid config JSON")
	}
	if v := os.Getenv(EnvRuntimeTransferTimeoutSeconds); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return c, errors.New("gateway: " + EnvRuntimeTransferTimeoutSeconds + " must be a non-negative integer (0 means no deadline)")
		}
		for i := range c.Runtimes {
			c.Runtimes[i].TransferTimeoutSeconds = n
		}
	}
	if c.Listen == "" || c.DataDir == "" || c.WebDir == "" || len(c.Runtimes) == 0 {
		return c, errors.New("gateway: listen, dataDir, webDir and runtimes required")
	}
	if err = ValidateDeploymentID(c.DeploymentID); err != nil {
		return c, err
	}
	return c, nil
}
func runtimeTransport(c RuntimeConfig) (*url.URL, *http.Transport, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, nil, errors.New("gateway: runtime URL must be a fixed HTTP(S) origin")
	}
	// The response-header wait covers user transfers (a large upload is
	// stored and synced before the runtime answers), so it follows the
	// optional TransferTimeoutSeconds and is otherwise not limited; control
	// calls carry their own fixed deadlines.
	t := &http.Transport{Proxy: nil, DisableCompression: true, ResponseHeaderTimeout: time.Duration(c.TransferTimeoutSeconds) * time.Second, IdleConnTimeout: 60 * time.Second}
	if u.Scheme == "http" {
		if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
			return nil, nil, errors.New("gateway: network runtime requires mTLS")
		}
	} else if u.Scheme == "https" {
		cert, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return nil, nil, errors.New("gateway: client certificate unavailable")
		}
		ca, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, nil, errors.New("gateway: runtime CA unavailable")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, nil, errors.New("gateway: invalid runtime CA")
		}
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}
	} else {
		return nil, nil, errors.New("gateway: runtime requires HTTP(S)")
	}
	return u, t, nil
}
