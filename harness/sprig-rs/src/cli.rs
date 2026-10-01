//! sprig's command line: one task in, one answer out. The answer goes to
//! stdout and everything else to stderr; --json for scripts; real exit
//! codes. The flags parse as Go's flag package parses them.

use crate::agent::{Agent, Model};
use crate::gate::{AllowAll, DaisugiGate, Executor, Gate, DEFAULT_GATE_CMD};
use crate::gostr::{fields, trim_space};
use crate::json::{encode, jmap, J};
use crate::session::{head_path, history_from_entries, new_id, SessionObserver, SessionWriter};
use std::io::{Read, Write};
use std::rc::Rc;
use std::time::Duration;

/// What `sprig --help` prints, as Go's flag package prints it.
pub const USAGE: &str = concat!(
    "usage: sprig [flags] \"<task>\"     (or:  echo \"<task>\" | sprig -)\n",
    "a minimal coding-agent harness \u{2014} the answer goes to stdout, logs to stderr.\n",
    "\n",
    "examples:\n",
    "  sprig \"add a test for parse_url\"\n",
    "  sprig --json \"list the TODOs\" | jq -r .answer\n",
    "\n",
    "flags:\n",
    "  -base-url string\n",
    "    \tthe server for the anthropic or openai wire (default: ANTHROPIC_BASE_URL, else https://api.anthropic.com; https://api.openai.com/v1 for openai)\n",
    "  -gate\n",
    "    \tverify each tool call against the envelope, fail-closed\n",
    "  -gate-cmd string\n",
    "    \tthe out-of-process envelope gate to call when --gate is set (default \"daisugi gate check --mode enforce\")\n",
    "  -json\n",
    "    \temit the result as JSON (for scripts and jq)\n",
    "  -key-fd int\n",
    "    \tread the provider key from this file descriptor (3 or more) and use no key from the environment (default -1)\n",
    "  -max-tokens int\n",
    "    \tstop before a model call once the run has used this many tokens (all four usage counts); -1 is no cap (default -1)\n",
    "  -max-turns int\n",
    "    \tgive up after this many model turns (default 20)\n",
    "  -mcp-config string\n",
    "    \tthe MCP servers whose tools the model may call, as a file or inline JSON {\"mcpServers\": {name: {command, args, env}}}; stdio servers only\n",
    "  -model string\n",
    "    \tthe model to ask (default: haiku on claude -p; SPRIG_MODEL, else claude-haiku-4-5, on the API)\n",
    "  -resume string\n",
    "    \tresume this session id from --session-dir: reopen its file, rebuild history from its head, and append\n",
    "  -scrub-env string\n",
    "    \ta comma list of more variables no tool, gate or backend process gets\n",
    "  -session string\n",
    "    \tsession id to write under --session-dir (default: a fresh one)\n",
    "  -session-dir string\n",
    "    \twrite the session tree (JSONL) here \u{2014} the same shape daisugi's gate writes on path D. Empty = off.\n",
    "  -spawn-keys-fd int\n",
    "    \tread the resolved agent profiles, with keys, from this file descriptor; the spawn tool passes them on (default -1)\n",
    "  -system-file string\n",
    "    \tuse the bytes of this file as the system prompt, in place of sprig's own\n",
    "  -tools string\n",
    "    \tthe tools the model may call, a comma list of read, write, edit, bash and spawn, or none (default \"read,write,edit,bash\")\n",
    "  -tree-cmd string\n",
    "    \tthe command the spawn tool runs to start a child (daisugi tree run); spawn is offered only with it\n",
    "  -version\n",
    "    \tprint the version and exit\n",
    "  -wire string\n",
    "    \tthe model wire: claude-code (claude -p), anthropic (the Messages API) or openai (chat completions); default anthropic when SPRIG_BACKEND=api, else claude-code\n",
);

/// What the command line sets on the backend.
#[derive(Debug, Clone, PartialEq, Default)]
pub struct ModelOptions {
    /// --model: the model to ask. Empty keeps the backend's default.
    pub model: String,
    /// The wall from --tools, in sprig's order: the only tools the backend
    /// offers the model.
    pub tools: Vec<String>,
    /// The provider key read from --key-fd; key_set says one was read.
    /// With key_set, the backend reads no key from the environment.
    pub key: Vec<u8>,
    pub key_set: bool,
    /// The model wire: claude-code, anthropic or openai, from --wire, else
    /// from SPRIG_BACKEND.
    pub wire: String,
    /// --base-url: the server for the anthropic or openai wire. Empty
    /// keeps the wire's default.
    pub base_url: String,
    /// --tools none: the backend offers the model no tool at all. `tools`
    /// is then empty.
    pub no_tools: bool,
    /// The bytes of --system-file; system_set says a file was read. With
    /// system_set they replace sprig's own system prompt.
    pub system: Vec<u8>,
    pub system_set: bool,
    /// The tools of the MCP servers sprig runs itself (the anthropic and
    /// openai wires).
    pub extra: Vec<crate::mcp_client::McpToolDef>,
    /// The --mcp-config JSON and the server names, for the claude-code
    /// wire, where claude runs the servers.
    pub mcp_config: Vec<u8>,
    pub mcp_servers: Vec<String>,
}

/// The wires --wire takes, in the order the help names them.
pub const WIRES: &[&str] = &["claude-code", "anthropic", "openai"];

/// The --json usage: the token counts of every assistant turn added up,
/// under the names the Messages API and claude -p give them. A count no
/// turn reported (a zero sum) is left out.
pub fn usage_sum(history: &[crate::agent::Message]) -> J {
    let asst = || history.iter().filter(|m| m.role == "assistant");
    let sum = |f: fn(&crate::session::Usage) -> i64| {
        asst().fold(0i64, |n, m| crate::agent::sat_add(n, f(&m.usage)))
    };
    let pairs = [
        ("input_tokens", sum(|u| u.fresh)),
        ("output_tokens", sum(|u| u.out)),
        ("cache_read_input_tokens", sum(|u| u.cache_read)),
        ("cache_creation_input_tokens", sum(|u| u.cache_write)),
    ];
    jmap(
        pairs
            .into_iter()
            .filter(|(_, n)| *n != 0)
            .map(|(k, n)| (k, J::Int(n)))
            .collect(),
    )
}

/// The parsed flags.
#[derive(Debug, PartialEq)]
pub struct Flags {
    pub version: bool,
    pub json: bool,
    pub gate: bool,
    pub max_turns: i64,
    pub max_tokens: i64,
    pub gate_cmd: Vec<u8>,
    pub session_dir: Vec<u8>,
    pub session: Vec<u8>,
    pub resume: Vec<u8>,
    pub tools: Vec<u8>,
    pub system_file: Vec<u8>,
    pub model: Vec<u8>,
    pub key_fd: i64,
    pub scrub_env: Vec<u8>,
    pub wire: Vec<u8>,
    pub base_url: Vec<u8>,
    pub tree_cmd: Vec<u8>,
    pub spawn_keys_fd: i64,
    pub mcp_config: Vec<u8>,
    /// The arguments after the flags.
    pub args: Vec<Vec<u8>>,
}

/// strconv's underscoreOK: underscores only between digits.
fn underscore_ok(s: &[u8]) -> bool {
    let lower = |c: u8| c | 0x20;
    let mut saw = b'^';
    let mut i = 0;
    let mut hex = false;
    if s.len() >= 2 && s[0] == b'0' && matches!(lower(s[1]), b'b' | b'o' | b'x') {
        i = 2;
        saw = b'0';
        hex = lower(s[1]) == b'x';
    }
    while i < s.len() {
        let c = s[i];
        i += 1;
        if c.is_ascii_digit() || hex && (b'a'..=b'f').contains(&lower(c)) {
            saw = b'0';
            continue;
        }
        if c == b'_' {
            if saw != b'0' {
                return false;
            }
            saw = b'_';
            continue;
        }
        if saw == b'_' {
            return false;
        }
        saw = b'!';
    }
    saw != b'_'
}

/// Go's strconv.ParseInt(s, 0, 64), its error as flag words it.
pub fn parse_int(s: &[u8]) -> Result<i64, &'static str> {
    const SYNTAX: &str = "parse error";
    const RANGE: &str = "value out of range";
    if s.is_empty() {
        return Err(SYNTAX);
    }
    let (neg, body) = match s[0] {
        b'+' => (false, &s[1..]),
        b'-' => (true, &s[1..]),
        _ => (false, s),
    };
    if body.is_empty() {
        return Err(SYNTAX);
    }
    let lower = |c: u8| c | 0x20;
    let (base, digits): (u64, &[u8]) = if body[0] == b'0' {
        if body.len() >= 3 && lower(body[1]) == b'b' {
            (2, &body[2..])
        } else if body.len() >= 3 && lower(body[1]) == b'o' {
            (8, &body[2..])
        } else if body.len() >= 3 && lower(body[1]) == b'x' {
            (16, &body[2..])
        } else {
            (8, &body[1..])
        }
    } else {
        (10, body)
    };
    let cutoff = u64::MAX / base + 1;
    let mut n: u64 = 0;
    let mut underscores = false;
    for &c in digits {
        let d = match c {
            b'_' => {
                underscores = true;
                continue;
            }
            b'0'..=b'9' => c - b'0',
            _ if lower(c).is_ascii_lowercase() => lower(c) - b'a' + 10,
            _ => return Err(SYNTAX),
        } as u64;
        if d >= base {
            return Err(SYNTAX);
        }
        if n >= cutoff {
            return Err(RANGE);
        }
        n *= base;
        let n1 = n.wrapping_add(d);
        if n1 < n {
            return Err(RANGE);
        }
        n = n1;
    }
    if underscores && !underscore_ok(body) {
        return Err(SYNTAX);
    }
    let cut = 1u64 << 63;
    if !neg && n >= cut {
        return Err(RANGE);
    }
    if neg && n > cut {
        return Err(RANGE);
    }
    Ok(if neg {
        (n as i64).wrapping_neg()
    } else {
        n as i64
    })
}

/// Go's strconv.ParseBool.
pub fn parse_bool(s: &[u8]) -> Option<bool> {
    match s {
        b"1" | b"t" | b"T" | b"TRUE" | b"true" | b"True" => Some(true),
        b"0" | b"f" | b"F" | b"FALSE" | b"false" | b"False" => Some(false),
        _ => None,
    }
}

/// Go's FlagSet.Parse with sprig's flags. Err holds what Go prints before
/// the usage (empty for -h and -help).
pub fn parse_flags(args: &[Vec<u8>]) -> Result<Flags, Vec<u8>> {
    let mut f = Flags {
        version: false,
        json: false,
        gate: false,
        max_turns: 20,
        max_tokens: -1,
        gate_cmd: DEFAULT_GATE_CMD.as_bytes().to_vec(),
        session_dir: Vec::new(),
        session: Vec::new(),
        resume: Vec::new(),
        tools: crate::tools::ALL_TOOL_NAMES.join(",").into_bytes(),
        system_file: Vec::new(),
        model: Vec::new(),
        key_fd: -1,
        scrub_env: Vec::new(),
        wire: Vec::new(),
        base_url: Vec::new(),
        tree_cmd: Vec::new(),
        spawn_keys_fd: -1,
        mcp_config: Vec::new(),
        args: Vec::new(),
    };
    let mut i = 0;
    while i < args.len() {
        let s = &args[i];
        if s.len() < 2 || s[0] != b'-' {
            break;
        }
        let mut minuses = 1;
        if s[1] == b'-' {
            minuses = 2;
            if s.len() == 2 {
                i += 1;
                break;
            }
        }
        let mut name: &[u8] = &s[minuses..];
        if name.is_empty() || name[0] == b'-' || name[0] == b'=' {
            return Err([b"bad flag syntax: ".as_slice(), s].concat());
        }
        i += 1;
        let mut value: Option<&[u8]> = None;
        if let Some(eq) = name[1..].iter().position(|&c| c == b'=') {
            value = Some(&name[eq + 2..]);
            name = &name[..eq + 1];
        }
        let dashed = [b"-".as_slice(), name].concat();
        let is_bool = matches!(name, b"version" | b"json" | b"gate");
        let is_known = is_bool
            || matches!(
                name,
                b"max-turns"
                    | b"max-tokens"
                    | b"gate-cmd"
                    | b"session-dir"
                    | b"session"
                    | b"resume"
                    | b"tools"
                    | b"system-file"
                    | b"model"
                    | b"key-fd"
                    | b"scrub-env"
                    | b"wire"
                    | b"base-url"
                    | b"tree-cmd"
                    | b"spawn-keys-fd"
                    | b"mcp-config"
            );
        if !is_known {
            if name == b"help" || name == b"h" {
                return Err(Vec::new());
            }
            return Err([b"flag provided but not defined: ".as_slice(), &dashed].concat());
        }
        if is_bool {
            let v = match value {
                Some(v) => match parse_bool(v) {
                    Some(b) => b,
                    None => {
                        return Err(format!(
                            "invalid boolean value {} for {}: parse error",
                            crate::goerr::quote(v),
                            String::from_utf8_lossy(&dashed)
                        )
                        .into_bytes())
                    }
                },
                None => true,
            };
            match name {
                b"version" => f.version = v,
                b"json" => f.json = v,
                _ => f.gate = v,
            }
            continue;
        }
        let v: Vec<u8> = match value {
            Some(v) => v.to_vec(),
            None if i < args.len() => {
                i += 1;
                args[i - 1].clone()
            }
            None => return Err([b"flag needs an argument: ".as_slice(), &dashed].concat()),
        };
        let int = |v: &[u8]| -> Result<i64, Vec<u8>> {
            parse_int(v).map_err(|why| {
                [
                    format!("invalid value {} for flag ", crate::goerr::quote(v)).as_bytes(),
                    &dashed,
                    format!(": {why}").as_bytes(),
                ]
                .concat()
            })
        };
        match name {
            b"max-turns" => f.max_turns = int(&v)?,
            b"max-tokens" => f.max_tokens = int(&v)?,
            b"key-fd" => f.key_fd = int(&v)?,
            b"scrub-env" => f.scrub_env = v,
            b"wire" => f.wire = v,
            b"base-url" => f.base_url = v,
            b"tree-cmd" => f.tree_cmd = v,
            b"spawn-keys-fd" => f.spawn_keys_fd = int(&v)?,
            b"mcp-config" => f.mcp_config = v,
            b"gate-cmd" => f.gate_cmd = v,
            b"session-dir" => f.session_dir = v,
            b"session" => f.session = v,
            b"tools" => f.tools = v,
            b"system-file" => f.system_file = v,
            b"model" => f.model = v,
            _ => f.resume = v,
        }
    }
    f.args = args[i..].to_vec();
    Ok(f)
}

/// Runs one task and returns the exit code.
pub fn run(
    args: &[Vec<u8>],
    stdin: &mut dyn Read,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
    version: &str,
    new_model: &dyn Fn(&ModelOptions) -> Result<Box<dyn Model>, Vec<u8>>,
) -> i32 {
    // Nothing a tool started outlives sprig, however it ends (MX-R-8).
    crate::procgroup::on_signal_kill_all();
    crate::procgroup::begin();
    let code = run_inner(args, stdin, stdout, stderr, version, new_model);
    crate::procgroup::kill_own();
    code
}

fn run_inner(
    args: &[Vec<u8>],
    stdin: &mut dyn Read,
    stdout: &mut dyn Write,
    stderr: &mut dyn Write,
    version: &str,
    new_model: &dyn Fn(&ModelOptions) -> Result<Box<dyn Model>, Vec<u8>>,
) -> i32 {
    let f = match parse_flags(args) {
        Ok(f) => f,
        Err(msg) => {
            if !msg.is_empty() {
                let _ = stderr.write_all(&[msg.as_slice(), b"\n"].concat());
            }
            let _ = stderr.write_all(USAGE.as_bytes());
            return 2;
        }
    };
    let say = |w: &mut dyn Write, parts: &[&[u8]]| {
        let _ = w.write_all(&parts.concat());
    };
    if f.version {
        say(stdout, &[version.as_bytes(), b"\n"]);
        return 0;
    }
    let (mut key, mut key_set) = (Vec::new(), false);
    if f.key_fd >= 0 {
        match crate::env::read_key_fd("--key-fd", f.key_fd) {
            Ok(k) if k.is_empty() => {
                say(
                    stderr,
                    &[format!("sprig: --key-fd {} held no key\n", f.key_fd).as_bytes()],
                );
                return 2;
            }
            Ok(k) if crate::env::key_has_control(&k) => {
                say(
                    stderr,
                    &[format!(
                        "sprig: --key-fd {} held a key with a control character\n",
                        f.key_fd
                    )
                    .as_bytes()],
                );
                return 2;
            }
            Ok(k) => (key, key_set) = (k, true),
            Err(e) => {
                say(stderr, &[b"sprig: ", &e, b"\n"]);
                return 2;
            }
        }
    }
    let mut spawn_keys = None;
    if f.spawn_keys_fd >= 0 {
        match crate::env::read_key_fd("--spawn-keys-fd", f.spawn_keys_fd) {
            Ok(k) => spawn_keys = Some(k),
            Err(e) => {
                say(stderr, &[b"sprig: ", &e, b"\n"]);
                return 2;
            }
        }
    }
    // No process sprig starts inherits a descriptor above 2, and none gets
    // a key variable or a --scrub-env name.
    crate::env::close_inherited_fds();
    let names: Vec<Vec<u8>> = f
        .scrub_env
        .split(|&c| c == b',')
        .map(|n| n.to_vec())
        .collect();
    crate::env::set_scrub_env(&names);
    let no_tools = f.tools == b"none";
    let wall = if no_tools {
        Vec::new()
    } else {
        match crate::tools::parse_tool_wall(&f.tools) {
            Ok(w) => w,
            Err(e) => {
                say(stderr, &[b"sprig: ", &e, b"\n"]);
                return 2;
            }
        }
    };
    let (mut system, mut system_set) = (Vec::new(), false);
    if !f.system_file.is_empty() {
        match crate::osx::read_file(&f.system_file) {
            Ok(b) => (system, system_set) = (b, true),
            Err(e) => {
                say(
                    stderr,
                    &[b"sprig: --system-file: cannot read ", &f.system_file, b": ", &e, b"\n"],
                );
                return 2;
            }
        }
    }
    let (mut mcp_specs, mut mcp_json) = (Vec::new(), Vec::new());
    if !f.mcp_config.is_empty() {
        match crate::mcp_client::parse_mcp_config(&f.mcp_config) {
            Ok((specs, json)) => (mcp_specs, mcp_json) = (specs, json),
            Err(e) => {
                say(stderr, &[b"sprig: ", &e, b"\n"]);
                return 2;
            }
        }
    }
    let spawn_on = wall.iter().any(|t| t == "spawn");
    if spawn_on {
        if fields(&f.tree_cmd).is_empty() {
            say(stderr, &[b"sprig: --tools: spawn needs --tree-cmd\n"]);
            return 2;
        }
        if f.session.is_empty() && f.resume.is_empty() {
            say(
                stderr,
                &[b"sprig: --tools: spawn needs --session, the parent of each child\n"],
            );
            return 2;
        }
    }
    let mut wire = f.wire.clone();
    if wire.is_empty() {
        wire = if std::env::var_os("SPRIG_BACKEND").is_some_and(|v| v == "api") {
            b"anthropic".to_vec()
        } else {
            b"claude-code".to_vec()
        };
    }
    if !WIRES.iter().any(|w| w.as_bytes() == wire) {
        say(
            stderr,
            &[format!(
                "sprig: --wire: unknown wire {}; the wires are {}\n",
                crate::goerr::quote(&wire),
                WIRES.join(", ")
            )
            .as_bytes()],
        );
        return 2;
    }
    if !f.base_url.is_empty() && wire == b"claude-code" {
        say(
            stderr,
            &[b"sprig: --base-url needs --wire anthropic or --wire openai\n"],
        );
        return 2;
    }
    if !f.resume.is_empty() && f.session_dir.is_empty() {
        say(
            stderr,
            &[b"sprig: --resume needs --session-dir too (which session store to resume from)\n"],
        );
        return 1;
    }
    let joined = f.args.join(b" ".as_slice());
    let mut task = trim_space(&joined).to_vec();
    if task.is_empty() || task == b"-" {
        let mut b = Vec::new();
        let _ = stdin.read_to_end(&mut b);
        task = trim_space(&b).to_vec();
    }
    if task.is_empty() {
        say(
            stderr,
            &["sprig: no task given \u{2014} pass it as an argument or on stdin (sprig -).\ntry: sprig --help\n".as_bytes()],
        );
        return 1;
    }
    let mut opts = ModelOptions {
        model: String::from_utf8_lossy(&f.model).into_owned(),
        tools: wall.clone(),
        key: key.clone(),
        key_set,
        wire: String::from_utf8_lossy(&wire).into_owned(),
        base_url: String::from_utf8_lossy(&f.base_url).into_owned(),
        no_tools,
        system,
        system_set,
        ..Default::default()
    };
    let mut mcp = None;
    if !mcp_specs.is_empty() {
        if wire == b"claude-code" {
            // One decision: claude runs the servers, from the same JSON.
            opts.mcp_config = mcp_json;
            opts.mcp_servers = mcp_specs.iter().map(|s| s.name.clone()).collect();
        } else {
            match crate::mcp_client::McpSet::start(&mcp_specs, stderr) {
                Ok(set) => {
                    opts.extra = set.defs.clone();
                    mcp = Some(set);
                }
                Err(e) => {
                    say(stderr, &[b"sprig: ", &crate::env::redact(e.as_bytes(), &key), b"\n"]);
                    return 1;
                }
            }
        }
    }
    let model = match new_model(&opts) {
        Ok(m) => m,
        Err(e) => {
            say(stderr, &[b"sprig: ", &crate::env::redact(&e, &key), b"\n"]);
            return 1;
        }
    };
    // The key the backend holds, from --key-fd or its environment, never
    // reaches an error line.
    let key = if key.is_empty() {
        model.held_key()
    } else {
        key
    };
    let model: Box<dyn Model> = if key.is_empty() {
        model
    } else {
        Box::new(crate::env::Redacting { inner: model, key: key.clone() })
    };
    let mut writer: Option<Rc<SessionWriter>> = None;
    let mut seed = Vec::new();
    let mut sid = Vec::new();
    if !f.session_dir.is_empty() {
        sid = if !f.resume.is_empty() {
            f.resume.clone()
        } else if !f.session.is_empty() {
            f.session.clone()
        } else {
            (new_id() + &new_id()).into_bytes()
        };
        let opened = if !f.resume.is_empty() {
            SessionWriter::open(&f.session_dir, &sid)
        } else {
            let cwd = crate::osx::getwd().unwrap_or_default();
            SessionWriter::create(&f.session_dir, &sid, &cwd)
        };
        let w = match opened {
            Ok(w) => w,
            Err(e) => {
                say(stderr, &[b"sprig: session tree: ", &e, b"\n"]);
                return 1;
            }
        };
        if !f.resume.is_empty() {
            match head_path(w.path()) {
                Ok(entries) => seed = history_from_entries(&entries),
                Err(e) => {
                    say(
                        stderr,
                        &[b"sprig: session tree: rebuilding history: ", &e, b"\n"],
                    );
                    return 1;
                }
            }
        }
        say(
            stderr,
            &[b"sprig: session ", &sid, b" at ", w.path(), b"\n"],
        );
        writer = Some(Rc::new(w));
    }
    let gate: Box<dyn Gate> = if f.gate {
        Box::new(DaisugiGate {
            command: fields(&f.gate_cmd),
            timeout: Duration::from_secs(10),
            session_id: sid,
        })
    } else {
        Box::new(AllowAll)
    };
    let obs: Option<Rc<dyn SessionObserver>> = writer.map(|w| w as Rc<dyn SessionObserver>);
    let mut tool_map = crate::tools::tools_for(&wall);
    if let Some(set) = &mcp {
        for (name, tool) in set.tools() {
            tool_map.insert(name, tool);
        }
    }
    let own_use = Rc::new(std::cell::Cell::new((0i64, 0i64)));
    let branch = Rc::new(std::cell::Cell::new(0i64));
    if spawn_on {
        let parent = if f.resume.is_empty() {
            f.session.clone()
        } else {
            f.resume.clone()
        };
        tool_map.insert(
            "spawn".into(),
            Box::new(crate::spawn::SpawnTool {
                command: fields(&f.tree_cmd),
                session: parent,
                keys: spawn_keys,
                own_use: own_use.clone(),
                default_tools: crate::spawn::host_tools(&wall),
                branch: branch.clone(),
            }),
        );
    }
    let mut exec = Executor::new(tool_map, gate);
    exec.session = obs.clone();
    let model_name = model.model_name();
    let mut agent = Agent::new(model, exec, f.max_turns);
    agent.session = obs;
    agent.seed = seed;
    agent.max_tokens = f.max_tokens;
    agent.cap_tokens = f.max_tokens >= 0;
    agent.no_tools = no_tools;
    if spawn_on {
        let b = branch.clone();
        agent.extra_tokens = Some(Box::new(move || b.get()));
        agent.own_use = Some(own_use);
    }
    let answer = match agent.run(&task) {
        Ok(a) => a,
        Err(e) => {
            say(stderr, &[b"sprig: ", &crate::env::redact(&e, &key), b"\n"]);
            return 1;
        }
    };
    if f.json {
        let j = jmap(vec![
            ("answer", J::Str(answer)),
            ("turns", J::Int(agent.history.len() as i64)),
            ("model_turns", J::Int(agent.model_turns())),
            ("model", J::s(&model_name)),
            ("usage", usage_sum(&agent.history)),
        ]);
        say(stdout, &[&encode(&j), b"\n"]);
    } else {
        say(stdout, &[&answer, b"\n"]);
    }
    0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(xs: &[&str]) -> Vec<Vec<u8>> {
        xs.iter().map(|s| s.as_bytes().to_vec()).collect()
    }

    #[test]
    fn ints_and_bools() {
        assert_eq!(parse_int(b"0x10"), Ok(16));
        assert_eq!(parse_int(b"1_0"), Ok(10));
        assert_eq!(parse_int(b"010"), Ok(8));
        assert_eq!(parse_int(b"-0b11"), Ok(-3));
        assert_eq!(parse_int(b"+7"), Ok(7));
        assert_eq!(parse_int(b"0"), Ok(0));
        assert_eq!(parse_int(b"_1"), Err("parse error"));
        assert_eq!(parse_int(b"1__0"), Err("parse error"));
        assert_eq!(parse_int(b"08"), Err("parse error"));
        assert_eq!(parse_int(b"x"), Err("parse error"));
        assert_eq!(parse_int(b""), Err("parse error"));
        assert_eq!(parse_int(b"9223372036854775807"), Ok(i64::MAX));
        assert_eq!(parse_int(b"9223372036854775808"), Err("value out of range"));
        assert_eq!(parse_int(b"-9223372036854775808"), Ok(i64::MIN));
        assert_eq!(
            parse_int(b"99999999999999999999x"),
            Err("value out of range")
        );
        assert_eq!(parse_bool(b"T"), Some(true));
        assert_eq!(parse_bool(b"False"), Some(false));
        assert_eq!(parse_bool(b"yes"), None);
    }

    #[test]
    fn flags_follow_go() {
        let f = parse_flags(&a(&[
            "-json",
            "--max-turns=3",
            "--gate-cmd",
            "x y",
            "task",
            "--gate",
        ]))
        .unwrap();
        assert!(f.json && !f.gate);
        assert_eq!((f.max_turns, f.gate_cmd.as_slice()), (3, b"x y".as_slice()));
        assert_eq!(f.args, a(&["task", "--gate"]));
        let f = parse_flags(&a(&["--", "--version"])).unwrap();
        assert_eq!(f.args, a(&["--version"]));
        let f = parse_flags(&a(&["-", "x"])).unwrap();
        assert_eq!(f.args, a(&["-", "x"]));
        assert_eq!(
            parse_flags(&a(&["--nope"])).unwrap_err(),
            b"flag provided but not defined: -nope"
        );
        assert_eq!(
            parse_flags(&a(&["---x"])).unwrap_err(),
            b"bad flag syntax: ---x"
        );
        assert_eq!(
            parse_flags(&a(&["-=x"])).unwrap_err(),
            b"bad flag syntax: -=x"
        );
        assert_eq!(
            parse_flags(&a(&["--max-turns"])).unwrap_err(),
            b"flag needs an argument: -max-turns"
        );
        assert_eq!(
            parse_flags(&a(&["--json=maybe"])).unwrap_err(),
            b"invalid boolean value \"maybe\" for -json: parse error"
        );
        assert_eq!(
            parse_flags(&a(&["--max-turns", "x"])).unwrap_err(),
            b"invalid value \"x\" for flag -max-turns: parse error"
        );
        assert_eq!(parse_flags(&a(&["-help"])).unwrap_err(), b"");
        assert_eq!(parse_flags(&a(&["-max-turns=-1"])).unwrap().max_turns, -1);
    }

    struct Fixed;
    impl Model for Fixed {
        fn next(&mut self, _: &[crate::agent::Message]) -> Result<crate::agent::Message, Vec<u8>> {
            Ok(crate::agent::Message {
                role: "assistant",
                text: b"hi <x>".to_vec(),
                ..Default::default()
            })
        }
    }

    /// A model that calls a tool named after the key, as one that echoes its
    /// request would.
    struct CallsKey;
    impl Model for CallsKey {
        fn next(&mut self, _: &[crate::agent::Message]) -> Result<crate::agent::Message, Vec<u8>> {
            Ok(crate::agent::Message {
                role: "assistant",
                calls: vec![crate::gate::ToolCall {
                    id: "1".into(),
                    name: "sk-SENTINEL/run".into(),
                    input: None,
                }],
                ..Default::default()
            })
        }
    }

    #[test]
    fn the_run_failure_line_hides_the_key() {
        use std::io::Write as _;
        use std::os::fd::IntoRawFd;
        let (r, mut w) = std::io::pipe().unwrap();
        w.write_all(b"sk-SENTINEL/run\n").unwrap();
        drop(w);
        let fd = r.into_raw_fd();
        let (mut o, mut e) = (Vec::new(), Vec::new());
        let nm = |_: &ModelOptions| -> Result<Box<dyn Model>, Vec<u8>> { Ok(Box::new(CallsKey)) };
        let args = a(&["--key-fd", &fd.to_string(), "--tools", "none", "task"]);
        let code = run(&args, &mut "".as_bytes(), &mut o, &mut e, "v1", &nm);
        let err = String::from_utf8_lossy(&e).to_string();
        assert!(code == 1 && !err.contains("SENTINEL") && err.contains("[key]"), "{code} {err}");
    }

    fn go(args: &[&str], stdin: &str) -> (i32, String, String) {
        let (mut o, mut e) = (Vec::new(), Vec::new());
        let nm = |_: &ModelOptions| -> Result<Box<dyn Model>, Vec<u8>> { Ok(Box::new(Fixed)) };
        let code = run(&a(args), &mut stdin.as_bytes(), &mut o, &mut e, "v1", &nm);
        (
            code,
            String::from_utf8(o).unwrap(),
            String::from_utf8(e).unwrap(),
        )
    }

    #[test]
    fn runs_follow_go() {
        assert_eq!(go(&["--version"], ""), (0, "v1\n".into(), "".into()));
        let (c, o, e) = go(&["-h"], "");
        assert_eq!((c, o.as_str(), e.as_str()), (2, "", USAGE));
        let (c, _, e) = go(&["--nope"], "");
        assert_eq!(
            (c, e),
            (2, format!("flag provided but not defined: -nope\n{USAGE}"))
        );
        assert_eq!(
            go(&[], " \n"),
            (1, "".into(), "sprig: no task given \u{2014} pass it as an argument or on stdin (sprig -).\ntry: sprig --help\n".into())
        );
        assert_eq!(
            go(&["--resume", "r", "t"], "").2,
            "sprig: --resume needs --session-dir too (which session store to resume from)\n"
        );
        assert_eq!(go(&["say", "hi"], ""), (0, "hi <x>\n".into(), "".into()));
        assert_eq!(
            go(&["--json", "-"], "task"),
            (
                0,
                "{\"answer\":\"hi \\u003cx\\u003e\",\"model\":\"\",\"model_turns\":1,\"turns\":2,\"usage\":{}}\n"
                    .into(),
                "".into()
            )
        );
        assert_eq!(
            go(&["--tools", "", "x"], ""),
            (
                2,
                "".into(),
                "sprig: --tools is empty; name one or more of read, write, edit, bash, spawn\n"
                    .into()
            )
        );
        assert_eq!(
            go(&["--tools=read,x", "x"], "").2,
            "sprig: --tools: unknown tool \"x\"; the tools are read, write, edit, bash, spawn\n"
        );
    }

    struct Scripted(Vec<crate::agent::Message>, String);
    impl Model for Scripted {
        fn next(&mut self, _: &[crate::agent::Message]) -> Result<crate::agent::Message, Vec<u8>> {
            Ok(self.0.remove(0))
        }
        fn model_name(&self) -> String {
            self.1.clone()
        }
    }

    #[test]
    fn json_usage_and_options_follow_go() {
        use crate::agent::Message;
        use crate::session::Usage;
        let got = std::cell::RefCell::new(ModelOptions::default());
        let nm = |o: &ModelOptions| -> Result<Box<dyn Model>, Vec<u8>> {
            *got.borrow_mut() = o.clone();
            let mut call = Message {
                role: "assistant",
                usage: Usage {
                    fresh: 1,
                    cache_read: 2,
                    out: 3,
                    ..Default::default()
                },
                ..Default::default()
            };
            call.calls.push(crate::gate::ToolCall {
                name: "bash".into(),
                ..Default::default()
            });
            let done = Message {
                role: "assistant",
                text: b"done".to_vec(),
                usage: Usage {
                    fresh: 10,
                    cache_read: 20,
                    out: 30,
                    ..Default::default()
                },
                ..Default::default()
            };
            Ok(Box::new(Scripted(vec![call, done], "m-1".into())))
        };
        let (mut o, mut e) = (Vec::new(), Vec::new());
        let args = a(&["--json", "--tools", "bash,read", "--model", "sonnet", "x"]);
        let code = run(&args, &mut b"".as_slice(), &mut o, &mut e, "v1", &nm);
        assert_eq!(code, 0);
        assert_eq!(
            String::from_utf8(o).unwrap(),
            "{\"answer\":\"done\",\"model\":\"m-1\",\"model_turns\":2,\"turns\":4,\"usage\":{\"cache_read_input_tokens\":22,\"input_tokens\":11,\"output_tokens\":33}}\n"
        );
        assert_eq!(
            *got.borrow(),
            ModelOptions {
                model: "sonnet".into(),
                tools: vec!["read".into(), "bash".into()],
                wire: "claude-code".into(),
                ..Default::default()
            }
        );
    }
}
