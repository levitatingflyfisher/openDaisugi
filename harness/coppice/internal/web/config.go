package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	opts.Log.Info("web: serving the phone",
		"listen", c.Listen, "tls", string(source), "gate_root", opts.Gate.Root)
	if source == TLSOff {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(certFile, keyFile)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
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
