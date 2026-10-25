package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/JiaWeiXie/lensdock/internal/supervisor"
)

// buildTLS loads the runtime's mTLS configuration, or returns nil when TLS is
// not configured. Call it before DropPrivileges: the key file is typically
// readable only by root.
//
// The listener demands a client certificate that chains to the configured CA
// and carries the clientAuth extended key usage (the Go default for client
// verification). When client names are configured, the certificate must also
// carry one of them as a DNS or URI SAN; the subject common name is never
// used. TLS 1.3 only, and HTTP/1.1 only, because the VNC and CDP proxies need
// WebSocket upgrades.
func buildTLS(c supervisor.TLSConfig) (*tls.Config, error) {
	if !c.Enabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load runtime certificate: %w", err)
	}
	caPEM, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("client CA file contains no certificate")
	}
	conf := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"http/1.1"},
	}
	if len(c.ClientNames) > 0 {
		names := make(map[string]bool, len(c.ClientNames))
		for _, n := range c.ClientNames {
			names[strings.ToLower(n)] = true
		}
		// VerifyConnection also runs on resumed sessions.
		conf.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !clientNameAllowed(cs.PeerCertificates[0], names) {
				return errors.New("client certificate name is not allowed")
			}
			return nil
		}
	}
	return conf, nil
}

// clientNameAllowed reports whether the leaf carries an allowed DNS or URI SAN.
func clientNameAllowed(leaf *x509.Certificate, names map[string]bool) bool {
	for _, n := range leaf.DNSNames {
		if names[strings.ToLower(n)] {
			return true
		}
	}
	for _, u := range leaf.URIs {
		if names[strings.ToLower(u.String())] {
			return true
		}
	}
	return false
}
