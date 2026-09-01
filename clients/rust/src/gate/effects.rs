//! `opendaisugi/effects.py`: the effect class of one call and the tier of a
//! deny.

use super::frames::{frame, frames};
use super::py::text::casefold;
use super::paths::{commonpath, isabs, join2, lexists, normpath};
use super::pystr::py_strip;
use super::record::Record;
use super::{catch, Runner, R};
use crate::interpreter_parse::shlex_split;

pub const SILENT: &str = "silent";
pub const UNDOABLE: &str = "undoable";
pub const PERMANENT: &str = "permanent";

const SEVERITY: &[&str] = &[
    "credential", "prod", "force_push", "delete", "push_shared", "unknown", "write_git_dir", "read_git_dir",
    "write_outside_workspace", "read_outside_workspace", "write_inside_workspace", "network_get", "test_run", "read",
];

const CREDENTIAL_DIRS: &[&str] =
    &[".ssh", ".gnupg", ".aws", ".kube", ".docker", ".password-store", ".azure", ".config/gcloud"];
const CREDENTIAL_FILES: &[&str] =
    &[".netrc", ".git-credentials", ".pypirc", ".npmrc", "credentials", "credentials.json", ".env"];
pub const CWD_HEADS: &[&str] = &["cd", "pushd", "popd"];
const DELETE_HEADS: &[&str] = &["rm", "rmdir", "shred", "unlink", "truncate", "dd"];
const PROD_HEADS: &[&str] = &[
    "kubectl", "helm", "terraform", "pulumi", "ansible", "ansible-playbook", "fly", "flyctl", "heroku", "vercel",
    "netlify", "gcloud", "aws", "az", "doctl", "ssh", "scp", "rsync",
];
const CREDENTIAL_HEADS: &[&str] = &["gpg", "pass", "ssh-keygen", "ssh-add", "security", "op"];
const PUBLISH: &[(&str, &str)] = &[
    ("npm", "publish"),
    ("pnpm", "publish"),
    ("yarn", "publish"),
    ("cargo", "publish"),
    ("uv", "publish"),
    ("twine", "upload"),
    ("docker", "push"),
    ("podman", "push"),
];

/// The most working directories one shell line is followed through.
pub const MAX_CWDS: usize = 32;

fn has(set: &[&str], s: &str) -> bool {
    set.contains(&s)
}

pub fn tier_for(effect: &str) -> &'static str {
    if has(&["read", "write_inside_workspace", "network_get", "test_run"], effect) {
        UNDOABLE
    } else {
        PERMANENT
    }
}

fn rank(c: &str) -> i64 {
    SEVERITY.iter().position(|s| *s == c).map(|i| i as i64).unwrap_or(-1)
}

fn worst(classes: &[String]) -> String {
    let mut best = match classes.first() {
        None => return "unknown".into(),
        Some(c) => c,
    };
    for c in &classes[1..] {
        if rank(c) < rank(best) {
            best = c;
        }
    }
    best.clone()
}

/// `effects.Workspace`.
#[derive(Debug, Clone, PartialEq)]
pub struct Workspace {
    pub cwd: String,
    pub base: String,
}

fn under(path: &str, base: &str) -> bool {
    commonpath(&[path, base]).as_deref() == Some(base)
}

fn under_proc(path: &str) -> bool {
    under(path, "/proc")
}

/// `any(part.casefold() == ".git" for part in rel.split("/"))`.
fn has_git_part(rel: &str) -> R<bool> {
    Ok(rel.split('/').any(|part| casefold(part) == ".git"))
}

/// Flags one command may carry and keep its class (`effects.Flags`).
#[derive(Debug, Clone, Default)]
pub struct Flags {
    short: &'static str,
    short_value: String,
    short_optional: String,
    long: Vec<&'static str>,
    long_value: Vec<&'static str>,
    long_optional: Vec<&'static str>,
    single_dash: bool,
    numeric: bool,
    no_positional: bool,
}

fn fl(short: &'static str, short_value: &'static str, long: &[&'static str], long_value: &[&'static str]) -> Flags {
    Flags {
        short,
        short_value: short_value.into(),
        long: long.to_vec(),
        long_value: long_value.to_vec(),
        ..Default::default()
    }
}

impl Flags {
    fn opt(mut self, long_optional: &[&'static str]) -> Self {
        self.long_optional = long_optional.to_vec();
        self
    }
    fn short_opt(mut self, s: &'static str) -> Self {
        self.short_optional = s.into();
        self
    }
    fn numeric(mut self) -> Self {
        self.numeric = true;
        self
    }
    fn single_dash(mut self) -> Self {
        self.single_dash = true;
        self
    }
    fn no_positional(mut self) -> Self {
        self.no_positional = true;
        self
    }
    /// `effects._optional`: every value flag takes its value only when
    /// attached.
    fn optional(self) -> Self {
        let mut lo = self.long_value.clone();
        lo.extend(self.long_optional.iter().copied());
        Flags {
            short: self.short,
            short_value: String::new(),
            short_optional: format!("{}{}", self.short_value, self.short_optional),
            long: self.long,
            long_value: vec![],
            long_optional: lo,
            single_dash: self.single_dash,
            numeric: self.numeric,
            no_positional: self.no_positional,
        }
    }
}

fn is_digits(s: &str) -> bool {
    !s.is_empty() && s.bytes().all(|c| c.is_ascii_digit())
}

/// `effects._operands`; `None` when a flag is outside spec.
fn operands(args: &[String], spec: &Flags) -> Option<Vec<String>> {
    let mut out = Vec::new();
    let mut ended = false;
    let mut i = 0;
    while i < args.len() {
        let a = &args[i];
        i += 1;
        if ended || a == "-" || !a.starts_with('-') {
            if spec.no_positional {
                return None;
            }
            out.push(a.clone());
            continue;
        }
        if a == "--" {
            ended = true;
            continue;
        }
        if spec.numeric && is_digits(&a[1..]) {
            continue;
        }
        if a.starts_with("--") || spec.single_dash {
            let (mut name, eq) = match a.split_once('=') {
                Some((n, _)) => (n.to_string(), true),
                None => (a.clone(), false),
            };
            if spec.single_dash {
                name = format!("-{}", name.trim_start_matches('-'));
            }
            let n = name.as_str();
            if eq {
                if !spec.long_value.contains(&n) && !spec.long_optional.contains(&n) {
                    return None;
                }
                continue;
            }
            if spec.long.contains(&n) || spec.long_optional.contains(&n) {
                continue;
            }
            if spec.long_value.contains(&n) {
                if i >= args.len() {
                    return None;
                }
                i += 1;
                continue;
            }
            return None;
        }
        let body = a[1..].as_bytes();
        for j in 0..body.len() {
            let c = body[j];
            if spec.short.as_bytes().contains(&c) {
                continue;
            }
            if spec.short_optional.as_bytes().contains(&c) {
                break;
            }
            if spec.short_value.as_bytes().contains(&c) {
                if j + 1 == body.len() {
                    if i >= args.len() {
                        return None;
                    }
                    i += 1;
                }
                break;
            }
            return None;
        }
    }
    Some(out)
}

fn read_flags(head: &str) -> Option<Flags> {
    Some(match head {
        "ls" => fl(
            "aAlhtrSd1FisGnogcuUvXx",
            "",
            &["--all", "--almost-all", "--human-readable", "--directory", "--classify", "--group-directories-first"],
            &["--sort", "--time-style"],
        )
        .opt(&["--color"]),
        "cat" => fl("nbsAETv", "", &["--number", "--show-all"], &[]),
        "head" => fl("qvz", "nc", &["--quiet", "--verbose"], &["--lines", "--bytes"]).numeric(),
        "tail" => fl("qvzfF", "nc", &["--quiet", "--verbose", "--follow"], &["--lines", "--bytes"]).numeric(),
        "grep" => fl(
            "ivnlLcwxEFPosqHhIzab",
            "eABCm",
            &[
                "--ignore-case", "--line-number", "--count", "--files-with-matches", "--files-without-match",
                "--invert-match", "--word-regexp", "--fixed-strings", "--extended-regexp", "--perl-regexp",
                "--only-matching", "--quiet", "--no-filename", "--with-filename",
            ],
            &[
                "--include", "--exclude", "--exclude-dir", "--max-count", "--regexp", "--context", "--after-context",
                "--before-context",
            ],
        )
        .opt(&["--color", "--colour"]),
        "wc" => fl("lwcmL", "", &[], &[]),
        "pwd" => fl("LP", "", &[], &[]).no_positional(),
        "echo" => fl("neE", "", &[], &[]),
        "printf" => fl("", "", &[], &[]),
        "stat" => fl("Ltf", "c", &[], &["--format", "--printf"]),
        "file" => fl("bLiz", "", &["--mime", "--brief"], &[]),
        "sort" => fl(
            "bdfgiMhnRrVucsz",
            "kt",
            &[
                "--numeric-sort", "--reverse", "--unique", "--human-numeric-sort", "--version-sort", "--ignore-case",
                "--stable",
            ],
            &["--key", "--field-separator"],
        ),
        "cut" => fl(
            "sz",
            "dfbc",
            &["--complement", "--only-delimited"],
            &["--delimiter", "--fields", "--bytes", "--characters"],
        ),
        "diff" => fl(
            "uqNwbBiyas",
            "U",
            &["--brief", "--new-file", "--ignore-all-space", "--side-by-side", "--text"],
            &["--unified"],
        )
        .opt(&["--color"]),
        "which" => fl("a", "", &[], &[]),
        "du" => fl(
            "shackmxb",
            "d",
            &["--summarize", "--human-readable", "--all", "--total", "--apparent-size"],
            &["--max-depth"],
        ),
        "df" => fl("hTkail", "", &["--human-readable", "--print-type"], &[]),
        "whoami" => fl("", "", &[], &[]).no_positional(),
        "true" | "false" | "cd" | "pushd" => fl("", "", &[], &[]),
        "basename" => fl("az", "s", &[], &[]),
        "dirname" => fl("z", "", &[], &[]),
        "realpath" => fl("emsqz", "", &[], &[]),
        "readlink" => fl("femnqsz", "", &[], &[]),
        "jq" => fl(
            "rcnesSCMaj",
            "",
            &[
                "--raw-output", "--compact-output", "--null-input", "--exit-status", "--slurp", "--sort-keys",
                "--color-output", "--monochrome-output", "--ascii-output", "--join-output",
            ],
            &[],
        ),
        "popd" => fl("", "", &[], &[]).no_positional(),
        _ => return None,
    })
}

fn make_flags(head: &str) -> Option<Flags> {
    Some(match head {
        "mkdir" => fl("pv", "m", &["--parents", "--verbose"], &["--mode"]),
        "touch" => fl("acm", "dtr", &[], &["--date", "--reference"]),
        _ => return None,
    })
}

const RECURSIVE_READERS: &[&str] = &["grep", "find", "du", "ls"];
const FIND_VALUE: &[&str] = &[
    "-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-type", "-xtype", "-maxdepth", "-mindepth",
    "-newer", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin", "-size", "-regex", "-iregex", "-user", "-group",
    "-perm",
];
const FIND_PLAIN: &[&str] = &[
    "-print", "-print0", "-empty", "-not", "-o", "-a", "-and", "-or", "-prune", "-true", "-false", "-readable",
    "-writable", "-executable", "-depth", "-xdev", "-mount", "-P", "(", ")", "!", ",",
];

fn pytest_flags() -> Flags {
    fl(
        "qvxsl",
        "kmrWn",
        &[
            "--lf", "--last-failed", "--ff", "--failed-first", "--nf", "--sw", "--stepwise", "--no-header", "--co",
            "--collect-only", "--quiet", "--verbose", "--exitfirst", "--showlocals",
        ],
        &[
            "--maxfail", "--tb", "--durations", "--deselect", "--ignore", "--ignore-glob", "--color", "--capture",
            "--import-mode", "--timeout",
        ],
    )
}
fn ruff_check() -> Flags {
    fl(
        "q",
        "",
        &["--quiet", "--no-cache", "--statistics", "--preview", "--diff"],
        &[
            "--select", "--ignore", "--extend-select", "--extend-ignore", "--output-format", "--target-version",
            "--line-length",
        ],
    )
}
fn ruff_format() -> Flags {
    fl(
        "q",
        "",
        &["--check", "--diff", "--quiet", "--preview", "--no-cache"],
        &["--line-length", "--target-version"],
    )
}
fn mypy_flags() -> Flags {
    fl(
        "",
        "",
        &[
            "--strict", "--ignore-missing-imports", "--show-error-codes", "--pretty", "--no-error-summary",
            "--check-untyped-defs",
        ],
        &["--python-version"],
    )
}
fn go_test() -> Flags {
    fl(
        "",
        "",
        &["-v", "-race", "-short", "-cover", "-failfast", "-json", "-benchmem"],
        &["-run", "-count", "-timeout", "-p", "-bench", "-cpu", "-tags", "-skip", "-parallel"],
    )
    .single_dash()
}
fn go_vet() -> Flags {
    fl("", "", &["-v", "-json"], &["-tags"]).single_dash()
}
fn cargo_test() -> Flags {
    fl(
        "q",
        "pj",
        &[
            "--release", "--quiet", "--all", "--workspace", "--lib", "--bins", "--no-fail-fast", "--doc",
            "--all-features", "--no-default-features", "--locked", "--frozen", "--offline",
        ],
        &["--package", "--test", "--features", "--jobs"],
    )
}
fn libtest() -> Flags {
    fl(
        "q",
        "",
        &["--nocapture", "--ignored", "--include-ignored", "--exact", "--show-output", "--quiet"],
        &["--test-threads", "--skip"],
    )
}
fn uv_run() -> Flags {
    fl("q", "", &["--no-sync", "--frozen", "--locked", "--offline", "--quiet"], &[])
}
fn curl_flags() -> Flags {
    fl(
        "sSLfIikv",
        "HAem",
        &[
            "--silent", "--show-error", "--location", "--fail", "--head", "--include", "--insecure", "--verbose",
            "--compressed", "--http1.1", "--http2",
        ],
        &["--header", "--user-agent", "--referer", "--max-time", "--connect-timeout", "--retry"],
    )
}

fn first(args: &[String]) -> &str {
    args.first().map(String::as_str).unwrap_or("")
}

fn tail(a: &[String]) -> &[String] {
    if a.is_empty() {
        a
    } else {
        &a[1..]
    }
}

fn split_dash_dash(rest: &[String]) -> (&[String], &[String]) {
    match rest.iter().position(|a| a == "--") {
        Some(c) => (&rest[..c], &rest[c + 1..]),
        None => (rest, &[]),
    }
}

fn first_non_flag(rest: &[String]) -> Option<usize> {
    rest.iter().position(|a| !a.starts_with('-'))
}

fn eq_list(a: &[String], b: &[&str]) -> bool {
    a.len() == b.len() && a.iter().zip(b).all(|(x, y)| x == y)
}

fn ok(a: &[String], f: &Flags) -> bool {
    operands(a, f).is_some()
}

fn is_test_run(head: &str, args: &[String]) -> bool {
    match head {
        "pytest" => ok(args, &pytest_flags()),
        "ruff" => {
            if first(args) == "check" {
                return ok(&args[1..], &ruff_check());
            }
            if first(args) == "format" && args.iter().any(|a| a == "--check" || a == "--diff") {
                return ok(&args[1..], &ruff_format());
            }
            false
        }
        "mypy" => ok(args, &mypy_flags()),
        "go" => {
            if first(args) == "test" {
                return ok(&args[1..], &go_test());
            }
            if first(args) == "vet" {
                return ok(&args[1..], &go_vet());
            }
            false
        }
        "cargo" => {
            if first(args) != "test" || args.is_empty() {
                return false;
            }
            let (a, b) = split_dash_dash(&args[1..]);
            ok(a, &cargo_test()) && ok(b, &libtest())
        }
        "npm" | "pnpm" | "yarn" => eq_list(args, &["test"]) || eq_list(args, &["run", "test"]),
        "node" => first(args) == "--test" && !args.is_empty() && args[1..].iter().all(|a| !a.starts_with('-')),
        "make" => eq_list(args, &["test"]) || eq_list(args, &["check"]),
        "python" | "python3" => args.len() >= 2 && args[0] == "-m" && args[1] == "pytest" && ok(&args[2..], &pytest_flags()),
        "uv" => {
            if first(args) != "run" || args.is_empty() {
                return false;
            }
            let rest = &args[1..];
            let tool = match first_non_flag(rest) {
                None => return false,
                Some(t) => t,
            };
            if !ok(&rest[..tool], &uv_run()) {
                return false;
            }
            if !["pytest", "ruff", "mypy"].contains(&rest[tool].as_str()) {
                return false;
            }
            is_test_run(&rest[tool], &rest[tool + 1..])
        }
        _ => false,
    }
}

fn test_operands(head: &str, args: &[String]) -> Option<Vec<String>> {
    match head {
        "pytest" => operands(args, &pytest_flags()),
        "ruff" => {
            if first(args) == "check" {
                return operands(&args[1..], &ruff_check());
            }
            operands(tail(args), &ruff_format())
        }
        "mypy" => operands(args, &mypy_flags()),
        "go" => {
            if first(args) == "test" {
                return operands(&args[1..], &go_test());
            }
            operands(tail(args), &go_vet())
        }
        "cargo" => {
            let (a, b) = split_dash_dash(tail(args));
            let mut x = operands(a, &cargo_test())?;
            x.extend(operands(b, &libtest())?);
            Some(x)
        }
        "node" => Some(tail(args).to_vec()),
        "python" | "python3" => {
            if args.len() < 2 {
                return operands(&[], &pytest_flags());
            }
            operands(&args[2..], &pytest_flags())
        }
        "uv" => {
            let rest = tail(args);
            let tool = first_non_flag(rest)?;
            test_operands(&rest[tool], &rest[tool + 1..])
        }
        _ => Some(vec![]),
    }
}

fn find_paths(args: &[String]) -> Vec<String> {
    let mut out = Vec::new();
    for a in args {
        if a == "-P" {
            continue;
        }
        if a.starts_with('-') || a == "(" || a == ")" || a == "!" || a == "," {
            break;
        }
        out.push(a.clone());
    }
    if out.is_empty() {
        vec![".".into()]
    } else {
        out
    }
}

fn find_class(args: &[String]) -> &'static str {
    let mut i = 0;
    while i < args.len() {
        let a = args[i].as_str();
        i += 1;
        if has(FIND_VALUE, a) {
            if i >= args.len() {
                return "unknown";
            }
            i += 1;
        } else if has(FIND_PLAIN, a) {
        } else if a.starts_with('-') {
            return "unknown";
        }
    }
    "read"
}

fn git_read(sub: &str) -> Option<Flags> {
    let f = match sub {
        "status" => fl("sb", "", &["--short", "--branch", "--ignored"], &[])
            .short_opt("u")
            .opt(&["--porcelain", "--untracked-files"]),
        "log" => fl(
            "p",
            "nSG",
            &[
                "--oneline", "--stat", "--graph", "--decorate", "--all", "--patch", "--no-merges", "--reverse",
                "--name-only", "--name-status", "--shortstat", "--first-parent", "--no-color", "--abbrev-commit",
                "--follow", "--no-ext-diff",
            ],
            &["--max-count", "--since", "--until", "--author", "--grep", "--format", "--pretty", "--date", "--decorate"],
        )
        .numeric(),
        "diff" => fl(
            "w",
            "U",
            &[
                "--stat", "--cached", "--staged", "--name-only", "--name-status", "--shortstat", "--no-color",
                "--numstat", "--check", "--no-ext-diff",
            ],
            &["--unified", "--diff-filter", "--color", "--stat"],
        ),
        "show" => fl(
            "s",
            "",
            &["--stat", "--name-only", "--name-status", "--oneline", "--no-patch", "--no-ext-diff"],
            &["--format", "--pretty"],
        ),
        "blame" => fl("wse", "L", &[], &[]),
        "rev-parse" => fl(
            "",
            "",
            &[
                "--abbrev-ref", "--show-toplevel", "--short", "--verify", "--git-dir", "--is-inside-work-tree",
                "--symbolic-full-name",
            ],
            &["--short"],
        ),
        "ls-files" => fl(
            "modscz",
            "",
            &["--others", "--modified", "--deleted", "--cached", "--exclude-standard", "--stage"],
            &[],
        ),
        "grep" => fl(
            "nilwvEFcI",
            "e",
            &["--cached", "--line-number", "--ignore-case", "--count", "--files-with-matches"],
            &[],
        ),
        "describe" => fl("", "", &["--tags", "--always", "--dirty", "--long"], &["--abbrev", "--match"]),
        "shortlog" => fl("sne", "", &["--summary", "--numbered", "--email"], &[]),
        "cat-file" => fl("tpse", "", &[], &[]),
        "remote" => fl("v", "", &[], &[]).no_positional(),
        _ => return None,
    };
    Some(f.optional())
}

fn git_write(sub: &str) -> Option<Flags> {
    let f = match sub {
        "add" => fl("uAvnN", "", &["--all", "--update", "--verbose", "--dry-run", "--intent-to-add"], &[]),
        "commit" => fl(
            "aqvs",
            "mF",
            &["--all", "--quiet", "--verbose", "--signoff", "--allow-empty", "--no-edit", "--amend"],
            &["--message", "--author", "--file"],
        ),
        "merge" => fl(
            "",
            "m",
            &["--no-ff", "--ff-only", "--ff", "--no-edit", "--squash", "--no-commit"],
            &["--message"],
        ),
        "rebase" => fl("", "", &["--continue"], &["--onto"]),
        "cherry-pick" => fl("n", "", &["--no-commit", "--continue"], &[]),
        "revert" => fl("n", "", &["--no-edit", "--no-commit", "--continue"], &[]),
        "reset" => fl("q", "", &["--soft", "--mixed", "--quiet"], &[]),
        "switch" => fl("", "c", &["--detach"], &["--create"]),
        _ => return None,
    };
    Some(f.optional())
}

const GIT_TREE_MOVERS: &[&str] =
    &["add", "commit", "merge", "rebase", "cherry-pick", "revert", "reset", "switch", "checkout", "stash"];
const GIT_DELETE: &[&str] = &["clean", "restore", "rm", "gc", "prune", "filter-branch", "update-ref"];

fn git_branch_list() -> Flags {
    fl(
        "arv",
        "",
        &["--all", "--remotes", "--verbose", "--list", "--show-current"],
        &["--contains", "--merged", "--no-merged", "--sort"],
    )
    .optional()
}
fn git_tag() -> Flags {
    fl("la", "m", &["--list"], &["--message"]).optional()
}
fn git_fetch() -> Flags {
    fl("qv", "", &["--all", "--tags", "--no-tags", "--quiet", "--verbose"], &[]).optional()
}
fn git_stash_save() -> Flags {
    fl("uq", "m", &["--include-untracked", "--quiet"], &["--message"]).optional()
}

fn moves_tree(tokens: &[String]) -> bool {
    if tokens.len() > 1 && tokens[0] == "git" && has(GIT_TREE_MOVERS, &tokens[1]) {
        return true;
    }
    !tokens.is_empty() && is_test_run(&tokens[0], &tokens[1..])
}

fn is_output_word(w: &str) -> bool {
    w.starts_with("--out") || (w.starts_with("-o") && !w.starts_with("--") && w.len() > 2)
}

fn is_http_url(w: &str) -> bool {
    let low = w.to_ascii_lowercase();
    low.starts_with("http://") || low.starts_with("https://")
}

fn sends_a_file(w: &str) -> bool {
    if w.starts_with('@') {
        return true;
    }
    if w.starts_with("--") {
        return match w.split_once('=') {
            Some((name, value)) => {
                value.starts_with('@') && (name.starts_with("--header") || name.starts_with("--data"))
            }
            None => false,
        };
    }
    w.starts_with('-') && (w.contains("H@") || w.contains("d@"))
}

fn git_class(args: &[String]) -> String {
    if args.is_empty() || args[0].starts_with('-') {
        return "unknown".into();
    }
    let (sub, rest) = (args[0].as_str(), &args[1..]);
    if sub == "push" {
        for a in rest {
            if ["-f", "--force", "--mirror", "--delete", "-d", "--prune"].contains(&a.as_str())
                || a.starts_with("--force")
                || a.starts_with('+')
                || a.starts_with(':')
            {
                return "force_push".into();
            }
        }
        return "push_shared".into();
    }
    if has(GIT_DELETE, sub) {
        return "delete".into();
    }
    if let Some(spec) = git_read(sub) {
        return if ok(rest, &spec) { "read" } else { "unknown" }.into();
    }
    if let Some(spec) = git_write(sub) {
        return if ok(rest, &spec) { "write_inside_workspace" } else { "unknown" }.into();
    }
    let s = match sub {
        "fetch" => match operands(rest, &git_fetch()) {
            None => "unknown",
            Some(ops) if ops.iter().any(|o| o.contains(':') || o.starts_with('+')) => "unknown",
            Some(_) => "network_get",
        },
        "branch" => match operands(rest, &git_branch_list()) {
            None => "unknown",
            Some(ops) if ops.len() > 2 => "unknown",
            Some(ops) if !ops.is_empty() => "write_inside_workspace",
            Some(_) => "read",
        },
        "tag" => match operands(rest, &git_tag()) {
            None => "unknown",
            Some(ops) if !ops.is_empty() && !rest.iter().any(|a| a == "-l" || a == "--list") => {
                "write_inside_workspace"
            }
            Some(_) => "read",
        },
        "stash" => {
            let (verb, tl) = if !rest.is_empty() && !rest[0].starts_with('-') {
                (rest[0].as_str(), &rest[1..])
            } else {
                ("push", rest)
            };
            match verb {
                "drop" | "clear" => "delete",
                "list" | "show" => {
                    if ok(tl, &fl("p", "", &["--stat"], &[])) {
                        "read"
                    } else {
                        "unknown"
                    }
                }
                "pop" | "apply" => {
                    if ok(tl, &fl("", "", &["--index"], &[])) {
                        "write_inside_workspace"
                    } else {
                        "unknown"
                    }
                }
                "push" | "save" => {
                    if ok(tl, &git_stash_save()) {
                        "write_inside_workspace"
                    } else {
                        "unknown"
                    }
                }
                _ => "unknown",
            }
        }
        "checkout" => {
            if (rest.len() == 2 || rest.len() == 3)
                && (rest[0] == "-b" || rest[0] == "-B")
                && rest[1..].iter().all(|x| !x.starts_with('-'))
            {
                "write_inside_workspace"
            } else {
                "delete"
            }
        }
        _ => "unknown",
    };
    s.into()
}

/// `effects._JQ_ENV.search(a)`.
fn jq_env(a: &str) -> bool {
    (a.contains("env") || a.contains("ENV"))
        && super::py::re::search(r"(?<![\w$])env(?!\w)|\$ENV(?!\w)", a).unwrap_or(false)
}

const NO_PATH_HEADS: &[&str] = &["echo", "printf", "true", "false", "which", "whoami", "pwd", "basename", "dirname"];
const CWD_READERS: &[&str] = &["ls", "du", "grep"];

fn is_shell_space(c: u8) -> bool {
    matches!(c, b' ' | b'\t' | b'\n' | b'\r' | 0x0b | 0x0c | 0x1c..=0x1f)
}

/// `effects._unsafe_words`.
fn unsafe_words(text: &str) -> bool {
    let b = text.as_bytes();
    let n = b.len();
    let mut i = 0;
    let mut start = true;
    let mut quote = 0u8;
    while i < n {
        let c = b[i];
        if quote == b'\'' {
            if c == b'\'' {
                quote = 0;
            }
            i += 1;
            continue;
        }
        if quote == b'"' {
            if c == b'\\' {
                i += 2;
                continue;
            }
            if c == b'"' {
                quote = 0;
            } else if c == b'$' || c == b'`' {
                return true;
            }
            i += 1;
            continue;
        }
        if c == b'\\' {
            i += 2;
            start = false;
            continue;
        }
        if c == b'\'' || c == b'"' {
            quote = c;
            start = false;
            i += 1;
            continue;
        }
        if is_shell_space(c) {
            start = true;
            i += 1;
            continue;
        }
        if b"$`*?[{}".contains(&c) {
            return true;
        }
        if c == b'~' && start && i + 1 < n && b[i + 1] != b'/' && !is_shell_space(b[i + 1]) {
            return true;
        }
        start = false;
        i += 1;
    }
    false
}

fn unsafe_redirect(p: &str) -> bool {
    p.contains(['$', '`', '*', '?', '[', '{', '}']) || (p.starts_with('~') && p.len() > 1 && p.as_bytes()[1] != b'/')
}

/// `effects._git_file_values`; `None` when a -F or --file names no path.
fn git_file_values(args: &[String]) -> Option<Vec<String>> {
    if first(args) != "commit" || args.is_empty() {
        return Some(vec![]);
    }
    let mut out = Vec::new();
    let rest = &args[1..];
    let mut i = 0;
    while i < rest.len() {
        let a = rest[i].as_str();
        i += 1;
        if a == "--" {
            break;
        }
        let value: Option<String>;
        if a == "--file" {
            value = rest.get(i).cloned();
            i += 1;
        } else if let Some(v) = a.strip_prefix("--file=") {
            value = Some(v.to_string());
        } else if a.starts_with('-') && !a.starts_with("--") && a.split('m').next().unwrap_or("").contains('F') {
            let t = &a[a.find('F').unwrap() + 1..];
            if !t.is_empty() {
                value = Some(t.to_string());
            } else {
                value = rest.get(i).cloned();
                i += 1;
            }
        } else {
            continue;
        }
        match value {
            Some(v) if !v.is_empty() => out.push(v),
            _ => return None,
        }
    }
    Some(out)
}

fn holds_git_dir(base: Option<&str>) -> bool {
    match base {
        Some(b) if !b.is_empty() => lexists(&join2(b, ".git")),
        _ => false,
    }
}

impl Runner {
    /// `effects._resolve`; `None` for a relative path with no absolute cwd.
    pub fn resolve_path(&self, path: &str, cwd: Option<&str>) -> R<Option<String>> {
        let _f = frame();
        let mut expanded = self.expanduser(path)?;
        if !isabs(&expanded) {
            match cwd {
                Some(c) if !c.is_empty() && isabs(c) => expanded = join2(c, &expanded),
                _ => return Ok(None),
            }
        }
        Ok(Some(self.realpath(&expanded)?))
    }

    fn credential_text(&self, norm: &str) -> R<bool> {
        let _f = frame();
        let parts: Vec<&str> = norm.split('/').filter(|p| !p.is_empty()).collect();
        for (i, part) in parts.iter().enumerate() {
            if has(CREDENTIAL_DIRS, part) {
                return Ok(true);
            }
            if i + 1 < parts.len() && has(CREDENTIAL_DIRS, &format!("{part}/{}", parts[i + 1])) {
                return Ok(true);
            }
        }
        if parts.is_empty() {
            return Ok(false);
        }
        if norm.starts_with('/') && parts[0] == "proc" && parts[1..].contains(&"environ") {
            return Ok(true);
        }
        if norm.starts_with('/') {
            let home = self.expanduser("~")?;
            for h in [normpath(&home), self.realpath(&home)?] {
                if under(norm, &join2(&h, ".config")) {
                    return Ok(true);
                }
            }
        }
        let name = parts[parts.len() - 1];
        let low = casefold(name);
        if low.ends_with(".pem") || low.ends_with(".key") {
            return Ok(true);
        }
        Ok(has(CREDENTIAL_FILES, name) || name.starts_with(".env."))
    }

    fn is_credential_path(&self, path: &str, cwd: Option<&str>) -> R<bool> {
        let _f = frame();
        let typed = normpath(&self.expanduser(path)?).replace('\\', "/");
        if self.credential_text(&typed)? {
            return Ok(true);
        }
        match self.resolve_path(path, cwd)? {
            Some(r) => self.credential_text(&r),
            None => Ok(false),
        }
    }

    fn inside(&self, path: &str, root: Option<&Workspace>) -> R<bool> {
        let _f = frame();
        let root = match root {
            Some(r) if isabs(&r.cwd) && isabs(&r.base) && !path.is_empty() => r,
            _ => return Ok(false),
        };
        let expanded = self.expanduser(path)?;
        if expanded.contains(['~', '$']) {
            return Ok(false);
        }
        let target = self.realpath(&join2(&root.cwd, &expanded))?;
        let base = self.realpath(&root.base)?;
        Ok(under(&target, &base))
    }

    fn write_class(&self, path: &str, root: Option<&Workspace>, cwd: Option<&str>) -> R<String> {
        let _f = frame();
        if under_proc(&normpath(&self.expanduser(path)?)) {
            return Ok("unknown".into());
        }
        let here = match root {
            Some(r) => Some(r.cwd.as_str()),
            None => cwd,
        };
        if let Some(resolved) = self.resolve_path(path, here)? {
            if under_proc(&resolved) {
                return Ok("unknown".into());
            }
        }
        if self.is_credential_path(path, here)? {
            return Ok("credential".into());
        }
        if !self.inside(path, root)? {
            return Ok("write_outside_workspace".into());
        }
        let root = root.expect("inside implies a workspace");
        let target = self.realpath(&join2(&root.cwd, &self.expanduser(path)?))?;
        let rel = self.relpath(&target, &self.realpath(&root.base)?)?;
        if has_git_part(&rel)? {
            return Ok("write_git_dir".into());
        }
        Ok("write_inside_workspace".into())
    }

    fn read_place(&self, path: &str, base: Option<&str>, cwd: Option<&str>) -> R<String> {
        let _f = frame();
        let typed = normpath(&self.expanduser(path)?);
        if under_proc(&typed) {
            return Ok("unknown".into());
        }
        if self.credential_text(&typed)? {
            return Ok("credential".into());
        }
        let resolved = match self.resolve_path(path, cwd)? {
            None => return Ok("read_outside_workspace".into()),
            Some(r) => r,
        };
        if under_proc(&resolved) {
            return Ok("unknown".into());
        }
        if self.credential_text(&resolved)? {
            return Ok("credential".into());
        }
        let base = match base {
            None => return Ok("read_outside_workspace".into()),
            Some(b) => b,
        };
        let real_base = self.realpath(base)?;
        if !under(&resolved, &real_base) {
            return Ok("read_outside_workspace".into());
        }
        if has_git_part(&self.relpath(&resolved, &real_base)?)? {
            return Ok("read_git_dir".into());
        }
        Ok("read".into())
    }

    fn covers_home(&self, arg: &str, cwd: Option<&str>) -> R<bool> {
        let _f = frame();
        let target = match self.resolve_path(arg, cwd)? {
            None => return Ok(false),
            Some(t) => t,
        };
        let home = self.realpath(&self.expanduser("~")?)?;
        Ok(commonpath(&[&target, &home]).as_deref() == Some(target.as_str()))
    }

    fn cwd_inside(&self, cwd: Option<&str>, base: Option<&str>) -> R<bool> {
        let _f = frame();
        match (cwd, base) {
            (Some(c), Some(b)) if !c.is_empty() && !b.is_empty() => Ok(under(&self.realpath(c)?, &self.realpath(b)?)),
            _ => Ok(false),
        }
    }

    /// `effects._command_class`.
    fn command_class(
        &self,
        tokens: &[String],
        root: Option<&Workspace>,
        cwd: Option<&str>,
        base: Option<&str>,
        cwd_base: Option<&str>,
    ) -> R<String> {
        let _f = frame();
        if tokens.is_empty() {
            return Ok("unknown".into());
        }
        let (head, args) = (tokens[0].as_str(), &tokens[1..]);
        let s = |x: &str| -> R<String> { Ok(x.to_string()) };
        if head.contains(['=', '/']) {
            return s("unknown");
        }
        if head == "env" || head == "printenv" {
            return s("credential");
        }
        if head == "jq" && args.iter().any(|a| jq_env(a)) {
            return s("credential");
        }
        if head == "curl" && args.iter().any(|a| sends_a_file(a)) {
            return s("credential");
        }
        if args.iter().any(|a| is_output_word(a)) {
            return s("unknown");
        }
        if args.iter().any(|a| a.contains(['$', '`'])) {
            return s("unknown");
        }
        for a in args {
            let mut paths = vec![a.clone()];
            if head == "git" && a.contains(':') && !a.contains("://") {
                let rev_path = a.split_once(':').map(|x| x.1).unwrap_or("");
                if rev_path.replace('\\', "/").split('/').any(|seg| seg == "..") {
                    return s("unknown");
                }
                paths.push(rev_path.to_string());
            }
            // any(p and _is_credential_path(p, cwd) for p in paths)
            let _g = frame();
            for p in &paths {
                if !p.is_empty() && self.is_credential_path(p, cwd)? {
                    return s("credential");
                }
            }
        }
        if has(CREDENTIAL_HEADS, head) {
            return s("credential");
        }
        if head == "gh" {
            return s(if first(args) == "auth" { "credential" } else { "push_shared" });
        }
        if has(PROD_HEADS, head) {
            return s("prod");
        }
        if has(DELETE_HEADS, head) {
            return s("delete");
        }
        if let Some((_, verb)) = PUBLISH.iter().find(|(h, _)| *h == head) {
            if first(args) == *verb && !args.is_empty() {
                return s("push_shared");
            }
        }
        if has(RECURSIVE_READERS, head) {
            // any(_covers_home(a, cwd) for a in args if ...)
            let _g = frame();
            for a in args {
                if !a.starts_with('-') && self.covers_home(a, cwd)? {
                    return s("credential");
                }
            }
        }
        if head == "git" {
            let cls = git_class(args);
            if tier_for(&cls) != UNDOABLE {
                return Ok(cls);
            }
            if !self.cwd_inside(cwd, cwd_base)? || !holds_git_dir(cwd_base) {
                return s("unknown");
            }
            let files = match git_file_values(args) {
                None => return s("unknown"),
                Some(f) => f,
            };
            let mut places = vec![];
            let mut bad = false;
            for p in &files {
                let pl = self.read_place(p, base, cwd)?;
                if pl != "read" {
                    bad = true;
                }
                places.push(pl);
            }
            if bad {
                let mut all = vec!["unknown".to_string()];
                all.extend(places);
                return Ok(worst(&all));
            }
            return Ok(cls);
        }
        if head == "curl" {
            return match operands(args, &curl_flags()) {
                Some(ops) if !ops.is_empty() && ops.iter().all(|o| is_http_url(o)) => s("network_get"),
                _ => s("unknown"),
            };
        }
        if is_test_run(head, args) {
            if !self.cwd_inside(cwd, cwd_base)? {
                return s("unknown");
            }
            let ops = match test_operands(head, args) {
                None => return s("unknown"),
                Some(o) => o,
            };
            let mut places = vec![];
            let mut all = true;
            for p in &ops {
                let pl = self.read_place(p, base, cwd)?;
                if pl != "read" {
                    all = false;
                }
                places.push(pl);
            }
            if all {
                return s("test_run");
            }
            let mut v = vec!["unknown".to_string()];
            v.extend(places);
            return Ok(worst(&v));
        }
        if head == "find" {
            let cls = find_class(args);
            if cls != "read" {
                return s(cls);
            }
            let mut places = vec![];
            for p in find_paths(args) {
                places.push(self.read_place(&p, base, cwd)?);
            }
            return Ok(worst(&places));
        }
        if let Some(spec) = make_flags(head) {
            let paths = match operands(args, &spec) {
                Some(p) if !p.is_empty() => p,
                _ => return s("unknown"),
            };
            let mut classes = vec![];
            for p in &paths {
                classes.push(self.write_class(p, root, cwd)?);
            }
            return Ok(worst(&classes));
        }
        if let Some(spec) = read_flags(head) {
            let mut ops = match operands(args, &spec) {
                None => return s("unknown"),
                Some(o) => o,
            };
            if has(NO_PATH_HEADS, head) || has(CWD_HEADS, head) {
                return s("read");
            }
            if ops.is_empty() && has(CWD_READERS, head) {
                ops = vec![".".into()];
            }
            let mut classes = vec!["read".to_string()];
            // *(_read_place(p, base, cwd) for p in ops) runs a generator frame.
            let _g = frame();
            for p in &ops {
                classes.push(self.read_place(p, base, cwd)?);
            }
            return Ok(worst(&classes));
        }
        s("unknown")
    }

    fn is_relative_word(&self, w: &str) -> R<bool> {
        let _f = frame();
        if w.is_empty() || w.starts_with('-') {
            return Ok(false);
        }
        Ok(!isabs(&self.expanduser(w)?))
    }

    /// `effects._next_cwd`; `None` is None.
    pub fn next_cwd(&self, tokens: &[String], cwd: Option<String>) -> R<Option<String>> {
        let _f = frame();
        if tokens.is_empty() || !has(CWD_HEADS, &tokens[0]) {
            return Ok(cwd);
        }
        let args = &tokens[1..];
        if tokens[0] == "popd" {
            return Ok(None);
        }
        let target = if tokens[0] == "cd" && args.is_empty() {
            self.expanduser("~")?
        } else if args.len() == 1 && !args[0].starts_with('-') {
            args[0].clone()
        } else {
            return Ok(None);
        };
        if target.replace('\\', "/").split('/').any(|seg| seg == "..") {
            return Ok(None);
        }
        let mut expanded = self.expanduser(&target)?;
        if !isabs(&expanded) {
            match &cwd {
                Some(c) if !c.is_empty() => expanded = join2(c, &expanded),
                _ => return Ok(None),
            }
        }
        let logical = normpath(&expanded);
        if self.realpath(&logical)? != logical {
            return Ok(None);
        }
        Ok(Some(logical))
    }

    /// `effects._shell_class`.
    fn shell_class(&self, command: &str, root: Option<Workspace>, cwd: Option<String>) -> R<String> {
        let _f = frame();
        if py_strip(command).is_empty() {
            return Ok("unknown".into());
        }
        {
            // shell_parser_available() runs parser_available(), which
            // decomposes "a && b" two frames down.
            let _g = frames(2);
            self.decompose("a && b")?;
        }
        let dec = match catch(self.decompose(command))? {
            Ok(d) => d,
            // A parse failure is unknown.
            Err(_) => return Ok("unknown".into()),
        };
        if !dec.ok || dec.commands.is_empty() {
            return Ok("unknown".into());
        }
        let mut token_lists = Vec::new();
        for c in &dec.commands {
            match shlex_split(c) {
                Ok(t) => token_lists.push(t),
                Err(_) => return Ok("unknown".into()),
            }
        }
        let base = root.as_ref().map(|r| r.base.clone());
        let moved_cwd = token_lists.iter().any(|t| !t.is_empty() && has(CWD_HEADS, &t[0]));
        let root = if moved_cwd { None } else { root };
        let mut classes: Vec<String> = Vec::new();
        let mut here = root.clone();
        let mut here_is_root = true;
        let mut read_base = base.clone();
        let mut cwd = cwd;
        let mut cwds: Vec<Option<String>> = vec![cwd.clone()];
        let mut lost = false;
        for (tokens, text) in token_lists.iter().zip(dec.commands.iter()) {
            if unsafe_words(text) {
                classes.push("unknown".into());
            }
            // After a cd or popd the gate cannot follow, a relative word
            // could name anything.
            if lost {
                let _g = frame();
                for a in tail(tokens) {
                    if self.is_relative_word(a)? {
                        classes.push("unknown".into());
                        break;
                    }
                }
            }
            classes.push(self.command_class(tokens, here.as_ref(), cwd.as_deref(), read_base.as_deref(), base.as_deref())?);
            if moves_tree(tokens) {
                here = None;
                here_is_root = root.is_none();
                read_base = None;
            }
            cwd = self.next_cwd(tokens, cwd)?;
            if !cwds.contains(&cwd) && cwds.len() >= MAX_CWDS {
                // Past this many directories the line counts as moved to a
                // place the gate cannot follow.
                cwd = None;
                lost = true;
            }
            if !cwds.contains(&cwd) {
                cwds.push(cwd.clone());
            }
            if !tokens.is_empty() && has(CWD_HEADS, &tokens[0]) && cwd.is_none() {
                lost = true;
            }
        }
        // Redirects are not ordered against the commands, so any tree change
        // anywhere in the line takes the workspace from every redirect.
        let redirect_root = if here_is_root { root.clone() } else { None };
        let redirect_cwd = if moved_cwd { None } else { cwds[0].clone() };
        let all: Vec<&String> = dec.writes.iter().chain(dec.reads.iter()).collect();
        for p in &all {
            if unsafe_redirect(p) {
                classes.push("unknown".into());
            }
            {
                // any(_is_credential_path(p, c) for c in cwds)
                let _g = frame();
                for c in &cwds {
                    if self.is_credential_path(p, c.as_deref())? {
                        classes.push("credential".into());
                        break;
                    }
                }
            }
            if lost && self.is_relative_word(p)? {
                classes.push("unknown".into());
            }
        }
        {
            // classes.extend(_write_class(...) for p in dec.writes)
            let _g = frame();
            for p in &dec.writes {
                classes.push(self.write_class(p, redirect_root.as_ref(), redirect_cwd.as_deref())?);
            }
        }
        for p in &dec.reads {
            // classes.extend(_read_place(...) for c in cwds)
            let _g = frame();
            for c in &cwds {
                classes.push(self.read_place(p, read_base.as_deref(), c.as_deref())?);
            }
        }
        Ok(worst(&classes))
    }

    /// `effects.workspace_root`.
    pub fn workspace_root(&self, cwd: Option<&str>, fixed: Option<&str>) -> R<Option<Workspace>> {
        let _f = frame();
        let (c, f) = match (cwd, fixed) {
            (Some(c), Some(f)) if !c.is_empty() && !f.is_empty() && isabs(c) && isabs(f) => (c, f),
            _ => return Ok(None),
        };
        let (a, b) = (self.realpath(c)?, self.realpath(f)?);
        if commonpath(&[&a, &b]).as_deref() != Some(b.as_str()) {
            return Ok(None);
        }
        Ok(Some(Workspace { cwd: a, base: b }))
    }

    /// `effects.effect_class` for a normalized record.
    pub fn effect_class(&self, rec: &Record, root: Option<Workspace>, call_cwd: Option<String>) -> R<String> {
        // effect_class and _effect_class, each a frame.
        let _f = frames(2);
        let (here, base) = match &root {
            Some(r) => (Some(r.cwd.clone()), Some(r.base.clone())),
            None => (call_cwd, None),
        };
        match rec.step_type.as_str() {
            "file_read" => {
                if rec.path.is_empty() {
                    return Ok("unknown".into());
                }
                self.read_place(&rec.path, base.as_deref(), here.as_deref())
            }
            "file_write" => {
                if rec.path.is_empty() {
                    return Ok("unknown".into());
                }
                self.write_class(&rec.path, root.as_ref(), here.as_deref())
            }
            "network" => Ok(if rec.tool_name == "WebFetch" || rec.tool_name == "WebSearch" {
                "network_get"
            } else {
                "unknown"
            }
            .into()),
            "shell" => self.shell_class(&rec.command, root, here),
            _ => Ok("unknown".into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn v(xs: &[&str]) -> Vec<String> {
        xs.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn classes_of_common_commands() {
        assert_eq!(git_class(&v(&["push", "--force"])), "force_push");
        assert_eq!(git_class(&v(&["status", "-s"])), "read");
        assert_eq!(git_class(&v(&["commit", "-m", "x"])), "write_inside_workspace");
        assert_eq!(git_class(&v(&["commit", "--bogus"])), "unknown");
        assert_eq!(git_class(&v(&["checkout", "-b", "x"])), "write_inside_workspace");
        assert_eq!(git_class(&v(&["checkout", "x"])), "delete");
        assert!(is_test_run("uv", &v(&["run", "pytest", "-q"])));
        assert!(!is_test_run("uv", &v(&["run", "python"])));
        assert!(jq_env("env.HOME") && jq_env("$ENV") && !jq_env("$env") && !jq_env("myenv"));
        assert!(unsafe_words("echo *.py") && !unsafe_words("echo '*.py'") && unsafe_words("ls ~root"));
        assert_eq!(git_file_values(&v(&["commit", "-F", "msg"])), Some(v(&["msg"])));
        assert_eq!(git_file_values(&v(&["commit", "-F"])), None);
    }
}
