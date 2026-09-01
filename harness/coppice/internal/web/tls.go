package web

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"time"
)

// TLSSource is where the server's certificate comes from.
type TLSSource string

const (
	// TLSTailscale reads the pair tailscale cert wrote. Let's Encrypt issues
	// it, so the machine name lands on a public certificate transparency
	// ledger. docs/how-to/phone.md says so before the operator chooses.
	TLSTailscale TLSSource = "tailscale"
	// TLSLocalCA reads the pair coppice web cert init wrote. Nothing about
	// the box leaves it.
	TLSLocalCA TLSSource = "localca"
	// TLSFiles reads an explicit pair.
	TLSFiles TLSSource = "files"
	// TLSOff serves plain HTTP and refuses any address that is not loopback.
	// It exists so a browser on this box gets a real secure context, which
	// HTTPS with an untrusted certificate does not give.
	TLSOff TLSSource = "off"
)

// ExpiryWarnWindow is how long before expiry the server starts complaining.
// A tailscale certificate lives 90 days and nothing renews it on its own.
const ExpiryWarnWindow = 14 * 24 * time.Hour

// TLSOptions picks the certificate and key to serve. CADir is the directory
// the caller resolved for the local CA. Nothing here looks up a home
// directory or any other default on its own.
type TLSOptions struct {
	Source   TLSSource
	CertFile string
	KeyFile  string
	CADir    string
	Listen   string
}

// statMissing reports whether path is missing, and the stat error if it
// asked the file system and got one back. A permission error and a missing
// file are not the same problem, and the operator needs to know which one
// they are looking at.
func statMissing(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return false, nil
	}
	return true, err
}

// Resolve returns the certificate and key to serve, or an error that teaches
// the command which produces them.
func (o TLSOptions) Resolve() (string, string, error) {
	switch o.Source {
	case TLSOff:
		return "", "", RequireLoopback(o.Listen)
	case TLSFiles:
		if o.CertFile == "" || o.KeyFile == "" {
			return "", "", fmt.Errorf("--tls files needs both --cert and --key")
		}
		if missing, statErr := statMissing(o.CertFile); missing {
			return "", "", fmt.Errorf("--cert %s: %w", o.CertFile, statErr)
		}
		if missing, statErr := statMissing(o.KeyFile); missing {
			return "", "", fmt.Errorf("--key %s: %w", o.KeyFile, statErr)
		}
		return o.CertFile, o.KeyFile, nil
	case TLSTailscale:
		if o.CertFile == "" || o.KeyFile == "" {
			return "", "", fmt.Errorf("--tls tailscale needs --cert and --key. " +
				"Run tailscale cert --cert-file <path> --key-file <path> <name>.<tailnet>.ts.net")
		}
		if missing, statErr := statMissing(o.CertFile); missing {
			return "", "", fmt.Errorf("no tailscale certificate at %s: %w. "+
				"Run tailscale cert --cert-file %s --key-file %s <name>.<tailnet>.ts.net",
				o.CertFile, statErr, o.CertFile, o.KeyFile)
		}
		if missing, statErr := statMissing(o.KeyFile); missing {
			return "", "", fmt.Errorf("no tailscale key at %s: %w. "+
				"Run tailscale cert --cert-file %s --key-file %s <name>.<tailnet>.ts.net",
				o.KeyFile, statErr, o.CertFile, o.KeyFile)
		}
		return o.CertFile, o.KeyFile, nil
	case TLSLocalCA:
		if o.CADir == "" {
			return "", "", fmt.Errorf("--tls localca needs --ca-dir")
		}
		certFile, keyFile := CADir{Path: o.CADir}.LeafPaths()
		if missing, statErr := statMissing(certFile); missing {
			return "", "", fmt.Errorf("no local certificate at %s: %w. Run coppice web cert init", certFile, statErr)
		}
		if missing, statErr := statMissing(keyFile); missing {
			return "", "", fmt.Errorf("no local key at %s: %w. Run coppice web cert init", keyFile, statErr)
		}
		return certFile, keyFile, nil
	default:
		return "", "", fmt.Errorf("unknown --tls %q. Use tailscale, localca, files, or off", o.Source)
	}
}

// LoadLeaf parses the first certificate in a PEM file.
func LoadLeaf(certFile string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s holds no PEM certificate", certFile)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		raw = rest
	}
}

// ExpiryWarning is the line to log when a certificate is close to running
// out, and the empty string when it is not. Renewal is the operator's, so
// the warning has to arrive before the phone stops working.
func ExpiryWarning(c *x509.Certificate, now time.Time) string {
	left := c.NotAfter.Sub(now)
	if left > ExpiryWarnWindow {
		return ""
	}
	if left <= 0 {
		return "The certificate has expired. Renew it, then restart coppice web serve."
	}
	if left < 24*time.Hour {
		hours := int(left.Hours())
		if hours < 1 {
			hours = 1
		}
		return fmt.Sprintf("The certificate expires in %d %s. Renew it before then.", hours, plural(hours, "hour"))
	}
	days := int(left.Hours() / 24)
	return fmt.Sprintf("The certificate expires in %d %s. Renew it before then.", days, plural(days, "day"))
}

// plural returns unit unchanged for a count of one, and unit with an added
// s otherwise. "1 days" reads like a bug, not a deadline.
func plural(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// RequireLoopback refuses plain HTTP anywhere a second machine could reach.
// Every pane on the box is on the other side of this listener.
func RequireLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen %q is not host:port", listen)
	}
	if host == "" {
		return fmt.Errorf("--tls off needs a loopback address. Use --listen 127.0.0.1:8443")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--tls off serves plain HTTP, so it only listens on loopback. "+
			"Use --listen 127.0.0.1:8443, or pick --tls localca to reach %s", host)
	}
	return nil
}
