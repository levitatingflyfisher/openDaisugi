package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the persisted phone server. Enabled is what makes the coppice
// server start it, so there is one process to keep alive instead of two.
type Config struct {
	Enabled      bool   `json:"enabled"`
	Listen       string `json:"listen"`
	TLS          string `json:"tls"`
	CertFile     string `json:"cert_file"`
	KeyFile      string `json:"key_file"`
	CADir        string `json:"ca_dir"`
	CAListen     string `json:"ca_listen"`
	Ntfy         string `json:"ntfy"`
	NtfyTopic    string `json:"ntfy_topic"`
	NtfyTokenEnv string `json:"ntfy_token_env"`
	ExternalURL  string `json:"external_url"`
	// GateRoot is the gate's ask directory. The caller resolves it, usually
	// with GateRoot(dataDir), so a box with a non-default data directory
	// still has every allow and deny land where the gate can see them.
	GateRoot string `json:"gate_root"`
	// VoiceURL is where daisugi voice serve listens. Empty means the record
	// button on the phone has nowhere to send a clip yet.
	VoiceURL string `json:"voice_url"`
	// VoiceTokenFile is the token file VoiceURL's server checks. A
	// VoiceURL on another machine needs one: the web token stays here.
	VoiceTokenFile string `json:"voice_token_file,omitempty"`
}

// LoadConfig reads the file at path. A missing file means the phone server
// is off, which is the right default for a box nobody has set up yet.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// SaveConfig writes c to path, mode 0600 inside a directory mode 0700.
func SaveConfig(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Serve runs the phone server until ctx ends. The caller resolves every
// path Config and Options carry; nothing here looks up a default of its
// own.
func Serve(ctx context.Context, c Config, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.GateRoot != "" {
		opts.Gate = AskChannel{Root: c.GateRoot}
	}
	if c.VoiceURL != "" {
		opts.VoiceURL = c.VoiceURL
		opts.VoiceTokenFile = c.VoiceTokenFile
	}
	source := TLSSource(c.TLS)
	if source == "" {
		source = TLSLocalCA
	}
	// --tls tailscale does the whole job: it reads this box's name and
	// tailnet addresses, gets or renews the certificate, and listens on
	// the tailnet and loopback unless the listen address names a host.
	tailscale := source == TLSTailscale && c.CertFile != "" && c.KeyFile != ""
	// blank is true when the listen address has no host: the serve then
	// binds the tailnet and loopback. A host is used exactly as given.
	blank := BlankHost(c.Listen)
	var ts Tailscale
	// pending is true while the tailnet is not up yet at boot: loopback
	// serves at once and the tailnet is bound when it comes. It is only
	// ever set when blank is.
	pending := false
	// renew gets or renews the pair. A renewal that fails while the pair
	// on disk is still good is one warning, and that pair is served; the
	// daily check tries again.
	renew := func(ts Tailscale) error {
		res, err := EnsureCert(ts.Name, c.CertFile, c.KeyFile, time.Now())
		if err != nil {
			if ValidPair(c.CertFile, c.KeyFile, time.Now()) {
				opts.Log.Warn("web: the certificate renewal failed", "err", err)
				return nil
			}
			return err
		}
		for _, line := range CertLines(ts.Name, res) {
			opts.Log.Info("web: " + line)
		}
		return nil
	}
	if tailscale {
		var err error
		if ts, err = ReadTailscale(); err == nil {
			err = renew(ts)
		}
		if err != nil {
			if !opts.WaitForTailnet || err.Error() == NoTailscale {
				return err
			}
			switch {
			case ValidPair(c.CertFile, c.KeyFile, time.Now()) && blank:
				opts.Log.Info("web: waiting for the tailnet address", "err", err)
				pending = true
			case ValidPair(c.CertFile, c.KeyFile, time.Now()):
				opts.Log.Warn("web: tailscale is not ready, so the certificate on disk serves", "err", err)
			default:
				opts.Log.Info("web: waiting for tailscale and a certificate", "err", err)
				// No good pair to serve with: wait for tailscale and a
				// certificate before anything serves.
				for wait := tailnetRetry(opts); err != nil; wait = NextTailnetRetry(wait) {
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(wait):
					}
					if ts, err = ReadTailscale(); err == nil {
						err = renew(ts)
					}
				}
			}
		}
	}
	certFile, keyFile, err := TLSOptions{
		Source: source, CertFile: c.CertFile, KeyFile: c.KeyFile,
		CADir: c.CADir, Listen: c.Listen,
	}.Resolve()
	if err != nil {
		return err
	}

	if certFile != "" {
		leaf, err := LoadLeaf(certFile)
		if err != nil {
			return err
		}
		if warn := ExpiryWarning(leaf, time.Now()); warn != "" {
			opts.Log.Warn("web: " + warn)
		}
	}

	if c.Ntfy != "" && c.NtfyTopic != "" {
		pub, err := NewPublisher(PushConfig{
			BaseURL: c.Ntfy, Topic: c.NtfyTopic,
			TokenEnv: c.NtfyTokenEnv, ClickBase: c.ExternalURL,
		}, nil, nil, opts.Log)
		if err != nil {
			return err
		}
		opts.Push = pub
		go func() {
			if err := pub.Watch(ctx, opts.Dial); err != nil && ctx.Err() == nil {
				opts.Log.Warn("web: the push watcher stopped", "err", err)
			}
		}()
	}

	// The ring of recent state events is fed from its own subscription,
	// so the floor page can read the last two hours after a reload.
	if opts.Events == nil {
		opts.Events = NewEventRing(opts.Now)
		go opts.Events.Run(ctx, opts.Dial, opts.Log)
	}

	// Resolve already refused --tls localca with no --ca-dir, so c.CADir is
	// non-empty here whenever source is TLSLocalCA.
	if source == TLSLocalCA && c.CAListen != "" && c.CAListen != "off" {
		caPEM, err := (CADir{Path: c.CADir}).CAPEM()
		if err != nil {
			return err
		}
		go func() {
			if err := ServeCACert(ctx, c.CAListen, caPEM, opts.Log); err != nil && ctx.Err() == nil {
				opts.Log.Warn("web: the CA hand-off stopped", "err", err)
			}
		}()
	}

	s, err := New(opts)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              c.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if tailscale {
		// A renewed pair on disk is served from the next handshake.
		reloader, err := NewCertReloader(certFile, keyFile)
		if err != nil {
			return err
		}
		srv.TLSConfig.GetCertificate = reloader.GetCertificate
		certFile, keyFile = "", ""
		go checkCertDaily(ctx, c.CertFile, c.KeyFile, opts)
		// The coppice server reads which certificate this serve uses, so
		// floor.facts can warn about it wherever it lives.
		if err := writeServing(opts.ServingFile, c.CertFile, c.KeyFile); err != nil {
			opts.Log.Warn("web: could not write the serving file", "err", err)
		}
		defer removeServing(opts.ServingFile)
	}
	opts.Log.Info("web: serving the phone",
		"listen", c.Listen, "tls", string(source), "gate_root", opts.Gate.Root)
	// Every address is bound before any is served, so a port that is
	// taken on one of them leaves nothing half up. Under --tls tailscale
	// with no host, a tailnet address the box will not bind is skipped;
	// the serve fails only when no tailnet address binds, and a saved one
	// at boot waits for the tailnet instead.
	var lns []net.Listener
	var addrs []string
	closeAll := func() {
		for _, l := range lns {
			l.Close()
		}
	}
	_, port, _ := net.SplitHostPort(c.Listen)
	if tailscale && blank {
		if !pending {
			tl, ta, skipped, foreign := bindTailnet(ts.IPs, port)
			logForeign(opts.Log, foreign)
			if len(tl) == 0 {
				if !opts.WaitForTailnet {
					return noTailnetBind(lastSkip(skipped))
				}
				opts.Log.Info("web: waiting for the tailnet address", "err", noTailnetBind(lastSkip(skipped)))
				pending = true
			} else {
				logSkipped(opts.Log, skipped)
			}
			lns, addrs = tl, ta
		}
		loop := net.JoinHostPort("127.0.0.1", port)
		ln, err := net.Listen("tcp", loop)
		if err != nil {
			closeAll()
			return err
		}
		lns, addrs = append(lns, ln), append(addrs, loop)
	} else {
		if tailscale && !ListenStaysOnTailnet(c.Listen) {
			opts.Log.Warn("web: listening beyond the tailnet and this box", "listen", c.Listen)
		}
		ln, err := net.Listen("tcp", c.Listen)
		if err != nil {
			return err
		}
		lns, addrs = append(lns, ln), append(addrs, c.Listen)
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if opts.Listening != nil {
		opts.Listening(addrs)
	}
	errc := make(chan error, 64)
	serveLn := func(ln net.Listener) {
		go func() {
			if source == TLSOff {
				errc <- srv.Serve(ln)
			} else {
				errc <- srv.ServeTLS(ln, certFile, keyFile)
			}
		}()
	}
	for _, ln := range lns {
		serveLn(ln)
	}
	if pending {
		go func() {
			for wait := tailnetRetry(opts); ; wait = NextTailnetRetry(wait) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				got, err := ReadTailscale()
				if err != nil {
					continue
				}
				tl, ta, skipped, foreign := bindTailnet(got.IPs, port)
				if len(tl) == 0 {
					continue
				}
				logForeign(opts.Log, foreign)
				logSkipped(opts.Log, skipped)
				opts.Log.Info("web: listening on the tailnet", "addrs", strings.Join(ta, ","))
				for _, ln := range tl {
					serveLn(ln)
				}
				return
			}
		}()
	}
	err = <-errc
	// One listener that fails takes the others down with it.
	srv.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// tailnetRetry is the first wait between two tries to bind the tailnet.
func tailnetRetry(opts Options) time.Duration {
	if opts.TailnetRetry > 0 {
		return opts.TailnetRetry
	}
	return TailnetRetryFirst
}

// ValidPair is true when the certificate parses and has not run out,
// and the key file holds a PEM private key.
func ValidPair(certFile, keyFile string, now time.Time) bool {
	leaf, err := LoadLeaf(certFile)
	if err != nil || !leaf.NotAfter.After(now) {
		return false
	}
	return keyIsPEM(keyFile)
}

// checkCertDaily reads this box's name from tailscale and runs
// EnsureCert for the pair every opts.CertCheckEvery until ctx ends, and
// logs what it did.
func checkCertDaily(ctx context.Context, certFile, keyFile string, opts Options) {
	every := opts.CertCheckEvery
	if every <= 0 {
		every = CertCheckEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ts, err := ReadTailscale()
		if err != nil {
			opts.Log.Warn("web: the certificate check failed", "err", err)
			continue
		}
		res, err := EnsureCert(ts.Name, certFile, keyFile, time.Now())
		if err != nil {
			opts.Log.Warn("web: the certificate check failed", "err", err)
			continue
		}
		for _, line := range CertLines(ts.Name, res) {
			opts.Log.Info("web: " + line)
		}
	}
}

// AutoStart is what the coppice server calls once its own socket is up. It
// reads the config at configPath. A config that is absent or disabled
// starts nothing and reports no error: the phone server is opt-in. The
// returned func stops the phone server; it is safe to call even when
// nothing started.
func AutoStart(ctx context.Context, configPath string, opts Options) (func(), error) {
	c, err := LoadConfig(configPath)
	if err != nil {
		return func() {}, err
	}
	if !c.Enabled {
		return func() {}, nil
	}
	// At boot tailscale may not be up yet, so a saved tailscale serve waits
	// for the tailnet rather than fail, and says which certificate it
	// serves beside web.json.
	opts.WaitForTailnet = true
	if opts.ServingFile == "" {
		opts.ServingFile = filepath.Join(filepath.Dir(configPath), "serving.json")
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// Serve starts the CA hand-off and the push watcher before it ever
		// tries to listen for the phone itself. A failed listen must not
		// leave either of those running, so cancel fires on every return
		// from Serve, not only when the caller stops the phone server.
		defer cancel()
		if err := Serve(ctx, c, opts); err != nil && ctx.Err() == nil {
			log := opts.Log
			if log == nil {
				log = slog.Default()
			}
			log.Error("web: the phone server did not run", "err", err)
		}
	}()
	return cancel, nil
}
