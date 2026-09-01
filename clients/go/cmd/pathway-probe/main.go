// Command pathway-probe runs the pathway matcher on many queries in one
// process, for clients/pathway_compare.py. It is a test instrument: the
// daisugi binary has no find command, as the Python CLI has none.
//
//	pathway-probe find DB_PATH < queries.jsonl   one {"task": ...} per line
//	pathway-probe tokens TOKENIZER_JSON < texts.jsonl
//	pathway-probe time DB_PATH TASK N            N cold-equivalent finds, timed
//
// find reads matcher_model from $HOME/.opendaisugi/config.yaml, as the
// oracle's find does, and writes one JSON line per query: the matched id
// and score, the stale warning, and why nothing could match.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"daisugi-verify/internal/config"
	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/pathways"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: pathway-probe find DB | tokens TOKENIZER_JSON | time DB TASK N")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "find":
		os.Exit(find(os.Args[2]))
	case "tokens":
		os.Exit(tokens(os.Args[2]))
	case "time":
		os.Exit(timeFind(os.Args[2:]))
	}
	os.Exit(2)
}

func env() potion.Env {
	home, _ := os.UserHomeDir()
	return potion.Env{
		Lookup:  os.LookupEnv,
		Home:    home,
		Notice:  func(s string) { fmt.Fprintln(os.Stderr, s) },
		NoFetch: os.Getenv("PATHWAY_PROBE_NO_FETCH") != "",
	}
}

func matcherKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(filepath.Join(home, ".opendaisugi", "config.yaml"))
	if err != nil {
		return "", err
	}
	return cfg.MatcherModel, nil
}

type answer struct {
	ID      *string  `json:"id"`
	Score   *float64 `json:"score"`
	Warning string   `json:"warning,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Refused string   `json:"refused,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func find(db string) int {
	key, err := matcherKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	s, err := pathways.Open(db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		return 1
	}
	defer s.Close()
	pe := env()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<26)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for in.Scan() {
		var q struct {
			Task      string   `json:"task"`
			Threshold *float64 `json:"threshold"`
		}
		if err := json.Unmarshal(in.Bytes(), &q); err != nil {
			fmt.Fprintln(os.Stderr, "query:", err)
			return 1
		}
		th := -1.0
		if q.Threshold != nil {
			th = *q.Threshold
		}
		res, err := s.Find(q.Task, key, pe, th)
		var a answer
		switch {
		case errors.Is(err, pathways.ErrNotCarried):
			a.Refused = err.Error()
		case err != nil:
			a.Error = err.Error()
		default:
			a.Warning, a.Reason = res.Warning, res.Reason
			if res.Match != nil {
				id, sc := res.Match.Pathway.ID(), res.Match.Similarity
				a.ID, a.Score = &id, &sc
			}
		}
		_ = enc.Encode(a)
	}
	return 0
}

func tokens(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tok, err := potion.ParseTokenizer(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<26)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		var text string
		if err := json.Unmarshal(in.Bytes(), &text); err != nil {
			fmt.Fprintln(os.Stderr, "text:", err)
			return 1
		}
		ids := tok.Encode(text)
		if ids == nil {
			ids = []int{}
		}
		b, _ := json.Marshal(ids)
		out.Write(append(b, '\n'))
	}
	return 0
}

func timeFind(args []string) int {
	if len(args) != 3 {
		return 2
	}
	var n int
	fmt.Sscan(args[2], &n)
	key, err := matcherKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for i := 0; i < n; i++ {
		t0 := time.Now()
		s, err := pathways.Open(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		res, err := s.Find(args[1], key, env(), -1)
		s.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		id := ""
		if res.Match != nil {
			id = res.Match.Pathway.ID()
		}
		fmt.Printf("%.2f ms %s\n", float64(time.Since(t0).Microseconds())/1000, id)
	}
	return 0
}
