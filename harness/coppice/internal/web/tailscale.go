package web

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// What coppice web serve --tls tailscale does on its own: it reads this
// box's MagicDNS name and tailnet addresses from tailscale status --json,
// gets a certificate for that name with tailscale cert when there is none
// or it has under RenewWindow left, and listens on the tailnet addresses
// and loopback. The Rust port says the same words; the web cases compare
// them.

// RenewWindow is how much time a certificate must have left to be kept. A
// tailscale certificate lasts 90 days, so this renews it after about 60.
const RenewWindow = 30 * 24 * time.Hour

// CertCheckEvery is how often a running phone server checks its tailscale
// certificate again.
const CertCheckEvery = 24 * time.Hour

// NoTailscale is the one line for a box with no tailscale on PATH.
const NoTailscale = "tailscale is not on PATH. Install Tailscale on this box, then run coppice web serve --tls tailscale again."

// againLine ends each refusal below: what to run once the fix is done.
const againLine = "then run coppice web serve --tls tailscale again."

// Tailscale is what coppice reads from tailscale status --json: the name
// tailscale cert can get a certificate for, and the tailnet addresses.
type Tailscale struct {
	Name string
	IPs  []string
}

// tsStatus is the part of tailscale status --json coppice reads. The
// field names are tailscale's ipnstate.Status, which has no json tags on
// these fields.
type tsStatus struct {
	BackendState string
	Self         *struct {
		DNSName      string
		TailscaleIPs []string
	}
	CertDomains []string
}

// ReadTailscale runs tailscale status --json. Each error is one line that
// names the fix.
func ReadTailscale() (Tailscale, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return Tailscale{}, errors.New(NoTailscale)
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(bin, "status", "--json")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Tailscale{}, fmt.Errorf("tailscale status --json failed%s. Check that tailscaled runs, %s",
			lastLine(errb.String()), againLine)
	}
	var st tsStatus
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		return Tailscale{}, fmt.Errorf("tailscale status --json printed what coppice cannot read. Update Tailscale, %s", againLine)
	}
	if st.BackendState != "Running" {
		return Tailscale{}, fmt.Errorf("Tailscale is not running on this box (it says %s). Run tailscale up, %s",
			st.BackendState, againLine)
	}
	if len(st.CertDomains) == 0 {
		return Tailscale{}, fmt.Errorf("Tailscale HTTPS certificates are off for this tailnet. "+
			"Turn on MagicDNS and HTTPS in the Tailscale admin console, %s", againLine)
	}
	ts := Tailscale{Name: st.CertDomains[0]}
	if st.Self != nil {
		name := strings.TrimSuffix(st.Self.DNSName, ".")
		for _, d := range st.CertDomains {
			if d == name {
				ts.Name = name
			}
		}
		ts.IPs = append(ts.IPs, st.Self.TailscaleIPs...)
	}
	if len(ts.IPs) == 0 {
		return Tailscale{}, fmt.Errorf("Tailscale gives this box no tailnet address. Run tailscale up, %s", againLine)
	}
	return ts, nil
}

// lastLine is ": " and the last line of a command's error output that is
// not blank, or "" when there is none.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return ""
	}
	return ": " + last
}

// CertResult is what EnsureCert did. First is true when no certificate was
// there before, Got when tailscale cert ran, and Days is the whole days
// the certificate it leaves has left.
type CertResult struct {
	First bool
	Got   bool
	Days  int
}

// CertDays is the whole days from now to notAfter, rounded down, so a
// certificate that ran out an hour ago has -1.
func CertDays(notAfter, now time.Time) int {
	return int(math.Floor(notAfter.Sub(now).Hours() / 24))
}

// smallFileMax is the most readSmall reads. A certificate, a key or the
// serving file is a few KiB.
const smallFileMax = 64 << 10

// readSmall reads a regular file of at most smallFileMax bytes, so a
// device or a pipe named where a certificate belongs never hangs or
// floods a reader.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > smallFileMax {
		return nil, fmt.Errorf("%s is larger than 64 KiB", path)
	}
	return io.ReadAll(io.LimitReader(f, smallFileMax))
}

// keyIsPEM is true when the file holds a PEM private key.
func keyIsPEM(keyFile string) bool {
	raw, err := readSmall(keyFile)
	if err != nil {
		return false
	}
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return false
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			return true
		}
		raw = rest
	}
}

// mkdirOwn makes dir 0700 when it is not there. A directory that is
// already there keeps its mode: it may be shared.
func mkdirOwn(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// CheckPairFiles refuses, in one line, a certificate path that holds
// something other than a PEM certificate, or a key path that holds
// something other than a PEM private key, so tailscale cert never writes
// over a file it did not make. Paths that are not there pass.
func CheckPairFiles(certFile, keyFile string) error {
	if _, err := os.Stat(certFile); err == nil {
		if _, err := LoadLeaf(certFile); err != nil {
			return fmt.Errorf("%s is not a PEM certificate, so tailscale cert will not write over it. "+
				"Move it away or name another --cert, %s", certFile, againLine)
		}
	}
	if _, err := os.Stat(keyFile); err == nil && !keyIsPEM(keyFile) {
		return fmt.Errorf("%s is not a PEM private key, so tailscale cert will not write over it. "+
			"Move it away or name another --key, %s", keyFile, againLine)
	}
	return nil
}

// EnsureCert keeps the pair at certFile and keyFile when its certificate
// has RenewWindow or more left, and else runs tailscale cert for name
// into it. It never writes over a file that is not a PEM certificate, or
// a key file that is not a PEM private key: those are refused in one line.
func EnsureCert(name, certFile, keyFile string, now time.Time) (CertResult, error) {
	var res CertResult
	if err := CheckPairFiles(certFile, keyFile); err != nil {
		return res, err
	}
	if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
		res.First = true
	} else if leaf, err := LoadLeaf(certFile); err == nil && leaf.NotAfter.Sub(now) >= RenewWindow && keyIsPEM(keyFile) {
		res.Days = CertDays(leaf.NotAfter, now)
		return res, nil
	}
	for _, dir := range []string{filepath.Dir(certFile), filepath.Dir(keyFile)} {
		if err := mkdirOwn(dir); err != nil {
			return res, err
		}
	}
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return res, errors.New(NoTailscale)
	}
	var errb bytes.Buffer
	cmd := exec.Command(bin, "cert", "--cert-file="+certFile, "--key-file="+keyFile, name)
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("tailscale cert failed%s. If it says access denied, run sudo tailscale set --operator=$USER once, %s",
			lastLine(errb.String()), againLine)
	}
	leaf, err := LoadLeaf(certFile)
	if err != nil {
		return res, fmt.Errorf("tailscale cert wrote %s, but it is not a certificate: %w", certFile, err)
	}
	res.Got = true
	res.Days = CertDays(leaf.NotAfter, now)
	return res, nil
}

// CertLines are the lines coppice web serve prints about what EnsureCert
// did: none for a kept certificate. The first certificate also names the
// public certificate logs the name is now in.
func CertLines(name string, res CertResult) []string {
	if !res.Got {
		return nil
	}
	verb := "Renewed the certificate"
	if res.First {
		verb = "Got a certificate"
	}
	lines := []string{fmt.Sprintf("%s for %s. It runs out in %d %s.", verb, name, res.Days, plural(res.Days, "day"))}
	if res.First {
		lines = append(lines, "This name is now in public certificate logs: "+name+".")
	}
	return lines
}

// ListenAddrs is where the phone server listens under --tls tailscale. A
// listen address with no host, such as :8443, means each tailnet address
// and 127.0.0.1 on that port. A listen address with a host is used as
// given.
func ListenAddrs(listen string, ips []string) ([]string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("--listen %q is not host:port", listen)
	}
	if host != "" {
		return []string{listen}, nil
	}
	var out []string
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, port))
	}
	return append(out, net.JoinHostPort("127.0.0.1", port)), nil
}

// ListeningLine is the one line coppice web serve --tls tailscale prints
// once it listens: the address the phone opens, and where it listens.
// only is true when the listen address had no host, so the server
// listens on the tailnet and loopback alone.
func ListeningLine(url string, addrs []string, only bool) string {
	line := "Serving " + url + " on " + strings.Join(addrs, " and ") + "."
	if only {
		line += " Only the tailnet and this box can reach it."
	}
	return line
}

// BlankHost is true when listen is host:port with no host, such as :8443.
func BlankHost(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	return err == nil && host == ""
}

// CertReloader serves the pair on disk, and reads it again when the
// certificate file changes, so a renewal reaches the next handshake with
// no restart.
type CertReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	mod               time.Time
	size              int64
	cert              *tls.Certificate
}

// NewCertReloader loads the pair once, so a bad pair fails at start.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.GetCertificate(nil); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate is tls.Config's GetCertificate. A pair that no longer
// loads keeps the last good one in service.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, err := os.Stat(r.certFile)
	if err == nil && r.cert != nil && info.ModTime().Equal(r.mod) && info.Size() == r.size {
		return r.cert, nil
	}
	pair, loadErr := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if loadErr != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, loadErr
	}
	r.cert = &pair
	if err == nil {
		r.mod, r.size = info.ModTime(), info.Size()
	}
	return r.cert, nil
}
