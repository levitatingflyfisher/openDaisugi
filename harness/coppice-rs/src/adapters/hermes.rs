//! Reads the conversation of a Hermes Agent session (Nous Research's
//! hermes-agent) so the server can show it as messages. Go's
//! internal/adapters/hermes is the oracle.
//!
//! Hermes keeps its sessions in one SQLite file, state.db, in its home:
//! $HERMES_HOME, else ~/.hermes, and for a named profile
//! ~/.hermes/profiles/<name>. This module only reads: it opens the file
//! read-only and never writes a Hermes file.

use std::collections::BTreeMap;
use std::path::Path;
use std::time::Duration;

use rusqlite::{Connection, OpenFlags};

/// How many parent sessions `messages` follows.
const CHAIN_MAX: usize = 8;

/// One message row of a session.
#[derive(Clone, Debug)]
pub struct Row {
    pub id: i64,
    pub session: String,
    pub role: String,
    pub content: String,
    pub tool_calls: String,
    pub at: f64,
}

/// The value of the flag spelled short or long in argv: "-p v",
/// "--profile v" and "--profile=v".
fn flag_value(argv: &[String], short: &str, long: &str) -> Option<String> {
    let mut i = 1;
    while i < argv.len() {
        let a = &argv[i];
        if a == "--" {
            return None;
        }
        if a == short || a == long {
            return argv.get(i + 1).cloned();
        }
        if let Some(v) = a.strip_prefix(&format!("{long}=")) {
            return Some(v.to_string());
        }
        i += 1;
    }
    None
}

/// The Hermes home a pane started with argv and env reads. The root is
/// HERMES_HOME of env, else user_home/.hermes; when HERMES_HOME is itself a
/// profile directory (its parent is "profiles") the root is two levels up,
/// as Hermes finds it. -p/--profile NAME picks <root>/profiles/NAME. With no
/// flag, a HERMES_HOME that names a profile stands, else the root's
/// active_profile file names the profile, else the root. It is "" when a
/// profile name is not a plain name.
pub fn home(argv: &[String], env: &BTreeMap<String, String>, user_home: &str) -> String {
    let mut root = format!("{}/.hermes", user_home.trim_end_matches('/'));
    let env_home = env.get("HERMES_HOME").map(|h| h.trim().to_string()).unwrap_or_default();
    if !env_home.is_empty() {
        root = env_home.clone();
        let p = Path::new(&env_home);
        if let Some(parent) = p.parent() {
            if parent.file_name().is_some_and(|n| n == "profiles") {
                root = parent.parent().map(|g| g.to_string_lossy().into_owned()).unwrap_or_default();
            }
        }
    }
    let flag = flag_value(argv, "-p", "--profile");
    let mut name = flag.clone().unwrap_or_default();
    if flag.is_none() && !env_home.is_empty() && root != env_home {
        return env_home;
    }
    if flag.is_none() {
        // The sticky profile: `hermes profile use NAME` writes it.
        if let Ok(b) = std::fs::read_to_string(Path::new(&root).join("active_profile")) {
            name = b.trim_start_matches('\u{feff}').trim().to_string();
        }
    }
    if name.is_empty() || name == "default" {
        if flag.is_some() && name.is_empty() {
            return String::new();
        }
        return root;
    }
    if name == "." || name == ".." || name.contains(['/', '\\']) {
        return String::new();
    }
    format!("{root}/profiles/{name}")
}

/// The state.db under a Hermes home.
pub fn state_path(home: &str) -> String {
    format!("{}/state.db", home.trim_end_matches('/'))
}

/// The words after the flag end at a subcommand, as Hermes ends them.
const SUBCOMMANDS: [&str; 17] = [
    "chat", "model", "gateway", "setup", "login", "logout", "auth", "status", "cron", "doctor", "config",
    "skills", "tools", "mcp", "sessions", "update", "profile",
];

/// The session id or title argv's -r/--resume or -c/--continue names, or
/// "". Hermes joins the words after the flag up to the next flag, so
/// `-c Pokemon Agent Dev` is one name. A bare -c names no session.
pub fn session_name(argv: &[String]) -> String {
    let mut i = 1;
    while i < argv.len() {
        let a = &argv[i];
        if a == "--" {
            return String::new();
        }
        for long in ["--resume=", "--continue="] {
            if let Some(v) = a.strip_prefix(long) {
                return v.to_string();
            }
        }
        if matches!(a.as_str(), "-r" | "--resume" | "-c" | "--continue") {
            let mut parts: Vec<&str> = Vec::new();
            let mut j = i + 1;
            while j < argv.len() && !argv[j].starts_with('-') && !SUBCOMMANDS.contains(&argv[j].as_str()) {
                parts.push(&argv[j]);
                j += 1;
            }
            return parts.join(" ");
        }
        i += 1;
    }
    String::new()
}

/// What Hermes scopes a resume to: the git repository root that holds dir,
/// else dir itself.
pub fn workspace_key(dir: &str) -> String {
    if dir.is_empty() {
        return String::new();
    }
    let mut d = Path::new(dir);
    loop {
        if d.join(".git").exists() {
            return d.to_string_lossy().into_owned();
        }
        match d.parent() {
            Some(up) if up != d => d = up,
            _ => return dir.to_string(),
        }
    }
}

/// Opens path read-only. Ok(None) when the file is missing.
fn open(path: &str) -> Result<Option<Connection>, ()> {
    if !Path::new(path).exists() {
        return Ok(None);
    }
    let c = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX)
        .map_err(|_| ())?;
    let _ = c.busy_timeout(Duration::from_millis(2000));
    Ok(Some(c))
}

fn columns(c: &Connection, table: &str) -> Result<Vec<String>, ()> {
    let mut st = c.prepare("SELECT name FROM pragma_table_info(?1)").map_err(|_| ())?;
    let rows = st.query_map([table], |r| r.get::<_, String>(0)).map_err(|_| ())?;
    rows.collect::<Result<Vec<_>, _>>().map_err(|_| ())
}

/// The sources of a terminal run (newer Hermes tags its terminal chat
/// "local"): Hermes resumes among these for -c.
const CLI_FAMILY: [&str; 3] = ["cli", "oneshot", "local"];

/// The session a pane that resumes shows, chosen the way `hermes -c`
/// chooses: the one name (id or title) the flags give, else the newest
/// session of the terminal family that has a message, scoped to workspace
/// first and then unscoped. An archived or hidden session is left out when
/// the columns exist. "" when the file is missing or holds no such session.
pub fn pick(path: &str, name: &str, workspace: &str) -> Result<String, ()> {
    let Some(c) = open(path)? else { return Ok(String::new()) };
    let sc = columns(&c, "sessions")?;
    if sc.is_empty() {
        return Ok(String::new());
    }
    let has = |n: &str| sc.iter().any(|x| x == n);
    if !name.is_empty() {
        if let Some(id) = by_name(&c, &sc, name) {
            return Ok(id);
        }
    }
    let filter = terminal_filter(&sc);
    let query = |extra: &str, args: &[&str]| -> Result<String, ()> {
        let q = format!(
            "SELECT s.id FROM sessions s JOIN messages m ON m.session_id = s.id WHERE 1=1{filter}{extra} \
             GROUP BY s.id ORDER BY MAX(m.timestamp) DESC, s.started_at DESC LIMIT 1"
        );
        let mut st = c.prepare(&q).map_err(|_| ())?;
        let ids = st
            .query_map(rusqlite::params_from_iter(args.iter()), |r| r.get::<_, Option<String>>(0))
            .map_err(|_| ())?
            .collect::<Result<Vec<_>, _>>()
            .map_err(|_| ())?;
        Ok(ids.into_iter().flatten().next().unwrap_or_default())
    };
    if !workspace.is_empty() && (has("cwd") || has("git_repo_root")) {
        let cols: Vec<&str> = ["cwd", "git_repo_root"].into_iter().filter(|x| has(x)).collect();
        let conds: Vec<String> = cols.iter().map(|x| format!("s.{x} = ?")).collect();
        let args: Vec<&str> = cols.iter().map(|_| workspace).collect();
        let id = query(&format!(" AND ({})", conds.join(" OR ")), &args)?;
        if !id.is_empty() {
            return Ok(id);
        }
    }
    query("", &[])
}

/// The WHERE terms that keep a session of the terminal family and leave out
/// an archived or hidden one.
fn terminal_filter(sc: &[String]) -> String {
    let has = |n: &str| sc.iter().any(|x| x == n);
    let mut filter = String::new();
    for col in ["archived", "hidden"] {
        if has(col) {
            filter.push_str(&format!(" AND s.{col} = 0"));
        }
    }
    if has("source") {
        filter.push_str(&format!(" AND s.source IN ('{}')", CLI_FAMILY.join("','")));
    }
    filter
}

/// How many seconds before the prompt a session row may carry: the two
/// clocks are read at different moments.
pub const PROMPT_WINDOW: f64 = 5.0;
/// How many seconds after the prompt Hermes may take to write its row.
pub const PROMPT_SPAN: f64 = 90.0;

/// Folds runs of white space, so a prompt typed in a terminal matches the
/// text Hermes stored.
fn squash(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// The session of a pane that has sent its first prompt: the terminal
/// session whose first user message is that prompt, written between
/// PROMPT_WINDOW seconds before `at` and PROMPT_SPAN seconds after it.
/// Hermes writes a session row at the first prompt, so the row is the pane's
/// own when the text and the moment agree. Of several matches the one
/// written closest to `at` wins, so two panes that send the same words each
/// get their own. A session whose tip is in `taken` (the tips other panes
/// hold) and a session that has a parent are passed over. "" when none
/// matches.
pub fn pick_by_prompt(path: &str, prompt: &str, at: f64, taken: &[String]) -> Result<String, ()> {
    let want = squash(prompt);
    if want.is_empty() {
        return Ok(String::new());
    }
    pick_first(path, at, taken, |c| squash(c) == want, |a, b| a.gap.partial_cmp(&b.gap))
}

/// The fallback for a pane whose prompt text matched no session (the
/// terminal showed one thing, Hermes stored another). It is the terminal
/// session of the profile whose first user message was written in the same
/// window as `pick_by_prompt` reads, the closest to the prompt first, that no
/// other pane holds. A session whose first message is in `skip` (squashed) is left for
/// the pane that typed it. "" when none is free.
pub fn pick_by_time(path: &str, at: f64, taken: &[String], skip: &[String]) -> Result<String, ()> {
    pick_first(path, at, taken, |c| !skip.contains(&squash(c)), |a, b| a.gap.partial_cmp(&b.gap))
}

struct FirstMsg {
    id: String,
    ts: f64,
    gap: f64,
}

/// Lists the parentless terminal sessions whose first user message is
/// stamped inside the prompt window around `at`, keeps those whose text
/// passes `ok`, orders them by `cmp` and returns the first one no other pane
/// holds.
fn pick_first(
    path: &str,
    at: f64,
    taken: &[String],
    ok: impl Fn(&str) -> bool,
    cmp: impl Fn(&FirstMsg, &FirstMsg) -> Option<std::cmp::Ordering>,
) -> Result<String, ()> {
    if at <= 0.0 {
        return Ok(String::new());
    }
    let Some(c) = open(path)? else { return Ok(String::new()) };
    let sc = columns(&c, "sessions")?;
    if sc.is_empty() {
        return Ok(String::new());
    }
    let mut filter = terminal_filter(&sc);
    if sc.iter().any(|x| x == "parent_session_id") {
        filter.push_str(" AND s.parent_session_id IS NULL");
    }
    let q = format!(
        "SELECT s.id, m.content, m.timestamp FROM sessions s \
         JOIN messages m ON m.id = (SELECT MIN(f.id) FROM messages f WHERE f.session_id = s.id AND f.role = 'user') \
         WHERE m.timestamp >= ?1 AND m.timestamp <= ?2{filter}"
    );
    let mut st = c.prepare(&q).map_err(|_| ())?;
    let rows = st
        .query_map(rusqlite::params![at - PROMPT_WINDOW, at + PROMPT_SPAN], |r| {
            Ok((r.get::<_, Option<String>>(0)?, r.get::<_, Option<String>>(1)?, r.get::<_, f64>(2)?))
        })
        .map_err(|_| ())?
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| ())?;
    let mut cands: Vec<FirstMsg> = rows
        .into_iter()
        .filter_map(|(id, content, ts)| {
            let id = id?;
            ok(&content.unwrap_or_default()).then(|| FirstMsg { id, ts, gap: (ts - at).abs() })
        })
        .collect();
    cands.sort_by(|a, b| cmp(a, b).unwrap_or(std::cmp::Ordering::Equal));
    for m in cands {
        if taken.is_empty() || !taken.contains(&tip_in(&c, &sc, &m.id)) {
            return Ok(m.id);
        }
    }
    Ok(String::new())
}

/// Follows a compression from session `id` to the live session that
/// replaced it. It is `id` when none did.
pub fn tip(path: &str, id: &str) -> String {
    let Ok(Some(c)) = open(path) else { return id.to_string() };
    let Ok(sc) = columns(&c, "sessions") else { return id.to_string() };
    tip_in(&c, &sc, id)
}

/// `tip` for several ids with one open of the database, in the order given.
/// Each id is its own tip when the database cannot be read.
pub fn tips(path: &str, ids: &[String]) -> Vec<String> {
    if ids.is_empty() {
        return Vec::new();
    }
    let Ok(Some(c)) = open(path) else { return ids.to_vec() };
    let Ok(sc) = columns(&c, "sessions") else { return ids.to_vec() };
    ids.iter().map(|id| tip_in(&c, &sc, id)).collect()
}

/// `tip` on an open database.
fn tip_in(c: &Connection, sc: &[String], id: &str) -> String {
    if !sc.iter().any(|x| x == "end_reason") {
        return id.to_string();
    }
    let mut cur = id.to_string();
    for _ in 0..CHAIN_MAX * 2 {
        let next: Option<String> = c
            .query_row(
                "SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id \
                 WHERE p.id = ?1 AND p.end_reason = 'compression' ORDER BY c.started_at DESC LIMIT 1",
                [cur.as_str()],
                |r| r.get(0),
            )
            .ok()
            .flatten();
        match next {
            Some(n) if !n.is_empty() => cur = n,
            _ => break,
        }
    }
    cur
}

/// Resolves an id or a title to a session id, and follows a compression
/// forward to its live tip.
fn by_name(c: &Connection, sc: &[String], name: &str) -> Option<String> {
    let has = |n: &str| sc.iter().any(|x| x == n);
    let one = |q: &str| -> Option<String> {
        c.query_row(q, [name], |r| r.get::<_, Option<String>>(0)).ok().flatten()
    };
    let mut cur = one("SELECT id FROM sessions WHERE id = ?1");
    if cur.is_none() && has("title") {
        cur = one("SELECT id FROM sessions WHERE title = ?1 ORDER BY started_at DESC LIMIT 1");
    }
    let mut cur = cur?;
    if !has("end_reason") {
        return Some(cur);
    }
    for _ in 0..CHAIN_MAX * 2 {
        let next: Option<String> = c
            .query_row(
                "SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id \
                 WHERE p.id = ?1 AND p.end_reason = 'compression' ORDER BY c.started_at DESC LIMIT 1",
                [cur.as_str()],
                |r| r.get(0),
            )
            .ok()
            .flatten();
        match next {
            Some(n) if !n.is_empty() => cur = n,
            _ => break,
        }
    }
    Some(cur)
}

/// The newest `limit` message rows of `session` and of its parent
/// sessions, oldest first, and whether older rows were left out. A row
/// Hermes marks inactive or as a compression summary is left out.
pub fn messages(path: &str, session: &str, limit: usize) -> Result<(Vec<Row>, bool), ()> {
    let Some(c) = open(path)? else { return Ok((Vec::new(), false)) };
    let mc = columns(&c, "messages")?;
    if mc.is_empty() {
        return Ok((Vec::new(), false));
    }
    let has = |n: &str| mc.iter().any(|x| x == n);
    let mut chain = vec![session.to_string()];
    while chain.len() < CHAIN_MAX {
        let parent: Option<String> = c
            .query_row(
                "SELECT parent_session_id FROM sessions WHERE id = ?1",
                [chain.last().unwrap().as_str()],
                |r| r.get(0),
            )
            .unwrap_or(None);
        match parent {
            Some(p) if !p.is_empty() && !chain.contains(&p) => chain.push(p),
            _ => break,
        }
    }
    let tool_calls = if has("tool_calls") { "tool_calls" } else { "NULL" };
    let mut filter = String::new();
    if has("active") {
        filter.push_str(" AND active = 1");
    }
    if has("_compressed_summary") {
        filter.push_str(" AND _compressed_summary = 0");
    }
    let marks = vec!["?"; chain.len()].join(",");
    let q = format!(
        "SELECT id, session_id, role, COALESCE(content, ''), COALESCE({tool_calls}, ''), timestamp \
         FROM messages WHERE session_id IN ({marks}){filter} ORDER BY id DESC LIMIT {}",
        limit + 1
    );
    let mut st = c.prepare(&q).map_err(|_| ())?;
    let it = st
        .query_map(rusqlite::params_from_iter(chain.iter()), |r| {
            Ok(Row {
                id: r.get(0)?,
                session: r.get(1)?,
                role: r.get(2)?,
                content: r.get(3)?,
                tool_calls: r.get(4)?,
                at: r.get(5)?,
            })
        })
        .map_err(|_| ())?;
    let mut rows = it.collect::<Result<Vec<_>, _>>().map_err(|_| ())?;
    let mut cut = false;
    if rows.len() > limit {
        rows.truncate(limit);
        cut = true;
    }
    rows.reverse();
    Ok((rows, cut))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture(dir: &Path) -> String {
        let path = dir.join("state.db").to_string_lossy().into_owned();
        let c = Connection::open(&path).unwrap();
        c.execute_batch(
            "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, parent_session_id TEXT,
               started_at REAL NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
             CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
               role TEXT NOT NULL, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL,
               _compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1);",
        )
        .unwrap();
        path
    }

    fn scratch(name: &str) -> std::path::PathBuf {
        let d = std::env::temp_dir().join(format!("hermes-{}-{}", name, std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    fn s(v: &[&str]) -> Vec<String> {
        v.iter().map(|x| x.to_string()).collect()
    }

    #[test]
    fn home_follows_the_profile_flag() {
        let none = BTreeMap::new();
        assert_eq!(home(&s(&["/x/hermes", "-p", "steward", "-c"]), &none, "/home/o"), "/home/o/.hermes/profiles/steward");
        assert_eq!(home(&s(&["hermes", "--profile=steward"]), &none, "/home/o"), "/home/o/.hermes/profiles/steward");
        assert_eq!(home(&s(&["hermes", "--profile", "default"]), &none, "/home/o"), "/home/o/.hermes");
        assert_eq!(home(&s(&["hermes", "-c"]), &none, "/home/o"), "/home/o/.hermes");
        let mut env = BTreeMap::new();
        env.insert("HERMES_HOME".to_string(), "/s/h".to_string());
        assert_eq!(home(&s(&["hermes", "-c"]), &env, "/home/o"), "/s/h");
        assert_eq!(home(&s(&["hermes", "-p", "../x"]), &none, "/home/o"), "");
        assert_eq!(session_name(&s(&["hermes", "-r", "s9", "-c"])), "s9");
        assert_eq!(session_name(&s(&["hermes", "--resume=s8"])), "s8");
        assert_eq!(session_name(&s(&["hermes", "-c"])), "");
        assert_eq!(session_name(&s(&["hermes", "-p", "steward", "-c"])), "");
        assert_eq!(session_name(&s(&["hermes", "-c", "-p", "steward"])), "");
        assert_eq!(session_name(&s(&["hermes", "-c", "Pokemon", "Agent", "Dev", "-p", "x"])), "Pokemon Agent Dev");
        assert_eq!(session_name(&s(&["hermes", "--continue=night shift"])), "night shift");
        assert_eq!(session_name(&s(&["hermes", "-c", "chat"])), "");
    }

    #[test]
    fn pick_prefers_the_terminal_and_messages_walk_the_chain() {
        let d = scratch("pick");
        let p = fixture(&d);
        let c = Connection::open(&p).unwrap();
        c.execute_batch(
            "INSERT INTO sessions (id, source, started_at) VALUES ('old','cli',100),('new','cli',200),('chat','mattermost',300);
             INSERT INTO sessions (id, source, parent_session_id, started_at) VALUES ('tip','cli','old',250);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES
               ('old','user','a',101),('old','assistant','b',102),('new','user','c',201),('chat','user','d',301),('tip','user','e',251);
             UPDATE messages SET active = 0 WHERE content = 'a';",
        )
        .unwrap();
        drop(c);
        assert_eq!(pick(&p, "", "").unwrap(), "tip");
        assert_eq!(pick(&p, "old", "").unwrap(), "old");
        let (rows, cut) = messages(&p, "tip", 100).unwrap();
        assert!(!cut);
        let texts: Vec<_> = rows.iter().map(|r| r.content.as_str()).collect();
        assert_eq!(texts, ["b", "e"]);
        let (rows, cut) = messages(&p, "tip", 1).unwrap();
        assert!(cut && rows.len() == 1 && rows[0].content == "e");
        assert_eq!(pick(&format!("{}/none.db", d.display()), "", "").unwrap(), "");
    }

    #[test]
    fn pick_knows_only_the_terminal_family() {
        let d = scratch("family");
        let p = fixture(&d);
        let c = Connection::open(&p).unwrap();
        c.execute_batch(
            "INSERT INTO sessions (id, source, started_at) VALUES ('only','mattermost',5);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES ('only','user','hi',6);",
        )
        .unwrap();
        assert_eq!(pick(&p, "", "").unwrap(), "");
        c.execute_batch(
            "INSERT INTO sessions (id, source, started_at) VALUES ('once','oneshot',7);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES ('once','user','hi',8);",
        )
        .unwrap();
        assert_eq!(pick(&p, "", "").unwrap(), "once");
        c.execute_batch(
            "INSERT INTO sessions (id, source, started_at) VALUES ('here','local',9);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES ('here','user','hi',10);",
        )
        .unwrap();
        assert_eq!(pick(&p, "", "").unwrap(), "here");
    }

    #[test]
    fn pick_prefers_the_workspace_and_leaves_out_a_hidden_session() {
        let d = scratch("ws");
        let p = fixture(&d);
        let c = Connection::open(&p).unwrap();
        c.execute_batch(
            "ALTER TABLE sessions ADD COLUMN cwd TEXT;
             ALTER TABLE sessions ADD COLUMN git_repo_root TEXT;
             ALTER TABLE sessions ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;
             INSERT INTO sessions (id, source, started_at, cwd, git_repo_root, hidden) VALUES
               ('here','cli',1,'/repo/sub','/repo',0),('elsewhere','cli',2,'/other',NULL,0),('hid','cli',3,'/repo','/repo',1);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES
               ('here','user','a',10),('elsewhere','user','b',20),('hid','user','c',30);",
        )
        .unwrap();
        assert_eq!(pick(&p, "", "/repo").unwrap(), "here");
        assert_eq!(pick(&p, "", "/nowhere").unwrap(), "elsewhere");
        assert_eq!(pick(&p, "", "").unwrap(), "elsewhere");
    }

    #[test]
    fn pick_resolves_a_title_and_follows_a_compression_to_its_tip() {
        let d = scratch("title");
        let p = fixture(&d);
        let c = Connection::open(&p).unwrap();
        c.execute_batch(
            "ALTER TABLE sessions ADD COLUMN title TEXT;
             ALTER TABLE sessions ADD COLUMN end_reason TEXT;
             INSERT INTO sessions (id, source, parent_session_id, started_at, title, end_reason) VALUES
               ('root','cli',NULL,1,'night shift','compression'),('tip','cli','root',2,NULL,NULL),('branch','cli','tip',3,NULL,NULL);
             INSERT INTO messages (session_id, role, content, timestamp) VALUES ('root','user','a',5),('tip','user','b',6);",
        )
        .unwrap();
        assert_eq!(pick(&p, "night shift", "").unwrap(), "tip");
        assert_eq!(pick(&p, "root", "").unwrap(), "tip");
        assert_eq!(pick(&p, "branch", "").unwrap(), "branch");
    }

    #[test]
    fn home_reads_the_sticky_profile_and_the_hermes_home_root() {
        let user = scratch("sticky");
        let u = user.to_string_lossy().into_owned();
        let root = user.join(".hermes");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("active_profile"), "steward\n").unwrap();
        let none = BTreeMap::new();
        let want = format!("{}/profiles/steward", root.display());
        assert_eq!(home(&s(&["hermes", "-c"]), &none, &u), want);
        assert_eq!(home(&s(&["hermes", "-p", "default"]), &none, &u), root.to_string_lossy());
        assert_eq!(home(&s(&["hermes", "-p", "other"]), &none, &u), format!("{}/profiles/other", root.display()));
        let custom = format!("{u}/custom");
        let mut env = BTreeMap::new();
        env.insert("HERMES_HOME".to_string(), custom.clone());
        assert_eq!(home(&s(&["hermes", "-p", "steward"]), &env, &u), format!("{custom}/profiles/steward"));
        let prof = format!("{custom}/profiles/ops");
        env.insert("HERMES_HOME".to_string(), prof.clone());
        assert_eq!(home(&s(&["hermes"]), &env, &u), prof);
        assert_eq!(home(&s(&["hermes", "-p", "steward"]), &env, &u), format!("{custom}/profiles/steward"));
    }

    #[test]
    fn workspace_key_is_the_git_root_else_the_directory() {
        let repo = scratch("wskey");
        let sub = repo.join("a").join("b");
        std::fs::create_dir_all(&sub).unwrap();
        assert_eq!(workspace_key(&sub.to_string_lossy()), sub.to_string_lossy());
        std::fs::create_dir(repo.join(".git")).unwrap();
        assert_eq!(workspace_key(&sub.to_string_lossy()), repo.to_string_lossy());
    }

    fn add_session(path: &str, id: &str, at: f64) {
        let c = Connection::open(path).unwrap();
        c.execute("INSERT INTO sessions (id, source, started_at) VALUES (?1, 'cli', ?2)", rusqlite::params![id, at]).unwrap();
        c.execute(
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?1, 'user', 'x', ?2)",
            rusqlite::params![id, at + 1.0],
        )
        .unwrap();
    }

    fn add_prompt(path: &str, id: &str, prompt: &str, at: f64) {
        let c = Connection::open(path).unwrap();
        c.execute("INSERT INTO sessions (id, source, started_at) VALUES (?1, 'cli', ?2)", rusqlite::params![id, at]).unwrap();
        c.execute(
            "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?1, 'user', ?2, ?3)",
            rusqlite::params![id, prompt, at],
        )
        .unwrap();
    }

    #[test]
    fn a_prompt_picks_the_session_whose_first_message_it_is() {
        let d = scratch("prompt");
        let path = fixture(&d);
        add_prompt(&path, "old", "hi", 100.0);
        add_prompt(&path, "a", "hi", 300.2);
        add_prompt(&path, "b", "hi", 303.2);
        add_prompt(&path, "other", "something else", 300.1);
        // The older session is before the window.
        assert_eq!(pick_by_prompt(&path, "hi", 300.0, &[]).unwrap(), "a");
        // The session written closest to the prompt wins.
        assert_eq!(pick_by_prompt(&path, "hi", 303.0, &[]).unwrap(), "b");
        // A session another pane holds is passed over.
        assert_eq!(pick_by_prompt(&path, "hi", 303.0, &["b".to_string()]).unwrap(), "a");
        // White space folds.
        assert_eq!(pick_by_prompt(&path, "  something   else ", 300.0, &[]).unwrap(), "other");
        // No match, and no prompt.
        assert_eq!(pick_by_prompt(&path, "nothing", 300.0, &[]).unwrap(), "");
        assert_eq!(pick_by_prompt(&path, "", 300.0, &[]).unwrap(), "");
        // Too late for the span.
        assert_eq!(pick_by_prompt(&path, "hi", 100.0 + 200.0, &["a".to_string(), "b".to_string()]).unwrap(), "");
        // pick still picks the newest session.
        assert_eq!(pick(&path, "", "").unwrap(), "b");
        assert_eq!(pick(&path, "old", "").unwrap(), "old");
    }

    #[test]
    fn tip_is_the_session_itself_without_a_compression() {
        let d = scratch("tip");
        let path = fixture(&d);
        add_session(&path, "a", 1.0);
        assert_eq!(tip(&path, "a"), "a");
        assert_eq!(tip(&path, "missing"), "missing");
    }
}
