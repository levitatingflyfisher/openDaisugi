package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/branchgit"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/rank"
	"daisugi-verify/internal/statefile"
	"daisugi-verify/internal/wardenroot"
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

// IsObjectID is tree.is_object_id: a full git object id, 40 or 64 lower-case
// hex digits, nothing else.
func IsObjectID(s string) bool { return branchgit.IsObjectID(s) }

// baseRepoOK is the spawn row's base and repo: both or neither, the base an
// object id, the repo a non-empty text.
func baseRepoOK(row *pyjson.Object) bool {
	b, r := row.Value("base"), row.Value("repo")
	if (b == nil) != (r == nil) {
		return false
	}
	if b == nil {
		return true
	}
	bs, ok1 := b.(string)
	rs, ok2 := r.(string)
	return ok1 && ok2 && IsObjectID(bs) && rs != ""
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
		extrasOK := (row.Value("profile") == nil || isStr(row, "profile")) &&
			(row.Value("model") == nil || isStr(row, "model")) &&
			(row.Value("branch_of") == nil || isStr(row, "branch_of")) &&
			(row.Value("why") == nil || isStr(row, "why")) &&
			baseRepoOK(row) &&
			count(row.Value("parent_tokens_used")) && count(row.Value("parent_turns_used"))
		return isStr(row, "parent") && isStr(row, "session") && isStr(row, "envelope_id") &&
			count(row.Value("tokens")) && count(row.Value("turns")) && deadlineOK && proved && extrasOK
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
	case "group":
		g, ok := row.Value("pgid").(pyjson.Int)
		if !ok {
			return false
		}
		n, ok := new(big.Int).SetString(g.Text, 10)
		return isStr(row, "session") && ok && n.Cmp(big.NewInt(1)) > 0 && n.Cmp(big.NewInt(maxPgid)) <= 0
	}
	return false
}

// maxPgid is tree.MAX_PGID: the largest process group id a group row may
// hold (pid_t is 32 bits).
const maxPgid = 1<<31 - 1

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
	// Profile and Model are the agent profile and model the starter ran
	// it on, as its spawn row holds them; nil for none.
	Profile, Model *string
	// BranchOf is the ranking a branch belongs to, and Why the reason the
	// branch was started.
	BranchOf, Why *string
	// Base is the commit a branch started from and Repo the repository's
	// git directory, as the parent recorded them; nil for none.
	Base, Repo *string
	// EndedAt is the ts of its end row; 0 while it runs.
	EndedAt float64
	// OwnTokensUsed and OwnTurnsUsed are its own use as the latest spawn
	// under it reported it.
	OwnTokensUsed, OwnTurnsUsed *big.Int
	// Pgid is the process group its starter put it in, as its group row
	// holds it; 0 for none.
	Pgid int
}

// Tree is tree.Tree.
type Tree struct {
	// Order holds the sessions in the order their rows were read.
	Order   []string
	Nodes   map[string]*Node
	Roots   []string
	Asks    map[string]*pyjson.Object
	AskList []string
	Answers map[string]string
	Skipped int
	// Proposals counts each parent's children, refusals and asks.
	Proposals map[string]int
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

func (n *Node) ownUsed(axis string) *big.Int {
	if axis == "tokens" {
		return n.OwnTokensUsed
	}
	return n.OwnTurnsUsed
}

func (n *Node) setOwnUsed(axis string, v *big.Int) {
	if axis == "tokens" {
		n.OwnTokensUsed = v
	} else {
		n.OwnTurnsUsed = v
	}
}

// Charge is Tree.charge: what a node costs its parent, its branch total
// once it ended with a report, else its whole reservation.
func (t *Tree) Charge(n *Node, axis string) *big.Int {
	if u := n.used(axis); n.State == "ended" && u != nil {
		return new(big.Int).Set(u)
	}
	// No report of its own: the larger of the reservation and the
	// children's charges, so an overrun below it still counts.
	below := new(big.Int)
	for _, c := range n.Children {
		below.Add(below, t.Charge(t.Nodes[c], axis))
	}
	if r := n.budget(axis); r != nil && r.Cmp(below) > 0 {
		below.Set(r)
	}
	if below.Cmp(maxCount) > 0 {
		below.Set(maxCount)
	}
	return below
}

// Left is Tree.left: what node may still reserve for children, below zero
// when a branch overran.
func (t *Tree) Left(n *Node, axis string) *big.Int {
	b := n.budget(axis)
	if b == nil {
		return nil
	}
	spent := new(big.Int)
	for _, c := range n.Children {
		spent.Add(spent, t.Charge(t.Nodes[c], axis))
	}
	if o := n.ownUsed(axis); o != nil {
		spent.Add(spent, o)
	}
	return new(big.Int).Sub(b, spent)
}

// BranchTotal is Tree.branch_total: own plus each child's charge, capped
// at 2**53; nil when the node has no report of its own.
func (t *Tree) BranchTotal(n *Node, axis string, own *big.Int) *big.Int {
	if own == nil {
		return nil
	}
	total := new(big.Int).Set(own)
	for _, c := range n.Children {
		total.Add(total, t.Charge(t.Nodes[c], axis))
	}
	if total.Cmp(maxCount) > 0 {
		total.Set(maxCount)
	}
	return total
}

// MintSession is tree.mint_session: n- and 12 hex of SHA-256 over the
// JSON text [parent, n].
func MintSession(parent string, n int) string {
	text := pyjson.CanonicalASCII([]any{parent, pyjson.Int{Text: fmt.Sprint(n)}})
	sum := sha256.Sum256([]byte(text))
	return "n-" + hex.EncodeToString(sum[:])[:12]
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
	t := &Tree{Nodes: map[string]*Node{}, Asks: map[string]*pyjson.Object{}, Answers: map[string]string{},
		Proposals: map[string]int{}}
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
			node := &Node{Session: sid, Parent: parent, EnvelopeID: row.Value("envelope_id").(string),
				Tokens: optBig(row.Value("tokens")), Turns: optBig(row.Value("turns")), Deadline: deadline,
				Proved: proved, State: "running"}
			if parent != nil {
				if s, ok := row.Value("profile").(string); ok {
					node.Profile = &s
				}
				if s, ok := row.Value("model").(string); ok {
					node.Model = &s
				}
				if s, ok := row.Value("branch_of").(string); ok {
					node.BranchOf = &s
				}
				if s, ok := row.Value("why").(string); ok {
					node.Why = &s
				}
				if s, ok := row.Value("base").(string); ok {
					node.Base = &s
				}
				if s, ok := row.Value("repo").(string); ok {
					node.Repo = &s
				}
			}
			t.Nodes[sid] = node
			t.Order = append(t.Order, sid)
			if parent == nil {
				t.Roots = append(t.Roots, sid)
			} else {
				p := t.Nodes[*parent]
				p.Children = append(p.Children, sid)
				p.Refused = 0
				t.Proposals[*parent]++
				for _, axis := range []string{"tokens", "turns"} {
					if u := row.Value("parent_" + axis + "_used"); u != nil {
						p.setOwnUsed(axis, optBig(u))
					}
				}
			}
		case "refused":
			p := t.Nodes[row.Value("parent").(string)]
			if p == nil {
				t.Skipped++
				continue
			}
			p.Refused++
			t.Proposals[row.Value("parent").(string)]++
		case "ask":
			id := row.Value("ask_id").(string)
			if t.Asks[id] != nil || t.Nodes[row.Value("parent").(string)] == nil {
				t.Skipped++
				continue
			}
			t.Asks[id] = row
			t.AskList = append(t.AskList, id)
			t.Proposals[row.Value("parent").(string)]++
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
			n.EndedAt, _ = Num(row.Value("ts"))
			n.TokensUsed = optBig(row.Value("tokens_used"))
			n.TurnsUsed = optBig(row.Value("turns_used"))
		case "group":
			n := t.Running(row.Value("session").(string))
			if n == nil {
				t.Skipped++
				continue
			}
			n.Pgid, _ = strconv.Atoi(row.Value("pgid").(pyjson.Int).Text)
		}
	}
	return t
}

// BranchGroups is Tree.branch_groups: the recorded process groups of the
// running nodes below session, deepest first.
func (t *Tree) BranchGroups(session string) []int {
	var out []int
	var walk func(string)
	walk = func(sid string) {
		for _, c := range t.Nodes[sid].Children {
			walk(c)
			if n := t.Nodes[c]; n.State == "running" && n.Pgid != 0 {
				out = append(out, n.Pgid)
			}
		}
	}
	if t.Nodes[session] != nil {
		walk(session)
	}
	return out
}

// RecordGroup is tree.record_group: the process group a starter put
// session in, so a kill of an ancestor's branch reaches it.
func RecordGroup(dataDir, session string, pgid int) error {
	return locked(dataDir, func() error {
		row := pyjson.NewObject().Set("event", "group").Set("session", session).
			Set("pgid", pyjson.Int{Text: strconv.Itoa(pgid)}).Set("ts", Now())
		return appendRows(dataDir, row)
	})
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

// hasControl is tree._check_model's test: a C0 control, DEL or a C1
// control, by code point.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 32 || (r >= 127 && r <= 159) {
			return true
		}
	}
	return false
}

func envelopeFile(wardenRoot, session string) string {
	return wardenroot.Join(wardenroot.EnvelopesDir(wardenRoot), session+".json")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// LoadRegisteredChecked is tree.load_registered_checked: the envelope
// registered for a session, read as the warden reads it (GR-R-1). env is nil
// when there is none or it does not read as one; refusal is not empty when
// the root, envelopes/ or the file is a link, is owned by another user, has
// a second link or is writable by group or others. A refusal means "cannot
// be read" (GR-R-7).
func LoadRegisteredChecked(session, wardenRoot string) (env *pyjson.Object, refusal string) {
	name := "default"
	if session != "" {
		name = wardenroot.SafeSessionID(session)
	}
	raw, present, refusal, err := statefile.ProbeRead(wardenRoot, []string{"envelopes"}, name+".json")
	if refusal != "" {
		return nil, refusal
	}
	if err != nil || !present {
		return nil, ""
	}
	v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, string(raw))
	if verr != nil {
		return nil, ""
	}
	o, _ := v.(*pyjson.Object)
	return o, ""
}

// LoadRegistered is tree.load_registered: the envelope registered for a
// session, or nil when there is none, it does not read as one or the warden
// would refuse to read it.
func LoadRegistered(session, wardenRoot string) *pyjson.Object {
	env, _ := LoadRegisteredChecked(session, wardenRoot)
	return env
}

func bigAny(b *big.Int) any {
	if b == nil {
		return nil
	}
	return pyjson.Int{Text: b.String()}
}

// MakeRoot is tree.make_root.
func MakeRoot(dataDir, wardenRoot string, env *pyjson.Object, session string, tokens, turns *big.Int) (path string, err error) {
	what := "Tried to make session " + session + " a root of the tree."
	if err := checkSession(session, what); err != nil {
		return "", err
	}
	if err := checkCounts([]string{"tokens", "turns"}, []*big.Int{tokens, turns}); err != nil {
		return "", err
	}
	err = locked(dataDir, func() error {
		t := ReadTree(dataDir)
		if t.Nodes[session] != nil || fileExists(envelopeFile(wardenRoot, session)) {
			return &Error{what, "Session " + session + " is in use already.", "Pick another session id.", 1}
		}
		row := pyjson.NewObject().Set("event", "root").Set("session", session).Set("envelope_id", env.Value("id")).
			Set("tokens", bigAny(tokens)).Set("turns", bigAny(turns)).Set("deadline", env.Value("deadline")).Set("ts", Now())
		if err := appendRows(dataDir, row); err != nil {
			return err
		}
		path, err = wardenroot.Register(env, session, wardenRoot)
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
	Session   string // the child's session, minted when none was given
}

// SpawnOpts are tree.spawn's keywords past the budget: the profile and
// model of the child (nil for none) and the parent's own use so far.
type SpawnOpts struct {
	Profile, Model                    *string
	BranchOf, Why                     *string
	Base, Repo                        *string
	ParentTokensUsed, ParentTurnsUsed *big.Int
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

// DoSpawn is tree.spawn with no profile, model or parent use.
func DoSpawn(dataDir, wardenRoot string, env *pyjson.Object, parent, session string, tokens, turns *big.Int, timeoutMs int) (*Spawn, error) {
	return DoSpawnOpts(dataDir, wardenRoot, env, parent, &session, tokens, turns, timeoutMs, SpawnOpts{})
}

// checkSpawnArgs is tree._check_spawn_args.
func checkSpawnArgs(parent string, sessionP *string, tokens, turns *big.Int, o SpawnOpts) error {
	session := "None"
	if sessionP != nil {
		session = *sessionP
	}
	what := "Tried to start session " + session + " under " + parent + "."
	if sessionP != nil {
		if err := checkSession(session, what); err != nil {
			return err
		}
	}
	if err := checkSession(parent, what); err != nil {
		return err
	}
	if err := checkCounts([]string{"tokens", "turns", "parent-tokens-used", "parent-turns-used"},
		[]*big.Int{tokens, turns, o.ParentTokensUsed, o.ParentTurnsUsed}); err != nil {
		return err
	}
	if o.Model != nil && hasControl(*o.Model) {
		return &Error{"Tried to read the model id.", "The model id holds a control character.",
			"Name the model with no control character.", 2}
	}
	for _, nv := range []struct {
		name string
		v    *string
	}{{"branch_of", o.BranchOf}, {"why", o.Why}} {
		if nv.v != nil {
			if n := utf8.RuneCountInString(*nv.v); n == 0 || n > 200 {
				return &Error{what, nv.name + " must be 1 to 200 characters of text.", "Fix it.", 1}
			}
		}
	}
	return nil
}

// DoSpawnOpts is tree.spawn. A nil session is minted (MintSession).
func DoSpawnOpts(dataDir, wardenRoot string, env *pyjson.Object, parent string, sessionP *string,
	tokens, turns *big.Int, timeoutMs int, o SpawnOpts) (*Spawn, error) {
	if err := checkSpawnArgs(parent, sessionP, tokens, turns, o); err != nil {
		return nil, err
	}
	var out *Spawn
	err := locked(dataDir, func() error {
		var e error
		out, e = spawnLocked(dataDir, wardenRoot, env, parent, sessionP, tokens, turns, timeoutMs, o)
		return e
	})
	return out, err
}

// spawnLocked is tree._spawn_locked: spawn with the ledger lock held.
func spawnLocked(dataDir, wardenRoot string, env *pyjson.Object, parent string, sessionP *string,
	tokens, turns *big.Int, timeoutMs int, o SpawnOpts) (*Spawn, error) {
	session := "None"
	if sessionP != nil {
		session = *sessionP
	}
	what := "Tried to start session " + session + " under " + parent + "."
	var out *Spawn
	{
		t := ReadTree(dataDir)
		if sessionP == nil {
			n := t.Proposals[parent]
			session = MintSession(parent, n)
			for t.Nodes[session] != nil || fileExists(envelopeFile(wardenRoot, session)) {
				n++
				session = MintSession(parent, n)
			}
			what = "Tried to start session " + session + " under " + parent + "."
		}
		pnode := t.Running(parent)
		if pnode == nil {
			return nil, &Error{what, parent + " is not a running node of the tree.",
				"Start the child under a running node; daisugi tree status lists them.", 1}
		}
		if t.Nodes[session] != nil || fileExists(envelopeFile(wardenRoot, session)) {
			return nil, &Error{what, "Session " + session + " is in use already.", "Pick another session id.", 1}
		}
		for _, a := range t.OpenAsks() {
			if a.Value("session") == session {
				return nil, &Error{what, "An ask for session " + session + " is open.",
					"Wait for the operator's answer, or pick another session id.", 1}
			}
		}
		if o.ParentTokensUsed != nil {
			pnode.OwnTokensUsed = o.ParentTokensUsed
		}
		if o.ParentTurnsUsed != nil {
			pnode.OwnTurnsUsed = o.ParentTurnsUsed
		}
		penv := LoadRegistered(parent, wardenRoot)
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
					return nil, err
				}
				out = &Spawn{Status: "asked", Reasons: reasons, AskID: id, Session: session}
				return out, nil
			}
			row := pyjson.NewObject().Set("event", "refused").Set("parent", parent).Set("session", session).
				Set("reasons", anys(reasons)).Set("ts", ts)
			if err := appendRows(dataDir, row); err != nil {
				return nil, err
			}
			out = &Spawn{Status: "refused", Reasons: reasons, Session: session}
			return out, nil
		}
		child := withKey(res.Child, "parent_envelope", penv.Value("id"))
		row := pyjson.NewObject().Set("event", "spawn").Set("parent", parent).Set("session", session).
			Set("envelope_id", child.Value("id")).Set("tokens", bigAny(tokens)).Set("turns", bigAny(turns)).
			Set("deadline", child.Value("deadline")).Set("proved", true)
		if o.Profile != nil {
			row.Set("profile", *o.Profile)
		}
		if o.Model != nil {
			row.Set("model", *o.Model)
		}
		if o.ParentTokensUsed != nil {
			row.Set("parent_tokens_used", bigAny(o.ParentTokensUsed))
		}
		if o.ParentTurnsUsed != nil {
			row.Set("parent_turns_used", bigAny(o.ParentTurnsUsed))
		}
		if o.BranchOf != nil {
			row.Set("branch_of", *o.BranchOf)
		}
		if o.Why != nil {
			row.Set("why", *o.Why)
		}
		if o.Base != nil {
			row.Set("base", *o.Base)
		}
		if o.Repo != nil {
			row.Set("repo", *o.Repo)
		}
		row.Set("ts", ts)
		if err := appendRows(dataDir, row); err != nil {
			return nil, err
		}
		path, err := wardenroot.Register(child, session, wardenRoot)
		if err != nil {
			return nil, err
		}
		out = &Spawn{Status: "started", Path: path, Session: session}
		if res.Inherited {
			out.Inherited = child.Value("deadline")
		}
		return out, nil
	}
}

// Branches is tree.Branches: the result of reserving all the branches of one
// task, or none.
type Branches struct {
	Status   string // started, refused or asked
	Reasons  []string
	Sessions []string
	AskID    string
}

// SpawnBranches is tree.spawn_branches: every branch of one task reserved
// under one lock, in branch order, or none (W3K-9, W3K-13).
func SpawnBranches(dataDir, wardenRoot string, env *pyjson.Object, parent string, sessions []string,
	tokens, turns *big.Int, timeoutMs int, o SpawnOpts) (*Branches, error) {
	n := len(sessions)
	if err := checkSpawnArgs(parent, nil, tokens, turns, o); err != nil {
		return nil, err
	}
	if (o.Base == nil) != (o.Repo == nil) || (o.Base != nil && !IsObjectID(*o.Base)) {
		return nil, &Error{"Tried to start branches under " + parent + ".",
			"base must be a 40 or 64 digit hex object id, and repo must come with it.", "Fix it.", 1}
	}
	for _, sid := range sessions {
		if err := checkSession(sid, "Tried to start session "+sid+" under "+parent+"."); err != nil {
			return nil, err
		}
	}
	var out *Branches
	err := locked(dataDir, func() error {
		t := ReadTree(dataDir)
		if pnode := t.Running(parent); pnode != nil {
			if o.ParentTokensUsed != nil {
				pnode.OwnTokensUsed = o.ParentTokensUsed
			}
			if o.ParentTurnsUsed != nil {
				pnode.OwnTurnsUsed = o.ParentTurnsUsed
			}
			for _, ax := range []struct {
				name string
				want *big.Int
			}{{"tokens", tokens}, {"turns", turns}} {
				left := t.Left(pnode, ax.name)
				if left != nil && ax.want != nil && new(big.Int).Mul(ax.want, big.NewInt(int64(n))).Cmp(left) > 0 {
					out = &Branches{Status: "refused", Reasons: []string{fmt.Sprintf(
						"the parent has %s %s left; %d branches of %s each need %s", left, ax.name, n, ax.want,
						new(big.Int).Mul(ax.want, big.NewInt(int64(n))))}}
					return nil
				}
			}
		}
		var done []string
		undo := func() {
			// End what started, with no report: each keeps its whole reservation.
			for _, sid := range done {
				_, _ = endLocked(dataDir, wardenRoot, sid, nil, nil)
			}
		}
		for _, sid := range sessions {
			sid := sid
			sp, e := spawnLocked(dataDir, wardenRoot, env, parent, &sid, tokens, turns, timeoutMs, o)
			if e != nil {
				var te *Error
				if errors.As(e, &te) {
					undo()
					out = &Branches{Status: "refused", Reasons: []string{te.Why}}
					return nil
				}
				return e
			}
			if sp.Status != "started" {
				// The same envelope and budget were proved for the first
				// branch, so a later refusal is a race.
				if len(done) == 0 {
					out = &Branches{Status: sp.Status, Reasons: sp.Reasons, AskID: sp.AskID}
					return nil
				}
				undo()
				out = &Branches{Status: "refused", Reasons: sp.Reasons}
				return nil
			}
			done = append(done, sid)
		}
		out = &Branches{Status: "started", Sessions: done}
		return nil
	})
	return out, err
}

// End is tree.end.
func End(dataDir, wardenRoot, session string, tokensUsed, turnsUsed *big.Int) (*Node, error) {
	what := "Tried to end session " + session + "."
	if err := checkSession(session, what); err != nil {
		return nil, err
	}
	if err := checkCounts([]string{"tokens-used", "turns-used"}, []*big.Int{tokensUsed, turnsUsed}); err != nil {
		return nil, err
	}
	var node *Node
	err := locked(dataDir, func() error {
		var e error
		node, e = endLocked(dataDir, wardenRoot, session, tokensUsed, turnsUsed)
		return e
	})
	return node, err
}

// endLocked is tree._end_locked: end with the ledger lock held.
func endLocked(dataDir, wardenRoot, session string, tokensUsed, turnsUsed *big.Int) (*Node, error) {
	what := "Tried to end session " + session + "."
	{
		t := ReadTree(dataDir)
		node := t.Running(session)
		if node == nil {
			return nil, &Error{what, session + " is not a running node of the tree.",
				"daisugi tree status lists the running nodes.", 1}
		}
		var busy []string
		for _, c := range node.Children {
			if t.Nodes[c].State == "running" {
				busy = append(busy, c)
			}
		}
		if len(busy) > 0 {
			return nil, &Error{what, "It has running children: " + strings.Join(busy, ", ") + ".", "End them first.", 1}
		}
		tokensTotal := t.BranchTotal(node, "tokens", tokensUsed)
		turnsTotal := t.BranchTotal(node, "turns", turnsUsed)
		row := pyjson.NewObject().Set("event", "end").Set("session", session).
			Set("tokens_used", bigAny(tokensTotal)).Set("turns_used", bigAny(turnsTotal)).Set("ts", Now())
		if err := appendRows(dataDir, row); err != nil {
			return nil, err
		}
		// Removed through the directory fd (GR-R-9). A refused envelopes/ is
		// one the warden refuses to read as well, so nothing there stays honored.
		var wr *statefile.WriteRefused
		why := ""
		removed, err := statefile.UnlinkFile(wardenRoot, []string{"envelopes"}, session+".json")
		switch {
		case err == nil && !removed:
			why = "it was not found" // N3: envelopes/ moved aside, or never there
		case err != nil && errors.As(err, &wr):
			why = wr.Text
		case err != nil:
			return nil, err
		}
		if why != "" {
			// The end row is written, so the exit code stays; the file left
			// behind is said (N1, N3).
			left := wardenRoot
			if !strings.HasSuffix(left, "/") {
				left += "/"
			}
			fmt.Fprintf(os.Stderr, "daisugi tree: the envelope of %s was not removed: %s; remove %senvelopes/%s.json after the fix\n",
				session, why, left, session)
		}
		node.State = "ended"
		node.TokensUsed, node.TurnsUsed = tokensTotal, turnsTotal
		return node, nil
	}
}

// EndSubtree is tree.end_subtree: end every running node under session
// (and session, with includeSelf), leaves first, each with no report, so
// each keeps its whole reservation. It returns the sessions it ended.
func EndSubtree(dataDir, wardenRoot, session string, includeSelf bool) []string {
	t := ReadTree(dataDir)
	var order []string
	var walk func(string)
	walk = func(sid string) {
		for _, c := range t.Nodes[sid].Children {
			walk(c)
		}
		if t.Nodes[sid].State == "running" && (includeSelf || sid != session) {
			order = append(order, sid)
		}
	}
	if t.Nodes[session] != nil {
		walk(session)
	}
	for _, sid := range order {
		_, _ = End(dataDir, wardenRoot, sid, nil, nil)
	}
	return order
}

// Answer is tree.answer: the ask, and the registered path on an allow.
func Answer(dataDir, wardenRoot, id, verdict string) (*pyjson.Object, string, error) {
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
		if t.Nodes[session] != nil || fileExists(envelopeFile(wardenRoot, session)) {
			return &Error{what, "Session " + session + " is in use already.", "Deny the ask.", 1}
		}
		if over := budgetReasons(t, pnode, optBig(ask.Value("tokens")), optBig(ask.Value("turns"))); len(over) > 0 {
			return &Error{what, "The budget does not fit: " + strings.Join(over, "; ") + ".", "Deny the ask.", 1}
		}
		v, verr := pmodel.ValidateJSON("Envelope", pmodel.Envelope, pyjson.Dumps(ask.Value("envelope"), false))
		if verr != nil {
			return verr
		}
		// One read of the parent serves the check and the edge below. A parent
		// the warden would refuse to read cannot be shown not to be physical,
		// so an allow is refused (GR-R-7). AT-5 holds under an allow too: a
		// physical parent starts no agent.
		ppar, refusal := LoadRegisteredChecked(parent, wardenRoot)
		if refusal != "" {
			return &Error{what, "The envelope of " + parent + " cannot be read: the warden refuses it.",
				"Deny the ask, then fix the file's owner, links or mode.", 1}
		}
		if ppar != nil && ppar.Value("stakes") == "physical" {
			return &Error{what, "stakes: the parent's stakes are physical, so it cannot start an agent.", "Deny the ask.", 1}
		}
		// An allow never widens the tree past its root: the child must fit
		// inside the root of its branch.
		root := parent
		for t.Nodes[root].Parent != nil {
			root = *t.Nodes[root].Parent
		}
		// A running non-root node always has a registered envelope, so its
		// absence means the file was removed or spoiled: AT-5 cannot be
		// shown to hold under an allow (N2, GR-R-17).
		if ppar == nil && parent != root {
			return &Error{what, "The envelope of " + parent + " is missing or not valid: the warden cannot read it.",
				"Deny the ask, then register the parent's envelope again.", 1}
		}
		edge := EdgeOK(LoadRegistered(root, wardenRoot), v.(*pyjson.Object), DefaultTimeoutMs)
		if !edge.Holds || edge.Child == nil {
			return &Error{what, "The child does not fit inside the root " + root + ": " + strings.Join(edge.Reasons, "; ") + ".",
				"Deny the ask, or register the child as a root of its own.", 1}
		}
		child := edge.Child
		if ppar != nil {
			child = withKey(child, "parent_envelope", ppar.Value("id"))
		}
		spawn := pyjson.NewObject().Set("event", "spawn").Set("parent", parent).Set("session", session).
			Set("envelope_id", child.Value("id")).Set("tokens", ask.Value("tokens")).Set("turns", ask.Value("turns")).
			Set("deadline", child.Value("deadline")).Set("proved", false).Set("ts", ts)
		if err := appendRows(dataDir, row, spawn); err != nil {
			return err
		}
		var err error
		path, err = wardenroot.Register(child, session, wardenRoot)
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
		doc := pyjson.NewObject().Set("session", n.Session).Set("parent", parent).
			Set("state", n.State).Set("proved", n.Proved).Set("envelope_id", n.EnvelopeID).
			Set("deadline", n.Deadline).Set("tokens", axisDoc(n, t, "tokens")).Set("turns", axisDoc(n, t, "turns")).
			Set("refused", pyjson.Int{Text: fmt.Sprint(n.Refused)}).Set("children", anysOrEmpty(n.Children))
		if n.Profile != nil {
			doc.Set("profile", *n.Profile)
		}
		if n.Model != nil {
			doc.Set("model", *n.Model)
		}
		if n.BranchOf != nil {
			doc.Set("branch_of", *n.BranchOf)
		}
		if n.Why != nil {
			doc.Set("why", *n.Why)
		}
		nodes = append(nodes, doc)
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
	doc := pyjson.NewObject().Set("nodes", nodes).Set("asks", asks).Set("skipped", pyjson.Int{Text: fmt.Sprint(t.Skipped)})
	if groups := branchGroups(t, dataDir); len(groups) > 0 {
		doc.Set("branches", groups)
	}
	return doc
}

// branchGroups is tree._branch_groups: the branches of each ranking, in the
// order their first branch was reserved, with the leader and the eliminated
// count when the ranking opened a card.
func branchGroups(t *Tree, dataDir string) []any {
	var order []string
	members := map[string][]*Node{}
	for _, sid := range t.Order {
		n := t.Nodes[sid]
		if n.BranchOf != nil {
			if _, ok := members[*n.BranchOf]; !ok {
				order = append(order, *n.BranchOf)
			}
			members[*n.BranchOf] = append(members[*n.BranchOf], n)
		}
	}
	if len(order) == 0 {
		return nil
	}
	cards := map[string]*rank.Card{}
	for _, c := range rank.ReadCards(dataDir) {
		if rid, ok := c.Opened.Value("ranking_id").(string); ok {
			cards[rid] = c
		}
	}
	out := []any{}
	for _, rid := range order {
		nodes := members[rid]
		sessions := []string{}
		ended := 0
		sum := new(big.Int)
		for _, n := range nodes {
			sessions = append(sessions, n.Session)
			if n.State == "ended" {
				ended++
			}
			sum.Add(sum, t.Charge(n, "tokens"))
		}
		if sum.Cmp(maxCount) > 0 {
			sum = new(big.Int).Set(maxCount)
		}
		why := ""
		if nodes[0].Why != nil {
			why = *nodes[0].Why
		}
		g := pyjson.NewObject().Set("ranking_id", rid).Set("why", why).Set("sessions", anys(sessions)).
			Set("ended", pyjson.Int{Text: fmt.Sprint(ended)}).Set("tokens", bigAny(sum))
		if card := cards[rid]; card != nil {
			opts, _ := card.Opened.Value("options").(*pyjson.Object)
			surv, _ := opts.Value("survivors").([]any)
			elim, _ := opts.Value("eliminated").([]any)
			g.Set("leader", card.Opened.Value("chosen")).Set("ran", pyjson.Int{Text: fmt.Sprint(len(surv) + len(elim))}).
				Set("eliminated", pyjson.Int{Text: fmt.Sprint(len(elim))})
		}
		out = append(out, g)
	}
	return out
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
		used, over := "?", ""
		if u := a.Value("used"); u != nil {
			used = intText(u)
			if d := new(big.Int).Sub(bigOf(u), bigOf(a.Value("budget"))); d.Sign() > 0 {
				over = ", over by " + d.String()
			}
		}
		return b + " (used " + used + over + ")"
	}
	if l := bigOf(a.Value("left")); l.Sign() < 0 {
		return b + " (over by " + new(big.Int).Neg(l).String() + ")"
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
		if p, ok := n.Value("profile").(string); ok {
			line += ", profile " + p
		}
		if m, ok := n.Value("model").(string); ok {
			line += ", model " + m
		}
		if r := intText(n.Value("refused")); r != "0" {
			line += ", " + r + " refused"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "The tree is empty.")
	}
	for _, x := range listOf(doc.Value("branches")) {
		g := obj(x)
		sessions := listOf(g.Value("sessions"))
		body := fmt.Sprintf("%d started, %s ended", len(sessions), intText(g.Value("ended")))
		if g.Value("leader") != nil {
			body = fmt.Sprintf("%d started, %s ended, %s eliminated, leader %s", len(sessions), intText(g.Value("ended")),
				intText(g.Value("eliminated")), anyText(g.Value("leader")))
		}
		lines = append(lines, fmt.Sprintf("  branches of %s (%s): %s, %s tokens", strOf(g, "ranking_id"), strOf(g, "why"),
			body, intText(g.Value("tokens"))))
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

func anyText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// EnvelopeExists is tree._envelope_file(...).exists().
func EnvelopeExists(wardenRoot, session string) bool {
	return fileExists(envelopeFile(wardenRoot, session))
}
