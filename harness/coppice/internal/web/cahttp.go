package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// CACertHandler serves exactly one file. A phone that trusts nothing yet
// cannot fetch the CA over the HTTPS the CA is for, so the hand-off is plain
// HTTP on a second port. A CA certificate is public by design, which is why
// this path carries no token, and why it must serve nothing else. The key
// beside it on disk is not public.
func CACertHandler(caPEM []byte) http.Handler {
	body := append([]byte(nil), caPEM...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ca.crt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Header().Set("Content-Disposition", `attachment; filename="coppice-ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(body)
	})
}

// CACertURL is what the terminal QR encodes. Scanning it opens the phone's
// browser on a download, which is the step Android's certificate installer
// needs. It reads a file, not a screenful of text.
func CACertURL(host, caListen string) string {
	_, port, err := net.SplitHostPort(caListen)
	if err != nil || port == "" {
		port = "8080"
	}
	return fmt.Sprintf("http://%s:%s/ca.crt", host, port)
}

// ServeCACert runs the hand-off listener until ctx ends.
func ServeCACert(ctx context.Context, listen string, caPEM []byte, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           CACertHandler(caPEM),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("web: serving the CA certificate", "listen", listen, "path", "/ca.crt")
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
