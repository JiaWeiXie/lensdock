// Package supervisor starts and supervises the runtime container's process
// tree (D-Bus, Xvfb, Openbox, x11vnc, Chromium) as unprivileged users, gates
// readiness on real probes, and reports sandbox facts.
//
// The supervisor process starts as root only long enough to run PrepareRoot
// and DropPrivileges. DropPrivileges uses syscall.AllThreadsSyscall for
// capset and prctl, which the Go runtime refuses when cgo is linked
// (ENOTSUP). The runtime binary MUST therefore be built with CGO_ENABLED=0.
package supervisor

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/apiwire"
	"github.com/JiaWeiXie/lensdock/internal/auth"
	"github.com/JiaWeiXie/lensdock/internal/netx"
	"github.com/JiaWeiXie/lensdock/internal/transfer"
)

// ErrUnsupportedPlatform is returned by Linux-only operations on other
// platforms.
var ErrUnsupportedPlatform = errors.New("supervisor: unsupported platform (linux only)")

// Environment variable names read by LoadConfig.
const (
	EnvOperatorTokenSHA256 = "LENSDOCK_OPERATOR_TOKEN_SHA256"
	EnvWorkspaceID         = "LENSDOCK_WORKSPACE_ID"
	EnvListen              = "LENSDOCK_LISTEN"
	EnvClipboardTTL        = "LENSDOCK_CLIPBOARD_TTL"

	// EnvTLSCertFile, EnvTLSKeyFile and EnvTLSClientCAFile turn the API
	// listener into an mTLS listener; they must be set together.
	EnvTLSCertFile     = "LENSDOCK_TLS_CERT_FILE"
	EnvTLSKeyFile      = "LENSDOCK_TLS_KEY_FILE"
	EnvTLSClientCAFile = "LENSDOCK_TLS_CLIENT_CA_FILE"
	// EnvTLSClientNames optionally restricts the accepted client
	// certificates to those carrying one of these comma-separated DNS or URI
	// subject alternative names.
	EnvTLSClientNames = "LENSDOCK_TLS_CLIENT_NAMES"
	// EnvHealthListen is the plain loopback listener that serves only
	// /healthz and /readyz while the API listener is TLS-only.
	EnvHealthListen = "LENSDOCK_HEALTH_LISTEN"

	// Transfer area limits.
	EnvFileQuotaBytes = "LENSDOCK_FILE_QUOTA_BYTES"
	EnvFileMaxBytes   = "LENSDOCK_FILE_MAX_BYTES"
	EnvFileMaxFiles   = "LENSDOCK_FILE_MAX_FILES"

	// EnvMode is the initial usage mode, NOVNC or AGENT (case-insensitive;
	// default AGENT). It applies only until a policy has been saved: the
	// saved policy wins afterwards. `supervise --mode` overrides it.
	EnvMode = "LENSDOCK_MODE"
	// EnvPolicyDir is the supervisor-private directory that persists the
	// usage policy (default /data/runtime). It must be a persistent volume
	// the browser user cannot access.
	EnvPolicyDir = "LENSDOCK_POLICY_DIR"
)

// DefaultPolicyDir is the default EnvPolicyDir.
const DefaultPolicyDir = "/data/runtime"

const (
	defaultWorkspaceID  = "default"
	defaultListen       = ":8080"
	defaultHealthListen = "127.0.0.1:8081"
	defaultClipboardTTL = 60 * time.Second
	maxClipboardTTL     = 10 * time.Minute
	maxClientNames      = 16
	maxClientNameLen    = 253
	maxFileLimit        = int64(1) << 44 // 16 TiB: only guards absurd values
)

var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// TLSConfig holds the mTLS file paths. The zero value means plain HTTP.
type TLSConfig struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
	// ClientNames, when non-empty, restricts accepted client certificates to
	// those with one of these DNS or URI SANs.
	ClientNames []string
}

// Enabled reports whether the API listener must use mTLS.
func (t TLSConfig) Enabled() bool { return t.CertFile != "" }

// Config is the runtime configuration derived from the environment.
type Config struct {
	WorkspaceID    string
	OperatorDigest auth.Digest
	Listen         string
	ClipboardTTL   time.Duration
	TLS            TLSConfig
	// HealthListen is the loopback listener used only when TLS is enabled.
	HealthListen string
	// File limits; zero means the transfer package default.
	FileQuotaBytes int64
	FileMaxBytes   int64
	FileMaxFiles   int
	// AgentMode is the initial usage mode: apiwire.AgentModeNOVNC or
	// apiwire.AgentModeAGENT.
	AgentMode string
	// PolicyDir persists the usage policy (absolute, supervisor-private).
	PolicyDir string
}

// LoadConfig reads and validates the runtime environment. Every problem is
// reported; error text names variables but never echoes the operator digest.
func LoadConfig(getenv func(string) string) (Config, error) {
	var cfg Config
	var errs []error

	if v := getenv(EnvOperatorTokenSHA256); v == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvOperatorTokenSHA256))
	} else if d, err := auth.ParseDigestHex(v); err != nil {
		errs = append(errs, fmt.Errorf("%s must be 64 hex characters", EnvOperatorTokenSHA256))
	} else {
		cfg.OperatorDigest = d
	}

	cfg.WorkspaceID = getenv(EnvWorkspaceID)
	if cfg.WorkspaceID == "" {
		cfg.WorkspaceID = defaultWorkspaceID
	} else if !workspaceIDPattern.MatchString(cfg.WorkspaceID) {
		errs = append(errs, fmt.Errorf("%s must match %s", EnvWorkspaceID, workspaceIDPattern))
	}

	cfg.Listen = getenv(EnvListen)
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	} else if err := validateListen(cfg.Listen); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvListen, err))
	}

	cfg.ClipboardTTL = defaultClipboardTTL
	if v := getenv(EnvClipboardTTL); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s must be a Go duration such as 60s", EnvClipboardTTL))
		case d <= 0 || d > maxClipboardTTL:
			errs = append(errs, fmt.Errorf("%s must be greater than 0 and at most %s", EnvClipboardTTL, maxClipboardTTL))
		default:
			cfg.ClipboardTTL = d
		}
	}

	errs = append(errs, loadTLS(getenv, &cfg)...)

	cfg.AgentMode = apiwire.AgentModeAGENT
	if v := getenv(EnvMode); v != "" {
		m, err := apiwire.ParseAgentMode(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvMode, err))
		} else {
			cfg.AgentMode = m
		}
	}
	cfg.PolicyDir = DefaultPolicyDir
	if v := getenv(EnvPolicyDir); v != "" {
		if !filepath.IsAbs(v) || filepath.Clean(v) != v {
			errs = append(errs, fmt.Errorf("%s must be a clean absolute path", EnvPolicyDir))
		} else {
			cfg.PolicyDir = v
		}
	}
	loadFileLimits(getenv, &cfg, &errs)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// loadTLS reads the mTLS and health-listener settings.
func loadTLS(getenv func(string) string, cfg *Config) []error {
	var errs []error
	t := TLSConfig{
		CertFile:     getenv(EnvTLSCertFile),
		KeyFile:      getenv(EnvTLSKeyFile),
		ClientCAFile: getenv(EnvTLSClientCAFile),
	}
	set := 0
	for _, f := range []struct{ env, path string }{
		{EnvTLSCertFile, t.CertFile}, {EnvTLSKeyFile, t.KeyFile}, {EnvTLSClientCAFile, t.ClientCAFile},
	} {
		if f.path == "" {
			continue
		}
		set++
		if !filepath.IsAbs(f.path) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path", f.env))
		}
	}
	if set != 0 && set != 3 {
		errs = append(errs, fmt.Errorf("%s, %s and %s must be set together", EnvTLSCertFile, EnvTLSKeyFile, EnvTLSClientCAFile))
	}
	if v := getenv(EnvTLSClientNames); v != "" {
		if set != 3 {
			errs = append(errs, fmt.Errorf("%s needs the TLS files to be configured", EnvTLSClientNames))
		}
		names := strings.Split(v, ",")
		if len(names) > maxClientNames {
			errs = append(errs, fmt.Errorf("%s lists more than %d names", EnvTLSClientNames, maxClientNames))
		}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" || len(n) > maxClientNameLen {
				errs = append(errs, fmt.Errorf("%s has an empty or overlong name", EnvTLSClientNames))
				break
			}
			t.ClientNames = append(t.ClientNames, n)
		}
	}
	cfg.TLS = t

	cfg.HealthListen = defaultHealthListen
	if v := getenv(EnvHealthListen); v != "" {
		cfg.HealthListen = v
	}
	host, _, err := net.SplitHostPort(cfg.HealthListen)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("%s: must be host:port: %w", EnvHealthListen, err))
	case !netx.IsLoopbackHost(host):
		errs = append(errs, fmt.Errorf("%s must bind a loopback address", EnvHealthListen))
	default:
		if err := validateListen(cfg.HealthListen); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvHealthListen, err))
		}
	}
	return errs
}

// loadFileLimits reads the transfer area limits. Unset variables keep zero,
// which the transfer package turns into its default; the effective pair is
// validated so a per-file limit can never exceed the quota.
func loadFileLimits(getenv func(string) string, cfg *Config, errs *[]error) {
	readInt := func(env string, max int64) (int64, bool) {
		v := getenv(env)
		if v == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > max {
			*errs = append(*errs, fmt.Errorf("%s must be a positive integer", env))
			return 0, false
		}
		return n, true
	}
	quota, okQ := readInt(EnvFileQuotaBytes, maxFileLimit)
	maxFile, okF := readInt(EnvFileMaxBytes, maxFileLimit)
	count, okC := readInt(EnvFileMaxFiles, 1<<31-1)
	if !okQ || !okF || !okC {
		return
	}
	cfg.FileQuotaBytes, cfg.FileMaxBytes, cfg.FileMaxFiles = quota, maxFile, int(count)
	effective := transfer.Limits{QuotaBytes: quota, MaxFileBytes: maxFile, MaxFiles: int(count)}.WithDefaults()
	if effective.MaxFileBytes > effective.QuotaBytes {
		*errs = append(*errs, fmt.Errorf("%s must not exceed %s", EnvFileMaxBytes, EnvFileQuotaBytes))
	}
}

func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be 1-65535")
	}
	return nil
}
