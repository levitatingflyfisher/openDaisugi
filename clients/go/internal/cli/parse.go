package cli

import (
	"fmt"
	"strings"
)

// opt is one command-line option, spelled the way the Python CLI (typer
// over click) spells it.
type opt struct {
	names    []string // "--root", or "--yes" and "-y"
	neg      string   // "--no-gate" for a --gate/--no-gate pair
	value    bool     // takes a value
	multiple bool     // may repeat; every value kept
	help     string
	metavar  string
}

// parsed holds what parseArgs read.
type parsed struct {
	vals  map[string][]string // by the option's first name
	flags map[string]*bool    // set flags; nil when absent
	args  []string
	help  bool
}

func (p *parsed) str(name, def string) string {
	if v := p.vals[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

func (p *parsed) has(name string) bool { return len(p.vals[name]) > 0 }

func (p *parsed) flag(name string) bool {
	b := p.flags[name]
	return b != nil && *b
}

func (p *parsed) flagSet(name string) bool { return p.flags[name] != nil }

// usageError is click's UsageError: exit 2 with the usage line first.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// parseArgs reads args as click does: options anywhere, "--opt=value" or
// "--opt value" (the next word is the value even when it starts with a
// dash), "--" ending the options, and --help anywhere.
func parseArgs(args []string, opts []opt, maxArgs int) (*parsed, error) {
	p := &parsed{vals: map[string][]string{}, flags: map[string]*bool{}}
	find := func(name string) (*opt, bool) {
		for i := range opts {
			o := &opts[i]
			for _, n := range o.names {
				if n == name {
					return o, true
				}
			}
			if o.neg != "" && o.neg == name {
				return o, false
			}
		}
		return nil, false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.args = append(p.args, args[i+1:]...)
			break
		}
		if a == "--help" {
			p.help = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			p.args = append(p.args, a)
			continue
		}
		name, value, hasEq := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, value, hasEq = a, "", false
		}
		o, positive := find(name)
		if o == nil {
			return nil, &usageError{fmt.Sprintf("No such option: %s", name)}
		}
		key := o.names[0]
		if !o.value {
			if hasEq {
				return nil, &usageError{fmt.Sprintf("Option '%s' does not take a value.", name)}
			}
			b := positive || o.neg == ""
			if o.neg != "" && name == o.neg {
				b = false
			}
			p.flags[key] = &b
			continue
		}
		if !hasEq {
			if i+1 >= len(args) {
				return nil, &usageError{fmt.Sprintf("Option '%s' requires an argument.", name)}
			}
			i++
			value = args[i]
		}
		if o.multiple {
			p.vals[key] = append(p.vals[key], value)
		} else {
			p.vals[key] = []string{value}
		}
	}
	if !p.help && len(p.args) > maxArgs {
		extra := p.args[maxArgs:]
		return nil, &usageError{fmt.Sprintf("Got unexpected extra argument(s) (%s)", strings.Join(extra, " "))}
	}
	return p, nil
}
