// Command alias-probe is a test instrument for clients/alias_compare.py:
// it reads the alias cases file named by its one argument and, for each
// case, registers and resolves aliases as clients/alias_cases.py has the
// oracle do, writing one JSON line per case. It is not shipped.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"

	"daisugi-verify/internal/aliases"
	"daisugi-verify/internal/pyjson"
)

func outcome(v any, err error) *pyjson.Object {
	if err != nil {
		var e *aliases.Error
		if errors.As(err, &e) {
			return pyjson.NewObject().Set("error", e.Class).Set("message", e.Msg)
		}
		return pyjson.NewObject().Set("error", "Exception").Set("message", err.Error())
	}
	return pyjson.NewObject().Set("ok", v)
}

func strs(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, x.(string))
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func run(c *pyjson.Object) *pyjson.Object {
	reg := aliases.New()
	out := pyjson.NewObject()
	if pyjson.Truthy(c.Value("system")) {
		out.Set("system", outcome(nil, aliases.LoadSystem(reg)))
	}
	var regs, res, looks []any
	for _, x := range asList(c.Value("register")) {
		a := x.(*pyjson.Object)
		regs = append(regs, outcome(nil, reg.Register(aliases.Alias{Name: a.Value("name").(string),
			Params: strs(a.Value("params")), Expr: a.Value("expr"), Tier: a.Value("tier").(string),
			Description: a.Value("description").(string)})))
	}
	for _, e := range asList(c.Value("resolve")) {
		res = append(res, outcome(aliases.ParseAndResolve(reg, e)))
	}
	for _, n := range asList(c.Value("lookup")) {
		a, err := reg.Lookup(n.(string))
		if err != nil {
			looks = append(looks, outcome(nil, err))
			continue
		}
		looks = append(looks, outcome(a.Dump(), nil))
	}
	out.Set("register", orEmpty(regs)).Set("resolve", orEmpty(res)).Set("lookup", orEmpty(looks))
	return out
}

func orEmpty(v []any) []any {
	if v == nil {
		return []any{}
	}
	return v
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: alias-probe CASES.jsonl")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for sc.Scan() {
		v, err := pyjson.Loads(sc.Text())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c := v.(*pyjson.Object)
		fmt.Fprintln(w, pyjson.Dumps(pyjson.NewObject().Set("id", c.Value("id")).Set("result", run(c)), true))
	}
}
