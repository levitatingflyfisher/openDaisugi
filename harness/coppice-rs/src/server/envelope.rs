//! envelope.propose, envelope.pending, envelope.approve, envelope.decline:
//! the steward proposes a project envelope and the owner approves it. Go's
//! envelope_propose.go is the oracle.
//!
//! A project envelope is the operator's file, <config dir>/envelopes/
//! <name>.json, and only the operator writes it. The server checks a
//! proposal with daisugi, keeps it pending and writes nothing. It writes
//! the file only when the owner approves. A pane never gets to approve.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use serde_json::{json, Value};

use super::steward_work::envelope_dir;
use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};

/// The message a pane or a plugin gets for approving, declining or listing
/// envelope proposals.
pub const PROPOSAL_REFUSAL: &str =
    "only the operator approves an envelope. A steward proposes one with envelope.propose.";

/// The most bytes a proposed envelope may have.
const PROPOSAL_MAX_BYTES: usize = 64 << 10;

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("envelope.propose", Arc::new(|s, c, r| s.handle_envelope_propose(c, r)));
    let _ = s.handle("envelope.pending", Arc::new(|s, c, r| s.handle_envelope_pending(c, r)));
    let _ = s.handle("envelope.approve", Arc::new(|s, c, r| s.handle_envelope_approve(c, r)));
    let _ = s.handle("envelope.decline", Arc::new(|s, c, r| s.handle_envelope_decline(c, r)));
}

/// One pending proposal. `envelope` is the raw JSON text of the proposal,
/// copied the way Go's json.RawMessage is copied (see gojson::compact). It
/// is never parsed and written back: approve writes exactly these bytes.
struct Proposal {
    project: String,
    dir: String,
    envelope: String,
    tokens: i64,
    turns: i64,
    help_wait_s: i64,
    by: String,
    at: String,
}

impl Proposal {
    fn to_json(&self) -> String {
        let q = |s: &str| gojson::marshal(&json!(s));
        let mut out = format!(
            "{{\"project\":{},\"dir\":{},\"envelope\":{},\"tokens\":{},\"turns\":{}",
            q(&self.project),
            q(&self.dir),
            self.envelope,
            self.tokens,
            self.turns
        );
        if self.help_wait_s != 0 {
            out.push_str(&format!(",\"help_wait_s\":{}", self.help_wait_s));
        }
        if !self.by.is_empty() {
            out.push_str(&format!(",\"by\":{}", q(&self.by)));
        }
        out.push_str(&format!(",\"at\":{}}}", q(&self.at)));
        out
    }

    /// Reads a pending file. The file sits on disk, so it is not trusted:
    /// the envelope must be an object, and no checks ride along.
    fn from_file(text: &str) -> Option<Proposal> {
        let pairs = crate::proto::object_pairs(text)?;
        if pairs.iter().any(|(k, _)| k == "checks") {
            return None;
        }
        let raw_of = |k: &str| pairs.iter().rev().find(|(n, _)| n == k).map(|(_, v)| v.get());
        let env = raw_of("envelope")?;
        if !env.trim_start().starts_with('{') {
            return None;
        }
        let v: Value = serde_json::from_str(text).ok()?;
        let o = v.as_object()?;
        let s = |k: &str| o.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let n = |k: &str| o.get(k).and_then(Value::as_i64).unwrap_or(0);
        let project = s("project");
        if project.is_empty() {
            return None;
        }
        Some(Proposal {
            project,
            dir: s("dir"),
            envelope: env.to_string(),
            tokens: n("tokens"),
            turns: n("turns"),
            help_wait_s: n("help_wait_s"),
            by: s("by"),
            at: s("at"),
        })
    }
}

/// Writes b to path through a temp file in the same dir.
fn write_file_atomic(path: &Path, b: &[u8], mode: u32) -> std::io::Result<()> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    let dir = path.parent().unwrap_or(Path::new("."));
    std::fs::DirBuilder::new().recursive(true).mode(0o700).create(dir)?;
    let tmp = dir.join(format!(".tmp-{}", super::taskenv::rand_hex(6)));
    std::fs::write(&tmp, b)?;
    let r = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(mode))
        .and_then(|_| std::fs::rename(&tmp, path));
    if r.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    r
}

/// Names one proposal: the UTC time to the nanosecond and a random nonce,
/// so a replacement in the same second still has a new stamp. The card
/// carries it, and approve must send it back.
fn proposal_stamp(secs: f64) -> String {
    let whole = secs.floor();
    let nanos = ((secs - whole) * 1e9) as u32;
    let (y, mo, d, h, mi, s) = crate::web::der::civil(whole as i64);
    format!(
        "{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}.{nanos:09}Z-{}",
        super::taskenv::rand_hex(4)
    )
}

/// Asks daisugi whether the file is a valid envelope: a valid envelope
/// fits inside itself (tree check FILE FILE).
fn check_envelope_file(path: &Path) -> Result<(), String> {
    let p = path.to_string_lossy().into_owned();
    match super::taskenv::run_daisugi_task(&["tree".into(), "check".into(), p.clone(), p]) {
        Ok((_, true, true)) => Ok(()),
        Ok((out, _, true)) => Err(super::taskenv::reasons_of(&out)),
        Ok((_, _, false)) => Err("cannot run daisugi to check the envelope: timed out".into()),
        Err(e) => Err(format!("cannot run daisugi to check the envelope: {e}")),
    }
}

/// The fields that widen what a project may do and have no row of their
/// own on the card. Go's proposalCard builds the same lines.
fn envelope_also(env: &Value) -> Vec<String> {
    let perms = env.get("permissions");
    let strs = |k: &str| -> Vec<String> {
        perms
            .and_then(|x| x.get(k))
            .and_then(Value::as_array)
            .map(|a| a.iter().filter_map(|v| v.as_str().map(String::from)).collect())
            .unwrap_or_default()
    };
    let num = |k: &str, def: f64| -> String {
        let v = perms.and_then(|x| x.get(k)).and_then(Value::as_f64).unwrap_or(def);
        format!("{v}")
    };
    let mut also = Vec::new();
    if perms.and_then(|x| x.get("shell_allow_decomposition")).and_then(Value::as_bool).unwrap_or(false) {
        also.push("compound shell commands allowed, each command checked".to_string());
    }
    let custom = strs("custom_step_allowlist");
    if !custom.is_empty() {
        also.push(format!("custom steps: {}", custom.join(", ")));
    }
    let tools: Vec<String> = perms
        .and_then(|x| x.get("mcp_rules"))
        .and_then(Value::as_array)
        .map(|a| a.iter().map(|r| r.get("tool").and_then(Value::as_str).unwrap_or("").to_string()).collect())
        .unwrap_or_default();
    if !tools.is_empty() {
        also.push(format!("tool argument rules for: {}", tools.join(", ")));
    }
    also.push(format!(
        "limits: {} s per step, {} MB output",
        num("max_execution_time_s", 30.0),
        num("max_output_size_mb", 10.0)
    ));
    if let Some(pol) = env.get("shell_interpreter_policy").and_then(Value::as_str) {
        if !pol.is_empty() && pol != "surface" {
            also.push(format!("shell interpreters: {pol}"));
        }
    }
    also
}

impl Server {
    fn proposal_dir(&self) -> PathBuf {
        self.cfg.data_dir.join("envelope-proposals")
    }

    fn proposal_path(&self, name: &str) -> PathBuf {
        self.proposal_dir().join(format!("{name}.json"))
    }

    fn read_proposals(&self) -> Vec<Proposal> {
        let mut out = Vec::new();
        let Ok(rd) = std::fs::read_dir(self.proposal_dir()) else { return out };
        let mut names: Vec<String> = rd.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect();
        names.sort();
        for n in names {
            if !n.ends_with(".json") || n.starts_with("candidate-") {
                continue;
            }
            let Ok(text) = std::fs::read_to_string(self.proposal_dir().join(&n)) else { continue };
            let Some(p) = Proposal::from_file(&text) else { continue };
            if format!("{}.json", p.project) != n {
                continue;
            }
            out.push(p);
        }
        out.sort_by(|a, b| a.at.cmp(&b.at));
        out
    }

    fn proposal_card(&self, p: &Proposal) -> Value {
        let env: Value = serde_json::from_str(&p.envelope).unwrap_or(Value::Null);
        let perms = env.get("permissions");
        let strs = |k: &str| -> Value {
            let l: Vec<Value> = perms
                .and_then(|x| x.get(k))
                .and_then(Value::as_array)
                .map(|a| a.iter().filter(|v| v.is_string()).cloned().collect())
                .unwrap_or_default();
            Value::Array(l)
        };
        let flag = |k: &str| perms.and_then(|x| x.get(k)).and_then(Value::as_bool).unwrap_or(false);
        let text = |k: &str| env.get(k).and_then(Value::as_str).unwrap_or("").to_string();
        let also = envelope_also(&env);
        let replaces = envelope_dir().join(format!("{}.json", p.project)).exists();
        gojson::map(vec![
            ("project", json!(p.project)),
            ("dir", json!(p.dir)),
            ("task", json!(text("task"))),
            ("stakes", json!(text("stakes"))),
            ("read", strs("file_read")),
            ("write", strs("file_write")),
            (
                "run",
                gojson::map(vec![
                    ("shell", json!(flag("shell"))),
                    ("allow", strs("shell_allowlist")),
                    ("mcp", strs("mcp_allowlist")),
                ]),
            ),
            (
                "reach",
                gojson::map(vec![("network", json!(flag("network"))), ("hosts", strs("network_hosts"))]),
            ),
            ("also", json!(also)),
            ("tokens", json!(p.tokens)),
            ("turns", json!(p.turns)),
            ("by", json!(p.by)),
            ("at", json!(p.at)),
            ("replaces", json!(replaces)),
        ])
    }

    fn handle_envelope_propose(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let bad = |m: &str| proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, m);
        let project = r.str("project").unwrap_or_default();
        if project.is_empty() {
            return bad("envelope.propose needs project. Pass the project's name or path.");
        }
        let dir = match self.resolve_project(&project) {
            Ok(d) => d,
            Err(e) => return bad(&e),
        };
        let name = super::steward_work::base(&dir);
        let Some(raw) = r.raw("envelope") else {
            return bad("envelope.propose needs envelope: the envelope object.");
        };
        let env = raw.trim();
        if !env.starts_with('{') || env.len() > PROPOSAL_MAX_BYTES || serde_json::from_str::<Value>(env).is_err() {
            return bad("envelope must be a JSON object under 64 KiB.");
        }
        if r.raw("checks").is_some() {
            return bad("a proposal carries no checks. Checks are commands, and only the operator writes them into the file.");
        }
        let tokens = r.int("tokens").unwrap_or(0);
        let turns = r.int("turns").unwrap_or(0);
        let help = r.int("help_wait_s").unwrap_or(0);
        if tokens < 0 || turns < 0 || help < 0 {
            return bad("tokens, turns and help_wait_s are not negative.");
        }
        let pdir = self.proposal_dir();
        {
            use std::os::unix::fs::DirBuilderExt;
            if let Err(e) = std::fs::DirBuilder::new().recursive(true).mode(0o700).create(&pdir) {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_INTERNAL,
                    &format!("cannot make {}: {e}", pdir.display()),
                );
            }
        }
        // The check reads a candidate in the proposal dir, never the
        // operator's envelope dir.
        let cand = pdir.join(format!("candidate-{}.json", super::taskenv::rand_hex(6)));
        if let Err(e) = write_file_atomic(&cand, env.as_bytes(), 0o600) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &format!("cannot write the candidate: {e}"));
        }
        let checked = check_envelope_file(&cand);
        let _ = std::fs::remove_file(&cand);
        if let Err(e) = checked {
            return bad(&format!("the envelope does not pass daisugi's check: {e}"));
        }
        let ro = super::role::role_or_operator(Some(c));
        let p = Proposal {
            project: name.clone(),
            dir,
            envelope: gojson::compact(env),
            tokens,
            turns,
            help_wait_s: help,
            by: if ro.pane { ro.pane_id.clone() } else { String::new() },
            at: proposal_stamp(self.clock_seconds()),
        };
        // A second proposal for the project replaces the pending one.
        if let Err(e) = write_file_atomic(&self.proposal_path(&name), p.to_json().as_bytes(), 0o600) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &format!("cannot keep the proposal: {e}"));
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("pending", json!(true)),
                ("project", json!(name)),
                (
                    "note",
                    json!("The owner sees this on the floor and the phone. Nothing is written until the owner approves."),
                ),
            ]),
        )
    }

    fn handle_envelope_pending(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        if c.token_named() {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, super::messages::CONFIG_REFUSAL);
        }
        let cards: Vec<Value> = self.read_proposals().iter().map(|p| self.proposal_card(p)).collect();
        proto::ok_resp(&r.id, gojson::map(vec![("proposals", Value::Array(cards))]))
    }

    /// The pending proposal named by the request.
    fn proposal_for(&self, r: &Request) -> Result<(Proposal, String), Response> {
        let name = r.str("project").unwrap_or_default();
        if name.is_empty() || name.contains('/') || name.starts_with('.') {
            return Err(proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{} needs project: the name on the card.", r.cmd),
            ));
        }
        for p in self.read_proposals() {
            if p.project == name {
                return Ok((p, name));
            }
        }
        Err(proto::err_resp(
            &r.id,
            proto::ERR_BAD_REQUEST,
            &format!("no pending envelope for {name}. It may be approved, declined or replaced already."),
        ))
    }

    fn handle_envelope_approve(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        if c.token_named() {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, super::messages::CONFIG_REFUSAL);
        }
        let (p, name) = match self.proposal_for(r) {
            Ok(v) => v,
            Err(e) => return e,
        };
        // The owner approves what the card showed. A replaced proposal has
        // a new stamp, so approve always names the one it saw.
        let at = r.str("at").unwrap_or_default();
        if at.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "envelope.approve needs at: the stamp on the card. Look at the pending list first.",
            );
        }
        if at != p.at {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("the proposal for {name} changed after you looked. Look again."),
            );
        }
        // The file was checked when it came in. It sat on disk since, so
        // check the stored bytes again before they become the operator's file.
        let cand = self.proposal_dir().join(format!("candidate-{}.json", super::taskenv::rand_hex(6)));
        if let Err(e) = write_file_atomic(&cand, p.envelope.trim().as_bytes(), 0o600) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &format!("cannot write the candidate: {e}"));
        }
        let checked = check_envelope_file(&cand);
        let _ = std::fs::remove_file(&cand);
        if let Err(e) = checked {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("the stored envelope does not pass daisugi's check: {e}"),
            );
        }
        // The envelope bytes go in as they were proposed.
        let mut b = format!("{{\n  \"envelope\": {},\n  \"tokens\": {},\n  \"turns\": {}", p.envelope, p.tokens, p.turns);
        if p.help_wait_s > 0 {
            b.push_str(&format!(",\n  \"help_wait_s\": {}", p.help_wait_s));
        }
        b.push_str("\n}\n");
        let file = envelope_dir().join(format!("{name}.json"));
        if let Err(e) = write_file_atomic(&file, b.as_bytes(), 0o600) {
            return proto::err_resp(&r.id, proto::ERR_INTERNAL, &format!("cannot write the envelope: {e}"));
        }
        let _ = std::fs::remove_file(self.proposal_path(&name));
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("approved", json!(name)), ("path", json!(file.to_string_lossy()))]),
        )
    }

    fn handle_envelope_decline(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        if c.token_named() {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, super::messages::CONFIG_REFUSAL);
        }
        let (_, name) = match self.proposal_for(r) {
            Ok(v) => v,
            Err(e) => return e,
        };
        let _ = std::fs::remove_file(self.proposal_path(&name));
        proto::ok_resp(&r.id, gojson::map(vec![("declined", json!(name))]))
    }
}
