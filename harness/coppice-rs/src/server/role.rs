//! Who a connection is, and what it may do. The kernel places a peer by
//! SO_PEERCRED and its process tree; a hello can only take rights away. A
//! pane can propose. It cannot allow.

use std::collections::HashMap;
use std::sync::Arc;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::state;
use crate::sys;
use crate::textwidth;

/// The one message a pane connection gets for a verb only the operator
/// may run.
pub const PANE_REFUSAL: &str = "a pane can propose. It cannot allow.";
/// The message a pane or a plugin gets for a verb that names a foreman.
pub const FOREMAN_REFUSAL: &str = "only the operator names a foreman.";
/// The message a plugin gets for an allow verb.
pub const PLUGIN_REFUSAL: &str = "a plugin can propose. It cannot allow.";
pub const TALK_REFUSAL: &str = "only the operator talks to the floor's foreman.";
pub const TRUST_REFUSAL: &str = "only the operator trusts a folder.";
pub const FOREMAN_LABEL_REFUSAL: &str = "only the operator names a pane foreman.";
pub const LABEL_TAKEN_REFUSAL: &str =
    "another live pane has that label. Only the operator gives two panes one name.";
pub const RENAME_REFUSAL: &str = "a pane may rename only itself. The operator renames any pane.";

/// The label of the floor's foreman when the config file names none.
pub const DEFAULT_FOREMAN_LABEL: &str = "foreman";

/// How many runes a pane label keeps.
pub const LABEL_MAX: usize = 64;
/// How many characters a harness name may have.
pub const HARNESS_MAX: usize = 32;
/// How many runes a note keeps.
const NOTE_MAX: usize = 200;
/// How many notes floor.notes keeps.
const NOTES_KEPT: usize = 200;
/// The longest name a hello may give.
const NAME_MAX: usize = 32;
const NAME_RULE: &str = "names are one word, up to 32 characters";

/// How deep the parent walk goes. A real process tree is never this deep.
const MAX_WALK: usize = 256;

/// What the kernel says about the process on the other end of a socket.
#[derive(Clone, Debug, Default)]
pub struct PeerFacts {
    pub pane: bool,
    pub pane_id: String,
    pub unknown: bool,
    pub plugin: String,
    pub is_self: bool,
}

/// Places pid. panes maps each pty pane's child pid to its pane id. The
/// peer is a pane when a pid on its parent chain is a pane's child, or is
/// the server with the peer below it; or when its session is a pane's
/// session, or the server's own session when the server leads it.
pub fn classify_peer(
    pid: i32,
    me: i32,
    leader: bool,
    panes: &HashMap<i32, String>,
    plugins: &HashMap<i32, String>,
    stat: &dyn Fn(i32) -> Result<(i32, i32), String>,
) -> PeerFacts {
    if pid == me {
        return PeerFacts {
            is_self: true,
            ..Default::default()
        };
    }
    let sid = match stat(pid) {
        Ok((_, sid)) => sid,
        Err(_) => {
            return PeerFacts {
                unknown: true,
                ..Default::default()
            }
        }
    };
    let mut cur = pid;
    let mut i = 0;
    while i < MAX_WALK && cur > 1 {
        if let Some(id) = panes.get(&cur) {
            return PeerFacts {
                pane: true,
                pane_id: id.clone(),
                ..Default::default()
            };
        }
        if let Some(id) = plugins.get(&cur) {
            return PeerFacts {
                plugin: id.clone(),
                ..Default::default()
            };
        }
        if cur == me {
            return PeerFacts {
                pane: true,
                ..Default::default()
            };
        }
        match stat(cur) {
            Ok((ppid, _)) => cur = ppid,
            Err(_) => {
                return PeerFacts {
                    unknown: true,
                    ..Default::default()
                }
            }
        }
        i += 1;
    }
    if let Some(id) = panes.get(&sid) {
        return PeerFacts {
            pane: true,
            pane_id: id.clone(),
            ..Default::default()
        };
    }
    if let Some(id) = plugins.get(&sid) {
        return PeerFacts {
            plugin: id.clone(),
            ..Default::default()
        };
    }
    if leader && sid == me {
        return PeerFacts {
            pane: true,
            ..Default::default()
        };
    }
    PeerFacts::default()
}

/// What a connection is.
#[derive(Clone, Debug, Default)]
pub struct Role {
    pub pane: bool,
    pub pane_id: String,
    pub no_allow: bool,
    pub unknown: bool,
    pub plugin: String,
}

impl Client {
    /// The client's role. A kernel pane is a pane whatever its hello said.
    /// A hello that names a pane makes any connection a pane. A peer the
    /// kernel could not place may not allow.
    pub fn role(&self) -> Role {
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        let mut r = Role::default();
        if st.facts.pane {
            r.pane = true;
            r.pane_id = st.facts.pane_id.clone();
        }
        if st.hello_role == "pane" {
            r.pane = true;
        }
        if r.pane && r.pane_id.is_empty() && !st.facts.pane {
            r.pane_id = st.hello_pane.clone();
        }
        r.plugin = st.facts.plugin.clone();
        if r.plugin.is_empty() {
            r.plugin = st.hello_plugin.clone();
        }
        r.unknown = st.facts.unknown;
        r.no_allow = r.pane || r.unknown || !r.plugin.is_empty();
        r
    }

    /// The connection's name and how the server knows it.
    /// The pane the kernel placed this connection in, or "". A hello
    /// names no pane here: only a process inside the pane counts.
    pub fn kernel_pane(&self) -> String {
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        if !st.facts.pane || !st.facts.plugin.is_empty() || st.facts.unknown {
            return String::new();
        }
        st.facts.pane_id.clone()
    }

    pub fn who(&self) -> (String, String) {
        let ro = self.role();
        if !ro.plugin.is_empty() {
            return (format!("plugin:{}", ro.plugin), "plugin".into());
        }
        if ro.pane && !ro.pane_id.is_empty() {
            return (format!("pane:{}", ro.pane_id), "pane".into());
        }
        if ro.pane {
            return ("pane".into(), "pane".into());
        }
        let st = self.st.lock().unwrap_or_else(|e| e.into_inner());
        if st.name.is_empty() {
            return (String::new(), "none".into());
        }
        (st.name.clone(), st.name_from.clone())
    }

    /// Whether this client is attached to the pane: only an attached
    /// client may speak as the operator.
    pub fn operator(&self, pane: &str) -> bool {
        self.st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .attached
            .contains_key(pane)
    }

    pub fn view_only_on(&self, pane: &str) -> bool {
        self.st
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .view_only
            .contains(pane)
    }
}

/// The role of a call the server makes itself with no client: the
/// operator's.
pub fn role_or_operator(c: Option<&Arc<Client>>) -> Role {
    c.map(|c| c.role()).unwrap_or_default()
}

/// The not_attached reply a write on a view-only pane gets.
pub fn refuse_view_only(r: &Request, id: &str) -> Response {
    proto::err_resp(
        &r.id,
        proto::ERR_NOT_ATTACHED,
        &format!("this connection is attached to {id} view only. Run: coppice attach {id}"),
    )
}

fn check_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > NAME_MAX {
        return Err(NAME_RULE.into());
    }
    if !name
        .chars()
        .all(|r| r.is_ascii_alphanumeric() || r == '-' || r == '_')
    {
        return Err(NAME_RULE.into());
    }
    Ok(())
}

/// label without control and format characters, cut to LABEL_MAX runes,
/// and trimmed.
pub fn clean_label(label: &str) -> String {
    let no_cf: String = label.chars().filter(|&c| !is_cf(c)).collect();
    textwidth::printable(no_cf.trim(), LABEL_MAX)
        .trim()
        .to_string()
}

/// Whether c is in Unicode's Cf (format) category.
pub(super) fn is_cf(c: char) -> bool {
    matches!(c as u32,
        0xAD | 0x600..=0x605 | 0x61C | 0x6DD | 0x70F | 0x890..=0x891 | 0x8E2 | 0x180E
        | 0x200B..=0x200F | 0x202A..=0x202E | 0x2060..=0x2064 | 0x2066..=0x206F
        | 0xFEFF | 0xFFF9..=0xFFFB | 0x110BD | 0x110CD | 0x13430..=0x1343F
        | 0x1BCA0..=0x1BCA3 | 0x1D173..=0x1D17A | 0xE0001 | 0xE0020..=0xE007F)
}

/// A harness name: a letter or digit, then letters, digits, '.', '_' or
/// '-', at most HARNESS_MAX in all.
pub fn valid_harness(name: &str) -> bool {
    if name.is_empty() || name.len() > HARNESS_MAX {
        return false;
    }
    name.chars()
        .enumerate()
        .all(|(i, r)| r.is_ascii_alphanumeric() || (i > 0 && (r == '.' || r == '_' || r == '-')))
}

pub fn is_foreman_label(label: &str) -> bool {
    clean_label(label).to_lowercase() == DEFAULT_FOREMAN_LABEL
}

fn takes_foreman_label(r: &Request) -> bool {
    matches!(
        r.cmd.as_str(),
        "pane.create" | "pane.split" | "pane.fork" | "pane.rename"
    ) && is_foreman_label(&r.s("label"))
}

/// Verbs that make no note when a pane runs them.
fn quiet(cmd: &str) -> bool {
    matches!(
        cmd,
        "hello"
            | "pane.report_state"
            | "pane.report_child"
            | "events.subscribe"
            | "events.pause"
            | "events.resume"
            | "floor.note"
            | "floor.notes"
            | "floor.facts"
    )
}

fn typing(cmd: &str) -> bool {
    matches!(
        cmd,
        "pane.send_keys" | "pane.send_text" | "pane.run" | "agent.prompt"
    )
}

fn operator_verb(cmd: &str) -> Option<&'static str> {
    match cmd {
        "task.set_foreman" | "floor.foreman" => Some(FOREMAN_REFUSAL),
        "floor.talk" | "floor.chat" => Some(TALK_REFUSAL),
        "pane.trust" => Some(TRUST_REFUSAL),
        _ => None,
    }
}

/// One ask a pane reported: when it expires, and its tier.
#[derive(Clone, Debug)]
pub struct NotedAsk {
    deadline: f64,
    tier: String,
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle("hello", Arc::new(|s, c, r| s.handle_hello(c, r)));
    let _ = s.handle("floor.note", Arc::new(|s, c, r| s.handle_note(c, r)));
    let _ = s.handle("floor.notes", Arc::new(|s, c, r| s.handle_notes(c, r)));
    let _ = s.handle(
        "agent.allow",
        Arc::new(|s, c, r| s.handle_answer("allow", c, r)),
    );
    let _ = s.handle(
        "agent.deny",
        Arc::new(|s, c, r| s.handle_answer("deny", c, r)),
    );
}

impl Server {
    /// The kernel's facts about a socket peer. A pid the kernel will not
    /// name is unknown.
    pub fn facts_of(&self, pid: Option<i32>) -> PeerFacts {
        let Some(pid) = pid.filter(|p| *p > 0) else {
            return PeerFacts {
                unknown: true,
                ..Default::default()
            };
        };
        // A policy's pid is recorded under this lock while it starts, so a
        // policy that dials at once waits here until it can be placed.
        let mut plug = self.plug_lock();
        plug.prune();
        classify_peer(
            pid,
            sys::getpid(),
            sys::leads_session(),
            &self.pane_pids(),
            &plug.pids,
            &sys::proc_stat,
        )
    }

    /// The child pid of every live pty pane, and every root pid a
    /// headless pane's adapter names, to its pane id.
    pub fn pane_pids(&self) -> HashMap<i32, String> {
        let mut out = HashMap::new();
        for (id, lp) in self.live.read().unwrap_or_else(|e| e.into_inner()).iter() {
            if let Some(p) = &lp.pty {
                // A pane whose process ended drops out at once, and so does
                // a pid that now belongs to another process.
                if p.pid() > 0 && lp.pty_pid_ok(p.pid()) {
                    out.insert(p.pid(), id.clone());
                }
            } else if let Some(a) = &lp.adapter {
                for root in a.pids() {
                    if root > 0 {
                        out.insert(root, id.clone());
                    }
                }
            }
        }
        out
    }

    /// The live headless harness of pane id when that harness holds ask
    /// itself.
    fn harness_asker(
        &self,
        id: &str,
        ask: &str,
    ) -> Option<std::sync::Arc<dyn crate::adapters::Proc>> {
        let lp = self.live(id)?;
        let a = lp.adapter.clone()?;
        (a.answerer() && a.owns_ask(ask)).then_some(a)
    }

    /// Places the peers a hello names. The names can only take rights
    /// away.
    fn place_named_peers(&self, r: &Request) -> Result<PeerFacts, Response> {
        let mut out = PeerFacts::default();
        if r.bool("peer_unknown") == Some(true) {
            out.unknown = true;
        }
        let Some(raw) = r.raw("peer_pids") else {
            return Ok(out);
        };
        let pids: Vec<i64> = match serde_json::from_str::<Option<Vec<i64>>>(raw) {
            Ok(v) => v.unwrap_or_default(),
            Err(_) => {
                return Err(proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "peer_pids must be a list of pids",
                ))
            }
        };
        if pids.is_empty() {
            out.unknown = true;
            return Ok(out);
        }
        let panes = self.pane_pids();
        let leader = sys::leads_session();
        let mut plug = self.plug_lock();
        plug.prune();
        for pid in pids {
            let f = classify_peer(
                pid as i32,
                sys::getpid(),
                leader,
                &panes,
                &plug.pids,
                &sys::proc_stat,
            );
            if out.plugin.is_empty() {
                out.plugin = f.plugin;
            }
            out.pane = out.pane || f.pane;
            if out.pane_id.is_empty() {
                out.pane_id = f.pane_id;
            }
            out.unknown = out.unknown || f.unknown;
        }
        Ok(out)
    }

    /// Records the role and the pane a client says it has. It runs in the
    /// read loop, so the verbs after it see it. A pane stays a pane.
    pub fn handle_hello(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let mut role_name = r.s("role");
        if role_name.is_empty() {
            role_name = "operator".into();
        }
        if !matches!(role_name.as_str(), "operator" | "pane" | "plugin") {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "role must be operator, pane or plugin",
            );
        }
        let mut plugin_id = String::new();
        if role_name == "plugin" {
            plugin_id = r.s("plugin");
            if plugin_id.is_empty() {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "role plugin needs plugin, the plugin id.",
                );
            }
        }
        let pane_id = r.s("pane");
        let name = r.s("name");
        let name_from = r.s("name_from");
        if !name_from.is_empty() && name_from != "token" && name_from != "socket" {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                "name_from must be token or socket.",
            );
        }
        if !name.is_empty() {
            if let Err(e) = check_name(&name) {
                return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e);
            }
        }
        {
            let st = c.st.lock().unwrap_or_else(|e| e.into_inner());
            if st.name_fixed
                && (!name.is_empty() || !name_from.is_empty())
                && (name != st.name || name_from != "token")
            {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    "this connection takes its name from its token.",
                );
            }
            if !st.name.is_empty() && !name.is_empty() && name != st.name {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("this connection is {}.", st.name),
                );
            }
        }
        let placed = match self.place_named_peers(r) {
            Ok(p) => p,
            Err(resp) => return resp,
        };
        let cur = c.role().plugin;
        if !cur.is_empty() && !plugin_id.is_empty() && plugin_id != cur {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("this connection is plugin {cur}."),
            );
        }
        if !plugin_id.is_empty() && (c.role().pane || placed.pane || role_name == "pane") {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, "a pane is not a plugin.");
        }
        if !plugin_id.is_empty() && self.plugin_spec(&plugin_id).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("no plugin {plugin_id} is enabled."),
            );
        }
        {
            let mut st = c.st.lock().unwrap_or_else(|e| e.into_inner());
            if !plugin_id.is_empty() && st.hello_plugin.is_empty() {
                st.hello_plugin = plugin_id.clone();
            }
            if st.facts.plugin.is_empty() {
                st.facts.plugin = placed.plugin.clone();
            }
            st.facts.pane = st.facts.pane || placed.pane;
            if st.facts.pane_id.is_empty() {
                st.facts.pane_id = placed.pane_id.clone();
            }
            st.facts.unknown = st.facts.unknown || placed.unknown;
            if st.hello_role != "pane" {
                st.hello_role = role_name.clone();
            }
            if !pane_id.is_empty() {
                st.hello_pane = pane_id.clone();
            }
            if name_from == "token" {
                st.name_fixed = true;
            }
            if !name.is_empty() && st.name.is_empty() {
                st.name = name.clone();
                st.name_from = if name_from == "token" && st.facts.is_self {
                    "token".into()
                } else {
                    "socket".into()
                };
            }
        }
        let got = c.role();
        let mut res: Vec<(&str, Value)> =
            vec![("role", json!("operator")), ("allow", json!(!got.no_allow))];
        let (who, _) = c.who();
        if !who.is_empty() {
            res.push(("name", json!(who)));
        }
        if !got.plugin.is_empty() {
            res[0] = ("role", json!("plugin"));
            res.push(("plugin", json!(got.plugin)));
            let needs = self
                .plugin_spec(&got.plugin)
                .map(|s| s.0)
                .unwrap_or_default();
            res.push(("needs", json!(needs)));
            return proto::ok_resp(&r.id, gojson::map(res));
        }
        if got.pane {
            res[0] = ("role", json!("pane"));
            if !got.pane_id.is_empty() {
                res.push(("pane", json!(got.pane_id)));
            }
        }
        proto::ok_resp(&r.id, gojson::map(res))
    }

    /// Refuses a verb the connection's role may not run.
    pub fn guard(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> (Response, bool) {
        let ro = c.role();
        let refuse = |msg: &str| (proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, msg), false);
        if let Some(msg) = operator_verb(&r.cmd) {
            if ro.no_allow {
                return refuse(msg);
            }
        }
        if let Some(resp) = self.chat_guard(c, &ro, r) {
            return (resp, false);
        }
        if let Some(resp) = self.sprig_guard(&ro, r) {
            return (resp, false);
        }
        if ro.no_allow && takes_foreman_label(r) {
            return refuse(FOREMAN_LABEL_REFUSAL);
        }
        if r.cmd == "pane.rename" && ro.no_allow {
            let pane = r.s("pane");
            if ro.pane_id.is_empty() || pane != ro.pane_id {
                return refuse(RENAME_REFUSAL);
            }
        }
        if r.cmd == "task.move" && (ro.pane || ro.unknown) {
            return refuse(PANE_REFUSAL);
        }
        if (r.cmd == "agent.allow" || r.cmd == "agent.deny")
            && ro.no_allow
            && !self.foreman_deny(&ro, r)
        {
            if !ro.plugin.is_empty() {
                return refuse(PLUGIN_REFUSAL);
            }
            return refuse(PANE_REFUSAL);
        }
        if !ro.plugin.is_empty() {
            if let Err(resp) = self.plugin_guard(&ro.plugin, r) {
                return (resp, false);
            }
        }
        if (r.cmd == "pane.report_state" || r.cmd == "pane.report_child") && ro.pane {
            let id = r.s("pane");
            if ro.pane_id.is_empty() || id != ro.pane_id {
                return refuse(PANE_REFUSAL);
            }
        }
        // A prompt to a gated pane gets the handler's own refusal, which
        // names the pane and says to answer its question first.
        if typing(&r.cmd)
            && ro.no_allow
            && !(super::ready::is_prompt_request(r) && self.gated(&r.s("pane")))
        {
            let id = r.s("pane");
            if let Some(eff) = self.effective_state(&id) {
                if eff.state == state::STATE_BLOCKED {
                    return refuse(PANE_REFUSAL);
                }
            }
        }
        if r.cmd == "pane.report_state" && ro.no_allow {
            // As Go reads the event into a struct with one source field.
            let src = r
                .raw("event")
                .map(|raw| proto::struct_string_field(raw, "source"));
            match src {
                Some(Ok(s)) if s != state::SRC_OPERATOR => {}
                _ => return refuse(PANE_REFUSAL),
            }
        }
        (proto::ok_resp("", Value::Null), true)
    }

    /// The label of pane id, or the id when it has none, or "pane" when the
    /// id is empty.
    pub fn label_of(&self, id: &str) -> String {
        if id.is_empty() {
            return "pane".into();
        }
        match self.tree.pane(id) {
            Some(p) if !p.label.is_empty() => p.label,
            _ => id.into(),
        }
    }

    /// Records a note for a verb a pane connection runs.
    pub fn note_verb(&self, c: &Arc<Client>, r: &Request) {
        if quiet(&r.cmd) {
            return;
        }
        let ro = c.role();
        let note = |who: &str, keys: &[&str]| -> String {
            let mut parts = vec![r.cmd.clone()];
            for k in keys {
                let Some(v) = r.str(k) else {
                    continue;
                };
                let mut v = v.split_whitespace().collect::<Vec<_>>().join(" ");
                if v.is_empty() {
                    continue;
                }
                if *k == "text" && v.chars().count() > 60 {
                    v = v.chars().take(60).collect();
                }
                parts.push(v);
            }
            format!("{who} › {}", parts.join("  "))
        };
        if !ro.plugin.is_empty() {
            if super::plugins::read_verb(&r.cmd) {
                return;
            }
            let text = note(&ro.plugin, &["pane", "text"]);
            self.note(&text, &r.s("pane"));
            return;
        }
        if !ro.pane {
            return;
        }
        let text = note(
            &self.label_of(&ro.pane_id),
            &["label", "pane", "harness", "text"],
        );
        self.note(&text, &ro.pane_id);
    }

    pub fn note(&self, text: &str, pane_id: &str) {
        self.note_to(text, pane_id, "");
    }

    /// Records one note, keeps the last NOTES_KEPT, and sends it to every
    /// client that subscribed to notes.
    pub fn note_to(&self, text: &str, pane_id: &str, to: &str) {
        let mut p: Vec<(&str, Value)> = vec![
            ("event", json!("note")),
            ("text", json!(textwidth::printable(text, NOTE_MAX))),
        ];
        if !pane_id.is_empty() {
            p.push(("pane", json!(pane_id)));
        }
        if !to.is_empty() {
            p.push(("to", json!(to)));
        }
        p.push(("ts", gojson::float(Server::now_seconds())));
        let n = gojson::obj(p);
        {
            let mut notes = self.notes.lock().unwrap_or_else(|e| e.into_inner());
            notes.push(n.clone());
            if notes.len() > NOTES_KEPT {
                let cut = notes.len() - NOTES_KEPT;
                notes.drain(..cut);
            }
        }
        self.broadcast("note", pane_id, &n);
    }

    fn handle_note(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let mut text = r.s("text").trim().to_string();
        if text.is_empty() {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, "floor.note needs text.");
        }
        let mut pane_id = r.s("pane");
        let ro = c.role();
        if !ro.plugin.is_empty() {
            pane_id.clear();
            text = format!("{} › {}", ro.plugin, text);
        } else if ro.pane {
            pane_id = ro.pane_id.clone();
            text = format!("{} › {}", self.label_of(&pane_id), text);
        }
        self.note(&text, &pane_id);
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("text", json!(textwidth::printable(&text, NOTE_MAX)))]),
        )
    }

    fn handle_notes(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let notes = self.notes.lock().unwrap_or_else(|e| e.into_inner()).clone();
        proto::ok_resp(&r.id, gojson::map(vec![("notes", Value::Array(notes))]))
    }

    /// Whether a connection with role ro may run the deny in r: a pane
    /// the kernel placed or named, no plugin, and the foreman that holds
    /// that exact ask of that exact pane now.
    fn foreman_deny(&self, ro: &Role, r: &Request) -> bool {
        if r.cmd != "agent.deny"
            || !ro.pane
            || ro.unknown
            || !ro.plugin.is_empty()
            || ro.pane_id.is_empty()
        {
            return false;
        }
        let pane = r.s("pane");
        let ask = self.resolve_hold(&pane, r.s("ask").trim());
        !pane.is_empty() && !ask.is_empty() && self.holder(&pane, &ask) == ro.pane_id
    }

    /// Answers the gate's ask, through the same file the phone writes.
    /// guard lets through only the operator, and a foreman's deny of the
    /// ask it holds. The foreman check runs again here.
    fn handle_answer(self: &Arc<Server>, decision: &str, c: &Arc<Client>, r: &Request) -> Response {
        let verb = format!("agent.{decision}");
        let ro = c.role();
        let foreman = ro.no_allow;
        if foreman && !self.foreman_deny(&ro, r) {
            return proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, PANE_REFUSAL);
        }
        let id = r.s("pane");
        if id.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{verb} needs pane. Run: coppice pane list"),
            );
        }
        if self.tree.pane(&id).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice pane list", sys::go_quote(&id)),
            );
        }
        let ask = self.resolve_hold(&id, r.s("ask").trim());
        if ask.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{verb} needs ask, the id of the ask to answer."),
            );
        }
        let Some(tier) = self.held_ask(&id, &ask) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "pane {id} does not hold ask {}. Run: coppice agent get {id}",
                    sys::go_quote(&ask)
                ),
            );
        };
        let mut reason = r.s("reason");
        if foreman {
            reason = format!("denied by the foreman {}", self.label_of(&ro.pane_id));
        } else if reason.is_empty() {
            reason = "answered from coppice by the operator".into();
        }
        let (by, from) = c.who();
        let res = if let Some(an) = self.harness_asker(&id, &ask) {
            // The harness holds this ask itself, not the gate. The same
            // allow rule applies: a permanent ask needs the pane name.
            let mut res = Ok(());
            if decision == "allow" {
                res = super::ask::check_allow(
                    &tier,
                    &self.label_of(&id),
                    &r.s("confirm"),
                    &r.s("scope"),
                );
            }
            if res.is_ok() {
                if let Err(e) = an.answer(&ask, decision == "allow", &reason) {
                    return proto::err_resp(&r.id, proto::ERR_ADAPTER, &e);
                }
            }
            res
        } else {
            super::ask::answer(
                &self.gate_root(),
                &super::ask::Reply {
                    tool_use_id: ask.clone(),
                    decision: decision.into(),
                    reason,
                    scope: r.s("scope"),
                    confirm: r.s("confirm"),
                    tier,
                    name: self.label_of(&id),
                    by,
                    who_from: from,
                },
            )
        };
        if res.is_ok() {
            self.forget_ask(&id, &ask);
            self.end_hold(&id, &ask);
        }
        match res {
            Ok(()) => proto::ok_resp(
                &r.id,
                gojson::map(vec![
                    ("pane", json!(id)),
                    ("ask", json!(ask)),
                    ("decision", json!(decision)),
                ]),
            ),
            Err(super::ask::AnswerError::NoAsk) => proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "no pending ask {}. The gate timed out, or someone answered it.",
                    sys::go_quote(&ask)
                ),
            ),
            Err(super::ask::AnswerError::BadScope) => {
                proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, "scope must be once or task.")
            }
            Err(e @ super::ask::AnswerError::NeedsName(_)) => {
                proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, &e.to_string())
            }
            Err(e @ super::ask::AnswerError::PermanentTask) => proto::err_resp(
                &r.id,
                proto::ERR_UNAUTHORIZED,
                &format!("{e} with the pane name."),
            ),
            Err(e) => proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!("cannot write the answer: {e}"),
            ),
        }
    }

    /// Records an ask a pane reported, with its deadline.
    pub fn note_ask(&self, pane: &str, ask: Option<&state::Ask>) {
        let Some(a) = ask.filter(|a| !a.id.is_empty()) else {
            return;
        };
        self.asks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .entry(pane.to_string())
            .or_default()
            .insert(
                a.id.clone(),
                NotedAsk {
                    deadline: a.deadline,
                    tier: state::normal_tier(&a.tier),
                },
            );
    }

    /// The tier of the live ask id that pane holds, or None. Expired asks
    /// are dropped here.
    fn held_ask(&self, pane: &str, id: &str) -> Option<String> {
        let now = Server::now_seconds();
        let mut g = self.asks.lock().unwrap_or_else(|e| e.into_inner());
        let m = g.get_mut(pane)?;
        m.retain(|_, a| now < a.deadline);
        m.get(id).map(|a| a.tier.clone())
    }

    fn forget_ask(&self, pane: &str, id: &str) {
        if let Some(m) = self
            .asks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .get_mut(pane)
        {
            m.remove(id);
        }
    }

    /// Drops every ask of a pane that closed, ends its hold, and sends
    /// every ask it held as a foreman to the operator.
    pub fn forget_asks(&self, pane: &str) {
        self.end_holds(pane);
        self.asks
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(pane);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_peer_below_a_pane_child_is_that_pane() {
        let mut panes = HashMap::new();
        panes.insert(50, "w1:p1".to_string());
        let tree: HashMap<i32, (i32, i32)> =
            [(70, (60, 70)), (60, (50, 50)), (50, (10, 50)), (10, (1, 1))]
                .into_iter()
                .collect();
        let stat = |p: i32| tree.get(&p).copied().ok_or_else(|| "gone".to_string());
        let f = classify_peer(70, 10, false, &panes, &HashMap::new(), &stat);
        assert!(f.pane && f.pane_id == "w1:p1");
        let f = classify_peer(99, 10, false, &panes, &HashMap::new(), &stat);
        assert!(f.unknown);
    }

    #[test]
    fn labels_lose_controls_and_format_marks() {
        assert_eq!(clean_label("  a\u{200b}b\u{1b}c  "), "abc");
        assert!(is_foreman_label(" Foreman "));
        assert!(valid_harness("claude-2.x"));
        assert!(!valid_harness("-x") && !valid_harness(""));
    }
}
