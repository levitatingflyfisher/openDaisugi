package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/opendaisugi/coppice/internal/proto"
)

// The per-agent daisugi mode (F11). The owner lowers one agent from
// enforcing to watching from its stack bar; coppice writes a marker the
// gate reads on that agent's next tool call:
// <gate root>/agents/<floor>/<pane>.mode. The gate lowers an enforce hook to
// audit only when the marker holds exactly "watching" (F11-R-1). Turning
// the gate off stays a typed command, so this verb never offers off.

// ModeRefusal is what a pane, a plugin or an unplaced peer gets for
// agent.set_mode.
const ModeRefusal = "only the operator sets an agent's daisugi mode."

// ModeWordsRefusal is what agent.set_mode says to a mode it does not take.
const ModeWordsRefusal = "mode must be enforcing or watching. Turning the gate off stays a command: daisugi gate disarm"

// modeDir is the directory under the gate root that holds the markers.
const modeDir = "agents"

// modeWatching is the marker's whole text.
const modeWatching = "watching\n"

// markerPaneID is the shape of a pane id a marker may be named for, the
// shape the gate checks before it reads one.
var markerPaneID = regexp.MustCompile(`^w[0-9]{1,9}:p[0-9]{1,9}$`)

// floorKey is this floor's directory under agents/: the first 16 hex
// digits of the SHA-256 of the socket path every pane gets as
// COPPICE_SOCK. Pane ids start at w1:p1 on every floor, and two floors
// whose data dirs share a parent share one gate root, so each floor keeps
// its markers apart (F11-R-6).
func (s *Server) floorKey() string {
	sum := sha256.Sum256([]byte(s.cfg.SocketPath))
	return hex.EncodeToString(sum[:])[:16]
}

// floorDir is this floor's marker directory.
func (s *Server) floorDir() string {
	return filepath.Join(s.cfg.GateRoot, modeDir, s.floorKey())
}

// realDir is true when p is a directory, false with no error when
// nothing is there, and an error when something else is there: a symlink
// (even to a directory) or a file. coppice makes and sweeps markers only
// in real directories, so a link in the gate root never leads its writes
// or removals elsewhere (F11-R-8).
func realDir(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return false, fmt.Errorf("%s is not a directory: it is a symlink or a file. Move it aside", p)
	}
	return true, nil
}

// floorDirReal is true when agents/ and this floor's directory are both
// real directories, false with no error when either is missing, and an
// error when either is something else.
func (s *Server) floorDirReal() (bool, error) {
	for _, d := range []string{filepath.Dir(s.floorDir()), s.floorDir()} {
		ok, err := realDir(d)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// makeFloorDir makes agents/ and this floor's directory, 0700, each only
// when nothing is there, and refuses a symlink or a file in their place.
func (s *Server) makeFloorDir() error {
	agents := filepath.Dir(s.floorDir())
	if err := os.MkdirAll(filepath.Dir(agents), 0o700); err != nil {
		return err
	}
	for _, d := range []string{agents, s.floorDir()} {
		ok, err := realDir(d)
		if err != nil {
			return err
		}
		if !ok {
			if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if ok, err := realDir(d); err != nil || !ok {
				return fmt.Errorf("%s is not a directory: it is a symlink or a file. Move it aside", d)
			}
		}
	}
	return nil
}

// markerPath is the marker of pane id, or "" when id has not the shape.
func (s *Server) markerPath(id string) string {
	if s.cfg.GateRoot == "" || !markerPaneID.MatchString(id) {
		return ""
	}
	return filepath.Join(s.floorDir(), id+".mode")
}

// markerMax is the largest marker read, as the gate's
// AGENT_MODE_MAX_BYTES.
const markerMax = 64

// modeMarker is "watching" when pane id's marker reads as the gate reads
// it (readMarker) and holds the marker text, and "" otherwise. The read
// is by fd from the directory down, never a path read after a check, so
// the stack shows a marker only when the gate would lower the agent
// (F11-R-9).
func (s *Server) modeMarker(id string) string {
	if s.markerPath(id) == "" {
		return ""
	}
	b, ok := readMarker(filepath.Dir(s.floorDir()), s.floorKey(), id+".mode")
	if !ok || (string(b) != modeWatching && string(b) != "watching") {
		return ""
	}
	return "watching"
}

// writeModeMarker writes the watching marker for pane id: a new file
// 0600 in the agents directory (0700), renamed over the old one, so the
// gate reads either the old marker or the whole new one.
func (s *Server) writeModeMarker(id string) error {
	p := s.markerPath(id)
	if p == "" {
		return fmt.Errorf("no marker for pane %q", id)
	}
	dir := filepath.Dir(p)
	if err := s.makeFloorDir(); err != nil {
		return err
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+id+".mode."+hex.EncodeToString(nonce[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(modeWatching)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// clearModeMarker removes pane id's marker. No marker is no error.
func (s *Server) clearModeMarker(id string) error {
	p := s.markerPath(id)
	if p == "" {
		return nil
	}
	if ok, err := s.floorDirReal(); err != nil || !ok {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// clearAllModeMarkers removes every marker in this floor's directory. A
// server start runs it: no agent of an earlier run is live, so every
// agent starts enforcing again (F11-R-4). Another floor's directory and
// other files stay (F11-R-6).
func (s *Server) clearAllModeMarkers() {
	if s.cfg.GateRoot == "" {
		return
	}
	dir := s.floorDir()
	if ok, err := s.floorDirReal(); err != nil {
		// Never remove anything through a symlink (F11-R-8).
		log.Printf("mode markers: %v", err)
		s.Note("daisugi mode markers left alone: "+err.Error(), "")
		return
	} else if !ok {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if id, ok := strings.CutSuffix(e.Name(), ".mode"); ok && markerPaneID.MatchString(id) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// handleSetMode answers agent.set_mode {pane, mode}: watching writes the
// pane's marker, enforcing removes it. The gate reads it on the agent's
// next tool call. The role check (operatorVerbs) has already refused a
// pane, a plugin and an unplaced peer.
func (s *Server) handleSetMode(c *Client, r *proto.Request) proto.Response {
	id := paneOf(r)
	// A view-only attach is refused before anything else is read
	// (F11-R-10).
	if c.viewOnlyOn(id) {
		return refuseViewOnly(r, id)
	}
	mode, _ := r.Str("mode")
	if mode != "enforcing" && mode != "watching" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest, ModeWordsRefusal)
	}
	rec, ok := s.tree.Pane(id)
	if !ok || s.markerPath(id) == "" {
		return proto.ErrResp(r.ID, proto.ErrNoSuchPane,
			fmt.Sprintf("no pane %q. Run: coppice agent list", id))
	}
	if rec.Closed {
		return proto.ErrResp(r.ID, proto.ErrPaneClosed,
			fmt.Sprintf("%s has ended, so no gate checks it.", s.labelOf(id)))
	}
	if rec.Harness == "" {
		return proto.ErrResp(r.ID, proto.ErrBadRequest,
			fmt.Sprintf("%s runs no harness, so no gate checks it.", s.labelOf(id)))
	}
	var err error
	if mode == "watching" {
		err = s.writeModeMarker(id)
	} else {
		err = s.clearModeMarker(id)
	}
	if err != nil {
		return proto.ErrResp(r.ID, proto.ErrInternal,
			fmt.Sprintf("could not set the daisugi mode of %s: %v", s.labelOf(id), err))
	}
	return proto.OKResp(r.ID, map[string]any{"pane": id, "mode": mode})
}
