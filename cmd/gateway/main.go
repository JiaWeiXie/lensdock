// Command gateway serves the LensDock UI and authenticated collaboration API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/gateway"
)

// shutdownTimeout bounds the whole orderly stop: ending streams, finishing
// in-flight requests and waiting for every handler.
const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lensdock-gateway:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 1 && args[0] == "healthcheck" {
		return healthcheck()
	}
	if len(args) == 1 && args[0] == "prepare-config" {
		return prepareConfig()
	}
	if len(args) > 0 && args[0] == "migrate" {
		return migrate(args[1:])
	}
	if len(args) == 0 || args[0] != "serve" {
		return errors.New("usage: lensdock-gateway serve --config FILE | healthcheck")
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("config", "", "private Gateway configuration")
	pairingFlag := fs.Bool("require-pairing", false, "require a pairing token for local browser access (also LENSDOCK_REQUIRE_PAIRING)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("--config is required")
	}
	c, err := gateway.LoadConfig(*path)
	if err != nil {
		return err
	}
	c.RequirePairing, err = pairingRequired(c.RequirePairing, os.Getenv("LENSDOCK_REQUIRE_PAIRING"))
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "require-pairing" {
			c.RequirePairing = *pairingFlag
		}
	})
	g, err := gateway.New(c, gateway.Integrations{})
	if err != nil {
		return err
	}
	defer g.Close()
	srv := gateway.Server(c, g)
	if c.TLSCertFile != "" || c.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
		if err != nil {
			return err
		}
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	} else {
		u, _ := url.Parse(c.Origin)
		if u.Scheme == "https" {
			return errors.New("HTTPS origin requires Gateway TLS certificate and key")
		}
	}
	// Credential files are loaded before the one-way privilege drop. Only the
	// three volume roots are prepared; existing files are never recursively chmodded.
	for _, name := range []string{"db", "private", "artifacts"} {
		p := filepath.Join(c.DataDir, name)
		if err := os.MkdirAll(p, 0700); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(p, 10000, 10000); err != nil {
				return err
			}
		}
	}
	if err := dropPrivileges(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	closeFeedback, err := g.OpenFeedback(ctx)
	if err != nil {
		return err
	}
	defer closeFeedback()
	failure := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			failure <- srv.ListenAndServeTLS("", "")
		} else {
			failure <- srv.ListenAndServe()
		}
	}()
	// closeFeedback and g.Close are deferred above, so storage is released only
	// after Shutdown has ended every stream and waited for its handler, whatever
	// made the server stop. Deadline errors from ordinary requests still surface.
	stopServing := func() error {
		timeout, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return g.Shutdown(timeout, srv)
	}
	select {
	case err := <-failure:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		if serr := stopServing(); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	case <-ctx.Done():
		return stopServing()
	}
}

// pairingRequired applies an explicit environment setting over the config file.
func pairingRequired(configured bool, env string) (bool, error) {
	if env == "" {
		return configured, nil
	}
	required, err := strconv.ParseBool(env)
	if err != nil {
		return false, errors.New("LENSDOCK_REQUIRE_PAIRING must be a boolean")
	}
	return required, nil
}
func healthcheck() error {
	origin := os.Getenv("LENSDOCK_HEALTH_ORIGIN")
	if origin == "" {
		origin = "http://127.0.0.1:18081"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errors.New("invalid health origin")
	}
	r, err := http.NewRequest("GET", "http://127.0.0.1:8080/healthz", nil)
	if err != nil {
		return err
	}
	r.Host = u.Host
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}
