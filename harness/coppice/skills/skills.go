// Package skills holds the pages coppice teaches a pane with. The binary
// carries them, so a floor on any machine can print them.
package skills

import _ "embed"

// Foreman is the page that makes a pane the foreman of the floor.
//
//go:embed foreman/SKILL.md
var Foreman string

// Pages maps each page name to its text.
var Pages = map[string]string{"foreman": Foreman}

// FloorHeader goes before the page when the server pastes it into the
// floor's new foreman. The page says the line that sent the pane there
// tells it which kind of foreman it is, and this is that line.
const FloorHeader = "You are the foreman of this floor. The operator talks to you from the floor, " +
	"and each sentence they say comes to you as a prompt. Your working directory is your own " +
	"scratch space under the state directory, outside the floor's own files and outside every " +
	"project. Here is your page."

// FloorLine makes a pane the floor's foreman when the page cannot go in as
// one paste. The pane runs coppice skill foreman, which prints the page.
const FloorLine = "Run: coppice skill foreman. Follow it. You are the foreman of this floor. Say ready."
