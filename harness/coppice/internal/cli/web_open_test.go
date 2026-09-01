package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/web"
)

// webOpenCLI is a CLI whose server check, browser and phone server are all
// stubs, so coppice web runs to the end without a server, a browser or a
// listener.
func webOpenCLI(t *testing.T, ensure error) (*CLI, *web.Config, *web.Options, *[]string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dataDir := t.TempDir()
	var cfg web.Config
	var opts web.Options
	var opened []string
	var out, errb bytes.Buffer
	c := &CLI{
		Version: "test", Socket: filepath.Join(dataDir, "server.sock"), DataDir: dataDir,
		In: strings.NewReader(""), Out: &out, Err: &errb,
		serveWeb: func(_ context.Context, cf web.Config, o web.Options) error {
			cfg, opts = cf, o
			return nil
		},
		ensureServer: func() error { return ensure },
		openURL:      func(u string) error { opened = append(opened, u); return nil },
	}
	return c, &cfg, &opts, &opened, &out, &errb
}

// coppice web with no verb serves the floor on this machine only, over
// plain HTTP, and opens the browser already signed in.
func TestWebAloneServesLoopbackAndOpensTheBrowser(t *testing.T) {
	c, cfg, opts, opened, out, errb := webOpenCLI(t, nil)
	if code := c.Run([]string{"web"}); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errb)
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("listen %q, want loopback", cfg.Listen)
	}
	if cfg.TLS != string(web.TLSOff) {
		t.Fatalf("tls %q, want off on loopback", cfg.TLS)
	}
	if len(*opened) != 1 || !strings.HasPrefix((*opened)[0], "http://"+cfg.Listen+"/#t=") {
		t.Fatalf("opened %v, want the signed-in loopback URL", *opened)
	}
	if !strings.Contains(out.String(), (*opened)[0]) {
		t.Fatalf("the URL is not printed:\n%s", out)
	}
	if len(opts.Views) == 0 {
		t.Fatal("the page gets no views")
	}
}

// --no-open prints the URL and leaves the browser alone.
func TestWebNoOpenOnlyPrintsTheURL(t *testing.T) {
	c, _, _, opened, out, errb := webOpenCLI(t, nil)
	if code := c.Run([]string{"web", "--no-open"}); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errb)
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v with --no-open", *opened)
	}
	if !strings.Contains(out.String(), "/#t=") {
		t.Fatalf("no sign-in URL printed:\n%s", out)
	}
}

// A server that cannot start stops coppice web before it serves a page
// that could only show errors.
func TestWebAloneStopsWhenTheServerCannotStart(t *testing.T) {
	c, cfg, _, opened, out, errb := webOpenCLI(t, errors.New("no server"))
	code := c.Run([]string{"web"})
	if code == 0 {
		t.Fatalf("exit 0 with no server: %s%s", out, errb)
	}
	if cfg.Listen != "" || len(*opened) != 0 {
		t.Fatal("served or opened a page with no server behind it")
	}
	if !strings.Contains(errb.String(), "no server") {
		t.Fatalf("the reason is not shown:\n%s", errb)
	}
}
