package cli

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/skills"
)

// The page names only commands the CLI has, and exactly the thirteen the
// foreman needs. It never teaches the foreman to say yes to an ask.
func TestTheSkillPageNamesOnlyCommandsThatExist(t *testing.T) {
	page, err := os.ReadFile("../../skills/foreman/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?m)^coppice ([a-z]+) ([a-z-]+)")
	seen := 0
	for _, m := range re.FindAllStringSubmatch(string(page), -1) {
		if _, ok := verbSpecs[m[1]][m[2]]; !ok {
			t.Fatalf("SKILL.md names %s %s, which the CLI does not have", m[1], m[2])
		}
		seen++
	}
	// coppice new has no verb after it, so the pattern does not count it.
	if seen != 12 {
		t.Fatalf("the page names %d commands, want 12", seen)
	}
	if strings.Contains(string(page), "allow") {
		t.Fatal("the page must not teach allow")
	}
}

// The binary carries the page, so a floor on another machine still has it.
func TestTheBinaryCarriesTheSamePage(t *testing.T) {
	page, err := os.ReadFile("../../skills/foreman/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if skills.Foreman != string(page) {
		t.Fatal("skills.Foreman differs from skills/foreman/SKILL.md")
	}
}

// coppice skill foreman prints the page and dials no socket.
func TestSkillForemanPrintsThePageWithNoServer(t *testing.T) {
	t.Setenv("COPPICE_NO_AUTOSTART", "1")
	var out, errb bytes.Buffer
	c := &CLI{Socket: t.TempDir() + "/none.sock", DataDir: t.TempDir(), Out: &out, Err: &errb}
	if code := c.Run([]string{"skill", "foreman"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if out.String() != skills.Foreman {
		t.Fatalf("printed %q", out.String())
	}
}

func TestSkillWithAnUnknownNameIsRefused(t *testing.T) {
	var out, errb bytes.Buffer
	c := &CLI{Out: &out, Err: &errb}
	if code := c.Run([]string{"skill", "nope"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "foreman") {
		t.Fatalf("refusal does not name the known page: %q", errb.String())
	}
}

// Every command line on the page parses with sample values, so no flag it
// teaches is unknown or takes the wrong kind of value. The prompt line
// carries --wait, because --until alone waits for nothing.
func TestEveryCommandOnTheSkillPageParses(t *testing.T) {
	samples := strings.NewReplacer(
		"<pane>", "w1:p1", "<text>", "hi there", "<name>", "docs", "<absolute dir>", "/srv/repo",
		"<dir>", "/srv/repo", "<ms>", "5000", "<argv>", "sh", "<ask>", "toolu_1", "<id>", "t1", "[", "", "]", "",
	)
	alt := regexp.MustCompile(`([a-z_]+)(\|[a-z_]+)+`)
	lines := regexp.MustCompile("(?m)^coppice .*$").FindAllString(skills.Foreman, -1)
	if len(lines) != 13 {
		t.Fatalf("%d command lines, want 13", len(lines))
	}
	sawWait := false
	for _, line := range lines {
		// coppice new is a command of its own, not a socket verb. Its
		// line must name a project and never attach.
		if strings.HasPrefix(line, "coppice new ") {
			if line != "coppice new <project> --no-attach" {
				t.Fatalf("the new line is %q", line)
			}
			continue
		}
		filled := alt.ReplaceAllString(samples.Replace(line), "$1")
		words := strings.Fields(filled)
		params, _, errMsg := parseVerbArgs(words[1], words[2], words[3:])
		if errMsg != "" {
			t.Fatalf("%q does not parse: %s", line, errMsg)
		}
		if words[1] == "agent" && words[2] == "prompt" {
			sawWait = params["wait"] == true
		}
	}
	if !sawWait {
		t.Fatal("the agent prompt line does not teach --wait")
	}
	for _, want := range []string{"coppice task create", "--task <id>", "coppice project list",
		"Always name the project", "one line per agent", "The gate stays outside you",
		"An ask that cannot be undone\n   is never yours"} {
		if !strings.Contains(skills.Foreman, want) {
			t.Fatalf("the page does not teach %q", want)
		}
	}
	for _, never := range []string{"task set-foreman", "task move"} {
		if strings.Contains(skills.Foreman, never) {
			t.Fatalf("the page teaches %q", never)
		}
	}
	if !strings.Contains(skills.Foreman, "may never come") {
		t.Fatal("the page does not say the typed line may never come")
	}
	if strings.Contains(skills.Foreman, "when the note says") || !strings.Contains(skills.Foreman, "hold id") {
		t.Fatal("the page does not say the typed line carries the hold id and the time")
	}
	if !strings.Contains(skills.Foreman, "only one that you hold") {
		t.Fatal("the page does not say the foreman may refuse only the asks it holds")
	}
}
