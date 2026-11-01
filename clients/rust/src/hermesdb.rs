//! Reads the chats of Hermes Agent (Nous Research) from its state.db files
//! and logs them into a pando store. The twin of Go's internal/hermesdb. It
//! only reads: the file opens read-only and no Hermes file is written.

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::time::SystemTime;

use pando::chat::{Chat, Meta, KIND_STEWARD, KIND_USER};
use pando::live::{Live, Source};
use rusqlite::{Connection, OpenFlags};

/// The Hermes root: HERMES_HOME (or its grandparent when it is a profile
/// dir), else home/.hermes.
pub fn root(hermes_home: &str, home: &str) -> PathBuf {
    let h = hermes_home.trim();
    if !h.is_empty() {
        let p = Path::new(h);
        if p.parent().and_then(|x| x.file_name()).is_some_and(|n| n == "profiles") {
            if let Some(r) = p.parent().and_then(|x| x.parent()) {
                return r.to_path_buf();
            }
        }
        return p.to_path_buf();
    }
    Path::new(home).join(".hermes")
}

/// One Hermes profile with a state.db.
pub struct Profile {
    pub name: String,
    pub path: PathBuf,
}

/// The profiles under root that have a state.db: default first, then the
/// named ones in name order. name, when not empty, keeps one.
pub fn profiles(root: &Path, name: &str) -> Vec<Profile> {
    let mut out = Vec::new();
    let mut add = |n: &str, dir: &Path| {
        let p = dir.join("state.db");
        if p.is_file() && (name.is_empty() || name == n) {
            out.push(Profile { name: n.to_string(), path: p });
        }
    };
    add("default", root);
    let mut names: Vec<String> = std::fs::read_dir(root.join("profiles"))
        .map(|rd| {
            rd.flatten()
                .filter(|e| e.path().is_dir())
                .map(|e| e.file_name().to_string_lossy().into_owned())
                .collect()
        })
        .unwrap_or_default();
    names.sort();
    for n in names {
        add(&n, &root.join("profiles").join(&n));
    }
    out
}

/// The source over the profiles under root.
pub struct HermesSource {
    pub root: PathBuf,
    pub profile: String,
    /// For each profile, the id of the last message it logged. A profile with
    /// a cursor is read by id: a message committed late with an old time is
    /// not missed, and a profile that fails again and again is not read from
    /// the start every poll. A profile with none (its first poll) is read by
    /// time.
    pub cursors: HashMap<String, i64>,
}

impl HermesSource {
    pub fn new(root: PathBuf, profile: &str) -> Self {
        HermesSource { root, profile: profile.to_string(), cursors: HashMap::new() }
    }
}

fn columns(db: &Connection, table: &str) -> Result<HashSet<String>, String> {
    let mut st = db.prepare("SELECT name FROM pragma_table_info(?1)").map_err(|e| e.to_string())?;
    let rows = st.query_map([table], |r| r.get::<_, String>(0)).map_err(|e| e.to_string())?;
    let mut out = HashSet::new();
    for r in rows {
        out.insert(r.map_err(|e| e.to_string())?);
    }
    Ok(out)
}

type Row = (i64, String, String, String, f64, String, String, String, String, String);

/// Logs the new messages of one profile. It gives how many were new, the id of
/// the last message it handled (0 when none), whether the db holds ids below
/// the cursor (it was made again) and the error that stopped it.
fn poll_profile(
    c: &mut Chat,
    p: &Profile,
    since: Option<SystemTime>,
    cursor: Option<i64>,
) -> (usize, i64, bool, Option<String>) {
    let fail = |e: String| (0, 0, false, Some(e));
    let mut reset = false;
    let mut cursor = cursor;
    let db = match Connection::open_with_flags(&p.path, OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI) {
        Ok(d) => d,
        Err(e) => return fail(e.to_string()),
    };
    let _ = db.busy_timeout(std::time::Duration::from_secs(2));
    let mc = match columns(&db, "messages") {
        Ok(m) => m,
        Err(e) => return fail(e),
    };
    if mc.is_empty() {
        return (0, 0, false, None);
    }
    if let Some(cur) = cursor {
        match db.query_row("SELECT COALESCE(MAX(id), 0) FROM messages", [], |r| r.get::<_, i64>(0)) {
            Ok(top) if top < cur => {
                // The db was made again and its ids start over: the old
                // cursor would hide every new row. Read by time once.
                cursor = None;
                reset = true;
            }
            Ok(_) => {}
            Err(e) => return fail(e.to_string()),
        }
    }
    let sc = columns(&db, "sessions").unwrap_or_default();
    let scol = |n: &str| {
        if sc.is_empty() {
            "''".to_string()
        } else if sc.contains(n) {
            format!("COALESCE(s.{n}, '')")
        } else {
            "''".to_string()
        }
    };
    let mut filter = String::from(" WHERE m.role IN ('user','assistant') AND COALESCE(m.content,'') <> ''");
    if mc.contains("active") {
        filter.push_str(" AND m.active = 1");
    }
    if mc.contains("_compressed_summary") {
        filter.push_str(" AND m._compressed_summary = 0");
    }
    // A delegate_task child is a session with source 'subagent' (and, since
    // schema v16, a _delegate_from marker in model_config). It is a worker of
    // the steward, not the owner's chat: its rows are never logged.
    if sc.contains("source") {
        filter.push_str(" AND COALESCE(s.source, '') <> 'subagent'");
    }
    if sc.contains("model_config") {
        filter.push_str(" AND instr(COALESCE(s.model_config, ''), '_delegate_from') = 0");
    }
    let since_secs = since
        .and_then(|t| t.duration_since(SystemTime::UNIX_EPOCH).ok())
        .map(|d| d.as_secs() as f64);
    // One bound: the cursor when the profile has one, else the time.
    let bound: Option<rusqlite::types::Value> = match (cursor, since_secs) {
        (Some(id), _) => {
            filter.push_str(" AND m.id > ?1");
            Some(id.into())
        }
        (None, Some(t)) => {
            filter.push_str(" AND m.timestamp >= ?1");
            Some(t.into())
        }
        _ => None,
    };
    let join = if sc.is_empty() { "" } else { " LEFT JOIN sessions s ON s.id = m.session_id" };
    let q = format!(
        "SELECT m.id, m.session_id, m.role, m.content, m.timestamp, {}, {}, {}, {}, {} FROM messages m{join}{filter} ORDER BY m.id",
        scol("source"),
        scol("user_id"),
        scol("display_name"),
        scol("chat_id"),
        scol("thread_id"),
    );
    let mut st = match db.prepare(&q) {
        Ok(s) => s,
        Err(e) => return fail(e.to_string()),
    };
    let map = |r: &rusqlite::Row| -> rusqlite::Result<Row> {
        Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?, r.get(4)?, r.get(5)?, r.get(6)?, r.get(7)?, r.get(8)?, r.get(9)?))
    };
    // A row that cannot be read is an error, not a skipped row: the poll is
    // not clean and the cursor stays, so the row is tried again.
    let collected: Result<Vec<Row>, rusqlite::Error> = match &bound {
        Some(b) => st.query_map([b], map).and_then(|it| it.collect()),
        None => st.query_map([], map).and_then(|it| it.collect()),
    };
    let rows = match collected {
        Ok(r) => r,
        Err(e) => return fail(e.to_string()),
    };
    let mut n = 0;
    let mut last = 0;
    // A session a lead's plugin put on the store's session map belongs to
    // its branch: its rows go there, never into the store above (round 8b).
    let mut router = pando::branch::Router::new(c.dir(), c.now_clock());
    for (id, sess, role, text, at, source, user, display, _chat_id, thread) in rows {
        let platform = if source.is_empty() { "hermes".to_string() } else { source };
        let mut chat = sess.clone();
        if !thread.is_empty() {
            chat = format!("{chat}/{thread}");
        }
        let (kind, who, head) = if role == "user" {
            let w = [display.as_str(), user.as_str(), "user"].into_iter().find(|s| !s.is_empty()).unwrap_or("user").to_string();
            (KIND_USER, w.clone(), format!("hermes user {w}"))
        } else {
            (KIND_STEWARD, p.name.clone(), "hermes assistant".to_string())
        };
        let live = Live {
            kind: kind.into(),
            text: format!("{head} in {platform} (profile {}): {text}", p.name),
            at: Some(SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(at.max(0.0) as u64)),
            act: format!("hermes:{}:{sess}:{id}", p.name),
            meta: Meta { src: format!("hermes:{}", p.name), chat: format!("{platform}:{chat}"), who },
        };
        let branch = router.target(&format!("hermes:{sess}"), "", "");
        let r = if branch.is_empty() {
            c.append_live_msg(&live)
        } else {
            router.chat(&branch).and_then(|b| b.append_live_msg(&live))
        };
        match r {
            Ok(true) => n += 1,
            Ok(false) => {}
            Err(e) => return (n, last, reset, Some(e)),
        }
        last = id;
    }
    (n, last, reset, None)
}

impl Source for HermesSource {
    fn name(&self) -> String {
        if self.profile.is_empty() {
            "hermes".into()
        } else {
            format!("hermes:{}", self.profile)
        }
    }

    fn poll(&mut self, c: &mut Chat, since: Option<SystemTime>) -> Result<usize, String> {
        let mut total = 0;
        let mut errs = Vec::new();
        for p in profiles(&self.root, &self.profile) {
            let cur = self.cursors.get(&p.name).copied();
            let (n, last, reset, err) = poll_profile(c, &p, since, cur);
            if reset {
                // Take the new ones by time once, and keep the id of the
                // last as the cursor from here on.
                if last > 0 {
                    self.cursors.insert(p.name.clone(), last);
                } else {
                    self.cursors.remove(&p.name);
                }
            } else if last > 0 && cur.map_or(true, |c0| last > c0) {
                self.cursors.insert(p.name.clone(), last);
            }
            total += n;
            if let Some(e) = err {
                errs.push(format!("{}: {e}", p.name));
            }
        }
        if errs.is_empty() {
            Ok(total)
        } else {
            Err(errs.join("; "))
        }
    }

    fn cursors(&self) -> HashMap<String, i64> {
        self.cursors.clone()
    }

    fn set_cursors(&mut self, cursors: HashMap<String, i64>) {
        self.cursors = cursors;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use pando::chat::Config;

    fn mkdb(dir: &Path, stmts: &[&str]) {
        std::fs::create_dir_all(dir).unwrap();
        let db = Connection::open(dir.join("state.db")).unwrap();
        for s in [
            "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, display_name TEXT, chat_id TEXT, thread_id TEXT, started_at REAL NOT NULL DEFAULT 0)",
            "CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_calls TEXT, timestamp REAL NOT NULL, _compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1)",
        ]
        .iter()
        .chain(stmts.iter())
        {
            db.execute(s, []).unwrap();
        }
    }

    fn tmp(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("hermesdb-t-{}-{tag}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn store(root: &Path) -> Chat {
        Chat::open(Config { dir: root.join("pando"), ..Default::default() }).unwrap()
    }

    #[test]
    fn polls_every_profile_once_with_who_and_where() {
        let root = tmp("every");
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','steward question',1000)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','steward answer',1001)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','tool','tool noise',1002)",
            "INSERT INTO messages (session_id, role, content, timestamp, active) VALUES ('s1','user','rewound',1003,0)",
        ]);
        mkdb(&root.join("profiles").join("household"), &[
            "INSERT INTO sessions VALUES ('m1','mattermost','u9','Dana','chan1','th7',1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('m1','user','buy milk',2000)",
        ]);
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        assert_eq!(s.poll(&mut c, None), Ok(3));
        assert_eq!(s.poll(&mut c, None), Ok(0));
        let mut found = false;
        for i in 0..c.count() {
            let m = c.message(i).unwrap();
            if m.who == "Dana" {
                found = true;
                assert!(m.kind == KIND_USER && m.src == "hermes:household" && m.chat.contains("mattermost") && m.chat.contains("th7") && m.text.contains("buy milk"), "{m:?}");
            }
            assert!(!m.text.contains("tool noise") && !m.text.contains("rewound"));
        }
        assert!(found);
        c.close();
        // A time bound keeps old rows out; one profile can be chosen.
        let mut c2 = store(&tmp("every2"));
        let far = SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(3000);
        assert_eq!(HermesSource::new(root.clone(), "household").poll(&mut c2, Some(far)), Ok(0));
        assert_eq!(HermesSource::new(root.clone(), "household").poll(&mut c2, None), Ok(1));
        c2.close();
    }

    #[test]
    fn a_bad_file_errors_and_is_never_written() {
        let root = tmp("bad");
        std::fs::write(root.join("state.db"), "not sqlite").unwrap();
        let mut c = store(&root);
        let r = HermesSource::new(root.clone(), "").poll(&mut c, None);
        assert!(r.is_err(), "a bad file must be an error, not a clean 0: {r:?}");
        assert_eq!(std::fs::read_to_string(root.join("state.db")).unwrap(), "not sqlite");
        c.close();
    }

    #[test]
    fn a_row_that_cannot_be_read_is_an_error_and_is_tried_again() {
        let root = tmp("row");
        // content stored as a BLOB cannot be read as text.
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user',x'ff00',1000)",
        ]);
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        let r = s.poll(&mut c, None);
        assert!(r.is_err(), "{r:?}");
        assert!(s.cursors().get("default").is_none(), "the cursor must stay so the row is tried again");
        c.close();
    }

    #[test]
    fn a_cursor_takes_a_late_row_with_an_old_time_and_one_bad_profile_stalls_only_itself() {
        let root = tmp("cursor");
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','first',5000)",
        ]);
        mkdb(&root.join("profiles").join("household"), &[
            "INSERT INTO sessions VALUES ('m1','mattermost','u9','Dana','c','',1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('m1','user','hello',5000)",
        ]);
        let bad = root.join("profiles").join("broken");
        std::fs::create_dir_all(&bad).unwrap();
        std::fs::write(bad.join("state.db"), "not sqlite").unwrap();
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        let r = s.poll(&mut c, Some(SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(4000)));
        assert!(r.as_ref().is_err_and(|e| e.contains("broken")), "{r:?}");
        let cur = s.cursors();
        assert!(cur["default"] > 0 && cur["household"] > 0 && !cur.contains_key("broken"), "{cur:?}");
        // A late row with an old time is still taken, by id.
        let db = Connection::open(root.join("state.db")).unwrap();
        db.execute("INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','late answer',10)", []).unwrap();
        drop(db);
        let far = SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(900_000);
        let n = s.poll(&mut c, Some(far));
        assert!(n.is_err());
        assert_eq!(c.count(), 3, "the late row was missed");
        // A new source with the saved cursors starts where the old one stopped.
        let mut s2 = HermesSource::new(root.clone(), "");
        s2.set_cursors(s.cursors());
        let _ = s2.poll(&mut c, None);
        assert_eq!(c.count(), 3, "a restart read again");
        c.close();
    }

    // A Hermes whose state.db was made again starts its ids at 1. With a
    // cursor from the old file, every new row would sit below it for good.
    #[test]
    fn a_recreated_db_is_read_again_from_the_start() {
        let root = tmp("recreated");
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','one',5000)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','two',5001)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','three',5002)",
        ]);
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        let t = |n: u64| Some(SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(n));
        assert_eq!(s.poll(&mut c, t(4000)).unwrap(), 3);
        assert_eq!(s.cursors().get("default"), Some(&3));
        // Hermes starts over: a new file, new session, ids from 1.
        std::fs::remove_file(root.join("state.db")).unwrap();
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('s2','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','user','fresh one',6000)",
        ]);
        assert_eq!(s.poll(&mut c, t(5500)).unwrap(), 1, "the new db was not read");
        assert_eq!(s.cursors().get("default"), Some(&1), "the cursor stayed at the old file's id");
        // And it goes on by id from there.
        let db = Connection::open(root.join("state.db")).unwrap();
        db.execute("INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','assistant','fresh two',6001)", []).unwrap();
        drop(db);
        assert_eq!(s.poll(&mut c, t(9000)).unwrap(), 1, "the next row was missed");
        c.close();
    }

    #[test]
    fn a_subagent_session_is_never_logged() {
        let root = tmp("subagent");
        mkdb(&root, &[
            "ALTER TABLE sessions ADD COLUMN model_config TEXT",
            "INSERT INTO sessions (id, source) VALUES ('s1','cli')",
            "INSERT INTO sessions (id, source) VALUES ('c1','subagent')",
            "INSERT INTO sessions (id, source, model_config) VALUES ('c2','cli','{\"_delegate_from\":\"s1\"}')",
            "INSERT INTO sessions (id, source, model_config) VALUES ('s2','cli','{\"model\":\"x\"}')",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','owner question',1000)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('c1','user','child task by source',1001)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('c2','assistant','child task by marker',1002)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','assistant','owner answer',1003)",
        ]);
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        assert_eq!(s.poll(&mut c, None), Ok(2));
        for i in 0..c.count() {
            assert!(!c.message(i).unwrap().text.contains("child task"));
        }
        c.close();
    }

    // Round 8b: a Hermes session on the store's session map is logged into
    // its branch, from its first row, and never into the store above.
    #[test]
    fn a_mapped_hermes_session_goes_to_its_branch() {
        let root = tmp("branch");
        mkdb(&root, &[
            "INSERT INTO sessions VALUES ('lead','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO sessions VALUES ('own','cli',NULL,NULL,NULL,NULL,1)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('lead','user','the lead plans the reader',1000)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('own','user','the owner asks at the root',1001)",
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('lead','assistant','reader plan done',1002)",
        ]);
        pando::branch::add_route(&root.join("pando"), "trellis", &["hermes:lead"]).unwrap();
        let mut c = store(&root);
        let mut s = HermesSource::new(root.clone(), "");
        assert_eq!(s.poll(&mut c, None), Ok(3));
        assert_eq!(c.count(), 1);
        assert!(c.message(0).unwrap().text.contains("owner asks"));
        c.close();
        let b = Chat::open_read_only(&root.join("pando").join("branches").join("trellis")).unwrap();
        assert_eq!(b.count(), 2);
        assert!(b.message(0).unwrap().text.contains("the lead plans"));
    }
}
