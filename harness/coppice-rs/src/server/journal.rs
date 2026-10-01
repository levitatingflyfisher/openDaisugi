//! The verdict journal: the last gate verdicts the server was told, newest
//! first through floor.journal, kept across a restart in
//! <data dir>/journal/verdicts.jsonl (0600).

use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::PathBuf;
use std::sync::Arc;

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::state::Verdict;

/// How many verdicts the journal keeps and the most one reply holds.
pub const JOURNAL_KEPT: usize = 500;

/// The most entries one pane holds in the journal, so a pane that reports a
/// verdict on every tool call cannot push the others out. When a pane
/// passes it, its own oldest entry goes.
pub const JOURNAL_PANE_KEPT: usize = 100;

/// Adds e at the end of ring. If its pane now holds more than
/// JOURNAL_PANE_KEPT, that pane's oldest entry goes, never another pane's.
/// Then the ring is cut to its last JOURNAL_KEPT.
fn journal_push(ring: &mut Vec<Entry>, e: Entry) {
    let pane = e.pane.clone();
    ring.push(e);
    if ring.iter().filter(|x| x.pane == pane).count() > JOURNAL_PANE_KEPT {
        if let Some(i) = ring.iter().position(|x| x.pane == pane) {
            ring.remove(i);
        }
    }
    if ring.len() > JOURNAL_KEPT {
        let cut = ring.len() - JOURNAL_KEPT;
        ring.drain(..cut);
    }
}

/// One verdict a gate told the server about. at is unix seconds.
#[derive(Clone, Debug)]
pub struct Entry {
    at: f64,
    pane: String,
    decision: String,
    tool: String,
    clause: String,
    mode: String,
}

impl Entry {
    fn to_value(&self) -> Value {
        let mut v = vec![
            ("at", gojson::float(self.at)),
            ("pane", json!(self.pane)),
            ("decision", json!(self.decision)),
            ("tool", json!(self.tool)),
            ("clause", json!(self.clause)),
        ];
        if !self.mode.is_empty() {
            v.push(("mode", json!(self.mode)));
        }
        gojson::obj(v)
    }

    /// Reads one line of the file. The rule is the same in the Go and the
    /// Rust server: the line is one JSON object, at is a number, pane and
    /// decision are strings that are not empty, tool, clause and mode are
    /// strings when they are there (null is not there), and a key is
    /// matched by its exact spelling. Anything else is not a verdict.
    fn from_line(line: &str) -> Option<Entry> {
        let v: Value = serde_json::from_str(line).ok()?;
        let o = v.as_object()?;
        let s = |k: &str| -> Option<String> {
            match o.get(k) {
                None | Some(Value::Null) => Some(String::new()),
                Some(Value::String(x)) => Some(x.clone()),
                Some(_) => None,
            }
        };
        let e = Entry {
            at: o.get("at")?.as_f64()?,
            pane: s("pane")?,
            decision: s("decision")?,
            tool: s("tool")?,
            clause: s("clause")?,
            mode: s("mode")?,
        };
        (!e.pane.is_empty() && !e.decision.is_empty()).then_some(e)
    }
}

/// The ring, oldest first, and the file's state.
#[derive(Default)]
pub struct Journal {
    loaded: bool,
    ring: Vec<Entry>,
    /// How many lines the file holds. It is cut back to the ring when it
    /// holds twice the ring.
    lines: usize,
}

/// Makes the directory of path, 0700.
fn make_dir(path: &std::path::Path) {
    use std::os::unix::fs::DirBuilderExt;
    if let Some(d) = path.parent() {
        let _ = std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(d);
    }
}

fn journal_path(s: &Server) -> PathBuf {
    s.cfg.data_dir.join("journal").join("verdicts.jsonl")
}

impl Server {
    fn journal_lock(&self) -> std::sync::MutexGuard<'_, Journal> {
        self.journal.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Reads the file once. It replays the lines through the quotas an
    /// append meets and cuts the file back to the ring. A torn last line (a
    /// crash in the middle of an append) is dropped alone. A file with a bad
    /// line before the last moves aside to <name>.broken-<UTC time> with a note, and the journal
    /// starts fresh.
    fn load_journal(&self, j: &mut Journal) {
        if j.loaded {
            return;
        }
        j.loaded = true;
        let path = journal_path(self);
        let b = match std::fs::read(&path) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return,
            Err(_) => {
                self.note_later(format!(
                    "cannot read {}. The verdict journal starts empty and is not saved.",
                    path.display()
                ));
                return;
            }
        };
        let text = String::from_utf8_lossy(&b);
        let lines: Vec<&str> = text.split('\n').filter(|l| !l.trim().is_empty()).collect();
        let mut ring = Vec::new();
        for (i, line) in lines.iter().enumerate() {
            match Entry::from_line(line) {
                Some(e) => journal_push(&mut ring, e),
                // A crash in the middle of an append leaves a torn last
                // line. Only that line goes. A bad line before it means
                // the file is not ours to trust.
                None if i + 1 == lines.len() => {
                    self.save_dropped_line(&path, line);
                    break;
                }
                None => {
                    let moved = super::messages::move_aside_file(&path);
                    self.note_later(format!(
                        "The verdict journal could not be read. It moved to {moved}. The journal starts fresh."
                    ));
                    return;
                }
            }
        }
        j.lines = ring.len();
        j.ring = ring;
        self.rewrite_journal(j);
    }

    /// Appends the torn last line of the journal to <journal>.dropped
    /// (0600) and says so in one note. The line is never deleted.
    fn save_dropped_line(&self, path: &std::path::Path, line: &str) {
        let mut dropped = path.as_os_str().to_owned();
        dropped.push(".dropped");
        let dropped = PathBuf::from(dropped);
        let mut ok = false;
        self.write_data(|| {
            ok = std::fs::OpenOptions::new()
                .append(true)
                .create(true)
                .mode(0o600)
                .custom_flags(libc::O_NOFOLLOW)
                .open(&dropped)
                .and_then(|mut f| f.write_all(format!("{line}\n").as_bytes()))
                .is_ok();
        });
        let tail = if ok {
            format!("saved to {}.", dropped.display())
        } else {
            "could not be saved.".to_string()
        };
        self.note_later(format!(
            "The last line of the verdict journal could not be read. It is dropped and {tail}"
        ));
    }

    /// Writes the ring over the file, 0600, through a temporary file.
    fn rewrite_journal(&self, j: &mut Journal) {
        let mut body = String::new();
        for e in &j.ring {
            body.push_str(&gojson::marshal(&e.to_value()));
            body.push('\n');
        }
        let path = journal_path(self);
        let tmp = path.with_extension("jsonl.tmp");
        let mut ok = false;
        self.write_data(|| {
            make_dir(&path);
            ok = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)
                .and_then(|mut f| f.write_all(body.as_bytes()))
                .and_then(|_| std::fs::rename(&tmp, &path))
                .is_ok();
        });
        if ok {
            j.lines = j.ring.len();
        }
    }

    /// Adds one verdict a gate reported to the journal and its file.
    pub fn note_verdict(&self, pane: &str, v: &Verdict, mode: &str) {
        if pane.is_empty() || v.decision.is_empty() {
            return;
        }
        let e = Entry {
            at: Server::now_seconds(),
            pane: pane.to_string(),
            decision: v.decision.clone(),
            tool: v.tool.clone(),
            clause: v.clause.clone(),
            mode: mode.to_string(),
        };
        let mut j = self.journal_lock();
        self.append_journal(&mut j, e);
    }

    /// Adds a verdict read from a sprig session tree, which no gate
    /// reports. It carries the tree's own time. A verdict with no usable
    /// time (none, or one after the server's clock) is skipped, since the
    /// server cannot place it. It is also skipped when the journal already
    /// holds the same verdict: the same pane, time, decision, tool and
    /// clause. A server that restarts and reads the tree again so adds
    /// nothing twice. Nor does it add a verdict the quota pruned: when the
    /// pane holds its quota, a verdict older than the pane's oldest entry is
    /// skipped.
    pub(super) fn note_tree_verdict(&self, pane: &str, v: &super::facts::GateFact, mode: &str) {
        if pane.is_empty() || v.decision.is_empty() {
            return;
        }
        if v.at <= 0.0 || v.at > Server::now_seconds() {
            return;
        }
        let mut j = self.journal_lock();
        self.load_journal(&mut j);
        let mut held = 0;
        let mut oldest = v.at;
        for e in j.ring.iter().filter(|e| e.pane == pane) {
            if e.at == v.at && e.decision == v.decision && e.tool == v.tool && e.clause == v.clause
            {
                return;
            }
            if held == 0 || e.at < oldest {
                oldest = e.at;
            }
            held += 1;
        }
        // A pane at its quota that reads a verdict older than its oldest
        // entry is reading one the quota already pruned. Adding it would
        // push out a newer one.
        if held >= JOURNAL_PANE_KEPT && v.at < oldest {
            return;
        }
        let e = Entry {
            at: v.at,
            pane: pane.to_string(),
            decision: v.decision.clone(),
            tool: v.tool.clone(),
            clause: v.clause.clone(),
            mode: mode.to_string(),
        };
        self.append_journal(&mut j, e);
    }

    /// Adds e to the ring and the file. The caller holds the journal lock.
    fn append_journal(&self, j: &mut Journal, e: Entry) {
        self.load_journal(j);
        let mut line = gojson::marshal(&e.to_value());
        line.push('\n');
        journal_push(&mut j.ring, e);
        if j.lines + 1 > 2 * JOURNAL_KEPT {
            self.rewrite_journal(j);
            return;
        }
        let path = journal_path(self);
        let mut ok = false;
        self.write_data(|| {
            make_dir(&path);
            ok = std::fs::OpenOptions::new()
                .append(true)
                .create(true)
                .mode(0o600)
                .custom_flags(libc::O_NOFOLLOW)
                .open(&path)
                .and_then(|mut f| f.write_all(line.as_bytes()))
                .is_ok();
        });
        if ok {
            j.lines += 1;
        }
    }

    /// floor.journal: the last verdicts, newest first. pane keeps one
    /// agent's, since keeps those made after that unix time, and limit
    /// (absent or below 1 is JOURNAL_KEPT, above it too) keeps the newest.
    /// A pane gets only its own entries, whatever pane it asks for: a pane
    /// that asks for another's gets none. A plugin and a peer the server
    /// cannot place get none, even if their hello names a pane. An operator
    /// or a view gets all.
    fn handle_floor_journal(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let mut pane = r.str("pane").unwrap_or_default();
        let mut since = 0.0;
        // A null since is no since, as Go reads it.
        if let Some(raw) = r.raw("since").filter(|raw| *raw != "null") {
            match serde_json::from_str::<f64>(raw) {
                Ok(f) => since = f,
                Err(_) => {
                    return proto::err_resp(
                        &r.id,
                        proto::ERR_BAD_REQUEST,
                        "floor.journal since is a unix time in seconds.",
                    )
                }
            }
        }
        let limit = r.int("limit").unwrap_or(0);
        let ro = c.role();
        if ro.no_allow {
            // Only a real pane sees its own lines, by the rule of
            // chat_guard. A plugin or a peer the server cannot place may
            // say in its hello that it is a pane, and still gets nothing.
            let real = ro.pane && !ro.unknown && ro.plugin.is_empty() && !ro.pane_id.is_empty();
            if !real || (!pane.is_empty() && pane != ro.pane_id) {
                return proto::ok_resp(&r.id, gojson::map(vec![("entries", Value::Array(vec![]))]));
            }
            pane = ro.pane_id;
        }
        let out = self.journal_list(&pane, since, limit);
        proto::ok_resp(&r.id, gojson::map(vec![("entries", Value::Array(out))]))
    }

    /// The entries newest first. limit below 1 or above JOURNAL_KEPT is
    /// JOURNAL_KEPT.
    pub(crate) fn journal_list(&self, pane: &str, since: f64, limit: i64) -> Vec<Value> {
        let limit = if limit < 1 || limit > JOURNAL_KEPT as i64 {
            JOURNAL_KEPT as i64
        } else {
            limit
        };
        let mut j = self.journal_lock();
        self.load_journal(&mut j);
        let mut out = Vec::new();
        for e in j.ring.iter().rev() {
            if out.len() as i64 >= limit {
                break;
            }
            if (!pane.is_empty() && e.pane != pane) || (since > 0.0 && e.at <= since) {
                continue;
            }
            out.push(e.to_value());
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::server::Config;

    fn scratch(name: &str) -> std::path::PathBuf {
        let d =
            std::env::temp_dir().join(format!("coppice-rs-journal-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn server(dir: &std::path::Path) -> Arc<Server> {
        Server::new(Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.join("data"),
            start_dir: dir.to_path_buf(),
            voice: Default::default(),
            foreman: Default::default(),
        })
    }

    const GOOD_LINE: &str =
        r#"{"at":1.5,"pane":"w1:p1","decision":"allow","tool":"Bash","clause":"c0"}"#;

    fn verdict(n: usize, decision: &str) -> Verdict {
        Verdict {
            decision: decision.into(),
            tool: "Bash".into(),
            clause: format!("c{n}"),
        }
    }

    fn clauses(list: &[Value]) -> Vec<String> {
        list.iter()
            .map(|e| e["clause"].as_str().unwrap().to_string())
            .collect()
    }

    #[test]
    fn verdicts_list_newest_first_in_a_0600_file() {
        use std::os::unix::fs::PermissionsExt;
        let dir = scratch("order");
        let s = server(&dir);
        s.note_verdict("w1:p1", &verdict(1, "allow"), "enforcing");
        s.note_verdict("w1:p2", &verdict(2, "deny"), "");
        assert_eq!(clauses(&s.journal_list("", 0.0, 0)), ["c2", "c1"]);
        assert_eq!(clauses(&s.journal_list("w1:p1", 0.0, 0)), ["c1"]);
        assert_eq!(clauses(&s.journal_list("", 0.0, 1)), ["c2"]);
        assert!(s.journal_list("w9:p9", 0.0, 0).is_empty());
        assert!(s.journal_list("", 1e12, 0).is_empty());
        let first = &s.journal_list("w1:p1", 0.0, 0)[0];
        assert_eq!(first["mode"], "enforcing");
        assert!(s.journal_list("w1:p2", 0.0, 0)[0].get("mode").is_none());
        let mode = std::fs::metadata(dir.join("data/journal/verdicts.jsonl"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o600);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_journal_keeps_five_hundred_across_a_restart() {
        let dir = scratch("keep");
        let s = server(&dir);
        // Six panes, so no pane passes its quota of 100.
        for i in 1..=520 {
            s.note_verdict(&format!("w1:p{}", i % 6 + 1), &verdict(i, "allow"), "");
        }
        let list = s.journal_list("", 0.0, 0);
        assert_eq!(list.len(), 500);
        assert_eq!(clauses(&list)[0], "c520");
        assert_eq!(clauses(&list)[499], "c21");
        let file = dir.join("data/journal/verdicts.jsonl");
        assert!(std::fs::read_to_string(&file).unwrap().lines().count() <= 1000);
        s.close();
        let s2 = server(&dir);
        let list = s2.journal_list("", 0.0, 0);
        assert_eq!(list.len(), 500);
        assert_eq!(clauses(&list)[0], "c520");
        assert_eq!(std::fs::read_to_string(&file).unwrap().lines().count(), 500);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_broken_file_moves_aside_and_the_journal_starts_fresh() {
        let dir = scratch("broken");
        let file = dir.join("data/journal/verdicts.jsonl");
        std::fs::create_dir_all(file.parent().unwrap()).unwrap();
        std::fs::write(&file, format!("not json\n{GOOD_LINE}\n")).unwrap();
        let s = server(&dir);
        assert!(s.journal_list("", 0.0, 0).is_empty());
        let moved: Vec<_> = std::fs::read_dir(file.parent().unwrap())
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().contains(".broken-"))
            .collect();
        assert_eq!(moved.len(), 1);
        s.note_verdict("w1:p1", &verdict(1, "allow"), "");
        assert_eq!(s.journal_list("", 0.0, 0).len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The entries floor.journal gives a connection that said this in its
    /// hello, asking for this pane ("" for no pane).
    fn journal_for(s: &Arc<Server>, role: &str, own: &str, plugin: &str, ask: &str) -> Vec<String> {
        let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
        let (c, _rx) = Client::new(a);
        {
            let mut st = c.st.lock().unwrap();
            st.hello_role = role.into();
            st.hello_pane = own.into();
            st.hello_plugin = plugin.into();
        }
        let mut r = Request::default();
        r.id = "j".into();
        r.cmd = "floor.journal".into();
        if !ask.is_empty() {
            let raw = serde_json::value::RawValue::from_string(format!("{ask:?}")).unwrap();
            r.params.insert("pane".into(), raw);
        }
        let res = s.handle_floor_journal(&c, &r);
        assert!(res.ok, "{:?}", res.error);
        clauses(res.result.unwrap()["entries"].as_array().unwrap())
    }

    #[test]
    fn a_plugin_or_an_unknown_peer_that_claims_a_pane_reads_nothing() {
        let dir = scratch("claim");
        let s = server(&dir);
        s.note_verdict("w1:p2", &verdict(1, "deny"), "");
        for kind in ["plugin", "unknown"] {
            for ask in ["", "w1:p2"] {
                let (a, _b) = std::os::unix::net::UnixStream::pair().unwrap();
                let (c, _rx) = Client::new(a);
                {
                    let mut st = c.st.lock().unwrap();
                    st.hello_role = "pane".into();
                    st.hello_pane = "w1:p2".into();
                    if kind == "plugin" {
                        st.facts.plugin = "merge".into();
                    } else {
                        st.facts.unknown = true;
                    }
                }
                let mut r = Request::default();
                r.id = "j".into();
                r.cmd = "floor.journal".into();
                if !ask.is_empty() {
                    let raw = serde_json::value::RawValue::from_string(format!("{ask:?}")).unwrap();
                    r.params.insert("pane".into(), raw);
                }
                let res = s.handle_floor_journal(&c, &r);
                assert!(res.ok);
                let list = res.result.unwrap();
                assert!(
                    list["entries"].as_array().unwrap().is_empty(),
                    "{kind} read {list}"
                );
            }
        }
        assert_eq!(journal_for(&s, "pane", "w1:p2", "", ""), ["c1"]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_pane_and_a_plugin_read_only_their_own_entries() {
        let dir = scratch("scope");
        let s = server(&dir);
        s.note_verdict("w1:p1", &verdict(1, "allow"), "");
        s.note_verdict("w1:p2", &verdict(2, "allow"), "");
        s.note_verdict("w1:p1", &verdict(3, "allow"), "");
        assert_eq!(journal_for(&s, "operator", "", "", ""), ["c3", "c2", "c1"]);
        assert_eq!(journal_for(&s, "pane", "w1:p2", "", ""), ["c2"]);
        assert_eq!(journal_for(&s, "pane", "w1:p2", "", "w1:p2"), ["c2"]);
        assert!(journal_for(&s, "pane", "w1:p2", "", "w1:p1").is_empty());
        assert!(journal_for(&s, "plugin", "", "merge", "").is_empty());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn tree_verdicts_are_keyed_on_the_whole_tuple_and_need_a_time() {
        use crate::server::facts::GateFact;
        let dir = scratch("tree");
        let s = server(&dir);
        let v = GateFact {
            decision: "deny".into(),
            tool: "bash".into(),
            clause: "no-rm".into(),
            at: 4.0,
        };
        let with = |f: &dyn Fn(&mut GateFact)| {
            let mut x = v.clone();
            f(&mut x);
            x
        };
        s.note_tree_verdict("w1:p1", &v, "");
        s.note_tree_verdict("w1:p1", &v, "enforcing");
        assert_eq!(s.journal_list("", 0.0, 0).len(), 1);
        s.note_tree_verdict("w1:p1", &with(&|x| x.clause = "no-curl".into()), "");
        s.note_tree_verdict("w1:p1", &with(&|x| x.decision = "ask".into()), "");
        s.note_tree_verdict("w1:p1", &with(&|x| x.tool = "write".into()), "");
        s.note_tree_verdict("w1:p2", &v, "");
        assert_eq!(s.journal_list("", 0.0, 0).len(), 5);
        for at in [0.0, -3.0, 1e12] {
            s.note_tree_verdict(
                "w1:p1",
                &with(&|x| {
                    x.at = at;
                    x.clause = "no time".into();
                }),
                "",
            );
        }
        assert_eq!(s.journal_list("", 0.0, 0).len(), 5);
        s.close();
        let s2 = server(&dir);
        s2.note_tree_verdict("w1:p1", &v, "");
        assert_eq!(s2.journal_list("", 0.0, 0).len(), 5);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn tree_verdicts_the_pane_quota_pruned_stay_out_after_a_restart() {
        use crate::server::facts::GateFact;
        let dir = scratch("treequota");
        let s = server(&dir);
        let tree = |i: usize| GateFact {
            decision: "deny".into(),
            tool: "bash".into(),
            clause: format!("t{i}"),
            at: (1000 + i) as f64,
        };
        for i in 1..=110 {
            s.note_tree_verdict("w1:p1", &tree(i), "");
        }
        assert_eq!(s.journal_list("", 0.0, 0).len(), 100);
        for i in 1..=110 {
            s.note_tree_verdict("w1:p1", &tree(i), "");
        }
        let list = clauses(&s.journal_list("", 0.0, 0));
        assert_eq!(
            (list.len(), list[0].as_str(), list[99].as_str()),
            (100, "t110", "t11")
        );
        s.note_tree_verdict("w1:p1", &tree(111), "");
        let list = clauses(&s.journal_list("", 0.0, 0));
        assert_eq!((list.len(), list[0].as_str()), (100, "t111"));
        s.note_tree_verdict("w1:p2", &tree(1), "");
        assert_eq!(s.journal_list("w1:p2", 0.0, 0).len(), 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_pane_is_kept_to_its_quota_of_the_journal() {
        let dir = scratch("quota");
        let s = server(&dir);
        let note = |s: &Arc<Server>, pane: &str, clause: String| {
            let mut v = verdict(0, "allow");
            v.clause = clause;
            s.note_verdict(pane, &v, "");
        };
        for i in 1..=130 {
            note(&s, "w1:p1", format!("a{i}"));
            if i % 4 == 0 {
                note(&s, "w1:p2", format!("b{}", i / 4));
            }
        }
        let check = |s: &Arc<Server>, when: &str| {
            let a = clauses(&s.journal_list("w1:p1", 0.0, 0));
            let b = clauses(&s.journal_list("w1:p2", 0.0, 0));
            assert_eq!(
                (a.len(), a[0].as_str(), a[99].as_str()),
                (100, "a130", "a31"),
                "{when}"
            );
            assert_eq!((b.len(), b[31].as_str()), (32, "b1"), "{when}");
        };
        check(&s, "live");
        s.close();
        check(&server(&dir), "after a restart");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_line_is_a_verdict_by_the_same_strict_rule_as_in_go() {
        let good = [
            GOOD_LINE,
            r#"{"at":1,"pane":"p","decision":"allow"}"#,
            r#"{"at":1,"pane":"p","decision":"allow","tool":null,"clause":null,"mode":null}"#,
            r#"{"at":1e2,"pane":"p","decision":"ask","extra":[1,2]}"#,
        ];
        for l in good {
            assert!(Entry::from_line(l).is_some(), "rejected {l}");
        }
        let bad = [
            "",
            "null",
            "5",
            "[]",
            r#""x""#,
            "{",
            r#"{"pane":"p","decision":"allow"}"#,
            r#"{"at":null,"pane":"p","decision":"allow"}"#,
            r#"{"at":"1","pane":"p","decision":"allow"}"#,
            r#"{"AT":1,"pane":"p","decision":"allow"}"#,
            r#"{"at":1,"pane":5,"decision":"allow"}"#,
            r#"{"at":1,"pane":"","decision":"allow"}"#,
            r#"{"at":1,"pane":"p","decision":""}"#,
            r#"{"at":1,"pane":"p"}"#,
            r#"{"at":1,"pane":"p","decision":"allow","tool":7}"#,
            r#"{"at":1,"pane":"p","decision":"allow","clause":false}"#,
            r#"{"at":1,"pane":"p","decision":"allow","mode":{}}"#,
            r#"{"at":1,"pane":"p","decision":"allow"} x"#,
        ];
        for l in bad {
            assert!(Entry::from_line(l).is_none(), "accepted {l}");
        }
    }

    #[test]
    fn only_a_torn_last_line_is_dropped() {
        let dir = scratch("torn");
        let file = dir.join("data/journal/verdicts.jsonl");
        std::fs::create_dir_all(file.parent().unwrap()).unwrap();
        std::fs::write(
            &file,
            format!("{GOOD_LINE}\n{{\"at\":2.5,\"pane\":\"w1:p1\",\"deci"),
        )
        .unwrap();
        let s = server(&dir);
        assert_eq!(clauses(&s.journal_list("", 0.0, 0)), ["c0"]);
        let moved = std::fs::read_dir(file.parent().unwrap())
            .unwrap()
            .filter_map(|e| e.ok())
            .filter(|e| e.file_name().to_string_lossy().contains(".broken-"))
            .count();
        assert_eq!(moved, 0);
        let torn =
            std::fs::read_to_string(dir.join("data/journal/verdicts.jsonl.dropped")).unwrap();
        assert_eq!(torn, "{\"at\":2.5,\"pane\":\"w1:p1\",\"deci\n");
        let mut said = false;
        for _ in 0..100 {
            said = s.notes.lock().unwrap().iter().any(|n| {
                n["text"]
                    .as_str()
                    .is_some_and(|t| t.contains("verdicts.jsonl.dropped"))
            });
            if said {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(20));
        }
        assert!(said, "no note names the dropped file");
        s.note_verdict("w1:p1", &verdict(1, "deny"), "");
        let body = std::fs::read_to_string(&file).unwrap();
        assert_eq!(body.matches('\n').count(), 2);
        assert!(body.ends_with('\n'));
        assert_eq!(s.journal_list("", 0.0, 0).len(), 2);
        let _ = std::fs::remove_dir_all(&dir);
    }
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "floor.journal",
        Arc::new(|s, c, r| s.handle_floor_journal(c, r)),
    );
}
