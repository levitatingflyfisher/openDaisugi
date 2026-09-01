// Command gen writes the deterministic VT byte streams the grid goldens use.
// Run it from harness/coppice: go run ./testdata/vt/gen
// Recorded streams from real harnesses are a separate, opt-in path. See
// testdata/vt/README.md.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

var streams = map[string]string{
	// Plain text, SGR colour, and a reset.
	"basic.bin": "Hello, \x1b[1;32mworld\x1b[0m!\r\nsecond line\r\n",
	// Soft wrap: 30 characters into a 20 column terminal.
	"wrap.bin": "abcdefghijklmnopqrstuvwxyz0123\r\n",
	// Scroll: more lines than the viewport holds.
	"scroll.bin": "l1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r\nl8\r\n",
	// Cursor addressing plus erase to end of line.
	"erase.bin": "aaaaaaaa\r\n\x1b[1;4H\x1b[K",
	// OSC 0 title and OSC 9;4 progress.
	"osc.bin": "\x1b]0;claude working\x07\x1b]9;4;1;40\x07ready\r\n",
	// A Claude-shaped idle prompt box, for the detection region.
	"promptbox.bin": "some output\r\n" +
		"────────────────────\r\n" +
		"❯ \r\n" +
		"────────────────────\r\n",
}

func main() {
	dir := filepath.Join("testdata", "vt")
	for name, body := range streams {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %d streams to %s\n", len(streams), dir)
}
