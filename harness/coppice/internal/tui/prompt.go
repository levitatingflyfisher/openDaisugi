package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/opendaisugi/coppice/internal/tiles"
)

// Kind is what a prompt line is: a plumbing command the floor runs itself,
// or talk meant for a foreman.
type Kind int

const (
	Plumbing Kind = iota
	Talk
)

// Harnesses are the harness names a prompt line may open by name.
var Harnesses = []string{"claude", "codex", "pi", "sprig", "opencode"}

// Verbs are the plumbing verbs the prompt line answers to.
var Verbs = []string{"list", "read", "close", "open", "swap", "rotate", "reset", "lock", "unlock", "layout"}

// Messages the layout words answer with.
const (
	lockedMessage      = "Layout is locked. Type unlock first."
	swapNeedsMessage   = "swap needs two slot numbers, as in swap 1 2"
	layoutNeedsMessage = "layout is all, focus, or one"
)

// floorFile is the file under the data dir that keeps the layout and the
// lock between runs.
const floorFile = "floor.json"

// floorState is the shape of floor.json.
type floorState struct {
	Layout string `json:"layout"`
	Locked bool   `json:"locked"`
}

// parseLayout reads a layout name. Any other word is refused.
func parseLayout(name string) (tiles.Layout, bool) {
	switch tiles.Layout(name) {
	case tiles.All, tiles.Focus, tiles.One:
		return tiles.Layout(name), true
	}
	return "", false
}

// readFloorState reads the layout and the lock from floor.json under
// dataDir. A missing or unreadable file, or no data dir, is focus and
// unlocked. An unknown layout name reads as focus and keeps the lock.
func readFloorState(dataDir string) (tiles.Layout, bool) {
	if dataDir == "" {
		return tiles.Focus, false
	}
	b, err := os.ReadFile(filepath.Join(dataDir, floorFile))
	if err != nil {
		return tiles.Focus, false
	}
	var st floorState
	if err := json.Unmarshal(b, &st); err != nil {
		return tiles.Focus, false
	}
	layout, ok := parseLayout(st.Layout)
	if !ok {
		layout = tiles.Focus
	}
	return layout, st.Locked
}

// writeFloorState writes the layout and the lock to floor.json under
// dataDir. The bytes go to a temp file in the same directory first and
// the file is renamed into place, so a crash mid-write never leaves a cut
// file behind. With no data dir nothing is written.
func writeFloorState(dataDir string, layout tiles.Layout, locked bool) error {
	if dataDir == "" {
		return nil
	}
	b, err := json.Marshal(floorState{Layout: string(layout), Locked: locked})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, floorFile+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(dataDir, floorFile)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// tilesMessage turns a refusal from the tiles model into the message line.
// The lock refusal names the word that lifts it.
func tilesMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, tiles.ErrLocked) {
		return lockedMessage
	}
	return err.Error()
}

// plumbingWords caps a verb-led plumbing line. A longer line led by a verb
// reads as a sentence, so it is talk.
const plumbingWords = 4

// Classify tells plumbing from talk. A line led by a harness name is
// plumbing at any length. A line led by a verb is plumbing when it has at
// most four words. Everything else is talk.
func Classify(line string, harnesses []string) Kind {
	words := strings.Fields(line)
	if len(words) == 0 {
		return Talk
	}
	for _, h := range harnesses {
		if words[0] == h {
			return Plumbing
		}
	}
	for _, v := range Verbs {
		if words[0] == v && len(words) <= plumbingWords {
			return Plumbing
		}
	}
	return Talk
}

// NoForeman is the message the floor shows for talk when no foreman is
// running. def is the harness Enter opens.
func NoForeman(def string) string {
	return "No foreman. Enter opens " + def + " here, or type \"foreman <harness>\" to let it run the floor."
}
