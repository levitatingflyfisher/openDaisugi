//go:build unix

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/web"
)

// writeCertFor writes a self-signed certificate at path that runs out
// after left.
func writeCertFor(t *testing.T, path string, left time.Duration) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "box.tail1.ts.net"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(left),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func phoneCertFact(t *testing.T, s *Server) (any, bool) {
	t.Helper()
	m := result(t, roundTrip(t, s, `{"id":"f","cmd":"floor.facts"}`)[0])
	v, ok := m["phone_cert_days"]
	return v, ok
}

// Under 7 days left on the phone's tailscale certificate, floor.facts
// says how many whole days are left, so the header can warn with Renew.
func TestFloorFactsCarriesPhoneCertDaysUnderSevenDays(t *testing.T) {
	s := routerServer(t)
	certFile := filepath.Join(s.cfg.DataDir, "elsewhere", "phone.crt")
	if err := web.SaveConfig(web.ConfigPath(s.cfg.DataDir), web.Config{Enabled: true, Listen: ":8443", TLS: "tailscale", CertFile: certFile}); err != nil {
		t.Fatal(err)
	}
	writeCertFor(t, certFile, 3*24*time.Hour-time.Minute)
	if v, ok := phoneCertFact(t, s); !ok || v != float64(2) {
		t.Fatalf("phone_cert_days %v %v, want 2", v, ok)
	}
	writeCertFor(t, certFile, 20*24*time.Hour)
	if v, ok := phoneCertFact(t, s); ok {
		t.Fatalf("phone_cert_days %v with 20 days left, want absent", v)
	}
	writeCertFor(t, certFile, -time.Hour)
	if v, ok := phoneCertFact(t, s); !ok || v != float64(-1) {
		t.Fatalf("phone_cert_days %v %v for a certificate that ran out, want -1", v, ok)
	}
}

// A running tailscale serve says which certificate it serves, so the
// warning follows it to any path, whatever web.json says.
func TestFloorFactsReadsTheCertificateTheRunningServeNames(t *testing.T) {
	s := routerServer(t)
	certFile := filepath.Join(s.cfg.DataDir, "my", "own.crt")
	writeCertFor(t, certFile, 2*24*time.Hour+time.Hour)
	if err := web.SaveConfig(web.ConfigPath(s.cfg.DataDir), web.Config{Enabled: true, Listen: ":8443", TLS: "localca"}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid": %d, "cert_file": %q}`, os.Getpid(), certFile)
	if err := os.WriteFile(web.ServingPath(s.cfg.DataDir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := phoneCertFact(t, s); !ok || v != float64(2) {
		t.Fatalf("phone_cert_days %v %v, want 2", v, ok)
	}
}

// The warning is only about a phone server that runs now or a saved one.
// A pair from a try that never served, or a setup turned off with
// --forget, warns about nothing. A saved setup with no cert_file reads
// the default pair.
func TestFloorFactsWarnsOnlyForARunningOrSavedServe(t *testing.T) {
	s := routerServer(t)
	certFile, _ := web.TailscalePaths(s.cfg.DataDir)
	writeCertFor(t, certFile, 24*time.Hour+time.Minute)
	if v, ok := phoneCertFact(t, s); ok {
		t.Fatalf("phone_cert_days %v with no serve and no saved setup, want absent", v)
	}
	cfg := web.Config{Enabled: true, Listen: ":8443", TLS: "tailscale"}
	if err := web.SaveConfig(web.ConfigPath(s.cfg.DataDir), cfg); err != nil {
		t.Fatal(err)
	}
	if v, ok := phoneCertFact(t, s); !ok || v != float64(1) {
		t.Fatalf("phone_cert_days %v %v for the saved setup, want 1", v, ok)
	}
	cfg.Enabled = false
	if err := web.SaveConfig(web.ConfigPath(s.cfg.DataDir), cfg); err != nil {
		t.Fatal(err)
	}
	if v, ok := phoneCertFact(t, s); ok {
		t.Fatalf("phone_cert_days %v after --forget, want absent", v)
	}
	if err := web.SaveConfig(web.ConfigPath(s.cfg.DataDir), web.Config{Enabled: true, Listen: ":8443", TLS: "localca"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := phoneCertFact(t, s); ok {
		t.Fatalf("phone_cert_days %v under --tls localca, want absent", v)
	}
	if err := os.WriteFile(web.ConfigPath(s.cfg.DataDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := phoneCertFact(t, s); ok {
		t.Fatalf("phone_cert_days %v with an unreadable web.json, want absent", v)
	}
}
