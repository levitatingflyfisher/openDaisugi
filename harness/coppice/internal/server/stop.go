package server

import "github.com/opendaisugi/coppice/internal/proto"

// RegisterStopCommand wires server.stop to the caller's own stop function.
// The server package has no notion of process exit, so the CLI supplies one.
//
// The handler calls stop and replies at once; it never waits for stop to
// finish. stop must itself run any teardown - Close, in the CLI's case - off
// this handler's own goroutine. Close waits, up to its own bound, for every
// in-flight handler on every connection to finish before it returns; calling
// Close synchronously from inside this handler would make it wait for itself.
func (s *Server) RegisterStopCommand(stop func()) error {
	return s.Handle("server.stop", func(_ *Client, r *proto.Request) proto.Response {
		stop()
		return proto.OKResp(r.ID, map[string]any{"stopping": true})
	})
}
