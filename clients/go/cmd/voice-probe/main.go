// Command voice-probe is a test instrument for clients/voice_cases.py: one
// query of the voice bridge's pure parts on stdin, answered as JSON on
// stdout. It is not shipped.
package main

import (
	"os"

	"daisugi-verify/internal/voice"
)

func main() {
	os.Exit(voice.Probe(os.Stdin, os.Stdout))
}
