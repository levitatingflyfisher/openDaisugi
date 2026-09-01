package cli

import (
	"fmt"

	"github.com/opendaisugi/coppice/internal/detect"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/server"
)

// registerAll wires every command server.New cannot register on its own:
// server.stop needs a stop function only the binary has, and pane.explain
// plus the manifest tick need a loaded detection set. It runs strictly
// before Listen, so no handler is ever registered after Serve has started.
//
// It returns the first registration error rather than swallowing it: a
// failed registration must fail server start, not silently serve a socket
// with a command missing.
func registerAll(s *server.Server) error {
	if err := s.RegisterStopCommand(func() {
		// Close must never run on this handler's own goroutine: Close waits,
		// bounded, for every in-flight handler on every connection, this one
		// included, to finish before it returns.
		go func() { _ = s.Close() }()
	}); err != nil {
		return err
	}

	set, err := detect.LoadSet(detect.OverrideDir())
	if err != nil {
		// Unreachable from outside this binary, as far as any override
		// directory content goes: LoadSet reads the override directory
		// through Set.add and Set.warn, and both fail soft, recording a
		// warning rather than returning an error. The only way to reach
		// this branch is a failure in the bundled, go:embed'd, manifests
		// this binary carries, which no override directory, broken or
		// otherwise, can cause. No test exercises this branch for that
		// reason.
		warn := fmt.Sprintf(
			"agent detection did not load: %v. Screen detection is off. Gate-reported state still works. "+
				"The agent-detection override directory is %s.", err, detect.OverrideDir())
		s.SetDetectionWarnings([]string{warn})
		// pane.explain stays in the usage text even now, so it must still
		// answer something instead of "no command": a control that vanished
		// silently is worse than one that says plainly why it does not work.
		return s.Handle("pane.explain", func(_ *server.Client, r *proto.Request) proto.Response {
			return proto.ErrResp(r.ID, proto.ErrInternal, "agent detection is off. "+warn)
		})
	}
	s.SetDetectionWarnings(set.Warnings())
	s.RegisterExplainCommand(set)
	s.StartManifestTick(set)
	return nil
}
