package sprig

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestChildEnvDropsKeysAndScrubNamesSorted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k1")
	t.Setenv("OPENAI_API_KEY", "k2")
	t.Setenv("SPRIG_API_KEY", "k3")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "k4")
	t.Setenv("MY_SECRET", "k5")
	t.Setenv("ZZ_KEEP", "yes")
	SetScrubEnv([]string{"MY_SECRET", ""})
	defer SetScrubEnv(nil)
	env := ChildEnv("")
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"k1", "k2", "k3", "k4", "k5"} {
		if strings.Contains(joined, "="+bad) {
			t.Fatalf("child env holds %s:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "ZZ_KEEP=yes") {
		t.Fatal("child env lost a plain variable")
	}
	for i := 1; i < len(env); i++ {
		a, _, _ := strings.Cut(env[i-1], "=")
		b, _, _ := strings.Cut(env[i], "=")
		if a > b {
			t.Fatalf("child env not sorted by name: %s before %s", a, b)
		}
	}
}

func TestChildEnvSetsPwdForADir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")
	env := ChildEnv("/tmp")
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") {
			n++
			if kv != "PWD=/tmp" {
				t.Fatalf("PWD=%q", kv)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want one PWD, got %d", n)
	}
}

func TestReadKeyFdTrimsOneNewlineAndCloses(t *testing.T) {
	fd := rawPipe(t, "sk-test\n\n")
	key, err := ReadKeyFd("--key-fd", fd)
	if err != nil || key != "sk-test\n" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("fd %d still open after the read (err=%v)", fd, err)
	}
}

func TestReadKeyFdRefusesStdio(t *testing.T) {
	if _, err := ReadKeyFd("--key-fd", 1); err == nil || !strings.Contains(err.Error(), "3 or more") {
		t.Fatalf("err=%v", err)
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("x sk-1 y sk-1", "sk-1"); got != "x [key] y [key]" {
		t.Fatal(got)
	}
	if got := Redact("abc", ""); got != "abc" {
		t.Fatal(got)
	}
}

type errModel struct{ text string }

func (e errModel) Next([]Message) (Message, error) { return Message{}, errors.New(e.text) }

func TestCLIKeyFdReachesTheBackendAndIsRedacted(t *testing.T) {
	fd := rawPipe(t, "sk-SENTINEL-unit\n")
	var got ModelOptions
	cli := &CLI{Version: "0", NewModel: func(o ModelOptions) (Model, error) {
		got = o
		return errModel{"provider said: bad key sk-SENTINEL-unit"}, nil
	}}
	var out, errb bytes.Buffer
	code := cli.Run([]string{"--key-fd", itoa(fd), "task"}, strings.NewReader(""), &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !got.KeySet || got.Key != "sk-SENTINEL-unit" {
		t.Fatalf("options %+v", got)
	}
	if strings.Contains(errb.String(), "SENTINEL") || !strings.Contains(errb.String(), "[key]") {
		t.Fatalf("stderr %q", errb.String())
	}
}

func TestCLIKeyFdEmptyRefuses(t *testing.T) {
	fd := rawPipe(t, "")
	var out, errb bytes.Buffer
	code := newCLI("x").Run([]string{"--key-fd", itoa(fd), "task"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "held no key") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
}

func TestBashToolGetsTheScrubbedEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-bash-leak")
	out, err := BashTool{}.Run(map[string]any{"cmd": "env"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "sk-bash-leak") {
		t.Fatal("bash saw the key")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// rawPipe is the read end of a pipe that holds text, as a bare
// descriptor: no *os.File owns it, so no finalizer closes its number
// after the code under test has closed it and the number is reused.
func rawPipe(t *testing.T, text string) int {
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	syscall.Write(p[1], []byte(text))
	syscall.Close(p[1])
	return p[0]
}

func TestKeyHasControl(t *testing.T) {
	if !KeyHasControl("a\nb") || !KeyHasControl("a\x7f") || KeyHasControl("sk-ok") {
		t.Fatal("control check")
	}
}

type keyedErrModel struct{ errModel }

func (keyedErrModel) HeldKey() string { return "sk-from-env" }

func TestCLIRedactsTheKeyTheBackendHolds(t *testing.T) {
	cli := &CLI{Version: "0", NewModel: func(ModelOptions) (Model, error) {
		return keyedErrModel{errModel{"echo sk-from-env"}}, nil
	}}
	var out, errb bytes.Buffer
	cli.Run([]string{"task"}, strings.NewReader(""), &out, &errb)
	if errb.String() != "sprig: echo [key]\n" {
		t.Fatalf("stderr %q", errb.String())
	}
}
