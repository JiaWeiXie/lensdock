package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JiaWeiXie/lensdock/internal/supervisor"
)

func TestHealthURLWithTLSUsesTheLoopbackHealthListener(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default health listen", map[string]string{"LENSDOCK_TLS_CERT_FILE": "/x", "LENSDOCK_LISTEN": ":9443"}, "http://127.0.0.1:8081/readyz"},
		{"custom health listen", map[string]string{"LENSDOCK_TLS_CERT_FILE": "/x", "LENSDOCK_HEALTH_LISTEN": "127.0.0.1:9100"}, "http://127.0.0.1:9100/readyz"},
		{"health listen wins over listen", map[string]string{"LENSDOCK_TLS_CERT_FILE": "/x", "LENSDOCK_LISTEN": ":8080", "LENSDOCK_HEALTH_LISTEN": "[::1]:9100"}, "http://[::1]:9100/readyz"},
		{"malformed health listen falls back", map[string]string{"LENSDOCK_TLS_CERT_FILE": "/x", "LENSDOCK_HEALTH_LISTEN": "nope"}, "http://127.0.0.1:8081/readyz"},
		{"no tls ignores health listen", map[string]string{"LENSDOCK_LISTEN": ":9090", "LENSDOCK_HEALTH_LISTEN": "127.0.0.1:9100"}, "http://127.0.0.1:9090/readyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := healthURL(func(k string) string { return tt.env[k] }); got != tt.want {
				t.Fatalf("healthURL = %s, want %s", got, tt.want)
			}
		})
	}
}

type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, cn string) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &pki{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf. clientAuth selects the extended key usage.
func (ca *pki) issue(t *testing.T, cn string, dns []string, ips []net.IP, uris []*url.URL, client bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	eku := x509.ExtKeyUsageServerAuth
	if client {
		eku = x509.ExtKeyUsageClientAuth
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     dns,
		IPAddresses:  ips,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMutualTLSListenerAdmitsOnlyTheGatewayIdentity(t *testing.T) {
	ca := newCA(t, "lensdock-test-ca")
	otherCA := newCA(t, "someone-else")
	server := ca.issue(t, "lensdock-runtime", []string{"runtime", "localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, nil, false)
	gateway := ca.issue(t, "lensdock-gateway", []string{"gateway", "localhost"}, nil, nil, true)
	cnOnly := ca.issue(t, "gateway", nil, nil, nil, true) // CN matches, no SAN
	stranger := ca.issue(t, "x", []string{"intruder"}, nil, nil, true)
	foreign := otherCA.issue(t, "lensdock-gateway", []string{"gateway"}, nil, nil, true)
	serverAuthOnly := ca.issue(t, "gateway", []string{"gateway"}, nil, nil, false) // serverAuth EKU only
	uriClient := ca.issue(t, "svc", nil, nil, []*url.URL{{Scheme: "spiffe", Host: "lensdock", Path: "/gateway"}}, true)

	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, "runtime.crt"), "CERTIFICATE", server.Certificate[0])
	keyDER, err := x509.MarshalECPrivateKey(server.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, "runtime.key"), "EC PRIVATE KEY", keyDER)
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}

	conf, err := buildTLS(supervisor.TLSConfig{
		CertFile:     filepath.Join(dir, "runtime.crt"),
		KeyFile:      filepath.Join(dir, "runtime.key"),
		ClientCAFile: filepath.Join(dir, "ca.crt"),
		ClientNames:  []string{"Gateway", "spiffe://lensdock/gateway"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if conf.MinVersion != tls.VersionTLS13 || conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("tls config = min %x clientAuth %v", conf.MinVersion, conf.ClientAuth)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = conf
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	get := func(certs ...tls.Certificate) error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots, Certificates: certs, MinVersion: tls.VersionTLS13,
		}}, Timeout: 5 * time.Second}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := get(gateway); err != nil {
		t.Fatalf("gateway identity rejected: %v", err)
	}
	if err := get(uriClient); err != nil {
		t.Fatalf("URI SAN identity rejected: %v", err)
	}
	for name, cert := range map[string]tls.Certificate{
		"CN only, no SAN":       cnOnly,
		"valid CA, wrong SAN":   stranger,
		"foreign CA":            foreign,
		"serverAuth-only usage": serverAuthOnly,
	} {
		if err := get(cert); err == nil {
			t.Errorf("%s: handshake succeeded, want rejection", name)
		}
	}
	if err := get(); err == nil {
		t.Error("no client certificate: handshake succeeded, want rejection")
	}

	// A TLS 1.2 client is refused.
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, Certificates: []tls.Certificate{gateway}, MaxVersion: tls.VersionTLS12,
	}}, Timeout: 5 * time.Second}
	if resp, err := c.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Error("TLS 1.2 client was accepted")
	}
}

func TestBuildTLSWithoutConfigurationIsPlain(t *testing.T) {
	conf, err := buildTLS(supervisor.TLSConfig{})
	if conf != nil || err != nil {
		t.Fatalf("buildTLS(zero) = %v, %v, want nil, nil", conf, err)
	}
}

func TestBuildTLSRejectsUnusableFiles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildTLS(supervisor.TLSConfig{CertFile: empty, KeyFile: empty, ClientCAFile: empty}); err == nil {
		t.Fatal("empty certificate files must be rejected")
	}
	if _, err := buildTLS(supervisor.TLSConfig{CertFile: filepath.Join(dir, "missing"), KeyFile: empty, ClientCAFile: empty}); err == nil {
		t.Fatal("a missing certificate must be rejected")
	}
}
