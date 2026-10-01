package foreman

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
)

// --standing dials tools.sock in the log dir of the data dir it is given,
// else of $COPPICE_DATA_DIR.
func TestStandingFindsTheSocketOfTheDataDir(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.MkdirAll(LogDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", StandingSock(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadBytes('\n')
			_, _ = c.Write([]byte(`{"ok":true,"result":"from the standing socket"}` + "\n"))
			c.Close()
		}
	}()
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}` + "\n"
	for _, tc := range []struct {
		name    string
		dataDir string
		env     string
		argv    []string
	}{
		{"flag", dir, "", []string{"--standing"}},
		{"no args", dir, "", nil},
		{"env", "", dir, []string{"--standing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COPPICE_DATA_DIR", tc.env)
			var out, errb strings.Builder
			if code := ToolsMainIn(tc.dataDir, tc.argv, strings.NewReader(call), &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			if !strings.Contains(out.String(), "from the standing socket") {
				t.Fatalf("out = %s", out.String())
			}
		})
	}
	var out, errb strings.Builder
	if code := ToolsMainIn(dir, []string{"--standing", "--sock", "x"}, strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("both flags: exit %d %q", code, errb.String())
	}
}
