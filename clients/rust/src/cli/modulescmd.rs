//! `daisugi modules` (`opendaisugi.modules`): the module map, and the
//! stages `daisugi dashboard` shares with it. The Go client's
//! `modulescmd.go` is the reference; rulings K4-1, K4-2 and K4-4 say where
//! the map differs from the oracle's.

use std::collections::HashMap;
use std::os::unix::fs::{FileTypeExt, MetadataExt};

use super::config::{self, ConfigErr, RowsErr};
use super::configcmd::JSON_OPT;
use super::gateroot::{join, parent, path_str};
use super::statuscmd::{matcher_threshold, DATA_DIR_OPT};
use super::{parse_args, Env, Res, Stop};
use crate::gate::py::text::{len as py_len, repr, strip};
use crate::gate::pyjson::{dumps_indent, loads, LoadError, Object, Value};
use crate::pathways::store::{Col, Store};
use crate::tracejournal::Journal;
use num_bigint::BigInt;

pub(super) const ACTIVE: &str = "active";
pub(super) const AVAILABLE: &str = "available";
pub(super) const POSSIBLE: &str = "possible";

/// `modules.Module`.
#[derive(Clone)]
pub(super) struct Module {
    pub name: String,
    pub state: &'static str,
    pub note: String,
}

fn m(name: impl Into<String>, state: &'static str, note: impl Into<String>) -> Module {
    Module {
        name: name.into(),
        state,
        note: note.into(),
    }
}

/// `modules.Stage`.
pub(super) struct Stage {
    pub key: &'static str,
    pub title: &'static str,
    pub role: &'static str,
    pub modules: Vec<Module>,
}

/// The glyphs of `console.BOX` and `console.ASCII_BOX` the views use.
pub(super) struct Glyphs {
    pub h: &'static str,
    pub v: &'static str,
    pub tl: &'static str,
    pub tr: &'static str,
    pub bl: &'static str,
    pub br: &'static str,
    pub on: &'static str,
    pub avail: &'static str,
    pub off: &'static str,
    pub flow: &'static str,
    pub down: &'static str,
}

pub(super) const BOX: Glyphs = Glyphs {
    h: "─",
    v: "│",
    tl: "┌",
    tr: "┐",
    bl: "└",
    br: "┘",
    on: "●",
    avail: "○",
    off: "·",
    flow: "▸",
    down: "▼",
};
pub(super) const ASCII_BOX: Glyphs = Glyphs {
    h: "-",
    v: "|",
    tl: "+",
    tr: "+",
    bl: "+",
    br: "+",
    on: "*",
    avail: "o",
    off: ".",
    flow: ">",
    down: "v",
};

impl Glyphs {
    fn state(&self, s: &str) -> &'static str {
        match s {
            ACTIVE => self.on,
            AVAILABLE => self.avail,
            _ => self.off,
        }
    }
}

/// `swap.STAGE_EFFECT`: how a choice at each stage takes effect.
fn stage_effect(key: &str) -> &'static str {
    match key {
        "harness" | "gate" | "envelope" | "floor_report" | "voice_engine" => "cfg",
        "verifier" | "shell" | "backend" | "matcher" | "router" | "distill" | "floor_backend" => {
            "live"
        }
        "stores" => "planned",
        _ => "",
    }
}

/// The extras this binary carries, answered as a base install of the
/// oracle with only them would answer (K4-1): potion and the bash grammar
/// are in it; the MiniLM and int8 matchers and the voice engines are not.
const CARRIES_MINILM: bool = false;
const CARRIES_POTION: bool = true;
const CARRIES_INT8: bool = false;
const INT8_REASON: &str = "needs opendaisugi[int8]";

const OPENCODE_GATE_NOTE: &str = "in-process deny-only hook; fail-closed on every call. apply_patch is \
    checked path by path. Writes to OpenCode's config, to .opencode plugin and tool directories and to any \
    opencode.json are hard-denied. Every ask is permanent: the gate has no fixed project root for OpenCode. A \
    coppice pane will not start without the plugin or with --pure. Outside coppice, OpenCode runs ungated if the \
    plugin fails to load or with --pure. Plugins under OPENCODE_CONFIG_DIR are not guarded";

const HOOK_SHAPE: &str =
    "~/.claude/settings.json holds hooks in a shape the oracle raises on, which this binary does not read yet";

/// What `modules._claude_hook_installed` reads from
/// ~/.claude/settings.json: the command of every hook, by event.
#[derive(Default)]
struct HookSettings {
    cmds: HashMap<&'static str, Vec<String>>,
}

impl HookSettings {
    /// `_claude_hook_installed(kind, event=event)`.
    fn installed(&self, kind: &str, event: &str) -> bool {
        self.cmds
            .get(event)
            .is_some_and(|cs| cs.iter().any(|c| c.contains(kind)))
    }
}

/// Reads ~/.claude/settings.json as the three checks the map makes read
/// it. A file that cannot be read or parsed holds no hook, as in Python;
/// a shape the oracle's `.get` or `in` raises on is an error (K4-4).
fn read_hook_settings(path: &str) -> Result<HookSettings, ()> {
    let Ok(raw) = std::fs::read(path) else {
        return Ok(HookSettings::default());
    };
    let Ok(text) = String::from_utf8(raw) else {
        return Ok(HookSettings::default());
    };
    let data = match loads(&text) {
        Ok(v) => v,
        Err(LoadError::Syntax(_)) => return Ok(HookSettings::default()),
        Err(_) => return Err(()),
    };
    let Value::Obj(data) = data else {
        return Err(());
    };
    let mut out = HookSettings::default();
    let Some(hv) = data.get("hooks") else {
        return Ok(out);
    };
    let Value::Obj(hooks) = hv else {
        return Err(());
    };
    for event in ["PreToolUse", "Stop", "Notification"] {
        let Some(ev) = hooks.get(event) else { continue };
        let Value::List(entries) = ev else {
            return Err(());
        };
        for en in entries {
            let Value::Obj(eo) = en else { return Err(()) };
            let Some(hl) = eo.get("hooks") else { continue };
            let Value::List(list) = hl else {
                return Err(());
            };
            for h in list {
                let Value::Obj(ho) = h else { return Err(()) };
                let cmd = match ho.get("command") {
                    None => String::new(),
                    Some(Value::Str(s)) => s.clone(),
                    Some(_) => return Err(()),
                };
                out.cmds.entry(event).or_default().push(cmd);
            }
        }
    }
    Ok(out)
}

fn exists(p: &str) -> bool {
    std::fs::metadata(p).is_ok()
}

fn is_dir(p: &str) -> bool {
    std::fs::metadata(p).is_ok_and(|m| m.is_dir())
}

/// `urllib.parse.urlparse(url).netloc`.
fn url_netloc(u: &str) -> String {
    let mut rest = u;
    if let Some(i) = u.find(':') {
        let scheme = &u[..i];
        let b = scheme.as_bytes();
        let ok = i > 0
            && b[0].is_ascii_alphabetic()
            && b[1..]
                .iter()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'+' | b'-' | b'.'));
        if ok {
            rest = &u[i + 1..];
        }
    }
    let Some(r) = rest.strip_prefix("//") else {
        return String::new();
    };
    match r.find(['/', '?', '#']) {
        Some(i) => r[..i].to_string(),
        None => r.to_string(),
    }
}

/// `modules._switchyard_module`.
fn switchyard_module(router: &str, present: bool) -> Module {
    let install = crate::switchyard::INSTALL_CMD;
    let selected = router == "switchyard";
    match (selected, present) {
        (true, true) => m(
            "NeMo Switchyard",
            ACTIVE,
            "selected; `daisugi gateway` starts it. The gateway row says if turns flow. See `daisugi router status`",
        ),
        (true, false) => m("NeMo Switchyard", POSSIBLE, format!("selected, but the binary is missing: {install}")),
        (false, true) => m(
            "NeMo Switchyard",
            AVAILABLE,
            "binary found: daisugi install --gateway --router switchyard --efficient-model <id>",
        ),
        (false, false) => m("NeMo Switchyard", POSSIBLE, format!("needs the binary: {install}")),
    }
}

/// `PathwayStore(db).stats()` read as int(count) and int(total_hits).
/// The hits are None when int() raises (a sum that is inf); `None` when
/// the store cannot be read at all.
pub(super) fn read_pathway_stats(db: &str) -> Option<(i64, Option<BigInt>)> {
    let s = Store::open(db).ok()?;
    let (c, h) = s.stats().ok()?;
    let hits = match h {
        Col::Int(x) => Some(BigInt::from(x)),
        Col::Real(x) if x.is_finite() => {
            use num_traits::FromPrimitive;
            BigInt::from_f64(x.trunc())
        }
        Col::Real(_) => None,
        _ => None,
    };
    Some((c, hits))
}

/// What the map reads before its first write, so that an input this
/// binary refuses changes nothing.
struct Pre {
    data_cfg: Result<config::Config, RowsErr>,
    hooks: HookSettings,
    roots: Vec<(String, String)>,
}

/// `load_config(path)`, with a top level that is no mapping told apart
/// as the AttributeError Python raises.
fn load_config(path: &str, home: &str) -> Result<config::Config, RowsErr> {
    config::rows(path, home).map(|r| r.2)
}

impl Env {
    /// `console.glyphs()`: box drawing on a terminal, ASCII when stdout is
    /// not one or under --plain.
    pub(super) fn glyphs(&self) -> &'static Glyphs {
        if self.plain || !stdout_tty() {
            &ASCII_BOX
        } else {
            &BOX
        }
    }

    /// `install.opencode_plugin_path`.
    fn opencode_plugin_path(&self) -> String {
        let base = match self.env.get("XDG_CONFIG_HOME") {
            Some(x) if x.starts_with('/') => path_str(x),
            _ => join(&self.home, ".config"),
        };
        join(&base, "opencode/plugins/daisugi-gate.ts")
    }

    /// `modules._coppice_socket_present`: a socket this user owns at the
    /// coppice server path, not followed through a link.
    fn coppice_socket_present(&self) -> bool {
        let base = match self.env.get("XDG_RUNTIME_DIR") {
            Some(rt) if !rt.is_empty() => join(&path_str(rt), "coppice"),
            _ => join(&self.data_home(), "coppice"),
        };
        match std::fs::symlink_metadata(format!("{base}/server.sock")) {
            // SAFETY: getuid has no preconditions.
            Ok(md) => md.file_type().is_socket() && md.uid() == unsafe { libc::getuid() },
            Err(_) => false,
        }
    }

    /// Reads, in the oracle's order, everything detect_stages reads before
    /// gather_status reads the stores. A refusal here changes nothing.
    fn wiring_read(&mut self, cmd: &str, data_dir: &str) -> Result<Pre, Stop> {
        // gather_status first asks the matcher's threshold, which loads the
        // config in the default data directory.
        let home = self.home.clone();
        let hcfg = match load_config(&join(&self.data_home(), "config.yaml"), &home) {
            Ok(c) => c,
            Err(e) => return Err(self.config_load_err(cmd, &e)),
        };
        if matcher_threshold(&hcfg.matcher_model).is_none() {
            self.errf(&format!(
                "matcher_model={} is not a built embedder.\n",
                repr(&hcfg.matcher_model)
            ));
            return Err(Stop::Exit(1));
        }
        let data_cfg = load_config(&join(data_dir, "config.yaml"), &home);
        if let Err(e @ RowsErr::Config(ConfigErr::Unsupported)) = &data_cfg {
            let why = format!("the config file is not one this binary reads: {e}");
            return Err(self.refuse(cmd, &why).unwrap_err());
        }
        let hooks = match read_hook_settings(&join(&home, ".claude/settings.json")) {
            Ok(h) => h,
            Err(()) => return Err(self.refuse(cmd, HOOK_SHAPE).unwrap_err()),
        };
        let roots = match self.transcript_roots() {
            Ok(r) => r,
            Err(why) => return Err(self.refuse(cmd, &why).unwrap_err()),
        };
        Ok(Pre {
            data_cfg,
            hooks,
            roots,
        })
    }

    /// `modules.detect_stages`: the wiring snapshot for `data_dir`.
    pub(super) fn detect_stages(&mut self, cmd: &str, data_dir: &str) -> Result<Vec<Stage>, Stop> {
        let pre = self.wiring_read(cmd, data_dir)?;
        let (journal_total, pathway_count) = store_counts(data_dir);
        let cfg = match pre.data_cfg {
            Ok(c) => c,
            Err(e) => return Err(self.config_load_err(cmd, &e)),
        };
        let env = self.env.clone();
        let which = |name: &str| super::install::look_path(&env, name).is_some();
        let present: Vec<&str> = pre
            .roots
            .iter()
            .filter(|(_, r)| exists(r))
            .map(|(h, _)| h.as_str())
            .collect();
        let pi_installed = exists(&join(
            &self.home,
            ".pi/agent/extensions/daisugi-gate/index.ts",
        ));
        let pi_present = is_dir(&join(&self.home, ".pi"));
        let oc_path = self.opencode_plugin_path();
        let oc_installed = exists(&oc_path);
        let oc_present = which("opencode") || is_dir(&parent(&parent(&oc_path)));
        let harness = |name: &str, hid: &str| {
            if present.contains(&hid) {
                m(name, ACTIVE, "transcripts found")
            } else {
                m(name, POSSIBLE, "not detected on this box")
            }
        };
        let backend = strip(
            self.env
                .get("OPENDAISUGI_LLM_BACKEND")
                .map(String::as_str)
                .unwrap_or(""),
        )
        .to_string();
        let gateway_on = self
            .env
            .get("OPENDAISUGI_GATEWAY_BASE_URL")
            .is_some_and(|v| !v.is_empty())
            || pre.hooks.installed("gateway", "PreToolUse");
        let matcher_sel = cfg.matcher_model.clone();
        let switchyard_row = switchyard_module(&cfg.gateway_router, which("switchyard-server"));
        let herdr_stop_on = pre.hooks.installed("--event stop", "Stop");
        let herdr_notif_on = pre.hooks.installed("--event notification", "Notification");
        let herdr_hooks_on = herdr_stop_on && herdr_notif_on;
        let herdr_hooks_partial = herdr_stop_on != herdr_notif_on;
        let herdr_binary = which("herdr");
        let coppice_socket = self.coppice_socket_present();
        let coppice_on_path = which("coppice");
        let tmux_on_path = which("tmux");
        let (herdr_state, herdr_note) = if herdr_hooks_on {
            (ACTIVE, "hooks installed; Herdr CLI contract unverified")
        } else if herdr_hooks_partial {
            (
                AVAILABLE,
                "one of two hooks installed; run `daisugi install --gate --report herdr`",
            )
        } else if herdr_binary {
            (AVAILABLE, "run `daisugi install --gate --report herdr`")
        } else {
            (POSSIBLE, "install Herdr first")
        };
        // effective_matcher: the oracle falls back to lexical for MiniLM and
        // int8 with the package absent; this binary does not (K4-2).
        let lexical_state = if matcher_sel == "lexical" {
            ACTIVE
        } else {
            AVAILABLE
        };
        let emb_state = |key: &str, installed: bool| {
            if matcher_sel == key && installed {
                ACTIVE
            } else if installed {
                AVAILABLE
            } else {
                POSSIBLE
            }
        };
        const LEXICAL_DESC: &str = "keyword floor — no model, no download";
        let mut named = backend.clone();
        if named.is_empty() {
            if let Some(b) = &cfg.llm_backend {
                named = strip(b).to_string();
            }
        }
        let has_key = self
            .env
            .get("ANTHROPIC_API_KEY")
            .is_some_and(|v| !v.is_empty())
            || self
                .env
                .get("ANTHROPIC_AUTH_TOKEN")
                .is_some_and(|v| !v.is_empty());
        let auto_pick = if !has_key && which("claude") {
            "claude-code"
        } else {
            "api"
        };
        let pick = |on: bool| if on { ACTIVE } else { AVAILABLE };
        let auto = if named.is_empty() {
            m("auto", ACTIVE, format!("pick what runs here: {auto_pick}"))
        } else {
            m("auto", AVAILABLE, "pick what runs here")
        };
        let mut backend_modules = vec![
            auto,
            m("claude-code", pick(named == "claude-code"), "no API key"),
            m(
                "anthropic-api",
                pick(named == "api" || named == "anthropic"),
                "BYOK",
            ),
            m(
                "llamafile / local",
                pick(named == "llamafile"),
                "base_url, offline",
            ),
            m("ollama", pick(named == "ollama"), "if running"),
        ];
        if let Some(u) = cfg.llm_base_url.as_deref().filter(|u| !u.is_empty()) {
            let mut netloc = url_netloc(u);
            if netloc.is_empty() {
                netloc = u.to_string();
            }
            let model = cfg
                .llm_host_model
                .as_deref()
                .filter(|s| !s.is_empty())
                .unwrap_or("model unset");
            let ctx = match &cfg.llm_context_window {
                Some(w) if *w != BigInt::from(0) => {
                    // Python's // floors toward negative infinity.
                    let (q, r) = (w / 1024, w % 1024);
                    let q = if r < BigInt::from(0) { q - 1 } else { q };
                    format!("{q}k")
                }
                _ => "context unknown".to_string(),
            };
            let kind = cfg.llm_host_kind.as_deref().unwrap_or("None");
            backend_modules.push(m(
                format!("{netloc} ({kind}, {model}, {ctx})"),
                AVAILABLE,
                "recorded. Export the env lines from `tiers setup --remote` to use it",
            ));
        }
        // The engine the config means (engines.resolve_engine): with the
        // voice settings at their defaults, the engine the hardware picks
        // among those installed (VO-17); faster-whisper never imports here.
        use crate::voice::engine::{DEFAULT_ENGINE, DEFAULT_MODEL};
        let (whisper_cli, moon_cli, para_cli) = (which("whisper-cli"), which("moonshine-cli"), which("parakeet-cli"));
        let (voice_sel, voice_model) = if cfg.voice_engine == DEFAULT_ENGINE && cfg.voice_model == DEFAULT_MODEL {
            let hw = crate::voice::hardware::parse_hardware_env(
                self.env.get(crate::voice::hardware::HARDWARE_ENV).map(|s| s.as_str()).unwrap_or(""),
            )
            .unwrap_or_else(|| self.voice_hardware());
            let mut installed = vec![];
            if moon_cli {
                installed.push("moonshine");
            }
            if para_cli {
                installed.push("parakeet");
            }
            let (e, m, _) = crate::voice::hardware::choose_engine(
                &hw,
                &installed,
                false,
                crate::voice::parakeet::parakeet_usable(
                    crate::voice::parakeet::PARAKEET_DEFAULT_MODEL,
                    &crate::voice::moonshine::ModelEnv {
                        vars: self.env.clone(),
                        home: self.home.clone(),
                        hardware: None,
                        timeouts: Default::default(),
                    },
                    std::env::consts::ARCH,
                ),
            );
            (e.to_string(), m.to_string())
        } else {
            (cfg.voice_engine.clone(), cfg.voice_model.clone())
        };
        let moon_model =
            if voice_sel == "moonshine" && voice_model != DEFAULT_MODEL { voice_model.clone() } else { "small".to_string() };
        let para_note = "model NVIDIA Parakeet-TDT-0.6B v2, CC-BY-4.0";
        let moon_note = if !moon_cli {
            "needs moonshine-cli on PATH (scripts/install.sh)".to_string()
        } else if ["tiny", "small", "medium"].contains(&moon_model.as_str()) {
            format!("moonshine-cli on PATH; {moon_model}, fetched on first use")
        } else {
            "moonshine-cli on PATH".to_string()
        };
        let voice_state = |key: &str, installed: bool| {
            if voice_sel == key && installed {
                ACTIVE
            } else if installed {
                AVAILABLE
            } else {
                POSSIBLE
            }
        };
        let gate_hook = pre.hooks.installed("daisugi hook", "PreToolUse");
        let cond = |b: bool, yes: &'static str, no: &'static str| if b { yes } else { no };
        let (pi_state, pi_note) = if pi_installed {
            (ACTIVE, "gate extension installed")
        } else if pi_present {
            (AVAILABLE, "pi detected. Run `daisugi install --harness pi`")
        } else {
            (POSSIBLE, "not detected on this box")
        };
        let (oc_state, oc_note) = if oc_installed {
            (
                ACTIVE,
                "gate plugin installed: in-process deny-only hook; fail-closed",
            )
        } else if oc_present {
            (
                AVAILABLE,
                "opencode detected. Run `daisugi install --harness opencode`",
            )
        } else {
            (POSSIBLE, "not detected on this box")
        };
        let envelope_llm = if backend.is_empty() {
            m("llm-generated", POSSIBLE, "set OPENDAISUGI_LLM_BACKEND")
        } else {
            m("llm-generated", AVAILABLE, format!("backend={backend}"))
        };
        let (coppice_state, coppice_note) = if coppice_socket {
            (ACTIVE, "server running")
        } else if coppice_on_path {
            (AVAILABLE, "built, run `coppice server start`")
        } else {
            (POSSIBLE, "build it in harness/coppice")
        };
        let stage = |key, title, role, modules| Stage {
            key,
            title,
            role,
            modules,
        };
        Ok(vec![
            stage(
                "harness",
                "harness (agent host)",
                "the agent whose tool-calls we sit in front of",
                vec![
                    harness("claude-code", "claude-code"),
                    harness("codex", "codex"),
                    m("pi", pi_state, pi_note),
                    m("opencode", oc_state, oc_note),
                    m("hermes", POSSIBLE, "adapter designed, not built"),
                    m("openclaw", POSSIBLE, "adapter designed, not built"),
                    m(
                        "<any via AGENTS.md>",
                        POSSIBLE,
                        "vendor-neutral hook contract",
                    ),
                ],
            ),
            stage(
                "gate",
                "gate (call-time)",
                "checks each proposed action BEFORE it runs; fail-closed",
                vec![
                    m(
                        "claude PreToolUse",
                        cond(gate_hook, ACTIVE, POSSIBLE),
                        cond(gate_hook, "installed", "run `daisugi install`"),
                    ),
                    m("codex hooks.json", AVAILABLE, "fail-open class — soft gate"),
                    m(
                        "pi tool_call (in-process)",
                        cond(pi_installed, ACTIVE, POSSIBLE),
                        cond(
                            pi_installed,
                            "native block, no exit-2 convention, no fail-open outer timeout",
                            "run `daisugi install --harness pi`",
                        ),
                    ),
                    m(
                        "opencode tool.execute.before, in-process",
                        cond(oc_installed, ACTIVE, POSSIBLE),
                        cond(
                            oc_installed,
                            OPENCODE_GATE_NOTE,
                            "run `daisugi install --harness opencode`",
                        ),
                    ),
                    m("audit / off", AVAILABLE, "config: gate mode"),
                ],
            ),
            stage(
                "verifier",
                "verifier (the checker)",
                "proves the action stays inside the envelope (SMT-backed)",
                vec![
                    m("python (in-process)", ACTIVE, "the oracle; always runs"),
                    // No checkout: no compiled client is built (K4-1).
                    m("rust", POSSIBLE, "cd clients/rust && cargo build --release"),
                    m(
                        "go (mvdan)",
                        POSSIBLE,
                        "cd clients/go && scripts/native.sh && scripts/build.sh",
                    ),
                    m(
                        "typescript",
                        POSSIBLE,
                        "cd clients/ts && npm install && npm run build",
                    ),
                    m(
                        "lean (proven core)",
                        POSSIBLE,
                        "cd clients/lean && lake build",
                    ),
                ],
            ),
            stage(
                "shell",
                "shell decomposition",
                "splits `a && b` into heads so each is checked (ADR-0010/14)",
                vec![
                    // The bash grammar is linked into this binary.
                    m("tree-sitter-bash", ACTIVE, "installed"),
                    m(
                        "reject-compound",
                        AVAILABLE,
                        "the fail-closed default when off",
                    ),
                ],
            ),
            stage(
                "envelope",
                "envelope source",
                "where the 'what is allowed' spec comes from",
                vec![
                    m(
                        "evidence-inferred",
                        ACTIVE,
                        "ADR-0016 — from observed steps, zero-LLM",
                    ),
                    envelope_llm,
                ],
            ),
            stage(
                "backend",
                "model backend",
                "the LLM for envelope generation / planning (Tier-1)",
                backend_modules,
            ),
            stage(
                "matcher",
                "matcher / embedder (pathway reuse)",
                "finds a stored pathway to reuse instead of re-planning",
                vec![
                    m(
                        "all-MiniLM-L6-v2",
                        emb_state("all-MiniLM-L6-v2", CARRIES_MINILM),
                        "needs opendaisugi[search]",
                    ),
                    m(
                        "potion static",
                        emb_state("potion", CARRIES_POTION),
                        "torch-free, numpy-only, ~30MB, offline",
                    ),
                    m(
                        "int8 / fp16 onnx",
                        emb_state("int8", CARRIES_INT8),
                        INT8_REASON,
                    ),
                    m("lexical / intent", lexical_state, LEXICAL_DESC),
                ],
            ),
            stage(
                "router",
                "token router / gateway",
                "routes turns to cheap vs frontier models to save tokens",
                vec![
                    m(
                        "daisugi gateway",
                        cond(gateway_on, ACTIVE, POSSIBLE),
                        cond(gateway_on, "base_url wired", "daisugi install --gateway"),
                    ),
                    switchyard_row,
                    m(
                        "off (direct)",
                        AVAILABLE,
                        "default; start routing with `daisugi gateway`",
                    ),
                ],
            ),
            stage(
                "distill",
                "distillation (the gardener)",
                "clusters verified traces into reusable pathways (`tend`)",
                vec![
                    m(
                        "sentence-transformers",
                        emb_state("all-MiniLM-L6-v2", CARRIES_MINILM),
                        "needs [search]",
                    ),
                    m(
                        "potion (torch-free)",
                        emb_state("potion", CARRIES_POTION),
                        "numpy-only clustering",
                    ),
                    m(
                        "int8 (onnx, no torch)",
                        emb_state("int8", CARRIES_INT8),
                        INT8_REASON,
                    ),
                    m("lexical (no model)", lexical_state, LEXICAL_DESC),
                    m(
                        "no-embedder",
                        AVAILABLE,
                        "journal only, 0 pathways (graceful)",
                    ),
                ],
            ),
            stage(
                "stores",
                "stores (local-first)",
                "where trust and pathways live — on your disk",
                vec![
                    m(
                        "journal (sqlite)",
                        ACTIVE,
                        format!("{journal_total} traces"),
                    ),
                    m(
                        "pathway store (sqlite)",
                        ACTIVE,
                        format!("{pathway_count} pathways"),
                    ),
                    m("git-backed store", AVAILABLE, "shareable pathway registry"),
                ],
            ),
            stage(
                "floor_report",
                "floor report (state → a pane host)",
                "tells the pane host: idle, working, blocked, done",
                vec![
                    m("herdr", herdr_state, herdr_note),
                    m("coppice", POSSIBLE, "the report path is not wired yet"),
                    m(
                        "none",
                        cond(!herdr_hooks_on, ACTIVE, AVAILABLE),
                        "no floor listening",
                    ),
                ],
            ),
            stage(
                "floor_backend",
                "pane backend",
                "drives the harness inside a real pane: coppice, herdr, or tmux",
                vec![
                    m("coppice", coppice_state, coppice_note),
                    m(
                        "herdr",
                        cond(herdr_binary, AVAILABLE, POSSIBLE),
                        cond(herdr_binary, "installed", "install herdr from herdr.dev"),
                    ),
                    m(
                        "tmux",
                        cond(tmux_on_path, AVAILABLE, POSSIBLE),
                        cond(tmux_on_path, "installed", "install tmux 3.2 or newer"),
                    ),
                ],
            ),
            stage(
                "voice_engine",
                "voice engine (speech to text)",
                "turns a recorded clip into text for the voice bridge",
                vec![
                    m(
                        "faster-whisper",
                        voice_state("faster-whisper", false),
                        "needs opendaisugi[voice]",
                    ),
                    m("moonshine", voice_state("moonshine", moon_cli), moon_note),
                    m(
                        "parakeet",
                        voice_state("parakeet", para_cli),
                        if para_cli {
                            format!("parakeet-cli on PATH; {para_note}")
                        } else {
                            format!("needs parakeet-cli on PATH (scripts/install.sh); {para_note}")
                        },
                    ),
                    m(
                        "whisper.cpp",
                        voice_state("whisper.cpp", whisper_cli),
                        cond(whisper_cli, "whisper-cli on PATH", "needs whisper-cli on PATH"),
                    ),
                ],
            ),
        ])
    }

    pub(super) fn modules_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "modules";
        let opts = [DATA_DIR_OPT, JSON_OPT];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Show the module wiring: what's active, available to swap in, or an open slot.",
                &opts,
            );
        }
        let data_dir = self.data_dir_of(&p);
        let stages = self.detect_stages(CMD, &data_dir)?;
        if p.flag("--json") {
            let text = wiring_json(&stages);
            self.out(&format!("{text}\n"));
            return Ok(());
        }
        let text = render_wiring(&data_dir, &stages, self.glyphs());
        self.out(&format!("{text}\n"));
        Ok(())
    }
}

/// The part of gather_status the map shows: the traces in the journal
/// and the pathways in the store, zero when either is missing or cannot be
/// read. Both are read read-only.
fn store_counts(data_dir: &str) -> (i64, i64) {
    let db = join(data_dir, "pathways.db");
    let pws = if exists(&db) {
        read_pathway_stats(&db).map(|(c, _)| c).unwrap_or(0)
    } else {
        0
    };
    let traces = Journal::open_read_only(data_dir)
        .ok()
        .and_then(|j| j.stats().ok())
        .map(|s| s.total)
        .unwrap_or(0);
    (traces, pws)
}

/// `sys.stdout.isatty()`.
pub(super) fn stdout_tty() -> bool {
    // SAFETY: isatty has no preconditions.
    unsafe { libc::isatty(1) == 1 }
}

/// `s[:n]` on code points.
fn py_cut(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

/// `s.ljust(n)`.
fn py_ljust(s: &str, n: usize) -> String {
    let l = py_len(s);
    if l < n {
        format!("{s}{}", " ".repeat(n - l))
    } else {
        s.to_string()
    }
}

/// `f"{v} {text[:inner].ljust(inner)} {v}"`.
pub(super) fn box_line(g: &Glyphs, text: &str, inner: usize) -> String {
    format!("{} {} {}", g.v, py_ljust(&py_cut(text, inner), inner), g.v)
}

/// The module row(s) of a stage, wrapped at inner.
pub(super) fn module_rows(g: &Glyphs, st: &Stage, inner: usize) -> Vec<String> {
    let mut out = vec![];
    let mut line = "  ".to_string();
    for md in &st.modules {
        let chunk = format!("{} {}   ", g.state(md.state), md.name);
        if py_len(&line) + py_len(&chunk) > inner {
            out.push(box_line(g, &line, inner));
            line = "  ".into();
        }
        line.push_str(&chunk);
    }
    if !strip(&line).is_empty() {
        out.push(box_line(g, &line, inner));
    }
    out
}

pub(super) const WIDTH: usize = 66;

/// `modules.render_wiring`.
fn render_wiring(data_dir: &str, stages: &[Stage], g: &Glyphs) -> String {
    let inner = WIDTH - 4;
    let mut out = vec![
        format!("openDaisugi — module wiring   (data dir: {data_dir})"),
        String::new(),
        "  a task from your agent".to_string(),
    ];
    for st in stages {
        out.push(format!("        {}", g.v));
        out.push(format!("        {}", g.down));
        let eff = stage_effect(st.key);
        let tag = if eff.is_empty() {
            String::new()
        } else {
            format!(" [{eff}] ")
        };
        let head = format!("{}{} {} ", g.tl, g.h, st.title);
        let fill = WIDTH.saturating_sub(py_len(&head) + py_len(&tag) + 1);
        out.push(format!("{head}{}{tag}{}", g.h.repeat(fill), g.tr));
        out.push(box_line(g, st.role, inner));
        out.extend(module_rows(g, st, inner));
        out.push(format!("{}{}{}", g.bl, g.h.repeat(WIDTH - 2), g.br));
    }
    out.push(format!("        {}", g.v));
    out.push(format!("        {}", g.down));
    out.push("  verified action runs  (or falls back / is refused)".into());
    out.push(String::new());
    out.push(format!(
        "legend:  {} active   {} available (swap in)   {} possible / planned",
        g.on, g.avail, g.off
    ));
    out.push("         [live]    = takes effect now".into());
    out.push("         [cfg]     = a real choice, but needs a restart/reinstall".into());
    out.push("         [planned] = you can record it, but nothing reads it yet".into());
    let count = |e: &str| stages.iter().filter(|s| stage_effect(s.key) == e).count();
    out.push(format!(
        "         {} live · {} need a restart/reinstall · {} planned (not wired yet).",
        count("live"),
        count("cfg"),
        count("planned")
    ));
    out.join("\n")
}

/// `dataclasses.asdict(stage)`.
pub(super) fn stage_object(st: &Stage) -> Object {
    let mods: Vec<Value> = st
        .modules
        .iter()
        .map(|md| {
            Value::Obj(
                Object::new()
                    .with("name", md.name.as_str())
                    .with("state", md.state)
                    .with("note", md.note.as_str()),
            )
        })
        .collect();
    Object::new()
        .with("key", st.key)
        .with("title", st.title)
        .with("role", st.role)
        .with("modules", mods)
}

/// `modules.wiring_json`.
fn wiring_json(stages: &[Stage]) -> String {
    let out: Vec<Value> = stages.iter().map(|s| Value::Obj(stage_object(s))).collect();
    dumps_indent(&Value::List(out), 2, true)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn netloc_as_urlparse() {
        for (input, want) in [
            ("http://gpu-box:11434/v1", "gpu-box:11434"),
            ("gpu-box:11434", ""),
            ("gpu-box", ""),
            ("https://u:p@h.example:8080/v1?q=1", "u:p@h.example:8080"),
            ("//host/path", "host"),
            ("http://h?x#y", "h"),
        ] {
            assert_eq!(url_netloc(input), want, "{input}");
        }
    }
}
