package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/auth"
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
}
type Config struct {
	Listen      string          `json:"listen"`
	Origin      string          `json:"origin"`
	DataDir     string          `json:"dataDir"`
	WebDir      string          `json:"webDir"`
	StatusFile  string          `json:"statusFile,omitempty"`
	TLSCertFile string          `json:"tlsCertFile,omitempty"`
	TLSKeyFile  string          `json:"tlsKeyFile,omitempty"`
	Identities  []Identity      `json:"identities"`
	Runtimes    []RuntimeConfig `json:"runtimes"`
	// DemoNoAuth is set only by the serve flag or LENSDOCK_DEMO_NO_AUTH, never
	// read from the config file. See enableDemo for what it permits.
	DemoNoAuth bool `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := auth.ReadPrivateFile(path, 1<<20)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("gateway: invalid config JSON")
	}
	if c.Listen == "" || c.DataDir == "" || c.WebDir == "" || len(c.Runtimes) == 0 {
		return c, errors.New("gateway: listen, dataDir, webDir and runtimes required")
	}
	return c, nil
}
func runtimeTransport(c RuntimeConfig) (*url.URL, *http.Transport, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, nil, errors.New("gateway: runtime URL must be a fixed HTTP(S) origin")
	}
	t := &http.Transport{Proxy: nil, DisableCompression: true, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 60 * time.Second}
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
