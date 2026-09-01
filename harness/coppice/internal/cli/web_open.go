package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/opendaisugi/coppice/internal/plugins"
	"github.com/opendaisugi/coppice/internal/web"
)

// defaultWebListen is where coppice web serves when the port is free.
// A fixed port keeps a bookmark working from one run to the next.
const defaultWebListen = "127.0.0.1:9443"

// webOpenCommand is coppice web with no verb: the floor in a browser on
// this machine, in one command. It starts the coppice server if none
// answers, serves on loopback over plain HTTP, prints the signed-in URL,
// and opens it unless --no-open is given.
func (c *CLI) webOpenCommand(args []string) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(c.Err)
	noOpen := fs.Bool("no-open", false, "print the URL and do not open the browser")
	listen := fs.String("listen", "", "the loopback address to serve on (default "+defaultWebListen+", or a free port)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	ensure := c.ensureServer
	if ensure == nil {
		ensure = func() error {
			cl, err := Dial(c.Socket, c.DataDir)
			if err != nil {
				return err
			}
			cl.Close()
			return nil
		}
	}
	if err := ensure(); err != nil {
		fmt.Fprintf(c.Err, "The coppice server is not running and did not start: %v\n", err)
		return 3
	}

	addr := *listen
	if addr == "" {
		addr = freeLoopback()
	}
	cfg := web.Config{
		Enabled: true, Listen: addr, TLS: string(web.TLSOff),
		GateRoot: web.GateRoot(c.DataDir),
	}
	if _, _, err := (web.TLSOptions{Source: web.TLSOff, Listen: addr}).Resolve(); err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}

	store := web.TokenStore{Path: web.TokenPath(c.DataDir)}
	tok, err := store.Load()
	if err != nil {
		if tok, err = store.Mint(); err != nil {
			fmt.Fprintf(c.Err, "%v\n", err)
			return 1
		}
	}
	link := "http://" + addr + "/#t=" + tok
	fmt.Fprintf(c.Out, "The floor is at %s\nPress ctrl-c to stop serving it.\n", link)
	if !*noOpen {
		open := c.openURL
		if open == nil {
			open = openInBrowser
		}
		if err := open(link); err != nil {
			fmt.Fprintf(c.Err, "Could not open a browser: %v. Open the address above.\n", err)
		}
	}

	serve := c.serveWeb
	if serve == nil {
		serve = web.Serve
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = serve(ctx, cfg, web.Options{
		Dial:    web.UnixDialer{Path: c.Socket},
		Tokens:  store,
		Gate:    web.AskChannel{Root: cfg.GateRoot},
		Views:   plugins.ViewsOf(c.loadPlugins()),
		ViewLib: plugins.SharedLib(),
	})
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return 1
	}
	return 0
}

// freeLoopback is defaultWebListen when that port is free, or else a port
// the kernel picks. The probe closes before the server binds, so another
// program can take the port in between; the server then says so.
func freeLoopback() string {
	if ln, err := net.Listen("tcp", defaultWebListen); err == nil {
		ln.Close()
		return defaultWebListen
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return defaultWebListen
	}
	defer ln.Close()
	return ln.Addr().String()
}

// openInBrowser hands url to the desktop's opener. With no display there
// is no browser to open, and the caller prints the URL instead.
func openInBrowser(url string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("no display")
		}
		name = "xdg-open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
