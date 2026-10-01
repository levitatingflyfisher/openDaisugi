//go:build mujoco

package robotics

import (
	"os"
	"strings"
	"testing"
)

func TestSmolVLARunsAChunkOnTheArm(t *testing.T) {
	dir := os.Getenv("DAISUGI_SMOLVLA_DIR")
	if dir == "" {
		t.Skip("DAISUGI_SMOLVLA_DIR is not set")
	}
	v, s, err := NewSmolVLA(fixture("two_joint_arm.xml"), dir, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	defer s.Close()
	r, err := v.Run(step(t, `{"type": "vla", "id": "v", "task": "pick up the block", "max_actions": 20, "timeout_s": 120}`), 120, 1<<20)
	if err != nil || r.RC != 0 {
		t.Fatalf("rc %d: %v %s", r.RC, err, r.Stdout)
	}
	if !strings.Contains(r.Stdout, `"actions_executed": 20`) {
		t.Errorf("stdout %s", r.Stdout)
	}
	if s.Chunks != 1 || len(s.LastChunk) != 50 || len(s.LastChunk[0]) != 6 || len(s.LastImage) != 240*320*3 {
		t.Errorf("chunks %d, last chunk %dx%d, image %d bytes", s.Chunks, len(s.LastChunk), len(s.LastChunk[0]), len(s.LastImage))
	}
	if q := v.Data().QPos(); q[0] == 0 && q[1] == 0 && q[2] == 0 {
		t.Error("the arm did not move")
	}
}

func TestSmolVLAWithoutTheModelIsAnError(t *testing.T) {
	if _, _, err := NewSmolVLA(fixture("two_joint_arm.xml"), t.TempDir(), 1, 0); err == nil {
		t.Fatal("an empty model directory opened")
	}
}
