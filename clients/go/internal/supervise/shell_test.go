package supervise

import (
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

func shellStep(cmd string) *pyjson.Object {
	return pyjson.NewObject().Set("id", "s1").Set("type", "shell").Set("command", cmd)
}

// The shell exits at once; a background child writes three seconds later.
// The reader keeps reading to EOF within the step's time.
func TestShellReadsALateBackgroundWriterToEOF(t *testing.T) {
	res, err := Shell{}.Run(shellStep("(sleep 3; echo late) & echo early"), 10, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 0 || res.Stdout != "early\nlate\n" || res.TimedOut {
		t.Fatalf("got %+v", res)
	}
}

func TestShellKeepsTheOutputReadSoFarPastTheStepTime(t *testing.T) {
	start := time.Now()
	res, err := Shell{}.Run(shellStep("sleep 30 & echo early"), 3, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 0 || res.Stdout != "early\n" {
		t.Fatalf("got %+v", res)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}
