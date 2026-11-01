package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/envdefault"
	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
)

// fakeCheck makes COPPICE_DAISUGI_BIN a script: `tree check F F` passes
// unless F holds the word BAD.
func fakeCheck(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "daisugi")
	body := "#!/bin/sh\nif [ \"$1 $2\" = \"tree check\" ] && grep -q BAD \"$3\"; then echo 'permissions: not valid'; exit 2; fi\nexit 0\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COPPICE_DAISUGI_BIN", p)
}

// proposalServer is a test server with a scratch config dir that pins one
// project, trellis.
func proposalServer(t *testing.T) *Server {
	t.Helper()
	fakeCheck(t)
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	project := filepath.Join(t.TempDir(), "trellis")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(config.Config{Projects: []string{project}}); err != nil {
		t.Fatal(err)
	}
	return newTestServer(t)
}

const envA = `{"id":"env_a","generated_by":"steward","task":"build trellis","permissions":{"file_read":["/src/trellis"],"file_write":["/src/trellis/lib"],"shell":true,"shell_allowlist":["go test"],"network":true,"network_hosts":["pub.dev"]}}`

func propose(t *testing.T, s *Server, env string, extra string) map[string]any {
	t.Helper()
	line := `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":` + env + `,"tokens":50000,"turns":30` + extra + `}`
	return result(t, roundTrip(t, s, line)[0])
}

// approve reads the pending card and approves the stamp it shows, as the
// phone does.
func approve(t *testing.T, s *Server, project string) proto.Response {
	t.Helper()
	cards := result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	for _, c := range cards {
		m := c.(map[string]any)
		if m["project"] == project {
			return roundTrip(t, s, `{"id":"a","cmd":"envelope.approve","project":"`+project+`","at":"`+m["at"].(string)+`"}`)[0]
		}
	}
	return roundTrip(t, s, `{"id":"a","cmd":"envelope.approve","project":"`+project+`","at":"none"}`)[0]
}

func envFile() string { return filepath.Join(envelopeDir(), "trellis.json") }

func TestAProposalNeverWritesTheEnvelopeFile(t *testing.T) {
	s := proposalServer(t)
	res := propose(t, s, envA, "")
	if res["pending"] != true {
		t.Fatalf("result = %v", res)
	}
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("a proposal wrote the operator's envelope file")
	}
	if _, err := os.Stat(envelopeDir()); err == nil {
		ents, _ := os.ReadDir(envelopeDir())
		if len(ents) > 0 {
			t.Fatalf("a proposal put %d files in the envelope dir", len(ents))
		}
	}
	cards := result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	if len(cards) != 1 {
		t.Fatalf("cards = %v", cards)
	}
	c := cards[0].(map[string]any)
	if c["project"] != "trellis" || c["tokens"].(float64) != 50000 || c["turns"].(float64) != 30 {
		t.Fatalf("card = %v", c)
	}
	if got := c["write"].([]any); len(got) != 1 || got[0] != "/src/trellis/lib" {
		t.Fatalf("write = %v", got)
	}
	reach := c["reach"].(map[string]any)
	if reach["network"] != true || reach["hosts"].([]any)[0] != "pub.dev" {
		t.Fatalf("reach = %v", reach)
	}
}

func TestApproveWritesExactlyTheProposedEnvelope(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	res := result(t, approve(t, s, "trellis"))
	if res["approved"] != "trellis" {
		t.Fatalf("result = %v", res)
	}
	b, err := os.ReadFile(envFile())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Envelope json.RawMessage `json:"envelope"`
		Tokens   int             `json:"tokens"`
		Turns    int             `json:"turns"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want, _ := envdefault.WithDeny([]byte(envA), nil)
	if string(got.Envelope) != string(want) || got.Tokens != 50000 || got.Turns != 30 {
		t.Fatalf("file = %s", b)
	}
	if !strings.Contains(string(b), `"granted_by": "owner"`) {
		t.Fatalf("the file does not say the owner granted it:\n%s", b)
	}
	if fi, _ := os.Stat(envFile()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if _, err := readProjectEnvelope(envFile(), "trellis"); err != nil {
		t.Fatalf("spawn cannot read what approve wrote: %v", err)
	}
	cards := result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	if len(cards) != 0 {
		t.Fatalf("an approved proposal stays pending: %v", cards)
	}
}

func TestDeclineDropsTheProposalAndWritesNothing(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	result(t, roundTrip(t, s, `{"id":"d","cmd":"envelope.decline","project":"trellis"}`)[0])
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("decline wrote the file")
	}
	if r := approve(t, s, "trellis"); r.OK {
		t.Fatal("a declined proposal was approved")
	}
}

func TestASecondProposalReplacesThePendingOne(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	envB := strings.Replace(envA, "env_a", "env_b", 1)
	propose(t, s, envB, "")
	cards := result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	if len(cards) != 1 {
		t.Fatalf("cards = %v", cards)
	}
	result(t, approve(t, s, "trellis"))
	b, _ := os.ReadFile(envFile())
	if !strings.Contains(string(b), "env_b") || strings.Contains(string(b), "env_a") {
		t.Fatalf("approve wrote the old proposal: %s", b)
	}
}

func TestAProposalThatFailsTheCheckOrCarriesChecksIsRefused(t *testing.T) {
	s := proposalServer(t)
	bad := strings.Replace(envA, "build trellis", "BAD", 1)
	r := roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+bad+`}`)[0]
	if r.OK || !strings.Contains(r.Error.Message, "check") {
		t.Fatalf("a failing envelope was kept: %+v", r)
	}
	r = roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+envA+`,"checks":{"t":"rm -rf /"}}`)[0]
	if r.OK {
		t.Fatal("a proposal with checks was kept")
	}
	r = roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"nowhere","envelope":`+envA+`}`)[0]
	if r.OK {
		t.Fatal("a proposal for an unknown project was kept")
	}
	cards := result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	if len(cards) != 0 {
		t.Fatalf("refused proposals are pending: %v", cards)
	}
}

func TestAPaneProposesButNeverApproves(t *testing.T) {
	s := proposalServer(t)
	rs := roundTrip(t, s,
		`{"id":"0","cmd":"hello","role":"pane","pane":"w1:p1"}`,
		`{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+envA+`}`,
		`{"id":"a","cmd":"envelope.approve","project":"trellis"}`,
		`{"id":"d","cmd":"envelope.decline","project":"trellis"}`,
		`{"id":"l","cmd":"envelope.pending"}`,
	)
	if !rs[1].OK {
		t.Fatalf("a pane could not propose: %+v", rs[1].Error)
	}
	for _, i := range []int{2, 3, 4} {
		if rs[i].OK || rs[i].Error.Message != ProposalRefusal {
			t.Fatalf("response %d = %+v, want the refusal", i, rs[i])
		}
	}
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("a pane wrote the envelope file")
	}
}

func TestAProposalSurvivesARestart(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	s2, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), DataDir: s.cfg.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	cards := result(t, roundTrip(t, s2, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
	if len(cards) != 1 {
		t.Fatalf("cards = %v", cards)
	}
}

func cardsOf(t *testing.T, s *Server) []any {
	t.Helper()
	return result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any)
}

func TestApproveWithoutAtIsRefused(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	for _, line := range []string{
		`{"id":"a","cmd":"envelope.approve","project":"trellis"}`,
		`{"id":"a","cmd":"envelope.approve","project":"trellis","at":""}`,
	} {
		if r := roundTrip(t, s, line)[0]; r.OK {
			t.Fatalf("approve without at succeeded: %s", line)
		}
	}
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("approve without at wrote the file")
	}
	if len(cardsOf(t, s)) != 1 {
		t.Fatal("the refused approve dropped the proposal")
	}
}

func TestAReplacementInTheSameSecondStillChangesTheStamp(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, "")
	at1 := cardsOf(t, s)[0].(map[string]any)["at"].(string)
	propose(t, s, strings.Replace(envA, "env_a", "env_b", 1), "")
	at2 := cardsOf(t, s)[0].(map[string]any)["at"].(string)
	if at1 == at2 {
		t.Fatalf("a replacement kept the stamp %s", at1)
	}
	r := roundTrip(t, s, `{"id":"a","cmd":"envelope.approve","project":"trellis","at":"`+at1+`"}`)[0]
	if r.OK {
		t.Fatal("a stale card approved the replacement")
	}
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("a stale approve wrote the file")
	}
}

// handPending writes a pending file the way a hand edit would: the server
// never wrote it.
func handPending(t *testing.T, s *Server, body string) {
	t.Helper()
	if err := os.MkdirAll(s.proposalDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.proposalPath("trellis"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestApproveChecksTheStoredEnvelopeAgain(t *testing.T) {
	s := proposalServer(t)
	cases := map[string]string{
		"a failing envelope":         `{"project":"trellis","dir":"/x","envelope":{"task":"BAD"},"tokens":1,"turns":1,"at":"t1"}`,
		"checks beside the envelope": `{"project":"trellis","dir":"/x","envelope":` + envA + `,"checks":{"t":"rm -rf /"},"tokens":1,"turns":1,"at":"t1"}`,
		"an envelope that is a list": `{"project":"trellis","dir":"/x","envelope":["x"],"tokens":1,"turns":1,"at":"t1"}`,
		"an envelope that is text":   `{"project":"trellis","dir":"/x","envelope":"x","tokens":1,"turns":1,"at":"t1"}`,
	}
	for name, body := range cases {
		handPending(t, s, body)
		r := roundTrip(t, s, `{"id":"a","cmd":"envelope.approve","project":"trellis","at":"t1"}`)[0]
		if r.OK {
			t.Fatalf("%s was approved", name)
		}
		if _, err := os.Stat(envFile()); err == nil {
			t.Fatalf("%s: approve wrote the file", name)
		}
	}
	// A hand-written failing envelope in a well-formed file reaches the
	// re-check, which refuses it by name.
	handPending(t, s, cases["a failing envelope"])
	r := roundTrip(t, s, `{"id":"a","cmd":"envelope.approve","project":"trellis","at":"t1"}`)[0]
	if r.OK || !strings.Contains(r.Error.Message, "check") {
		t.Fatalf("response = %+v", r)
	}
}

func TestTheCardShowsEveryFieldThatWidens(t *testing.T) {
	s := proposalServer(t)
	env := `{"id":"e","generated_by":"steward","task":"t","shell_interpreter_policy":"allow","permissions":{"shell":true,"shell_allowlist":["go"],` +
		`"shell_allow_decomposition":true,"custom_step_allowlist":["approach_dish"],"mcp_rules":[{"tool":"github/*"}],` +
		`"max_execution_time_s":900,"max_output_size_mb":50}}`
	propose(t, s, env, "")
	also := cardsOf(t, s)[0].(map[string]any)["also"].([]any)
	var all []string
	for _, a := range also {
		all = append(all, a.(string))
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{"compound shell", "approach_dish", "github/*", "900 s per step", "50 MB", "interpreters: allow"} {
		if !strings.Contains(text, want) {
			t.Errorf("the card omits %q:\n%s", want, text)
		}
	}
	propose(t, s, envA, "")
	also = cardsOf(t, s)[0].(map[string]any)["also"].([]any)
	if len(also) != 1 || !strings.Contains(also[0].(string), "30 s per step") {
		t.Fatalf("default card also = %v", also)
	}
}

// pendingCards is the envelope.pending list.
func pendingCards(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range result(t, roundTrip(t, s, `{"id":"l","cmd":"envelope.pending"}`)[0])["proposals"].([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

// trellisDir is the pinned project's dir in a proposalServer.
func trellisDir(t *testing.T) string {
	t.Helper()
	cfg, _, err := config.Load()
	if err != nil || len(cfg.Projects) != 1 {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	return cfg.Projects[0]
}

// A proposal that asks for nothing past the default of a trusted project
// is written at once, marked as the default grant, and makes no card.
func TestAProposalInsideTheDefaultIsWrittenWithNoCard(t *testing.T) {
	s := proposalServer(t)
	armWarden(t, s)
	dir := trellisDir(t)
	env := `{"generated_by":"steward","task":"fix","permissions":{"file_read":["` + dir + `/**"],"file_write":["` + dir + `/lib"],"shell":true,"shell_allowlist":["git","flutter"]}}`
	line := `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":` + env + `}`
	res := result(t, roundTrip(t, s, line)[0])
	if res["approved"] != "trellis" || res["granted_by"] != "default" || res["pending"] != false {
		t.Fatalf("result = %v", res)
	}
	if cards := pendingCards(t, s); len(cards) != 0 {
		t.Fatalf("a proposal inside the default made a card: %v", cards)
	}
	b, err := os.ReadFile(envFile())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"granted_by": "default"`, `"deny_git_push"`, `"dir": "` + dir + `"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the file lacks %s:\n%s", want, b)
		}
	}
}

// A proposal that asks for more makes a card, and the card says each way it
// is wider and what stays denied.
func TestAWiderProposalCardsAndSaysWhatIsWider(t *testing.T) {
	s := proposalServer(t)
	propose(t, s, envA, `,"allow":["git push"]`)
	cards := pendingCards(t, s)
	if len(cards) != 1 {
		t.Fatalf("cards = %v", cards)
	}
	var wider []string
	for _, w := range cards[0]["wider"].([]any) {
		wider = append(wider, w.(string))
	}
	got := strings.Join(wider, "\n")
	for _, want := range []string{"reads outside the project: /src/trellis", "a budget: 50000 tokens, 30 turns", "network: pub.dev", "runs denied verbs: git push"} {
		if !strings.Contains(got, want) {
			t.Errorf("wider lacks %q:\n%s", want, got)
		}
	}
	denied := cards[0]["denied"].([]any)
	if len(denied) != len(envdefault.Denied)-1 || denied[0] != "gh release" {
		t.Fatalf("denied = %v", denied)
	}
	if _, err := os.Stat(envFile()); err == nil {
		t.Fatal("a wider proposal wrote the envelope file")
	}
	// Approve writes the envelope with every deny but the allowed verb.
	result(t, approve(t, s, "trellis"))
	b, _ := os.ReadFile(envFile())
	if strings.Contains(string(b), "deny_git_push") || !strings.Contains(string(b), "deny_gh_release") {
		t.Fatalf("the approved file has the wrong denies:\n%s", b)
	}
}

// A project outside every trusted root cards, however narrow the proposal.
func TestAProposalOutsideTheTrustedRootsCards(t *testing.T) {
	s := proposalServer(t)
	dir := trellisDir(t)
	if err := config.Save(config.Config{Steward: config.Steward{TrustedRoots: []string{"/nowhere/code"}}}); err != nil {
		t.Fatal(err)
	}
	// The project is not pinned now: resolveProject finds it among the
	// recent dirs.
	if err := saveRecentDirs(s.cfg.DataDir, []string{dir}); err != nil {
		t.Fatal(err)
	}
	env := `{"generated_by":"steward","task":"fix","permissions":{"file_read":["` + dir + `"]}}`
	res := result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`}`)[0])
	if res["pending"] != true {
		t.Fatalf("result = %v", res)
	}
	cards := pendingCards(t, s)
	if len(cards) != 1 || !strings.Contains(fmt.Sprint(cards[0]["wider"]), "outside the trusted roots") {
		t.Fatalf("cards = %v", cards)
	}
}

func TestAllowNamesOnlyDeniedVerbs(t *testing.T) {
	s := proposalServer(t)
	resp := roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+envA+`,"allow":["rm -rf"]}`)[0]
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "not a denied verb") {
		t.Fatalf("resp = %+v", resp)
	}
}

// A proposal never replaces the owner's envelope without a card, even one
// that fits the default: it could drop the budget the owner set.
func TestAProposalThatWouldReplaceTheOwnersEnvelopeCards(t *testing.T) {
	s := proposalServer(t)
	armWarden(t, s)
	dir := trellisDir(t)
	if err := os.MkdirAll(envelopeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := `{"envelope":{"generated_by":"o","task":"t","permissions":{}},"tokens":3000000,"turns":600,"granted_by":"owner"}`
	if err := os.WriteFile(envFile(), []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}
	env := `{"generated_by":"steward","task":"fix","permissions":{"file_read":["` + dir + `"]}}`
	res := result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`}`)[0])
	if res["pending"] != true {
		t.Fatalf("result = %v", res)
	}
	if b, _ := os.ReadFile(envFile()); string(b) != owner {
		t.Fatalf("the owner's file changed:\n%s", b)
	}
	cards := pendingCards(t, s)
	if len(cards) != 1 || !strings.Contains(fmt.Sprint(cards[0]["wider"]), "replaces an envelope the owner approved") {
		t.Fatalf("cards = %v", cards)
	}
	// A file the default grant wrote is the steward's to replace.
	def := `{"envelope":{"generated_by":"d","task":"t","permissions":{}},"tokens":0,"turns":0,"granted_by":"default"}`
	if err := os.WriteFile(envFile(), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	res = result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`}`)[0])
	if res["approved"] != "trellis" {
		t.Fatalf("result = %v", res)
	}
}

// Review r9 findings 1 and 2: a proposal that fits never reaches the file
// as its own bytes. The card-free grant writes the default envelope itself,
// and a bare ** or a proposal with its own deny invariant cards.
func TestAFittingProposalWritesTheDefaultNotItsOwnBytes(t *testing.T) {
	s := proposalServer(t)
	armWarden(t, s)
	dir := trellisDir(t)
	for _, env := range []string{
		`{"generated_by":"steward","task":"t","permissions":{"file_read":["**"],"file_write":["**"],"shell":true,"shell_allowlist":["*"]}}`,
		`{"generated_by":"steward","task":"t","permissions":{"file_read":["../trellis/**"]}}`,
		`{"generated_by":"steward","task":"t","permissions":{"file_read":["` + dir + `"]},"invariants":[{"type":"deny_git_push","description":"x","expr":null,"enforce":false}]}`,
	} {
		res := result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`}`)[0])
		if res["pending"] != true {
			t.Fatalf("%s fits: %v", env, res)
		}
		if _, err := os.Stat(envFile()); err == nil {
			t.Fatalf("%s wrote the envelope file", env)
		}
	}
	roundTrip(t, s, `{"id":"d","cmd":"envelope.decline","project":"trellis"}`)
	env := `{"generated_by":"steward","task":"narrow","permissions":{"file_read":["` + dir + `/lib"],"shell":true,"shell_allowlist":["git"]}}`
	res := result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`,"help_wait_s":90}`)[0])
	if res["approved"] != "trellis" {
		t.Fatalf("result = %v", res)
	}
	pe, err := readProjectEnvelope(envFile(), "trellis")
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes.TrimSpace(pe.Envelope)) != string(bytes.TrimSpace(envdefault.Envelope(dir))) || pe.HelpWaitS != 90 {
		t.Fatalf("the card-free grant wrote:\n%s", pe.Envelope)
	}
}

// Review r9 finding 5a: a fitting proposal is card-free only when its
// workers will run fenced.
func TestAFittingProposalCardsWhenTheFenceIsOff(t *testing.T) {
	s := proposalServer(t)
	dir := trellisDir(t)
	env := `{"generated_by":"steward","task":"t","permissions":{"file_read":["` + dir + `"]}}`
	res := result(t, roundTrip(t, s, `{"id":"p","cmd":"envelope.propose","project":"trellis","envelope":`+env+`}`)[0])
	if res["pending"] != true {
		t.Fatalf("result = %v", res)
	}
	if cards := pendingCards(t, s); len(cards) != 1 || !strings.Contains(fmt.Sprint(cards[0]["wider"]), "the fence is off") {
		t.Fatalf("cards = %v", cards)
	}
}
