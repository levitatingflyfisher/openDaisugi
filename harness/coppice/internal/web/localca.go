package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// CAValidity is ten years. The operator installs this once on a phone and
	// should not be asked again.
	CAValidity = 10 * 365 * 24 * time.Hour
	// LeafValidity is 398 days, the browsers' ceiling for a server
	// certificate. Staying under it keeps the local path behaving like the
	// public one.
	LeafValidity = 398 * 24 * time.Hour
)

// CAMeta is what the leaf currently covers. cert show reads it so the
// operator does not have to parse a certificate to answer which addresses
// the leaf covers.
type CAMeta struct {
	Version  int       `json:"version"`
	Names    []string  `json:"names"`
	IPs      []string  `json:"ips"`
	Created  time.Time `json:"created"`
	NotAfter time.Time `json:"not_after"`
}

// CADir is one CA directory: one CA the phone installs once, and one leaf
// reissued whenever the box changes address. The caller resolves Path before
// building this value.
type CADir struct{ Path string }

func (d CADir) file(name string) string { return filepath.Join(d.Path, name) }

// LeafPaths are the server certificate and key --tls localca serves.
func (d CADir) LeafPaths() (string, string) { return d.file("leaf.crt"), d.file("leaf.key") }

// CAPEM is the certificate the phone installs.
func (d CADir) CAPEM() ([]byte, error) { return os.ReadFile(d.file("ca.crt")) }

// Meta reads what the current leaf covers.
func (d CADir) Meta() (CAMeta, error) {
	raw, err := os.ReadFile(d.file("meta.json"))
	if err != nil {
		return CAMeta{}, err
	}
	var m CAMeta
	err = json.Unmarshal(raw, &m)
	return m, err
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writeFileMode(path string, body []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, body, mode); err != nil {
		return err
	}
	// WriteFile only applies mode when it creates the file, so an existing
	// file keeps whatever mode it already had. Set it again here.
	return os.Chmod(path, mode)
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	return writeFileMode(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), mode)
}

// loadCA names the exact file a failure came from. A corrupt ca.key and a
// corrupt ca.crt read as two different errors, so an operator restoring a
// backup that truncated one file can restore only that one and keep the
// root every phone in the house already trusts.
func (d CADir) loadCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certFile := d.file("ca.crt")
	keyFile := d.file("ca.key")
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("%s is not a PEM file", certFile)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("%s is not a PEM file", keyFile)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certFile, err)
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	key, ok := anyKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not an ECDSA key", keyFile)
	}
	return cert, key, nil
}

// createCA mints the root the phone installs once. It uses P-256 rather than
// RSA: the certificate lands near 670 bytes of PEM, and a QR of it is still
// narrow enough to draw in a terminal. An RSA-2048 root would be twice that
// size and the code would be a wall nobody can scan.
func (d CADir) createCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "openDaisugi coppice local CA", Organization: []string{"openDaisugi"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	// SubjectKeyId is left empty. For a CA template Go derives it from the
	// public key.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(d.file("ca.crt"), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	if err := writePEM(d.file("ca.key"), "PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// Init makes the CA if there is none, keeps it if there is, and always
// reissues the leaf for the given names and addresses. Keeping the CA is the
// whole point: the operator installs it on the phone once, and a change of
// address must not undo that.
func (d CADir) Init(names []string, ips []net.IP, now time.Time) (CAMeta, error) {
	if len(names) == 0 && len(ips) == 0 {
		return CAMeta{}, errors.New("give at least one name or one address")
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return CAMeta{}, err
	}
	if err := os.Chmod(d.Path, 0o700); err != nil {
		return CAMeta{}, err
	}

	// Create only when there is nothing there. Every other load failure is a
	// CA that exists and will not read: a truncated key, a file someone
	// edited, the wrong key type. Minting a new root over it would break the
	// one promise this design makes, that the phone installs a certificate
	// once, and it would break it silently at the worst moment.
	var (
		caCert *x509.Certificate
		caKey  *ecdsa.PrivateKey
		err    error
	)
	if _, statErr := os.Stat(d.file("ca.crt")); errors.Is(statErr, os.ErrNotExist) {
		// A private key with no certificate is a partial CA, not an absent
		// one. Minting a new root here would overwrite that key with no
		// warning, and the key might be the only copy of an existing CA.
		if _, keyErr := os.Stat(d.file("ca.key")); keyErr == nil {
			return CAMeta{}, fmt.Errorf(
				"%s is present but %s is not. A new CA would overwrite that key. "+
					"Move %s aside, or restore %s, then run this again",
				d.file("ca.key"), d.file("ca.crt"), d.file("ca.key"), d.file("ca.crt"))
		}
		caCert, caKey, err = d.createCA(now)
		if err != nil {
			return CAMeta{}, err
		}
	} else {
		caCert, caKey, err = d.loadCA()
		if err != nil {
			return CAMeta{}, fmt.Errorf(
				"the CA in %s will not load: %w. The phone already trusts it, so this "+
					"command will not replace it. Move %s aside to start a new one, then "+
					"install the new CA on every phone", d.Path, err, d.Path)
		}
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CAMeta{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return CAMeta{}, err
	}
	cn := ""
	if len(names) > 0 {
		cn = names[0]
	} else {
		cn = ips[0].String()
	}
	notBefore := now.Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"openDaisugi"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(LeafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return CAMeta{}, err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return CAMeta{}, err
	}
	certFile, keyFile := d.LeafPaths()
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return CAMeta{}, err
	}
	if err := writePEM(keyFile, "PRIVATE KEY", leafKeyDER, 0o600); err != nil {
		return CAMeta{}, err
	}

	meta := CAMeta{Version: 1, Names: names, Created: now.UTC(), NotAfter: tmpl.NotAfter.UTC()}
	for _, ip := range ips {
		meta.IPs = append(meta.IPs, ip.String())
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return CAMeta{}, err
	}
	if err := writeFileMode(d.file("meta.json"), body, 0o600); err != nil {
		return CAMeta{}, err
	}
	return meta, nil
}
