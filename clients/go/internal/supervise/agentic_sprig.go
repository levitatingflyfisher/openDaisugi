package supervise

import (
	"fmt"
	"strings"

	"daisugi-verify/internal/pystr"
)

// sprigExitText is _sprig_exit_text: the exit code (or the signal) and the
// last line sprig wrote to stderr.
func sprigExitText(rc int, stderr []byte) string {
	if rc < 0 {
		return fmt.Sprintf("agentic sub-agent failed: sprig was killed by signal %d", -rc)
	}
	text := fmt.Sprintf("agentic sub-agent failed: sprig exited with code %d", rc)
	var last string
	for _, ln := range strings.Split(pystr.DecodeReplace(stderr), "\n") {
		if s := pystr.Strip(ln); s != "" {
			last = s
		}
	}
	if last != "" {
		text += ": " + pystr.Slice(last, 0, 500)
	}
	return text
}

// sprigDrainS is SPRIG_DRAIN_S: how long the output is read after the
// timeout kill.
const sprigDrainS = 2
