// Package skills holds the pages coppice teaches a pane with. The binary
// carries them, so a floor on any machine can print them.
package skills

import _ "embed"

// Steward is the page that makes a pane the steward of the floor.
//
//go:embed steward/SKILL.md
var Steward string

// StewardTurn is the system page of one turn of the chat-mode steward. The
// bytes are fixed, so the model's cache can hold them.
//
//go:embed steward-turn/SKILL.md
var StewardTurn string

// Pages maps each page name to its text.
var Pages = map[string]string{"steward": Steward}

// FloorHeader goes before the page when the server pastes it into the
// floor's new steward. The page says the line that sent the pane there
// tells it which kind of steward it is, and this is that line.
const FloorHeader = "You are the steward of this floor. The operator talks to you from the floor, " +
	"and each sentence they say comes to you as a prompt. Your working directory is your own " +
	"scratch space under the state directory, outside the floor's own files and outside every " +
	"project. Here is your page."

// FloorLine makes a pane the floor's steward when the page cannot go in as
// one paste. The pane runs coppice skill steward, which prints the page.
const FloorLine = "Run: coppice skill steward. Follow it. You are the steward of this floor. Say ready."
