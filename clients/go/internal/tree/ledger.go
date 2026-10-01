package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Error is tree.TreeError: what the command tried, why not, and the fix.
type Error struct {
	What, Why, Fix string
	Code           int
}

func (e *Error) Error() string { return e.Why }

// LedgerPath is tree.ledger_path.
func LedgerPath(dataDir string) string { return filepath.Join(dataDir, "tree", "ledger.jsonl") }

var maxCount = new(big.Int).Lsh(big.NewInt(1), 53)

// count is tree._count: None, or an int (never a bool) from 0 to 2**53.
func count(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		return ok && n.Sign() >= 0 && n.Cmp(maxCount) <= 0
	}
	return false
}

func finite(v any) bool {
	switch v.(type) {
	case pyjson.Int, pyjson.Float, float64:
	default:
		return false
	}
	f, ok := Num(v)
	return ok && !isInfNaN(f)
}

func isInfNaN(f float64) bool {
	return f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308
}

func isStr(o *pyjson.Object, k string) bool { _, ok := o.Value(k).(string); return ok }

func allStrs(v any) bool {
	l, ok := v.([]any)
	if !ok {
		return false
	}
	for _, x := range l {
		if _, ok := x.(string); !ok {
			return false
		}
	}
	return true
}

// fits is tree._fits.
func fits(row *pyjson.Object) bool {
	if !finite(row.Value("ts")) {
		return false
	}
	deadlineOK := row.Value("deadline") == nil || finite(row.Value("deadline"))
	switch row.Value("event") {
	case "root":
		return isStr(row, "session") && isStr(row, "envelope_id") && count(row.Value("tokens")) &&
			count(row.Value("turns")) && deadlineOK
	case "spawn":
		_, proved := row.Value("proved").(bool)
		return isStr(row, "parent") && isStr(row, "session") && isStr(row, "envelope_id") &&
			count(row.Value("tokens")) && count(row.Value("turns")) && deadlineOK && proved
	case "refused":
		return isStr(row, "parent") && isStr(row, "session") && allStrs(row.Value("reasons"))
	case "ask":
		_, env := row.Value("envelope").(*pyjson.Object)
		return isStr(row, "ask_id") && isStr(row, "parent") && isStr(row, "session") &&
			count(row.Value("tokens")) && count(row.Value("turns")) && env && allStrs(row.Value("reasons"))
	case "answer":
		a := row.Value("answer")
		return isStr(row, "ask_id") && (a == "allow" || a == "deny")
	case "end":
		return isStr(row, "session") && count(row.Value("tokens_used")) && count(row.Value("turns_used"))
	}
	return false
}

// Node is tree.Node. Budgets and uses are nil for none.
type Node struct {
	Session, EnvelopeID string
	Parent              *string
	Tokens, Turns       *big.Int
	Deadline            any
	Proved              bool
	State               string
	TokensUsed          *big.Int
	TurnsUsed           *big.Int
	Children            []string
	Refused             int
}

// Tree is tree.Tree.
type Tree struct {
	Nodes   map[string]*Node
	Roots   []string
	Asks    map[string]*pyjson.Object
	AskList []string
	Answers map[string]string
	Skipped int
}

// Running is Tree.running.
func (t *Tree) Running(s string) *Node {
	if n := t.Nodes[s]; n != nil && n.State == "running" {
		return n
	}
	return nil
}

func (n *Node) budget(axis string) *big.Int {
	if axis == "tokens" {
		return n.Tokens
	}
	return n.Turns
}

func (n *Node) used(axis string) *big.Int {
	if axis == "tokens" {
		return n.TokensUsed
	}
	return n.TurnsUsed
}

// Left is Tree.left: what node may still reserve for children.
func (t *Tree) Left(n *Node, axis string) *big.Int {
	b := n.budget(axis)
	if b == nil {
		return nil
	}
	spent := new(big.Int)
	for _, c := range n.Children {
		child := t.Nodes[c]
		reserved := new(big.Int)
		if r := child.budget(axis); r != nil {
			reserved.Set(r)
		}
		if u := child.used(axis); child.State == "ended" && u != nil && u.Cmp(reserved) < 0 {
			reserved.Set(u)
		}
		spent.Add(spent, reserved)
	}
	return new(big.Int).Sub(b, spent)
}

// OpenAsks is Tree.open_asks, in the order the asks were made.
func (t *Tree) OpenAsks() []*pyjson.Object {
	var out []*pyjson.Object
	for _, k := range t.AskList {
		if _, done := t.Answers[k]; !done {
			out = append(out, t.Asks[k])
		}
	}
	return out
}

func optBig(v any) *big.Int {
	if v == nil {
		return nil
	}
	return bigOf(v)
}

// ReadTree is tree.read_tree.
func ReadTree(dataDir string) *Tree {
	t := &Tree{Nodes: map[string]*Node{}, Asks: map[string]*pyjson.Object{}, Answers: map[string]string{}}
	raw, err := os.ReadFile(LedgerPath(dataDir))
	if err != nil {
		return t
	}
	text := pystr.DecodeReplace(raw)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if pystr.Strip(line) == "" {
			continue
		}
		v, jerr := pyjson.LoadsPy(line, 900)
		if jerr != nil {
			t.Skipped++
			continue
		}
		row, ok := v.(*pyjson.Object)
		if !ok || !fits(row) {
			t.Skipped++
			continue
		}
		switch row.Value("event") {
		case "root", "spawn":
			sid := row.Value("session").(string)
			var parent *string
			if row.Value("event") == "spawn" {
				p := row.Value("parent").(string)
				parent = &p
			}
			if t.Nodes[sid] != nil || (parent != nil && t.Running(*parent) == nil) {
				t.Skipped++
				continue
			}
			proved := true
			if b, ok := row.Value("proved").(bool); ok {
				proved = b
			}
			var deadline any
			if d := row.Value("deadline"); d != nil {
				f, _ := Num(d)
				deadline = pyjson.Float(f)
			}
			t.Nodes[sid] = &Node{Session: sid, Parent: parent, EnvelopeID: row.Value("envelope_id").(string),
				Tokens: optBig(row.Value("tokens")), Turns: optBig(row.Value("turns")), Deadline: deadline,
				Proved: proved, State: "running"}
			if parent == nil {
				t.Roots = append(t.Roots, sid)
			} else {
				p := t.Nodes[*parent]
				p.Children = append(p.Children, sid)
				p.Refused = 0
			}
		case "refused":
			p := t.Nodes[row.Value("parent").(string)]
			if p == nil {
				t.Skipped++
				continue
			}
			p.Refused++
		case "ask":
			id := row.Value("ask_id").(string)
			if t.Asks[id] != nil || t.Nodes[row.Value("parent").(string)] == nil {
				t.Skipped++
				continue
			}
			t.Asks[id] = row
			t.AskList = append(t.AskList, id)
		case "answer":
			id := row.Value("ask_id").(string)
			a := t.Asks[id]
			if _, done := t.Answers[id]; a == nil || done {
				t.Skipped++
				continue
			}
			t.Answers[id] = row.Value("answer").(string)
			t.Nodes[a.Value("parent").(string)].Refused = 0
		case "end":
			n := t.Running(row.Value("session").(string))
			if n == nil {
				t.Skipped++
				continue
			}
			n.State = "ended"
			n.TokensUsed = optBig(row.Value("tokens_used"))
			n.TurnsUsed = optBig(row.Value("turns_used"))
		}
	}
	return t
}

// locked is tree._locked: the fold, the check and the append under one
// lock on tree/ledger.lock.
func locked(dataDir string, fn func() error) error {
	d := filepath.Dir(LedgerPath(dataDir))
	if err := os.MkdirAll(d, 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "ledger.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func appendRows(dataDir string, rows ...*pyjson.Object) error {
	f, err := os.OpenFile(LedgerPath(dataDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(pyjson.Dumps(r, true) + "\n")
	}
	_, err = f.WriteString(b.String())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Now is tree.now.
func Now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func checkSession(s, what string) error {
	if !ValidSession(s) {
		return &Error{what, "The session id " + Q(s) + " is not 1 to 64 of A-Z a-z 0-9 . _ - " +
			"(no dot at either end), or it is default.", "Pick another session id.", 2}
	}
	return nil
}

func checkCounts(names []string, vals []*big.Int) error {
	for i, v := range vals {
		if v != nil && (v.Sign() < 0 || v.Cmp(maxCount) > 0) {
			return &Error{"Tried to read the budget.", "--" + names[i] + " must be a whole number from 0 to 2**53.",
				"Fix the option and run again.", 2}
		}
	}
	return nil
}

func envelopeFile(gateRoot, session string) string {
	return gateroot.Join(gateroot.EnvelopesDir(gateRoot), session+".json")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// LoadRegistered is tree.load_registered: the envelope registered for a
// session, or nil when there is none or it does not read as one.
func LoadRegistered(session, gateRoot string) *pyjson.Object {
	name := "default"
	if session != "" {
		name = gateroot.SafeSessionID(session)
	}
	raw, err := os.ReadFile(gateroot.Join(gateroot.EnvelopesDir(gateRoot), name+".json"))
	if err != nil {
		return nil
	}
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, string(raw))
	if verr != nil {
		return nil
	}
	o, _ := v.(*pyjson.Object)
	return o
}

func bigAny(b *big.Int) any {
	if b == nil {
		return nil
	}
	return pyjson.Int{Text: b.String()}
}

// MakeRoot is tree.make_root.
func MakeRoot(dataDir, gateRoot string, env *pyjson.Object, session string, tokens, turns *big.Int) (path string, err error) {
	what := "Tried to make session " + session + " a root of the tree."
	if err := checkSession(session, what); err != nil {
		return "", err
	}
	if err := checkCounts([]string{"tokens", "turns"}, []*big.Int{tokens, turns}); err != nil {
		return "", err
	}
	err = locked(dataDir, func() error {
		t := ReadTree(dataDir)
		if t.Nodes[session] != nil || fileExists(envelopeFile(gateRoot, session)) {
			return &Error{what, "Session " + session + " is in use already.", "Pick another session id.", 1}
		}
		row := pyjson.NewObject().Set("event", "root").Set("session", session).Set("envelope_id", env.Value("id")).
			Set("tokens", bigAny(tokens)).Set("turns", bigAny(turns)).Set("deadline", env.Value("deadline")).Set("ts", Now())
		if err := appendRows(dataDir, row); err != nil {
			return err
		}
		path, _, err = gateroot.Register(env, session, gateRoot)
		return err
	})
	return path, err
}

// Spawn is tree.Spawn.
type Spawn struct {
	Status    string // started, refused or asked
	Reasons   []string
	Path      string
	AskID     string
	Inherited any
}

func askID(parent, session string, env *pyjson.Object, n int) string {
	text := pyjson.CanonicalASCII([]any{parent, session, env, pyjson.Int{Text: fmt.Sprint(n)}})
	sum := sha256.Sum256([]byte(text))
	return "ask_" + hex.EncodeToString(sum[:])[:12]
}

func budgetReasons(t *Tree, parent *Node, tokens, turns *big.Int) []string {
	var out []string
	for _, ax := range []struct {
		name string
		want *big.Int
	}{{"tokens", tokens}, {"turns", turns}} {
		left := t.Left(parent, ax.name)
		if left == nil {
			continue
		}
		if ax.want == nil {
			out = append(out, ax.name+": the parent has a budget, so the child must ask for an amount")
		} else if ax.want.Cmp(left) > 0 {
			out = append(out, fmt.Sprintf("%s: the child asks for %s; the parent has %s left", ax.name, ax.want, left))
		}
	}
	return out
}

func strOf(o *pyjson.Object, k string) string { s, _ := o.Value(k).(string); return s }

// DoSpawn is tree.spawn.
func DoSpawn(dataDir, gateRoot string, env *pyjson.Object, parent, session string, tokens, turns *big.Int, timeoutMs int) (*Spawn, error) {
	what := "Tried to start session " + session + " under " + parent + "."
	if err := checkSession(session, what); err != nil {
		return nil, err
	}
	if err := checkSession(parent, what); err != nil {
		return nil, err
	}
	if err := checkCounts([]string{"tokens", "turns"}, []*big.Int{tokens, turns}); err != nil {
		return nil, err
	}
	var out *Spawn
	err := locked(dataDir, func() error {
		t := ReadTree(dataDir)
		pnode := t.Running(parent)
		if pnode == nil {
			return &Error{what, parent + " is not a running node of the tree.",
				"Start the child under a running node; daisugi tree status lists them.", 1}
		}
		if t.Nodes[session] != nil || fileExists(envelopeFile(gateRoot, session)) {
			return &Error{what, "Session " + session + " is in use already.", "Pick another session id.", 1}
		}
		for _, a := range t.OpenAsks() {
			if a.Value("session") == session {
				return &Error{what, "An ask for session " + session + " is open.",
					"Wait for the operator's answer, or pick another session id.", 1}
			}
		}
		penv := LoadRegistered(parent, gateRoot)
		var res *Result
		var reasons []string
		if penv != nil && penv.Value("stakes") == "physical" {
			reasons = []string{"stakes: the parent's stakes are physical, so it cannot start an agent"}
		} else {
			res = EdgeOK(penv, env, timeoutMs)
			reasons = append(reasons, res.Reasons...)
		}
		reasons = append(reasons, budgetReasons(t, pnode, tokens, turns)...)
		ts := Now()
		if len(reasons) > 0 {
			if pnode.Refused >= AskAfter {
				id := askID(parent, session, env, len(t.Asks))
				row := pyjson.NewObject().Set("event", "ask").Set("ask_id", id).Set("parent", parent).
					Set("session", session).Set("tokens", bigAny(tokens)).Set("turns", bigAny(turns)).
					Set("envelope", env).Set("reasons", anys(reasons)).Set("ts", ts)
				if err := appendRows(dataDir, row); err != nil {
					return err
				}
				out = &Spawn{Status: "asked", Reasons: reasons, AskID: id}
				return nil
			}
			row := pyjson.NewObject().Set("event", "refused").Set("parent", parent).Set("session", session).
				Set("reasons", anys(reasons)).Set("ts", ts)
			if err := appendRows(dataDir, row); err != nil {
				return err
			}
			out = &Spawn{Status: "refused", Reasons: reasons}
			return nil
		}
		child := withKey(res.Child, "parent_envelope", penv.Value("id"))
		row := pyjson.NewObject().Set("event", "spawn").Set("parent", parent).Set("session", session).
			Set("envelope_id", child.Value("id")).Set("tokens", bigAny(tokens)).Set("turns", bigAny(turns)).
			Set("deadline", child.Value("deadline")).Set("proved", true).Set("ts", ts)
		if err := appendRows(dataDir, row); err != nil {
			return err
		}
		path, _, err := gateroot.Register(child, session, gateRoot)
		if err != nil {
			return err
		}
		out = &Spawn{Status: "started", Path: path}
		if res.Inherited {
			out.Inherited = child.Value("deadline")
		}
		return nil
	})
	return out, err
}

// End is tree.end.
func End(dataDir, gateRoot, session string, tokensUsed, turnsUsed *big.Int) (*Node, error) {
	what := "Tried to end session " + session + "."
	if err := checkSession(session, what); err != nil {
		return nil, err
	}
	if err := checkCounts([]string{"tokens-used", "turns-used"}, []*big.Int{tokensUsed, turnsUsed}); err != nil {
		return nil, err
	}
	var node *Node
	err := locked(dataDir, func() error {
		t := ReadTree(dataDir)
		node = t.Running(session)
		if node == nil {
			return &Error{what, session + " is not a running node of the tree.",
				"daisugi tree status lists the running nodes.", 1}
		}
		var busy []string
		for _, c := range node.Children {
			if t.Nodes[c].State == "running" {
				busy = append(busy, c)
			}
		}
		if len(busy) > 0 {
			return &Error{what, "It has running children: " + strings.Join(busy, ", ") + ".", "End them first.", 1}
		}
		row := pyjson.NewObject().Set("event", "end").Set("session", session).
			Set("tokens_used", bigAny(tokensUsed)).Set("turns_used", bigAny(turnsUsed)).Set("ts", Now())
		if err := appendRows(dataDir, row); err != nil {
			return err
		}
		if err := os.Remove(envelopeFile(gateRoot, session)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		node.State = "ended"
		node.TokensUsed, node.TurnsUsed = tokensUsed, turnsUsed
		return nil
	})
	return node, err
}

// Answer is tree.answer: the ask, and the registered path on an allow.
func Answer(dataDir, gateRoot, id, verdict string) (*pyjson.Object, string, error) {
	what := "Tried to answer ask " + id + "."
	if verdict != "allow" && verdict != "deny" {
		return nil, "", &Error{what, "The answer must be allow or deny.", "Run it again with allow or deny.", 2}
	}
	var ask *pyjson.Object
	var path string
	err := locked(dataDir, func() error {
		t := ReadTree(dataDir)
		ask = t.Asks[id]
		if ask == nil {
			return &Error{what, "There is no ask " + id + ".", "daisugi tree status lists the open asks.", 1}
		}
		if a, done := t.Answers[id]; done {
			return &Error{what, "It is answered already: " + a + ".", "An answer stands.", 1}
		}
		ts := Now()
		row := pyjson.NewObject().Set("event", "answer").Set("ask_id", id).Set("answer", verdict).Set("ts", ts)
		if verdict == "deny" {
			return appendRows(dataDir, row)
		}
		parent, session := strOf(ask, "parent"), strOf(ask, "session")
		pnode := t.Running(parent)
		if pnode == nil {
			return &Error{what, parent + " is not a running node of the tree.", "Deny the ask.", 1}
		}
		if t.Nodes[session] != nil || fileExists(envelopeFile(gateRoot, session)) {
			return &Error{what, "Session " + session + " is in use already.", "Deny the ask.", 1}
		}
		if over := budgetReasons(t, pnode, optBig(ask.Value("tokens")), optBig(ask.Value("turns"))); len(over) > 0 {
			return &Error{what, "The budget does not fit: " + strings.Join(over, "; ") + ".", "Deny the ask.", 1}
		}
		v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, pyjson.Dumps(ask.Value("envelope"), false))
		if verr != nil {
			return verr
		}
		// AT-5 holds under an allow too: a physical parent starts no agent.
		if ppar := LoadRegistered(parent, gateRoot); ppar != nil && ppar.Value("stakes") == "physical" {
			return &Error{what, "stakes: the parent's stakes are physical, so it cannot start an agent.", "Deny the ask.", 1}
		}
		// An allow never widens the tree past its root: the child must fit
		// inside the root of its branch.
		root := parent
		for t.Nodes[root].Parent != nil {
			root = *t.Nodes[root].Parent
		}
		edge := EdgeOK(LoadRegistered(root, gateRoot), v.(*pyjson.Object), DefaultTimeoutMs)
		if !edge.Holds || edge.Child == nil {
			return &Error{what, "The child does not fit inside the root " + root + ": " + strings.Join(edge.Reasons, "; ") + ".",
				"Deny the ask, or register the child as a root of its own.", 1}
		}
		child := edge.Child
		if penv := LoadRegistered(parent, gateRoot); penv != nil {
			child = withKey(child, "parent_envelope", penv.Value("id"))
		}
		spawn := pyjson.NewObject().Set("event", "spawn").Set("parent", parent).Set("session", session).
			Set("envelope_id", child.Value("id")).Set("tokens", ask.Value("tokens")).Set("turns", ask.Value("turns")).
			Set("deadline", child.Value("deadline")).Set("proved", false).Set("ts", ts)
		if err := appendRows(dataDir, row, spawn); err != nil {
			return err
		}
		var err error
		path, _, err = gateroot.Register(child, session, gateRoot)
		return err
	})
	return ask, path, err
}

func axisDoc(n *Node, t *Tree, axis string) *pyjson.Object {
	return pyjson.NewObject().Set("budget", bigAny(n.budget(axis))).Set("used", bigAny(n.used(axis))).
		Set("left", bigAny(t.Left(n, axis)))
}

// StatusDoc is tree.status_doc.
func StatusDoc(dataDir string) *pyjson.Object {
	t := ReadTree(dataDir)
	nodes := []any{}
	var walk func(string)
	walk = func(sid string) {
		n := t.Nodes[sid]
		var parent any
		if n.Parent != nil {
			parent = *n.Parent
		}
		nodes = append(nodes, pyjson.NewObject().Set("session", n.Session).Set("parent", parent).
			Set("state", n.State).Set("proved", n.Proved).Set("envelope_id", n.EnvelopeID).
			Set("deadline", n.Deadline).Set("tokens", axisDoc(n, t, "tokens")).Set("turns", axisDoc(n, t, "turns")).
			Set("refused", pyjson.Int{Text: fmt.Sprint(n.Refused)}).Set("children", anysOrEmpty(n.Children)))
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range t.Roots {
		walk(r)
	}
	asks := []any{}
	for _, a := range t.OpenAsks() {
		asks = append(asks, pyjson.NewObject().Set("ask_id", a.Value("ask_id")).Set("parent", a.Value("parent")).
			Set("session", a.Value("session")).Set("tokens", a.Value("tokens")).Set("turns", a.Value("turns")).
			Set("reasons", a.Value("reasons")))
	}
	return pyjson.NewObject().Set("nodes", nodes).Set("asks", asks).Set("skipped", pyjson.Int{Text: fmt.Sprint(t.Skipped)})
}

func anysOrEmpty(ss []string) []any {
	out := anys(ss)
	if out == nil {
		return []any{}
	}
	return out
}

func amount(a *pyjson.Object, state string) string {
	if a.Value("budget") == nil {
		return "-"
	}
	b := intText(a.Value("budget"))
	if state == "ended" {
		used := "?"
		if u := a.Value("used"); u != nil {
			used = intText(u)
		}
		return b + " (used " + used + ")"
	}
	return b + " (left " + intText(a.Value("left")) + ")"
}

// StatusText is tree.status_text.
func StatusText(doc *pyjson.Object) string {
	var lines []string
	depth := map[string]int{}
	for _, x := range listOf(doc.Value("nodes")) {
		n := obj(x)
		d := 0
		if p, ok := n.Value("parent").(string); ok {
			d = depth[p] + 1
		}
		s := strOf(n, "session")
		depth[s] = d
		state := strOf(n, "state")
		label := state
		if !truthy(n.Value("proved")) {
			label += ", operator allow, not proved"
		}
		line := fmt.Sprintf("%s%s (%s) tokens %s, turns %s", strings.Repeat("  ", d), s, label,
			amount(obj(n.Value("tokens")), state), amount(obj(n.Value("turns")), state))
		if dl := n.Value("deadline"); dl != nil {
			line += ", deadline " + Q(dl)
		}
		if r := intText(n.Value("refused")); r != "0" {
			line += ", " + r + " refused"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "The tree is empty.")
	}
	if asks := listOf(doc.Value("asks")); len(asks) > 0 {
		lines = append(lines, "Open asks:")
		for _, x := range asks {
			a := obj(x)
			first := strs(a.Value("reasons"))
			r := ""
			if len(first) > 0 {
				r = first[0]
			}
			lines = append(lines, fmt.Sprintf("  %s: %s asks to start %s: %s", strOf(a, "ask_id"), strOf(a, "parent"), strOf(a, "session"), r))
		}
		lines = append(lines, "Answer one with: daisugi tree answer ASK allow|deny")
	}
	if sk := intText(doc.Value("skipped")); sk != "0" {
		lines = append(lines, sk+" ledger rows did not read or fit; skipped.")
	}
	return strings.Join(lines, "\n") + "\n"
}
