package sprig

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func procAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	return len(f) > 0 && f[0] != "Z"
}

// A bash call runs in a process group of its own; when sprig ends, every
// such group is killed, so a background process a call left dies too.
func TestSprigKillsItsToolGroupsWhenItEnds(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := "sleep 300 </dev/null >/dev/null 2>&1 & echo $! > " + pidFile + "; ps -o pgid= -p $$"
	turns := []Message{
		{Role: "assistant", Calls: []ToolCall{{Name: "bash", Input: map[string]any{"cmd": cmd}}}},
		{Role: "assistant", Text: "done"},
	}
	c := &CLI{Version: "9", NewModel: func(ModelOptions) (Model, error) { return &scriptModel{turns: turns}, nil }}
	var out, errb bytes.Buffer
	if code := c.Run([]string{"x"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("no pid")
	}
	end := time.Now().Add(5 * time.Second)
	for procAlive(pid) && time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
	}
	if procAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the background process outlived sprig")
	}
}

// Each bash call's group is not sprig's own group.
func TestABashCallHasAGroupOfItsOwn(t *testing.T) {
	out, err := BashTool{}.Run(map[string]any{"cmd": "ps -o pgid= -p $$; echo $$"})
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(out)
	if len(f) != 2 || f[0] != f[1] || f[0] == strconv.Itoa(syscall.Getpgrp()) {
		t.Fatalf("got %q", out)
	}
	KillToolGroups()
}
