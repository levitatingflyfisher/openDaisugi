package sprig

import (
	"fmt"
	"slices"
	"strings"
)

// HelpKinds are the three kinds of the help tool, in the order the
// descriptions name them.
var HelpKinds = []string{"need_info", "escalate", "give_up"}

// HelpDescription is what the model reads about the help tool. It says
// when to call it and that a missing fact is asked for, never guessed.
const HelpDescription = "Ask your parent for help, or stop. kind is need_info, escalate or give_up; text is your message. " +
	"Call it when you are stuck after a few tries, when the brief lacks a fact you need, " +
	"when the next step needs the owner's decision, or when the task cannot be done inside your envelope. " +
	"Never guess past a missing fact: ask. " +
	"need_info: ask one clear question; the run pauses and goes on when the answer comes. " +
	"escalate: the same, for a decision that belongs to the owner; it goes to the owner. " +
	"give_up: stop now; text is your final report, with what you changed."

// HelpTool is the help tool. A valid call records the kind and the text and
// ends the run: the loop returns once the calls of that turn are done. It
// has no effect in the world, so the warden is not asked about it
// (unwardened).
type HelpTool struct {
	Kind, Text string
	Called     bool
}

func (*HelpTool) Name() string { return "help" }

// Unwardened marks a tool the executor runs without a warden check.
func (*HelpTool) Unwardened() bool { return true }

func (h *HelpTool) Run(in map[string]any) (string, error) {
	kind, text := str(in["kind"]), strings.TrimSpace(str(in["text"]))
	if !slices.Contains(HelpKinds, kind) {
		return "", fmt.Errorf("help: kind is one of %s", strings.Join(HelpKinds, ", "))
	}
	if text == "" {
		return "", fmt.Errorf("help: text is empty. Say what you need or what you did")
	}
	if h.Called {
		return "", fmt.Errorf("help: you already called help in this run")
	}
	h.Kind, h.Text, h.Called = kind, text, true
	if kind == "give_up" {
		return "Your report was sent. The run ends now.", nil
	}
	return "Your question was sent. The run pauses here. The answer comes as the next message.", nil
}
