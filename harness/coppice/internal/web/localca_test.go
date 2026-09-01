package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CLI resolves the data directory once and passes it in. This package
// never reads the home directory on its own, so the default path is a
// function of that data directory, not a lookup.
func TestLocalCADirIsWebCAUnderTheDataDir(t *testing.T) {
	got := LocalCADir("/srv/coppice-data")
	want := filepath.Join("/srv/coppice-data", "web", "ca")
	if got != want {
		t.Fatalf("LocalCADir returned %q, want %q", got, want)
	}
}

func initCA(t *testing.T) (CADir, CAMeta) {
	t.Helper()
	d := CADir{Path: filepath.Join(t.TempDir(), "ca")}
	meta, err := d.Init([]string{"localhost", "box.tail1234.ts.net"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return d, meta
}

func TestInitWritesTheDocumentedLayoutWithPrivateModes(t *testing.T) {
	d, _ := initCA(t)
	for name, want := range map[string]os.FileMode{
		"ca.crt":    0o644,
		"ca.key":    0o600,
		"leaf.crt":  0o644,
		"leaf.key":  0o600,
		"meta.json": 0o600,
	} {
		fi, err := os.Stat(filepath.Join(d.Path, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode is %o, want %o", name, fi.Mode().Perm(), want)
		}
	}
	di, err := os.Stat(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("ca directory mode is %o, want 700", di.Mode().Perm())
	}
}

func TestTheLeafChainsToTheCAAndCarriesEveryNameAndAddress(t *testing.T) {
	d, _ := initCA(t)
	caPEM, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt is not a PEM certificate")
	}
	certFile, _ := d.LeafPaths()
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("the leaf does not verify against its own CA: %v", err)
	}
	if err := leaf.VerifyHostname("box.tail1234.ts.net"); err != nil {
		t.Errorf("the second name is missing: %v", err)
	}
	found := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			found = true
		}
	}
	if !found {
		t.Error("127.0.0.1 is not in the leaf's addresses")
	}
}

// The honest proof: a real handshake, not just a chain that parses.
func TestARealTLSHandshakeSucceedsWithTheCAInTheRootPool(t *testing.T) {
	d, _ := initCA(t)
	certFile, keyFile := d.LeafPaths()
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from the box")
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	ts.StartTLS()
	defer ts.Close()

	caPEM, _ := d.CAPEM()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Get(ts.URL)
	if err != nil {
		t.Fatalf("handshake against our own CA failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from the box" {
		t.Fatalf("body was %q", body)
	}
}

// Re-running cert init after the box changes address must not ask the
// operator to install a second CA on the phone.
func TestReInitKeepsTheSameCAAndReissuesOnlyTheLeaf(t *testing.T) {
	d, _ := initCA(t)
	before, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	certFile, _ := d.LeafPaths()
	leafBefore, _ := os.ReadFile(certFile)

	if _, err := d.Init([]string{"localhost"}, []net.IP{net.ParseIP("10.0.0.5")}, time.Now()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.CAPEM()
	if string(before) != string(after) {
		t.Fatal("re-init replaced the CA the phone already trusts")
	}
	leafAfter, _ := os.ReadFile(certFile)
	if string(leafBefore) == string(leafAfter) {
		t.Fatal("re-init did not reissue the leaf")
	}
	leaf, _ := LoadLeaf(certFile)
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Errorf("the new address is missing from the reissued leaf: %v", err)
	}
}

func TestTheLeafDoesNotOutliveThreeHundredNinetyEightDays(t *testing.T) {
	d, _ := initCA(t)
	certFile, _ := d.LeafPaths()
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 398*24*time.Hour {
		t.Fatalf("leaf validity is %v, over the browser limit", leaf.NotAfter.Sub(leaf.NotBefore))
	}
}

// LeafValidity itself must not drift past the browser ceiling. This locks
// the constant, not just one measured leaf.
func TestLeafValidityDoesNotExceedTheBrowserCeiling(t *testing.T) {
	if LeafValidity > 398*24*time.Hour {
		t.Fatalf("LeafValidity is %v, over the 398-day browser ceiling", LeafValidity)
	}
}

// The failure this names: treating every load failure as "there is no CA
// yet" means one corrupt byte in ca.key silently mints a new root, and every
// phone in the house stops trusting the box with nothing on screen to say
// why. Re-init keeps the CA or it refuses. It never replaces it.
func TestInitRefusesRatherThanReplacingACAItCannotRead(t *testing.T) {
	d, _ := initCA(t)
	before, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path, "ca.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Init([]string{"localhost"}, nil, time.Now()); err == nil {
		t.Fatal("Init replaced a CA it could not read")
	} else if !strings.Contains(err.Error(), "will not load") {
		t.Errorf("the error does not say what happened: %v", err)
	}
	after, _ := d.CAPEM()
	if string(before) != string(after) {
		t.Fatal("ca.crt was overwritten, so every phone that trusted it is now locked out")
	}
}

// The failure this names: a corrupt ca.key and a corrupt ca.crt must not
// read as the same failure. An operator who restores a backup that
// truncated one file needs to know which one, so they can restore only
// that file and keep the root every phone in the house already trusts.
func TestInitRefusalNamesTheCorruptKeyFile(t *testing.T) {
	d, _ := initCA(t)
	if err := os.WriteFile(filepath.Join(d.Path, "ca.key"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.Init([]string{"localhost"}, nil, time.Now())
	if err == nil {
		t.Fatal("Init replaced a CA it could not read")
	}
	if !strings.Contains(err.Error(), "ca.key") {
		t.Fatalf("the error does not name ca.key: %v", err)
	}
	if strings.Contains(err.Error(), "ca.crt") {
		t.Fatalf("the error blames ca.crt for a corrupt ca.key: %v", err)
	}
}

func TestInitRefusalNamesTheCorruptCertFile(t *testing.T) {
	d, _ := initCA(t)
	if err := os.WriteFile(filepath.Join(d.Path, "ca.crt"), []byte("not a cert\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := d.Init([]string{"localhost"}, nil, time.Now())
	if err == nil {
		t.Fatal("Init replaced a CA it could not read")
	}
	if !strings.Contains(err.Error(), "ca.crt") {
		t.Fatalf("the error does not name ca.crt: %v", err)
	}
	if strings.Contains(err.Error(), "ca.key") {
		t.Fatalf("the error blames ca.key for a corrupt ca.crt: %v", err)
	}
}

// The failure this names: a stray delete leaves a private key with no
// certificate. Minting a new root over that key throws away recovery value
// with no warning, and it breaks the same promise Init already keeps for a
// key that fails to parse.
func TestInitRefusesToOverwriteAnOrphanedCAKey(t *testing.T) {
	d, _ := initCA(t)
	if err := os.Remove(filepath.Join(d.Path, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(filepath.Join(d.Path, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Init([]string{"localhost"}, nil, time.Now()); err == nil {
		t.Fatal("Init minted a new CA over a private key that might be the only copy of an existing one")
	}
	keyAfter, err := os.ReadFile(filepath.Join(d.Path, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(keyBefore) != string(keyAfter) {
		t.Fatal("ca.key changed, so the old CA's private key is gone")
	}
}

// The failure this names: a certificate for nothing at all is a certificate
// no browser will accept, and the operator should hear that now.
func TestInitRefusesWithNoNameAndNoAddress(t *testing.T) {
	d := CADir{Path: filepath.Join(t.TempDir(), "ca")}
	if _, err := d.Init(nil, nil, time.Now()); err == nil {
		t.Fatal("Init accepted an empty name and address list")
	}
}

func TestMetaRecordsWhatTheLeafCovers(t *testing.T) {
	d, meta := initCA(t)
	read, err := d.Meta()
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Names) != 2 || read.Names[0] != "localhost" {
		t.Fatalf("meta names are %v", read.Names)
	}
	if len(read.IPs) != 1 || read.IPs[0] != "127.0.0.1" {
		t.Fatalf("meta IPs are %v", read.IPs)
	}
	if read.NotAfter.Before(meta.Created) {
		t.Fatal("meta.NotAfter is before meta.Created")
	}
}

// The CA and the leaf both stay on P-256, so ca.crt stays near 664 bytes of
// PEM, small enough for a QR a phone camera can read. The leaf also
// declares server auth, the one extended key usage a browser checks.
func TestTheCAAndLeafKeysAreP256AndTheLeafDeclaresServerAuth(t *testing.T) {
	d, _ := initCA(t)
	caPEM, err := d.CAPEM()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		t.Fatal("ca.crt is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKey, ok := ca.PublicKey.(*ecdsa.PublicKey)
	if !ok || caKey.Curve != elliptic.P256() {
		t.Fatalf("the CA key is not P-256: %T", ca.PublicKey)
	}
	certFile, _ := d.LeafPaths()
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || leafKey.Curve != elliptic.P256() {
		t.Fatalf("the leaf key is not P-256: %T", leaf.PublicKey)
	}
	found := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			found = true
		}
	}
	if !found {
		t.Fatal("the leaf does not declare server auth")
	}
}
