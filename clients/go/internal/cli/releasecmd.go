package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/registry"
	"daisugi-verify/internal/signing"
)

// This file is `daisugi release keygen|sign|verify` (opendaisugi/release.py
// and the CLI over it): ed25519 release-signing keys and signed SHA-256
// manifests, on the one key system the pathway bundles use.

const releaseHelp = `Usage: daisugi release [OPTIONS] COMMAND [ARGS]...

  Signed release manifests: an ed25519 keypair, a signed SHA-256 manifest
  over the artifacts, and its verification against trusted signers.

Commands:
  keygen  Generate an ed25519 release-signing keypair.
  sign    Build a SHA-256 manifest over ARTIFACTS and sign it.
  verify  Verify a release: trusted signature AND intact artifacts.
`

func (e *Env) release(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		e.out("%s", releaseHelp)
		return nil
	}
	switch args[0] {
	case "keygen":
		return e.releaseKeygen(args[1:])
	case "sign":
		return e.releaseSign(args[1:])
	case "verify":
		return e.releaseVerify(args[1:])
	}
	e.errf("Usage: daisugi release [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi release --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// raise ends a command the way the oracle ends one that raised: exit 1,
// the exception's qualified type and its text on stderr. Input this
// binary does not read the way the oracle does is refused instead.
func (e *Env) raise(cmd string, err error) error {
	var pe *signing.PyError
	var ve *pmodel.ValidationError
	var un *registry.Unread
	var de *pyjson.DecodeError
	switch {
	case errors.As(err, &un):
		return e.refuse(cmd, err)
	case errors.As(err, &pe):
		e.errf("daisugi %s: %s\n", cmd, pe.Error())
		return exit(1)
	case errors.As(err, &ve):
		e.errf("daisugi %s: pydantic_core._pydantic_core.ValidationError: %s\n", cmd, strings.SplitN(ve.String(), "\n", 2)[0])
		return exit(1)
	case errors.As(err, &de):
		e.errf("daisugi %s: json.decoder.JSONDecodeError: %s\n", cmd, de.Error())
		return exit(1)
	}
	return e.failPy(cmd, err)
}

// cwd is Path.cwd(): the kernel's working directory.
func cwd() string {
	d, err := syscall.Getwd()
	if err != nil {
		return "."
	}
	return d
}

// readTextStrip is path.read_text(encoding="utf-8").strip().
func readTextStrip(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", &signing.PyError{Type: "UnicodeDecodeError", Msg: "'utf-8' codec can't decode the file " + path}
	}
	return pystr.Strip(string(raw)), nil
}

// writeText is path.write_text(text): created 0666 less the umask, or
// truncated in place keeping its mode.
func writeText(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o666)
}

func (e *Env) releaseKeygen(args []string) error {
	const cmd = "release keygen"
	opts := []opt{
		{names: []string{"--out-dir"}, value: true, metavar: "PATH", help: "Directory to write the keypair into."},
		{names: []string{"--name"}, value: true, metavar: "TEXT", help: "Key file basename."},
	}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Generate an ed25519 release-signing keypair.", opts)
	}
	outDir := gateroot.PathStr(p.str("--out-dir", cwd()))
	name := p.str("--name", "release_signing")
	if err := mkdirParentsPy(outDir); err != nil {
		return e.raise(cmd, err)
	}
	priv, pub, err := signing.GenerateKeypair()
	if err != nil {
		return e.fail(cmd, err)
	}
	privPath := gateroot.PathStr(gateroot.Join(outDir, name+".key"))
	pubPath := gateroot.PathStr(gateroot.Join(outDir, name+".pub"))
	if err := writeText(privPath, priv+"\n"); err != nil {
		return e.raise(cmd, err)
	}
	if err := os.Chmod(privPath, 0o600); err != nil {
		return e.raise(cmd, err)
	}
	if err := writeText(pubPath, pub+"\n"); err != nil {
		return e.raise(cmd, err)
	}
	e.out("private key → %s (chmod 600 — keep it offline)\n", privPath)
	e.out("public key  → %s\n", pubPath)
	e.out("public key (b64): %s\n", pub)
	return nil
}

// mkdirParentsPy is Path.mkdir(parents=True, exist_ok=True): an existing
// file at the path is FileExistsError.
func mkdirParentsPy(d string) error {
	if st, err := os.Stat(d); err == nil {
		if st.IsDir() {
			return nil
		}
		return &signing.PyError{Type: "FileExistsError", Msg: fmt.Sprintf("[Errno 17] File exists: %s", pystr.Repr(d))}
	}
	return os.MkdirAll(d, 0o777)
}

// sortKeys is v with every object's keys sorted, for json.dumps(sort_keys=True).
func sortKeys(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		keys := append([]string{}, x.Keys()...)
		sort.Strings(keys)
		out := pyjson.NewObject()
		for _, k := range keys {
			out.Set(k, sortKeys(x.Value(k)))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = sortKeys(el)
		}
		return out
	}
	return v
}

// isoNow is datetime.now(timezone.utc).isoformat() with "+00:00" as "Z":
// the microseconds only when they are not zero.
func isoNow() string {
	t := time.Now().UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "Z"
}

// sha256File is release.sha256_file.
func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// canonicalManifest is canonicalize_manifest: the manifest without its
// signature, as sorted compact ASCII JSON.
func canonicalManifest(m *pyjson.Object) []byte {
	body := pyjson.NewObject()
	for _, k := range m.Keys() {
		if k != "signature" {
			body.Set(k, m.Value(k))
		}
	}
	return []byte(pyjson.CanonicalASCII(body))
}

func (e *Env) releaseSign(args []string) error {
	const cmd = "release sign"
	opts := []opt{
		{names: []string{"--version"}, value: true, metavar: "TEXT", help: "Release version string."},
		{names: []string{"--key"}, value: true, metavar: "PATH", help: "Path to the base64 ed25519 private key."},
		{names: []string{"--signer"}, value: true, metavar: "TEXT", help: "Signer name to bind into the manifest."},
		{names: []string{"--out", "-o"}, value: true, metavar: "PATH", help: "Where to write the signed manifest."},
	}
	p, err := parseArgs(args, opts, 1<<30)
	if err != nil {
		return e.usageArgs(cmd, "ARTIFACTS...", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " ARTIFACTS...", "Build a SHA-256 manifest over ARTIFACTS and sign it.", opts)
	}
	if len(p.args) == 0 {
		return e.usageArgs(cmd, "ARTIFACTS...", &usageError{"Missing argument 'artifacts'."})
	}
	for _, o := range []string{"--version", "--key", "--signer"} {
		if !p.has(o) {
			return e.usageArgs(cmd, "ARTIFACTS...", &usageError{fmt.Sprintf("Missing option '%s'.", o)})
		}
	}
	arts := make([]string, len(p.args))
	var missing []string
	for i, a := range p.args {
		arts[i] = gateroot.PathStr(a)
		if _, err := os.Stat(arts[i]); err != nil {
			missing = append(missing, arts[i])
		}
	}
	if len(missing) > 0 {
		e.errf("no such artifact(s): %s\n", strings.Join(missing, ", "))
		return exit(2)
	}
	if dup := duplicateNames(arts); len(dup) > 0 {
		e.errf("two artifacts share a name: %s (the manifest names each artifact by its file name)\n",
			strings.Join(dup, ", "))
		return exit(2)
	}
	created := isoNow()
	type entry struct {
		name, sha string
		size      int64
	}
	var entries []entry
	for _, a := range arts {
		sum, n, err := sha256File(a)
		if err != nil {
			return e.raise(cmd, err)
		}
		entries = append(entries, entry{pathName(a), sum, n})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	list := make([]any, len(entries))
	for i, en := range entries {
		list[i] = pyjson.NewObject().Set("name", en.name).Set("sha256", en.sha).Set("size", pyjson.Int{Text: fmt.Sprint(en.size)})
	}
	priv, err := readTextStrip(gateroot.PathStr(p.str("--key", "")))
	if err != nil {
		return e.raise(cmd, err)
	}
	m := pyjson.NewObject().Set("manifest_version", pyjson.Int{Text: "1"}).Set("version", p.str("--version", "")).
		Set("created_at", created).Set("artifacts", list).Set("signer", p.str("--signer", "")).Set("signature", nil)
	sig, perr := signing.SignBytes(canonicalManifest(m), priv)
	if perr != nil {
		return e.raise(cmd, perr)
	}
	m.Set("signature", sig)
	out := gateroot.PathStr(p.str("--out", "release-manifest.json"))
	if err := writeText(out, pyjson.DumpsIndent(sortKeys(m), 2, true)+"\n"); err != nil {
		return e.raise(cmd, err)
	}
	e.out("signed manifest (%d artifacts) → %s\n", len(entries), out)
	return nil
}

// duplicateNames is release.duplicate_names: the file names more than
// one artifact has, sorted.
func duplicateNames(arts []string) []string {
	count := map[string]int{}
	for _, a := range arts {
		count[pathName(a)]++
	}
	var out []string
	for n, c := range count {
		if c > 1 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// pathName is Path(p).name.
func pathName(p string) string {
	p = gateroot.PathStr(p)
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	if p == "." {
		return ""
	}
	return p
}

// strOf is str(v) for a value json.loads makes.
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

func typeError(msg string) error { return &signing.PyError{Type: "TypeError", Msg: msg} }

// verifyArtifacts is verify_artifacts: the names missing or changed under dir.
func verifyArtifacts(m *pyjson.Object, dir string) ([]string, error) {
	raw, present := m.Get("artifacts")
	if !present {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		if o, isObj := raw.(*pyjson.Object); isObj {
			// Iterating a dict gives its keys; entry["name"] on a str
			// raises TypeError.
			if o.Len() == 0 {
				return nil, nil
			}
			return nil, typeError("string indices must be integers, not 'str'")
		}
		if s, isStr := raw.(string); isStr {
			if s == "" {
				return nil, nil
			}
			return nil, typeError("string indices must be integers, not 'str'")
		}
		return nil, typeError(fmt.Sprintf("'%s' object is not iterable", pyTypeName(raw)))
	}
	var bad []string
	for _, it := range list {
		en, ok := it.(*pyjson.Object)
		if !ok {
			return nil, typeError("an artifact entry is not a dict")
		}
		nv, has := en.Get("name")
		if !has {
			return nil, &signing.PyError{Type: "KeyError", Msg: "'name'"}
		}
		name, ok := nv.(string)
		if !ok {
			return nil, typeError("unsupported operand type(s) for /: 'PosixPath' and '" + pyTypeName(nv) + "'")
		}
		path := name
		if !strings.HasPrefix(name, "/") {
			path = gateroot.Join(dir, name)
		}
		if _, err := os.Stat(path); err != nil {
			bad = append(bad, name)
			continue
		}
		sum, _, err := sha256File(path)
		if err != nil {
			return nil, err
		}
		want, has := en.Get("sha256")
		if !has {
			return nil, &signing.PyError{Type: "KeyError", Msg: "'sha256'"}
		}
		if w, isStr := want.(string); !isStr || w != sum {
			bad = append(bad, name)
		}
	}
	return bad, nil
}

func (e *Env) releaseVerify(args []string) error {
	const cmd = "release verify"
	opts := []opt{
		{names: []string{"--artifact-dir"}, value: true, metavar: "PATH", help: "Directory holding the artifacts to check."},
		{names: []string{"--signer"}, value: true, multiple: true, metavar: "TEXT", help: "Trusted signer name(s) to accept. Repeatable."},
		{names: []string{"--registry"}, value: true, metavar: "PATH", help: "Trusted-signer registry JSON."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "MANIFEST_PATH", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " MANIFEST_PATH", "Verify a release: trusted signature AND intact artifacts. Fails closed.", opts)
	}
	if len(p.args) == 0 {
		return e.usageArgs(cmd, "MANIFEST_PATH", &usageError{"Missing argument 'manifest_path'."})
	}
	regPath := gateroot.PathStr(p.str("--registry", gateroot.Join(e.home, ".opendaisugi/trusted_signers.json")))
	reg, err := signing.LoadRegistry(regPath)
	if err != nil {
		return e.raise(cmd, err)
	}
	names := p.vals["--signer"]
	if len(names) == 0 {
		names = reg.Names()
	}
	if len(names) == 0 {
		e.errf("no trusted signers: add the release pubkey to %s or pass --signer\n", regPath)
		return exit(1)
	}
	raw, err := os.ReadFile(gateroot.PathStr(p.args[0]))
	if err != nil {
		return e.raise(cmd, err)
	}
	if !utf8.Valid(raw) {
		return e.refuse(cmd, errors.New("the manifest is not UTF-8"))
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	if derr != nil {
		return e.raise(cmd, derr)
	}
	m, ok := v.(*pyjson.Object)
	if !ok {
		return e.raise(cmd, &signing.PyError{Type: "AttributeError",
			Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(v))})
	}
	signer := m.Value("signer")
	sigOK := false
	for _, n := range names {
		pub, has := reg.Get(n)
		if !has || pub == nil {
			continue
		}
		sig := m.Value("signature")
		if !pyjson.Truthy(sig) {
			continue
		}
		s, isStr := sig.(string)
		if isStr && signing.VerifyWith(canonicalManifest(m), s, pub) {
			sigOK = true
			break
		}
	}
	bad, err := verifyArtifacts(m, gateroot.PathStr(p.str("--artifact-dir", cwd())))
	if err != nil {
		return e.raise(cmd, err)
	}
	var reason string
	switch {
	case !sigOK:
		reason = "signature not verifiable under any trusted signer"
	case len(bad) > 0:
		reason = "artifact hash mismatch: " + strings.Join(bad, ", ")
	default:
		reason = "signature valid and all artifacts intact"
	}
	if sigOK && len(bad) == 0 {
		e.out("OK — %s (signer: %s)\n", reason, strOf(signer))
		return nil
	}
	e.errf("FAILED — %s\n", reason)
	return exit(1)
}
