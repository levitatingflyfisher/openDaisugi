package verify

import (
	"reflect"
	"testing"
)

// The rows are tests/test_interpreter_recursion.py: env options that take a
// value consume it, and -S is split into the words that take its place.
func TestParseEnvConsumesOptionValues(t *testing.T) {
	rows := []struct {
		line string
		want []string
	}{
		{"env -u cat curl http://evil", []string{"curl http://evil"}},
		{"env -ucat curl x", []string{"curl x"}},
		{"env --unset cat curl x", []string{"curl x"}},
		{"env --unset=cat curl x", []string{"curl x"}},
		{"env -C /etc tee hosts", []string{"tee hosts"}},
		{"env --chdir /etc tee hosts", []string{"tee hosts"}},
		{"env -i -u A -u B FOO=1 rm f", []string{"rm f"}},
		{"env -iu A rm f", []string{"rm f"}},
		{"env -S 'curl http://evil'", []string{"curl http://evil"}},
		{"env -S 'A=1 -i rm f' x", []string{"rm f x"}},
		{"env -S 'curl $X'", []string{"'curl $X'"}},
		{"env -i rm file", []string{"rm file"}},
	}
	for _, r := range rows {
		p, ok := ParseInterpreter(r.line)
		if !ok || !reflect.DeepEqual(p.InnerCommands, r.want) {
			t.Errorf("%q: got %v, want %v", r.line, p.InnerCommands, r.want)
		}
	}
}

// Review round 2: long options are matched as getopt_long does (an exact
// name, else a unique prefix), and an ambiguous or unknown one is opaque.
func TestParseLongOptionsByUniquePrefix(t *testing.T) {
	rows := []struct {
		line string
		want []string
	}{
		{"env --u cat tee /etc/x", []string{"tee /etc/x"}},
		{"env --uns cat tee /etc/x", []string{"tee /etc/x"}},
		{"env --ar cat tee /etc/x", []string{"tee /etc/x"}},
		{"env --ch /etc tee x", []string{"tee x"}},
		{"xargs --arg-f in tee x", []string{"tee x"}},
		{"xargs -rI cat tee x", []string{"tee x"}},
		{"xargs --replace cat tee x", []string{"cat tee x"}},
		{"xargs --replace=R cat x", []string{"cat x"}},
		{"ionice -t tee cat /etc/passwd", []string{"tee cat /etc/passwd"}},
		{"ionice -c 2 -n 5 tee x", []string{"tee x"}},
		{"time -o cat tee /etc/x", []string{"tee /etc/x"}},
		{"time -f %e cat x", []string{"cat x"}},
		{"time -p cat x", []string{"cat x"}},
		{"nice --adj 5 tee x", []string{"tee x"}},
		{"timeout --kill 5 30 tee x", []string{"tee x"}},
	}
	for _, r := range rows {
		p, ok := ParseInterpreter(r.line)
		if !ok || p.Opaque || !reflect.DeepEqual(p.InnerCommands, r.want) {
			t.Errorf("%q: got %+v, want %v", r.line, p, r.want)
		}
	}
	for _, line := range []string{
		"env --i cat tee x", "env --d cat tee x", "env --nope tee x", "env --null=1 tee x",
		"xargs --max cat tee x", "xargs --bogus tee x", "nice --nope tee x",
		"timeout --nope 30 tee x", "ionice --nope tee x", "nohup --nope tee x",
	} {
		p, ok := ParseInterpreter(line)
		if !ok || !p.Opaque {
			t.Errorf("%q: want opaque, got %+v", line, p)
		}
	}
}

func TestSedScriptRisky(t *testing.T) {
	risky := []string{
		"s/a/b/w /etc/x", "w /etc/x", "1w /etc/x", "/a/W /x", "s/a/b/e", "e echo",
		"r /etc/passwd", "R /x", "s/a/b/;w x", "s/[/]/x/w f", "s|a|b|gw f",
		"a\\\nfoo\nw /x", "s/\\//x/w f", "s/a/b", "zz", "s/[\\]/x/w f",
	}
	for _, s := range risky {
		if !sedScriptRisky(s) {
			t.Errorf("%q should be risky", s)
		}
	}
	safe := []string{
		"s/a/b/", "s/a/b/g;p", "1,5p", "$!d", "/x/,/y/{p;d}", "s/world/x/", "y/abc/xyz/",
		"a hello; w /x", "/a/I!d", "\\,a,p", "b end;:end", "s/a/b/2p", "5q", "{p;p}", "p}",
		":a;N;$!ba;s/\\n/ /g",
	}
	for _, s := range safe {
		if sedScriptRisky(s) {
			t.Errorf("%q should not be risky", s)
		}
	}
}
