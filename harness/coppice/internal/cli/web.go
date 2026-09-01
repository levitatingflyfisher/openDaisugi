package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/opendaisugi/coppice/internal/plugins"
	"github.com/opendaisugi/coppice/internal/web"
)

// webUsage is what coppice web prints with no subcommand, or an unknown one.
const webUsage = `coppice web - the floor in a browser

  coppice web [--no-open] [--listen ADDR]
      Start the server if it is not running, serve the floor on this
      machine only, and open it in the browser, signed in.

  coppice web cert init [--name NAME]... [--ip ADDR]... [--ca-dir DIR] [--ca-listen ADDR] [--qr url|pem|off]
      Make the local CA if there is none, then issue a server certificate.
      Prints a QR the phone scans to install the CA.
  coppice web cert show [--ca-dir DIR]
      Print what the current certificate covers and when it expires.
  coppice web cert tailscale NAME.TAILNET.ts.net [--dir DIR]
      Ask tailscale for a Let's Encrypt certificate and write it where
      coppice web serve --tls tailscale looks for it.
  coppice web serve [--listen ADDR] [--tls tailscale|localca|files|off]
                    [--cert FILE --key FILE] [--ca-dir DIR] [--ca-listen ADDR]
                    [--external-url URL] [--gate-root DIR] [--voice-url URL [--voice-token-file FILE]]
                    [--ntfy URL --ntfy-topic NAME --ntfy-token-env VAR]
                    [--persist|--forget] [--web-push] [--qr]
      Serve the phone client. Mints a token on the first run and prints it.
  coppice web token [--for NAME] [--rotate] [--url URL] [--qr] [--listen ADDR]
      Print the current token, its sign-in URL, and a QR to scan. With
      --for, the token is the one minted for NAME, and every allow and
      deny made with it carries that name.
  coppice web token list
      Print the names that hold a token. It never prints a token.
  coppice web token --revoke NAME
      Retire the token minted for NAME.
`

// stringList collects a repeatable flag into a slice, one value per flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// runWeb dispatches coppice web. Every subcommand here works on files under
// the data directory the global flags already resolved, so there is no
// remote form of it yet.
func (c *CLI) runWeb(remote string, argv []string) int {
	if remote != "" {
		fmt.Fprintln(c.Err, "coppice web does not take --remote. Run it on the box the phone reaches.")
		return 1
	}
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return c.webOpenCommand(argv)
	}
	switch argv[0] {
	case "cert":
		return c.webCertCommand(argv[1:])
	case "serve":
		return c.webServeCommand(argv[1:])
	case "token":
		return c.webTokenCommand(argv[1:])
	default:
		fmt.Fprintf(c.Err, "Unknown command: coppice web %s\n\n%s", argv[0], webUsage)
		return 1
	}
}

// webCertCommand dispatches coppice web cert's own subcommands.
func (c *CLI) webCertCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.Err, webUsage)
		return 1
	}
	switch args[0] {
	case "init":
		return c.webCertInit(args[1:])
	case "show":
		return c.webCertShow(args[1:])
	case "tailscale":
		return c.webCertTailscale(args[1:])
	default:
		fmt.Fprintf(c.Err, "Unknown command: coppice web cert %s\n\n%s", args[0], webUsage)
		return 1
	}
}

// webCertInit makes the CA if there is none, keeps it if there already is
// one, and always issues a fresh leaf for the given names and addresses.
// It ends by printing a QR the phone scans to fetch the CA over the plain
// HTTP hand-off, since a phone that trusts nothing yet cannot reach it any
// other way.
func (c *CLI) webCertInit(args []string) int {
	fs := flag.NewFlagSet("cert init", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	var names, ips stringList
	fs.Var(&names, "name", "a DNS name the phone will use. Repeatable.")
	fs.Var(&ips, "ip", "an address the phone will use. Repeatable.")
	caDir := fs.String("ca-dir", web.LocalCADir(c.DataDir), "where the CA lives")
	caListen := fs.String("ca-listen", ":8080", "the plain HTTP port that hands the CA to the phone")
	qrKind := fs.String("qr", "url", "url, pem, or off")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *qrKind != "url" && *qrKind != "pem" && *qrKind != "off" {
		fmt.Fprintf(c.Err, "--qr %q is not one of url, pem, off.\n", *qrKind)
		return 1
	}
	var parsed []net.IP
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			fmt.Fprintf(c.Err, "%q is not an address. Use --ip 192.168.1.20.\n", raw)
			return 1
		}
		parsed = append(parsed, ip)
	}
	d := web.CADir{Path: *caDir}
	meta, err := d.Init(names, parsed, time.Now())
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	fmt.Fprintf(c.Out, "CA        %s\n", filepath.Join(*caDir, "ca.crt"))
	fmt.Fprintf(c.Out, "server    %s\n", filepath.Join(*caDir, "leaf.crt"))
	fmt.Fprintf(c.Out, "covers    %s\n", strings.Join(append(append([]string{}, meta.Names...), meta.IPs...), " "))
	fmt.Fprintf(c.Out, "expires   %s\n", meta.NotAfter.Format(time.RFC3339))

	host := "127.0.0.1"
	if len(meta.Names) > 0 {
		host = meta.Names[0]
	} else if len(meta.IPs) > 0 {
		host = meta.IPs[0]
	}
	url := web.CACertURL(host, *caListen)
	fmt.Fprintf(c.Out, "\nInstall the CA on the phone. Scan this, save the file, then open it.\n%s\n\n", url)
	switch *qrKind {
	case "off":
	case "pem":
		pem, err := d.CAPEM()
		if err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		web.WriteQR(c.Out, string(pem))
	default:
		web.WriteQR(c.Out, url)
	}
	fmt.Fprintf(c.Out, "\nStart the server with: coppice web serve --tls localca --ca-listen %s\n", *caListen)
	return 0
}

// webCertShow reports what the current leaf covers and when it runs out,
// so an operator never has to reach for openssl to read their own
// certificate.
func (c *CLI) webCertShow(args []string) int {
	fs := flag.NewFlagSet("cert show", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	caDir := fs.String("ca-dir", web.LocalCADir(c.DataDir), "where the CA lives")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	d := web.CADir{Path: *caDir}
	meta, err := d.Meta()
	if err != nil {
		fmt.Fprintf(c.Err, "No local CA in %s. Run coppice web cert init.\n", *caDir)
		return 1
	}
	certFile, _ := d.LeafPaths()
	leaf, err := web.LoadLeaf(certFile)
	if err != nil {
		fmt.Fprintf(c.Err, "The certificate in %s will not parse. Run coppice web cert init.\n", *caDir)
		return 1
	}
	fmt.Fprintf(c.Out, "CA        %s\n", filepath.Join(*caDir, "ca.crt"))
	fmt.Fprintf(c.Out, "covers    %s\n", strings.Join(append(append([]string{}, meta.Names...), meta.IPs...), " "))
	fmt.Fprintf(c.Out, "expires   %s\n", leaf.NotAfter.Format(time.RFC3339))
	if warn := web.ExpiryWarning(leaf, time.Now()); warn != "" {
		fmt.Fprintf(c.Out, "warning   %s\n", warn)
	}
	return 0
}

// webCertTailscale asks the tailscale binary for a Let's Encrypt
// certificate and writes it under the pair TailscalePaths names, so
// coppice web serve --tls tailscale finds it with no extra flags.
func (c *CLI) webCertTailscale(args []string) int {
	certDefault, keyDefault := web.TailscalePaths(c.DataDir)
	fs := flag.NewFlagSet("cert tailscale", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	dir := fs.String("dir", filepath.Dir(certDefault), "where to write the pair")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(c.Err, "Give the MagicDNS name. Example: coppice web cert tailscale box.tail1234.ts.net")
		return 1
	}
	name := fs.Arg(0)
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	// The same two basenames TailscalePaths names, so coppice web serve
	// finds this pair without being told where it landed.
	certFile := filepath.Join(*dir, filepath.Base(certDefault))
	keyFile := filepath.Join(*dir, filepath.Base(keyDefault))
	// Both paths are explicit. What tailscale names a file when nothing
	// tells it was never checked, so this never leans on it.
	cmd := exec.Command("tailscale", "cert",
		"--cert-file="+certFile, "--key-file="+keyFile, name)
	cmd.Stdout = c.Out
	cmd.Stderr = c.Err
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(c.Err, "tailscale cert failed. Turn on MagicDNS and HTTPS in the admin console, then try again.")
		return 1
	}
	fmt.Fprintf(c.Out, "cert      %s\nkey       %s\n", certFile, keyFile)
	fmt.Fprintf(c.Out, "\nStart the server with:\n  coppice web serve --tls tailscale --cert %s --key %s\n", certFile, keyFile)
	fmt.Fprintln(c.Out, "Let's Encrypt issues this and it lasts 90 days. Renewal is yours.")
	return 0
}

// voiceURLErr is what --voice-url prints for anything that is not an
// absolute http or https address. Empty is fine and means the phone's
// record button has no server to reach yet; a bare host:port, the shape
// every other address in this command reads as, is not, since a proxy
// needs a full URL to build a request from.
const voiceURLErr = "--voice-url needs an http:// or https:// address, for example http://127.0.0.1:7477."

// validVoiceURL is true for an empty string, and for anything else only
// when it parses as an absolute http or https URL with a host. Checked
// before web.json is ever written, so a typo is caught at the terminal
// the operator is already looking at, not three restarts later on the
// phone's own status line.
func validVoiceURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// webServeCommand runs the phone server in the foreground. --persist saves
// the settings so the coppice server starts the same phone server on its
// own; --forget turns that off without throwing away the rest of what was
// saved.
func (c *CLI) webServeCommand(args []string) int {
	fs := flag.NewFlagSet("web serve", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	listen := fs.String("listen", ":8443", "address to serve the phone client on")
	tlsSource := fs.String("tls", "localca", "tailscale, localca, files, or off")
	certFile := fs.String("cert", "", "certificate file for --tls files or --tls tailscale")
	keyFile := fs.String("key", "", "key file for --tls files or --tls tailscale")
	caDir := fs.String("ca-dir", web.LocalCADir(c.DataDir), "where the local CA lives")
	caListen := fs.String("ca-listen", ":8080", "plain HTTP port that hands the CA to the phone, or off")
	externalURL := fs.String("external-url", "", "the URL the phone uses, for notification links")
	ntfy := fs.String("ntfy", "", "your ntfy server, for push")
	ntfyTopic := fs.String("ntfy-topic", "", "the ntfy topic to publish to")
	ntfyTokenEnv := fs.String("ntfy-token-env", "", "name of the environment variable holding the ntfy token")
	gateRoot := fs.String("gate-root", web.GateRoot(c.DataDir), "the gate's ask directory")
	voiceURL := fs.String("voice-url", "", "the address of daisugi voice serve, for the phone's record button")
	voiceTokenFile := fs.String("voice-token-file", "", "the token file that voice server checks; needed when --voice-url is on another machine")
	webPush := fs.Bool("web-push", false, "not built. Use ntfy.")
	persist := fs.Bool("persist", false, "start with the coppice server from now on")
	forget := fs.Bool("forget", false, "stop starting with the coppice server, then exit")
	qr := fs.Bool("qr", true, "print the sign-in QR on start")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *webPush {
		fmt.Fprintln(c.Err,
			"Web push is not built. Push goes through your own ntfy.\n"+
				"Start again with --ntfy URL --ntfy-topic NAME.")
		return 1
	}
	if !validVoiceURL(*voiceURL) {
		fmt.Fprintln(c.Err, voiceURLErr)
		return 1
	}
	// coppice web cert tailscale always writes to the same pair TailscalePaths
	// names. --tls tailscale with no explicit --cert or --key reads that
	// same pair back, so the two commands agree on one name instead of each
	// inventing its own. This only applies to the tailscale source: --tls
	// files keeps its own "needs both --cert and --key" refusal when they
	// are left blank. Exactly one of the pair set is refused here rather
	// than left for Resolve to report by flag name, since a config with
	// only one of cert_file or key_file persisted would leave an operator
	// reading web.json no way to tell which field is the problem.
	switch {
	case *tlsSource == string(web.TLSTailscale) && *certFile == "" && *keyFile == "":
		*certFile, *keyFile = web.TailscalePaths(c.DataDir)
	case *tlsSource == string(web.TLSTailscale) && (*certFile == "") != (*keyFile == ""):
		fmt.Fprintln(c.Err, "--tls tailscale needs both --cert and --key, or neither. "+
			"Leave both blank to use the pair coppice web cert tailscale wrote, "+
			"or set both --cert and --key yourself.")
		return 1
	}

	configPath := web.ConfigPath(c.DataDir)
	cfg := web.Config{
		Enabled: true, Listen: *listen, TLS: *tlsSource,
		CertFile: *certFile, KeyFile: *keyFile, CADir: *caDir, CAListen: *caListen,
		Ntfy: *ntfy, NtfyTopic: *ntfyTopic, NtfyTokenEnv: *ntfyTokenEnv,
		ExternalURL: *externalURL, GateRoot: *gateRoot, VoiceURL: *voiceURL,
		VoiceTokenFile: *voiceTokenFile,
	}
	if *forget {
		// Turning autostart off must not also forget the ntfy settings the
		// operator saved. Edit the file that is there rather than writing a
		// fresh one out of flag defaults. This does not need the
		// configuration below to be servable: it only ever turns a phone
		// server off, never on.
		saved, err := web.LoadConfig(configPath)
		if err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		if saved.Listen == "" {
			saved = cfg // nothing was ever saved, so record what was asked for
		}
		saved.Enabled = false
		if err := web.SaveConfig(configPath, saved); err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		fmt.Fprintln(c.Out, "The phone server will not start with the coppice server.")
		return 0
	}

	// The configuration is proved servable before anything is printed or
	// saved. A refusal here must leave no token on screen, no QR drawn, and
	// no web.json behind: a run that cannot serve has nothing worth
	// remembering or scanning.
	if _, _, err := (web.TLSOptions{
		Source: web.TLSSource(*tlsSource), CertFile: *certFile, KeyFile: *keyFile,
		CADir: *caDir, Listen: *listen,
	}).Resolve(); err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}

	if *persist {
		if err := web.SaveConfig(configPath, cfg); err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		fmt.Fprintf(c.Out, "Saved %s. The coppice server will start the phone server.\n", configPath)
	}

	store := web.TokenStore{Path: web.TokenPath(c.DataDir)}
	tok, err := store.Load()
	if err != nil {
		// A first run with no token should hand the operator a working
		// phone, not a refusal. Minting one keeps every request
		// authenticated, which is the part that matters.
		tok, err = store.Mint()
		if err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		fmt.Fprintln(c.Out, "Minted a new web token.")
	}
	if *qr {
		printSignIn(c.Out, tok, *externalURL, *listen, *tlsSource)
	}

	serve := c.serveWeb
	if serve == nil {
		serve = web.Serve
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The views are the same ones the coppice server's own web server
	// serves, so a page from either lists the same views.
	err = serve(ctx, cfg, web.Options{
		Dial:           web.UnixDialer{Path: c.Socket},
		Tokens:         store,
		Gate:           web.AskChannel{Root: *gateRoot},
		VoiceURL:       *voiceURL,
		VoiceTokenFile: *voiceTokenFile,
		Views:          plugins.ViewsOf(c.loadPlugins()),
		ViewLib:        plugins.SharedLib(),
	})
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	return 0
}

// signInURL builds the address the phone opens. The scheme follows the TLS
// source: printing an https URL for a plain HTTP listener would hand the
// operator a QR that cannot connect.
func signInURL(base, listen, tlsSource string) string {
	if base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "https"
	if tlsSource == string(web.TLSOff) {
		scheme = "http"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	if scheme == "http" {
		host = "127.0.0.1" // --tls off only ever listens on loopback
	}
	_, port, splitErr := net.SplitHostPort(listen)
	if splitErr != nil || port == "" {
		port = "8443"
	}
	return scheme + "://" + host + ":" + port
}

// printSignIn writes the token, the sign-in URL, and a QR of it to w.
func printSignIn(w io.Writer, token, externalURL, listen, tlsSource string) {
	url := signInURL(externalURL, listen, tlsSource) + "/#t=" + token
	fmt.Fprintf(w, "token     %s\nsign in   %s\n\nScan this on the phone.\n", token, url)
	web.WriteQR(w, url)
}

// webTokenFor returns the token for name, or the operator's token when
// name is empty. It mints one when there is none, or when rotate is set.
func (c *CLI) webTokenFor(store web.TokenStore, name string, rotate bool) (string, error) {
	if name == "" {
		tok, err := store.Load()
		if err != nil || rotate {
			return store.Mint()
		}
		return tok, nil
	}
	if err := web.CheckName(name); err != nil {
		return "", err
	}
	tok, err := store.Lookup(name)
	if errors.Is(err, web.ErrNoToken) || (err == nil && rotate) {
		return store.MintFor(name)
	}
	return tok, err
}

// webTokenList prints the names that hold a token, one per line.
func (c *CLI) webTokenList(store web.TokenStore, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(c.Err, "coppice web token list takes no arguments.")
		return 1
	}
	names, err := store.Names()
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintln(c.Out, "No named tokens. Run: coppice web token --for NAME")
		return 0
	}
	for _, n := range names {
		fmt.Fprintln(c.Out, n)
	}
	return 0
}

// webTokenCommand prints the current bearer token, minting one if none
// exists yet, or a fresh one when --rotate is given.
func (c *CLI) webTokenCommand(args []string) int {
	store := web.TokenStore{Path: web.TokenPath(c.DataDir)}
	if len(args) > 0 && args[0] == "list" {
		return c.webTokenList(store, args[1:])
	}
	fs := flag.NewFlagSet("web token", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	rotate := fs.Bool("rotate", false, "mint a new token and retire the old one")
	forName := fs.String("for", "", "the name of the person the token is for")
	revoke := fs.String("revoke", "", "retire the token minted for this name")
	url := fs.String("url", "", "the URL the phone uses, overriding the saved one")
	qr := fs.Bool("qr", true, "print the sign-in QR")
	listen := fs.String("listen", "", "the port the phone connects to, overriding the saved one")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	// A word left over after the flags is refused before any token is read
	// or minted. A list asked for after a flag would otherwise print a
	// token.
	if fs.NArg() > 0 {
		fmt.Fprintf(c.Err, "coppice web token takes no argument %q. To list the names, run: coppice web token list\n", fs.Arg(0))
		return 1
	}
	if *revoke != "" {
		if err := store.Revoke(*revoke); err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
		fmt.Fprintf(c.Out, "Retired the token for %s.\n", *revoke)
		return 0
	}
	tok, err := c.webTokenFor(store, *forName, *rotate)
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	if *forName != "" {
		fmt.Fprintf(c.Out, "name      %s\n", *forName)
	}
	cfg, err := web.LoadConfig(web.ConfigPath(c.DataDir))
	if err != nil {
		fmt.Fprintf(c.Err, "web: could not read the saved settings, guessing at the address: %v\n", err)
	}
	// --url and --listen, left blank, fall back to what was saved by
	// coppice web serve --persist, then to :8443. Without this, an operator
	// who saved a non-default listen or external URL would still get a QR
	// built from these flags' own unrelated defaults.
	base, addr := *url, *listen
	if base == "" {
		base = cfg.ExternalURL
	}
	if addr == "" {
		addr = cfg.Listen
	}
	if addr == "" {
		addr = ":8443"
	}
	if *qr {
		printSignIn(c.Out, tok, base, addr, cfg.TLS)
	} else {
		fmt.Fprintf(c.Out, "token     %s\nsign in   %s\n", tok, signInURL(base, addr, cfg.TLS)+"/#t="+tok)
	}
	return 0
}
