package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// TailnetRetryMax is the longest a saved phone server waits between two
// tries to bind its tailnet address at boot.
const TailnetRetryMax = 60 * time.Second

// TailnetRetryFirst is the first wait.
const TailnetRetryFirst = 2 * time.Second

// NextTailnetRetry doubles a wait, up to TailnetRetryMax.
func NextTailnetRetry(d time.Duration) time.Duration {
	d *= 2
	if d > TailnetRetryMax {
		d = TailnetRetryMax
	}
	return d
}

// skippedAddr is a tailnet address that would not bind, and why.
type skippedAddr struct {
	addr string
	err  error
}

// tailnetRanges are the addresses Tailscale gives a device: the CGNAT
// range for IPv4 and its own ULA prefix for IPv6.
var tailnetRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// TailnetAddr is true for an address the serve may bind as a tailnet
// address: one inside Tailscale's ranges, or a loopback address, which
// reaches only this box. An empty, unspecified or unparsable address is
// never one, so nothing tailscale reports can widen the bind.
func TailnetAddr(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Zone() != "" || a.IsUnspecified() {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return true
	}
	for _, p := range tailnetRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ListenStaysOnTailnet is true when a listen address with a host names
// loopback or a tailnet address.
func ListenStaysOnTailnet(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	return host == "localhost" || TailnetAddr(host)
}

// bindTailnet binds each tailnet address on port. It returns what it
// bound, their addresses, the ones it skipped because they would not
// bind, and the ones it never tried because they are not tailnet
// addresses.
func bindTailnet(ips []string, port string) ([]net.Listener, []string, []skippedAddr, []string) {
	var lns []net.Listener
	var addrs []string
	var skipped []skippedAddr
	var foreign []string
	for _, ip := range ips {
		if !TailnetAddr(ip) {
			foreign = append(foreign, ip)
			continue
		}
		a := net.JoinHostPort(ip, port)
		ln, err := net.Listen("tcp", a)
		if err != nil {
			skipped = append(skipped, skippedAddr{a, err})
			continue
		}
		lns = append(lns, ln)
		addrs = append(addrs, a)
	}
	return lns, addrs, skipped, foreign
}

// logForeign writes one line for each address tailscale gave that is not
// a tailnet address.
func logForeign(log *slog.Logger, foreign []string) {
	for _, ip := range foreign {
		log.Warn("web: skipped an address that is not a tailnet address", "addr", ip)
	}
}

// logSkipped writes one line for each tailnet address that would not
// bind.
func logSkipped(log *slog.Logger, skipped []skippedAddr) {
	for _, s := range skipped {
		log.Warn("web: skipped a tailnet address it cannot bind", "addr", s.addr, "err", s.err)
	}
}

// lastSkip is the error of the last skipped address, or nil.
func lastSkip(skipped []skippedAddr) error {
	if len(skipped) == 0 {
		return nil
	}
	return skipped[len(skipped)-1].err
}

// noTailnetBind is the error for a serve that could bind no tailnet
// address.
func noTailnetBind(last error) error {
	if last == nil {
		last = errors.New("tailscale gives no address")
	}
	return fmt.Errorf("No tailnet address could be bound: %v. Check that tailscale is up, %s", last, againLine)
}

// Serving is what a running tailscale serve writes to tell the coppice
// server which certificate it serves, so floor.facts can warn about it
// wherever it lives.
type Serving struct {
	PID      int    `json:"pid"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// ServingPath is that file under a data directory.
func ServingPath(dataDir string) string {
	return filepath.Join(dataDir, "web", "serving.json")
}

// writeServing writes the serving file, mode 0600. A file that cannot be
// written only costs the warning, so the error is the caller's to log.
func writeServing(path, certFile, keyFile string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, _ := json.Marshal(Serving{PID: os.Getpid(), CertFile: certFile, KeyFile: keyFile})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// removeServing removes the serving file when this process wrote it.
func removeServing(path string) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var s Serving
	if json.Unmarshal(raw, &s) == nil && s.PID == os.Getpid() {
		os.Remove(path)
	}
}

// ServingCert is the certificate a running serve named in the file at
// path, and false when there is no file, it does not read, or the process
// that wrote it is gone.
func ServingCert(path string) (string, bool) {
	c, _, ok := ServingPair(path)
	return c, ok
}

// ServingPair is the certificate and key a running serve named in the
// file at path, and false as ServingCert says.
func ServingPair(path string) (string, string, bool) {
	raw, err := readSmall(path)
	if err != nil {
		return "", "", false
	}
	var s Serving
	if json.Unmarshal(raw, &s) != nil || s.PID <= 0 || s.CertFile == "" {
		return "", "", false
	}
	if err := syscall.Kill(s.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return "", "", false
	}
	return s.CertFile, s.KeyFile, true
}
