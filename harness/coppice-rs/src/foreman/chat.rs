//! The chat: the log, the tree, the view, and how the tree is built. The
//! twin of Go's internal/foreman chat.go, build.go and view.go.

use std::collections::{HashMap, HashSet, VecDeque};
use std::fs;
use std::os::unix::fs::FileExt;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use serde::Deserialize;

use super::ctx::Ctx;
use super::fsutil::{self, clip, split};
use super::order::{chain, node_of, NodeId, Planner, MAX_LEVEL};

/// The message kinds. A thought is not a kind: model thoughts are never
/// logged.
pub const KIND_USER: &str = "user";
pub const KIND_FOREMAN: &str = "foreman";
pub const KIND_WORK: &str = "work";
pub const KIND_PAST: &str = "past";
pub const KIND_NOTE: &str = "note";
pub const KIND_TOOL: &str = "tool";

const KINDS: [&str; 6] = [
    KIND_USER,
    KIND_FOREMAN,
    KIND_WORK,
    KIND_PAST,
    KIND_NOTE,
    KIND_TOOL,
];

/// The defaults of Config.
pub const DEFAULT_NODE_BYTES: usize = 512;
pub const DEFAULT_VIEW_MAX: usize = 128 * 1024;
pub const DEFAULT_VIEW_MIN: usize = 64 * 1024;
pub const DEFAULT_SPLIT_BYTES: usize = 32 * 1024;
/// The most a zoom reply holds, and the page it takes when no size is given.
pub const ZOOM_PAGE_BYTES: usize = 32 * 1024;

/// Stands in a view line for a node the compactor has not made yet.
pub const UNBUILT: &str = "(not summarized yet: zoom it)";

/// Another process has the chat open for writing.
pub const ERR_LOCKED: &str = "another process has this foreman chat open";

/// A failed node waits 30 s, then 2 min, then 5 min before it is asked
/// again. An append asks again at once.
const RETRY_STEPS: [Duration; 3] = [
    Duration::from_secs(30),
    Duration::from_secs(120),
    Duration::from_secs(300),
];

/// The clock of a chat.
pub type Clock = Arc<dyn Fn() -> SystemTime + Send + Sync>;

/// Seconds since the epoch of t, negative before it.
pub fn unix_secs(t: SystemTime) -> i64 {
    match t.duration_since(SystemTime::UNIX_EPOCH) {
        Ok(d) => d.as_secs() as i64,
        Err(e) => {
            let d = e.duration();
            -(d.as_secs() as i64) - i64::from(d.subsec_nanos() > 0)
        }
    }
}

/// t as a message date: UTC, to the second, 2006-01-02T15:04:05Z.
pub fn date_text(t: SystemTime) -> String {
    let (y, mo, d, h, mi, s) = crate::web::der::civil(unix_secs(t));
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}Z")
}

/// t as a day file name stem: 2006-01-02.
pub fn day_text(t: SystemTime) -> String {
    let (y, mo, d, ..) = crate::web::der::civil(unix_secs(t));
    format!("{y:04}-{mo:02}-{d:02}")
}

/// t as 20060102T150405Z.
pub fn stamp_text(t: SystemTime) -> String {
    let (y, mo, d, h, mi, s) = crate::web::der::civil(unix_secs(t));
    format!("{y:04}{mo:02}{d:02}T{h:02}{mi:02}{s:02}Z")
}

/// Whether s is a day as Go's time.Parse("2006-01-02") reads one.
pub fn is_day(s: &str) -> bool {
    let b = s.as_bytes();
    if b.len() != 10 || b[4] != b'-' || b[7] != b'-' {
        return false;
    }
    let num = |r: &[u8]| -> Option<i64> {
        r.iter()
            .all(u8::is_ascii_digit)
            .then(|| r.iter().fold(0i64, |n, c| n * 10 + (c - b'0') as i64))
    };
    let (Some(y), Some(m), Some(d)) = (num(&b[0..4]), num(&b[5..7]), num(&b[8..10])) else {
        return false;
    };
    let dim = match m {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 if (y % 4 == 0 && y % 100 != 0) || y % 400 == 0 => 29,
        2 => 28,
        _ => return false,
    };
    (1..=dim).contains(&d)
}

/// A message date as a SystemTime, for a date written by date_text.
pub fn parse_date(s: &str) -> Option<SystemTime> {
    let b = s.as_bytes();
    if !s.is_ascii()
        || b.len() != 20
        || b[19] != b'Z'
        || !is_day(&s[..10])
        || b[10] != b'T'
        || b[13] != b':'
        || b[16] != b':'
    {
        return None;
    }
    let two = |r: &[u8]| -> Option<i64> {
        r.iter()
            .all(u8::is_ascii_digit)
            .then(|| ((r[0] - b'0') * 10 + (r[1] - b'0')) as i64)
    };
    let (h, mi, sec) = (two(&b[11..13])?, two(&b[14..16])?, two(&b[17..19])?);
    if h > 23 || mi > 59 || sec > 59 {
        return None;
    }
    let y: i64 = s[0..4].parse().ok()?;
    let m: i64 = s[5..7].parse().ok()?;
    let d: i64 = s[8..10].parse().ok()?;
    // Howard Hinnant's days_from_civil.
    let yy = if m <= 2 { y - 1 } else { y };
    let era = yy.div_euclid(400);
    let yoe = yy - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    let secs = days * 86400 + h * 3600 + mi * 60 + sec;
    Some(if secs >= 0 {
        SystemTime::UNIX_EPOCH + Duration::from_secs(secs as u64)
    } else {
        SystemTime::UNIX_EPOCH - Duration::from_secs((-secs) as u64)
    })
}

/// One line of the log, in main/YYYY-MM-DD.jsonl.
#[derive(Clone, Debug, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct Message {
    pub i: i64,
    pub kind: String,
    pub text: String,
    pub size: i64,
    pub date: String,
    /// The client act id of the owner's words (FM-R-65). It is set on the
    /// last part of a split text, in the same append, so a repeat of the
    /// act is known after a restart.
    pub act: String,
}

impl Message {
    /// The line as Go's encoder writes it.
    pub fn encode(&self) -> String {
        let mut s = format!("{{\"i\":{},\"kind\":", self.i);
        fsutil::push_json_str(&mut s, &self.kind);
        s.push_str(",\"text\":");
        fsutil::push_json_str(&mut s, &self.text);
        s.push_str(&format!(",\"size\":{},\"date\":", self.size));
        fsutil::push_json_str(&mut s, &self.date);
        if !self.act.is_empty() {
            s.push_str(",\"act\":");
            fsutil::push_json_str(&mut s, &self.act);
        }
        s.push_str("}\n");
        s
    }
}

/// One line of the tree, in tree/YYYY-MM-DD.jsonl: the day it was built.
#[derive(Clone, Debug, Default, PartialEq, Deserialize)]
#[serde(default)]
pub struct Node {
    pub l: i64,
    pub i: i64,
    pub text: String,
    pub size: i64,
}

impl Node {
    pub fn id(&self) -> NodeId {
        NodeId {
            l: self.l as i32,
            i: self.i,
        }
    }

    fn encode(&self) -> String {
        let mut s = format!("{{\"l\":{},\"i\":{},\"text\":", self.l, self.i);
        fsutil::push_json_str(&mut s, &self.text);
        s.push_str(&format!(",\"size\":{}}}\n", self.size));
        s
    }
}

/// Makes tree lines with a model, or a fake in tests. Each result should
/// fit in limit bytes; a longer one is clipped.
pub trait Summarizer: Send + Sync {
    /// One line for a message longer than limit.
    fn compress(&self, ctx: &Ctx, m: &Message, limit: usize) -> Result<String, String>;
    /// One line for two sibling lines that do not fit in limit joined.
    fn merge(
        &self,
        ctx: &Ctx,
        id: NodeId,
        left: &str,
        right: &str,
        limit: usize,
    ) -> Result<String, String>;
}

/// Opens a chat.
#[derive(Clone, Default)]
pub struct Config {
    /// The foreman data dir. It is made 0700 if missing.
    pub dir: PathBuf,
    /// The clock; None is the system clock.
    pub now: Option<Clock>,
    /// Caps a tree line. 0 means 512.
    pub node_bytes: usize,
    /// ViewMax starts a batch merge, which runs until the view is at most
    /// ViewMin. 0 means 128 KiB and 64 KiB.
    pub view_max: usize,
    pub view_min: usize,
    /// Caps one message; a longer text is logged as several messages in a
    /// row. 0 means 32 KiB.
    pub split_bytes: usize,
}

impl Config {
    fn fill(&mut self) {
        if self.now.is_none() {
            self.now = Some(Arc::new(SystemTime::now));
        }
        if self.node_bytes == 0 {
            self.node_bytes = DEFAULT_NODE_BYTES;
        }
        if self.view_max == 0 {
            self.view_max = DEFAULT_VIEW_MAX;
        }
        if self.view_min == 0 {
            self.view_min = DEFAULT_VIEW_MIN;
        }
        if self.split_bytes == 0 {
            self.split_bytes = DEFAULT_SPLIT_BYTES;
        }
    }

    fn now(&self) -> SystemTime {
        match &self.now {
            Some(f) => f(),
            None => SystemTime::now(),
        }
    }
}

/// Where a message's line is, so its text need not stay in memory.
#[derive(Clone, Debug)]
struct MsgRef {
    file: PathBuf,
    off: u64,
    n: usize,
    date: String,
    /// True for an imported message (kind past).
    past: bool,
}

/// One tree line that needs the model.
#[derive(Clone, Debug, PartialEq)]
pub struct Job {
    pub id: NodeId,
    /// The message to compress, for a level-0 node.
    pub msg: Message,
    /// The two lines to merge, for a parent.
    pub left: String,
    pub right: String,
    /// The most bytes the line may have.
    pub limit: usize,
}

/// What the chat remembers of the front node after its call failed.
struct RetryState {
    id: NodeId,
    at: SystemTime,
    fails: usize,
    err: String,
}

/// The view as view.json keeps it.
#[derive(Clone, Debug, Default, PartialEq)]
struct ViewState {
    count: i64,
    batch: bool,
    lines: Vec<NodeId>,
}

impl ViewState {
    fn encode(&self) -> String {
        let lines: Vec<String> = self
            .lines
            .iter()
            .map(|id| format!("[{},{}]", id.l, id.i))
            .collect();
        format!(
            "{{\"count\":{},\"batch\":{},\"lines\":[{}]}}\n",
            self.count,
            self.batch,
            lines.join(",")
        )
    }
}

/// view.json on disk. Each line is an [l, i] pair.
#[derive(Deserialize, Default)]
#[serde(default)]
struct ViewFile {
    count: i64,
    batch: bool,
    lines: Vec<Vec<i64>>,
}

/// Why a load failed. Only a history fault moves the history aside; an I/O
/// error is returned as it is, and the dir stays where it is.
#[derive(Debug)]
enum Fault {
    History { reason: String, msg: String },
    Io(String),
}

impl Fault {
    fn text(&self) -> &str {
        match self {
            Fault::History { msg, .. } => msg,
            Fault::Io(m) => m,
        }
    }
}

fn corrupt(reason: impl Into<String>, msg: impl Into<String>) -> Fault {
    Fault::History {
        reason: reason.into(),
        msg: msg.into(),
    }
}

/// One rendered view line: its node and its text, "id|text" and a newline.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ViewLine {
    pub id: NodeId,
    pub text: String,
}

/// Puts a text on one line.
pub fn flat(s: &str) -> String {
    s.replace("\r\n", " ").replace(['\n', '\r'], " ")
}

/// Is a line as it is written: at most limit bytes.
fn finish(text: &str, limit: usize) -> String {
    clip(text, limit).to_string()
}

#[cfg(test)]
thread_local! {
    /// When set, runs between the reads of a load. Tests use it to write
    /// while a reader reads.
    pub static LOAD_HOOK: std::cell::RefCell<Option<Box<dyn FnMut(&str)>>> = const { std::cell::RefCell::new(None) };
}

#[cfg(test)]
thread_local! {
    /// Makes the chmod of a moved-aside history fail.
    pub static FAIL_CHMOD_ASIDE: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

#[cfg(test)]
thread_local! {
    /// Makes the write of view.json fail.
    pub static FAIL_VIEW_WRITE: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
}

fn hook(_stage: &str) {
    #[cfg(test)]
    LOAD_HOOK.with(|h| {
        if let Some(f) = h.borrow_mut().as_mut() {
            f(_stage)
        }
    });
}

/// One open foreman chat. Not safe for concurrent use: the caller runs one
/// call at a time.
pub struct Chat {
    cfg: Config,
    read_only: bool,
    lock: Option<fs::File>,
    msgs: Vec<MsgRef>,
    nodes: HashMap<NodeId, Node>,
    /// Maps a client act id to the messages that carry it, oldest first:
    /// every part of a text that was split.
    acts: HashMap<String, Vec<i64>>,
    /// The nodes still to write, in write order.
    pending: VecDeque<NodeId>,
    /// The retry state of the front node after a failed call.
    wait: Option<RetryState>,
    /// A line the model made whose write to the tree file failed.
    held: Option<(NodeId, String)>,
    view: ViewState,
    /// Set when a failed append could not be cut back off the log.
    broken: Option<String>,
    /// Set when Open moved a broken history aside or repaired it.
    notice: String,
    /// The tree lines a load could not use (U3), by file. A writer repairs
    /// them; a reader skips them.
    bad_tree: Vec<BadTreeFile>,
    /// Why view.json could not be used, or "".
    bad_view: String,
    /// The notes of torn tree tails a load copied and cut.
    torn_parts: std::cell::RefCell<Vec<String>>,
    /// Set while the tree loads: a torn tail is copied before it is cut.
    copy_torn: std::cell::Cell<bool>,
    /// The nodes a replay of the view steps past (I-1): the nodes a repair
    /// cut from the tree, which the compactor builds again later.
    skip: HashSet<NodeId>,
    /// Set when the save of view.json failed.
    view_save_failed: bool,
}

/// The bad lines of one tree file, by line index (0 is the first line).
struct BadTreeFile {
    path: PathBuf,
    idx: HashSet<usize>,
}

impl Chat {
    fn main_dir(&self) -> PathBuf {
        self.cfg.dir.join("main")
    }

    fn tree_dir(&self) -> PathBuf {
        self.cfg.dir.join("tree")
    }

    fn view_path(&self) -> PathBuf {
        self.cfg.dir.join("view.json")
    }

    fn empty(cfg: Config, read_only: bool) -> Chat {
        Chat {
            cfg,
            read_only,
            lock: None,
            msgs: Vec::new(),
            nodes: HashMap::new(),
            acts: HashMap::new(),
            pending: VecDeque::new(),
            wait: None,
            held: None,
            view: ViewState::default(),
            broken: None,
            notice: String::new(),
            bad_tree: Vec::new(),
            bad_view: String::new(),
            torn_parts: std::cell::RefCell::new(Vec::new()),
            copy_torn: std::cell::Cell::new(false),
            skip: HashSet::new(),
            view_save_failed: false,
        }
    }

    /// Opens the chat in cfg.dir for writing. It takes the chat's lock,
    /// drops a line a crash left half written, builds the nodes that need
    /// no model, and brings the saved view up to the last message whose
    /// nodes are all written.
    ///
    /// coppice must always start. If the history cannot be read, Open
    /// moves the whole dir aside to <dir>.broken-<UTC time> (the newest
    /// broken_keep copies stay, default 5; older ones are removed), starts a new empty chat, and says so in notice().
    pub fn open(mut cfg: Config) -> Result<Chat, String> {
        cfg.fill();
        let (reason, herr) = match Chat::open_once(cfg.clone()) {
            Ok(c) => return Ok(c),
            Err(Fault::Io(m)) => return Err(m),
            Err(Fault::History { reason, msg }) => (reason, msg),
        };
        let broken = move_aside(&cfg).map_err(|m| format!("foreman: {herr}; {m}"))?;
        match Chat::open_once(cfg.clone()) {
            Ok(mut c) => {
                c.notice = format!(
                    "Foreman history could not be read ({reason}); it was saved to {broken} and a new chat started."
                );
                // A tool call the old history left without an end is still
                // unknown.
                if let Err(e) = super::calls::carry_dangling_calls(
                    std::path::Path::new(&broken),
                    &cfg.dir,
                ) {
                    c.notice.push_str(&format!(
                        " Its call log could not be read ({e}): check the workers."
                    ));
                }
                Ok(c)
            }
            Err(Fault::History { msg, .. }) => Err(format!(
                "foreman: a new chat in {} could not be read: {msg}",
                cfg.dir.display()
            )),
            Err(Fault::Io(m)) => Err(m),
        }
    }

    fn open_once(cfg: Config) -> Result<Chat, Fault> {
        for d in [cfg.dir.clone(), cfg.dir.join("main"), cfg.dir.join("tree")] {
            fsutil::ensure_dir(&d).map_err(Fault::Io)?;
        }
        fsutil::tighten_files(&cfg.dir).map_err(Fault::Io)?;
        let lock = lock_dir(&cfg.dir.join("lock")).map_err(Fault::Io)?;
        let mut c = Chat::empty(cfg, false);
        c.lock = Some(lock);
        c.load()?;
        let mut notes: Vec<String> = Vec::new();
        if !c.bad_tree.is_empty() || !c.torn_parts.borrow().is_empty() {
            notes.push(c.repair_tree().map_err(Fault::Io)?);
        }
        let mut view_saved = String::new();
        if !c.bad_view.is_empty() {
            view_saved = c.copy_view_aside().map_err(Fault::Io)?;
        }
        if !c.bad_view.is_empty() || c.view_missing() {
            c.skip = c.cut_nodes();
        }
        c.queue_missing();
        let res = c.settle();
        c.skip.clear();
        if let Err(e) = res {
            // Only a failed save of the rebuilt view.json moves the history.
            // Any other error is not the view's and is returned as it is.
            if !c.bad_view.is_empty() && c.view_save_failed {
                return Err(corrupt("view.json could not be rebuilt", e));
            }
            return Err(Fault::Io(e));
        }
        if !view_saved.is_empty() {
            let mut note = format!("Foreman view.json could not be used. It was copied to {view_saved}");
            if c.view.count > 0 || c.msgs.is_empty() {
                note.push_str(" and the view was rebuilt from the tree.");
            } else {
                note.push_str(". The view is empty and will be rebuilt as the tree is built.");
            }
            notes.push(note);
        }
        c.notice = notes.join(" ");
        Ok(c)
    }

    /// The UTC time in a .broken-<time> name.
    fn stamp_now(&self) -> String {
        stamp_text(self.cfg.now())
    }

    /// Copies the bad tree lines verbatim to <file>.broken-<time>, rewrites
    /// each tree file without them (temp file, fsync, rename), and returns
    /// the one note. The nodes the lines held are not in nodes, so
    /// queue_missing builds them again from the log. The newest broken_keep
    /// copies stay (default 5); older ones are removed.
    fn repair_tree(&mut self) -> Result<String, String> {
        let mut parts: Vec<String> = std::mem::take(&mut *self.torn_parts.borrow_mut());
        for bf in std::mem::take(&mut self.bad_tree) {
            let data = fs::read(&bf.path).map_err(|e| fsutil::path_err("open", &bf.path, &e))?;
            // Split on the raw newlines: the copy holds each bad line
            // exactly as it was, and the rest of the file keeps its bytes.
            let (mut keep, mut bad) = (Vec::new(), Vec::new());
            for (k, line) in data.split_inclusive(|&b| b == b'\n').enumerate() {
                if bf.idx.contains(&k) {
                    bad.extend_from_slice(line);
                } else {
                    keep.extend_from_slice(line);
                }
            }
            let saved = free_name(&format!(
                "{}.broken-{}",
                bf.path.to_string_lossy(),
                self.stamp_now()
            ));
            write_new(Path::new(&saved), &bad)?;
            crate::broken::prune(Path::new(&saved));
            fsutil::write_atomic(&bf.path, &keep)?;
            let rel = bf
                .path
                .strip_prefix(&self.cfg.dir)
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_else(|_| bf.path.to_string_lossy().into_owned());
            parts.push(if bf.idx.len() == 1 {
                format!("1 bad line in {rel} was copied to {saved} and removed.")
            } else {
                format!(
                    "{} bad lines in {rel} were copied to {saved} and removed.",
                    bf.idx.len()
                )
            });
        }
        Ok(format!(
            "Foreman tree repaired: {} The nodes those lines held will be built again from the log.",
            parts.join(" ")
        ))
    }

    /// Copies view.json alone to view.json.broken-<time> and leaves it where
    /// it is. The view starts empty and settle rebuilds it by replay over the
    /// tree; the save of the rebuilt view replaces the bad file. An open that
    /// fails first leaves the bad file in place, so the next open repairs it
    /// again and says so (N-2).
    fn copy_view_aside(&mut self) -> Result<String, String> {
        let old = self.view_path();
        let data = fs::read(&old).map_err(|e| fsutil::path_err("open", &old, &e))?;
        let saved = free_name(&format!(
            "{}.broken-{}",
            old.to_string_lossy(),
            self.stamp_now()
        ));
        write_new(Path::new(&saved), &data)?;
        crate::broken::prune(Path::new(&saved));
        self.view = ViewState::default();
        Ok(saved)
    }

    /// True when there is no view.json and the log is not empty. The replay
    /// then steps past the cut nodes as for a bad view (N-2).
    fn view_missing(&self) -> bool {
        !self.msgs.is_empty()
            && matches!(fs::symlink_metadata(self.view_path()),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound)
    }

    /// What a reader says about the tree lines it skipped, or "".
    fn skip_note(&self) -> String {
        let parts: Vec<String> = self
            .bad_tree
            .iter()
            .map(|bf| {
                let rel = bf
                    .path
                    .strip_prefix(&self.cfg.dir)
                    .map(|p| p.to_string_lossy().into_owned())
                    .unwrap_or_else(|_| bf.path.to_string_lossy().into_owned());
                if bf.idx.len() == 1 {
                    format!("1 bad line in {rel} was skipped.")
                } else {
                    format!("{} bad lines in {rel} were skipped.", bf.idx.len())
                }
            })
            .collect();
        if parts.is_empty() {
            return String::new();
        }
        format!(
            "{} Their nodes show as not summarized yet. Nothing was changed.",
            parts.join(" ")
        )
    }

    /// Reads the chat in dir without the lock and writes nothing. A line a
    /// writer has half written is skipped. A history it cannot read is
    /// reported with its path and never moved.
    pub fn open_read_only(dir: &Path) -> Result<Chat, String> {
        let mut cfg = Config {
            dir: dir.to_path_buf(),
            ..Default::default()
        };
        cfg.fill();
        fs::metadata(dir).map_err(|e| fsutil::path_err("stat", dir, &e))?;
        let mut c = Chat::empty(cfg, true);
        c.load().map_err(|f| {
            format!(
                "foreman: the history in {} could not be read: {}",
                dir.display(),
                f.text()
            )
        })?;
        let mut notes: Vec<String> = Vec::new();
        let n = c.skip_note();
        if !n.is_empty() {
            notes.push(n);
        }
        if !c.bad_view.is_empty() || c.view_missing() {
            c.skip = c.cut_nodes();
            c.replay_view();
            c.skip.clear();
        }
        if !c.bad_view.is_empty() {
            if c.view.count > 0 || c.msgs.is_empty() {
                notes.push(format!(
                    "view.json could not be used ({}). The view was rebuilt in memory. Nothing was changed.",
                    c.bad_view
                ));
            } else {
                notes.push(format!(
                    "view.json could not be used ({}). The view is empty until the tree is built. Nothing was changed.",
                    c.bad_view
                ));
            }
        }
        c.notice = notes.join(" ");
        Ok(c)
    }

    /// Releases the lock.
    pub fn close(&mut self) {
        self.lock = None;
    }

    /// A one-line note for the owner from Open, or "".
    pub fn notice(&self) -> &str {
        &self.notice
    }

    /// The number of messages in the chat.
    pub fn count(&self) -> i64 {
        self.msgs.len() as i64
    }

    /// The nodes not built yet, in write order. Tests read the order.
    #[cfg(test)]
    pub fn pending_ids(&self) -> Vec<NodeId> {
        self.pending.iter().copied().collect()
    }

    /// The number of nodes not built yet.
    pub fn pending(&self) -> usize {
        self.pending.len()
    }

    /// Calls f for each whole line of path with its offset. A last line
    /// with no newline was cut by a crash: a writer truncates it away, a
    /// reader skips it.
    fn read_lines(
        &self,
        path: &Path,
        mut f: impl FnMut(u64, &[u8]) -> Result<(), String>,
    ) -> Result<(), Fault> {
        let data = fs::read(path).map_err(|e| Fault::Io(fsutil::path_err("open", path, &e)))?;
        let end = data.iter().rposition(|&b| b == b'\n').map_or(0, |p| p + 1);
        if end < data.len() && !self.read_only {
            if self.copy_torn.get() && path.parent() == Some(self.tree_dir().as_path()) {
                self.copy_torn_tail(path, &data[end..]).map_err(Fault::Io)?;
            }
            fs::OpenOptions::new()
                .write(true)
                .open(path)
                .and_then(|f| f.set_len(end as u64))
                .map_err(|e| Fault::Io(fsutil::path_err("truncate", path, &e)))?;
        }
        let mut off: u64 = 0;
        let mut pos = 0;
        while pos < end {
            let nl = data[pos..end]
                .iter()
                .position(|&b| b == b'\n')
                .map_or(end, |p| pos + p);
            let mut line = &data[pos..nl];
            // bufio.ScanLines drops a carriage return at the end of a line.
            if line.last() == Some(&b'\r') {
                line = &line[..line.len() - 1];
            }
            if let Err(e) = f(off, line) {
                let rel = path
                    .strip_prefix(&self.cfg.dir)
                    .map(|p| p.to_string_lossy().into_owned())
                    .unwrap_or_else(|_| path.to_string_lossy().into_owned());
                return Err(corrupt(
                    format!("a bad line in {rel}"),
                    format!("{} at byte {off}: {e}", path.display()),
                ));
            }
            off += line.len() as u64 + 1;
            pos = nl + 1;
        }
        Ok(())
    }

    /// Reads the log, the tree and the view. A reader reads view.json
    /// first and then only the messages it covers and the nodes inside
    /// them, so it sees one consistent snapshot however far the writer has
    /// gone since.
    fn load(&mut self) -> Result<(), Fault> {
        let mut vf = None;
        if self.read_only {
            vf = self.read_view_file().or_else(|f| self.view_fault(f))?;
            hook("view");
        }
        let limit = vf.as_ref().map_or(-1, |v: &ViewFile| v.count);
        self.load_messages(limit)?;
        hook("main");
        self.load_tree()?;
        if !self.read_only {
            vf = self.read_view_file().or_else(|f| self.view_fault(f))?;
        }
        if let Err(f) = self.apply_view(vf) {
            self.view_fault(f)?;
            self.view = ViewState::default();
            if limit >= 0 {
                // The log was capped by a view that is now rejected: read it all.
                self.load_messages(-1)?;
                self.load_tree()?;
            }
        }
        Ok(())
    }

    /// Copies the torn last line of a tree file to a .broken file before a
    /// writer cuts it off, and keeps the words for the repair note.
    fn copy_torn_tail(&self, path: &Path, tail: &[u8]) -> Result<(), String> {
        let saved = free_name(&format!(
            "{}.broken-{}",
            path.to_string_lossy(),
            self.stamp_now()
        ));
        write_new(Path::new(&saved), tail)?;
        crate::broken::prune(Path::new(&saved));
        let rel = path
            .strip_prefix(&self.cfg.dir)
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| path.to_string_lossy().into_owned());
        self.torn_parts.borrow_mut().push(format!(
            "1 torn last line in {rel} was copied to {saved} and removed."
        ));
        Ok(())
    }

    /// The nodes the tree lacks that it should hold. Nodes are built in
    /// queue order and a failed node stops all behind it, so a node that is
    /// pending in the normal way has no built node after it. A missing node
    /// that has one was cut from the tree. A replay of the view steps past
    /// these (I-1); a node that is merely pending stays in the tail word for
    /// word.
    fn cut_nodes(&self) -> HashSet<NodeId> {
        let mut order: Vec<NodeId> = Vec::new();
        let mut last: Option<usize> = None;
        for i in 0..self.msgs.len() as i64 {
            for id in chain(i) {
                if self.nodes.contains_key(&id) {
                    last = Some(order.len());
                }
                order.push(id);
            }
        }
        let mut cut = HashSet::new();
        if let Some(last) = last {
            for id in &order[..last] {
                if !self.nodes.contains_key(id) {
                    cut.insert(*id);
                }
            }
        }
        cut
    }

    /// Turns a view.json that cannot be used (bad JSON, or not matching the
    /// log) into bad_view and no view file, so the opener can rebuild it.
    /// Any other fault is returned as it is.
    fn view_fault(&mut self, f: Fault) -> Result<Option<ViewFile>, Fault> {
        match f {
            Fault::History { reason, .. } => {
                self.bad_view = reason;
                Ok(None)
            }
            io => Err(io),
        }
    }

    fn load_messages(&mut self, limit: i64) -> Result<(), Fault> {
        let files = day_files(&self.main_dir()).map_err(Fault::Io)?;
        let mut all: Vec<(i64, MsgRef)> = Vec::new();
        let mut acts: HashMap<String, Vec<i64>> = HashMap::new();
        let mut act_of: Vec<(i64, String)> = Vec::new();
        for f in files {
            let path = self.main_dir().join(&f);
            self.read_lines(&path, |off, line| {
                let m: Message = serde_json::from_slice(line).map_err(|e| e.to_string())?;
                if limit < 0 || m.i < limit {
                    if !m.act.is_empty() {
                        act_of.push((m.i, m.act.clone()));
                    }
                    all.push((
                        m.i,
                        MsgRef {
                            file: path.clone(),
                            off,
                            n: line.len(),
                            past: m.kind == KIND_PAST,
                            date: m.date,
                        },
                    ));
                }
                Ok(())
            })?;
        }
        all.sort_by_key(|(i, _)| *i);
        act_of.sort_by_key(|(i, _)| *i);
        for (i, a) in act_of {
            acts.entry(a).or_default().push(i);
        }
        self.acts = acts;
        self.msgs = Vec::with_capacity(all.len());
        for (k, (i, r)) in all.into_iter().enumerate() {
            let k = k as i64;
            if i < k {
                return Err(corrupt(
                    format!("message {i} is in the log twice"),
                    format!("foreman: the log has message {i} twice"),
                ));
            }
            if i > k {
                return Err(corrupt(
                    format!("message {k} is missing"),
                    format!("foreman: the log has no message {k}"),
                ));
            }
            self.msgs.push(r);
        }
        Ok(())
    }

    /// Reads the tree. A node past the end of the log is an error for a
    /// writer; a reader skips it, as the writer built it after the snapshot.
    fn load_tree(&mut self) -> Result<(), Fault> {
        self.nodes = HashMap::new();
        self.bad_tree = Vec::new();
        self.torn_parts.borrow_mut().clear();
        self.copy_torn.set(true);
        let files = day_files(&self.tree_dir()).map_err(Fault::Io)?;
        let count = self.msgs.len() as i64;
        let read_only = self.read_only;
        let mut found: Vec<Node> = Vec::new();
        let mut bad_files: Vec<BadTreeFile> = Vec::new();
        for f in files {
            let path = self.tree_dir().join(&f);
            let mut idx: i64 = -1;
            let mut bad: HashSet<usize> = HashSet::new();
            self.read_lines(&path, |_, line| {
                idx += 1;
                let Some(n) = parse_node(line) else {
                    bad.insert(idx as usize);
                    return Ok(());
                };
                if n.l < 0
                    || n.l > MAX_LEVEL as i64
                    || n.i < 0
                    || n.i > (1i64 << (MAX_LEVEL as i64 - n.l))
                {
                    bad.insert(idx as usize);
                    return Ok(());
                }
                let id = n.id();
                if id.last() >= count {
                    // A reader skips a node past its snapshot: the writer
                    // built it later. For a writer it is a bad line.
                    if !read_only {
                        bad.insert(idx as usize);
                    }
                    return Ok(());
                }
                found.push(n);
                Ok(())
            })?;
            if !bad.is_empty() {
                bad_files.push(BadTreeFile { path, idx: bad });
            }
        }
        for n in found {
            self.nodes.insert(n.id(), n);
        }
        self.bad_tree = bad_files;
        self.copy_torn.set(false);
        Ok(())
    }

    /// Lists every node that should exist but does not, in the order a live
    /// chat would have built it.
    fn queue_missing(&mut self) {
        self.pending.clear();
        for i in 0..self.msgs.len() as i64 {
            self.queue_for(i);
        }
    }

    /// Queues message i's node and every parent it completes.
    fn queue_for(&mut self, i: i64) {
        for id in chain(i) {
            if !self.nodes.contains_key(&id) {
                self.pending.push_back(id);
            }
        }
    }

    /// Logs text as one message of kind, or several in a row when it is
    /// longer than split_bytes, dated now. See append_at.
    pub fn append(&mut self, kind: &str, text: &str) -> (Vec<i64>, Option<String>) {
        self.append_at(kind, text, None)
    }

    /// Logs text as one or more messages of kind with the given date (now
    /// if None). Each line is on disk, fsynced, before the next step. Then
    /// the new nodes are queued, the ones that need no model are written,
    /// and the view takes every message whose nodes are all written. It
    /// never calls a model.
    ///
    /// A failed write is cut back off the log and its id is not used. When
    /// a split text fails part way, the parts already logged stay and their
    /// ids are returned with the error.
    pub fn append_at(
        &mut self,
        kind: &str,
        text: &str,
        date: Option<SystemTime>,
    ) -> (Vec<i64>, Option<String>) {
        self.append_act(kind, text, date, "")
    }

    /// The last message that carries the client act id, if the log holds
    /// one. The set is rebuilt from the log when the chat opens.
    pub fn act_msg(&self, act: &str) -> Option<i64> {
        self.acts.get(act).and_then(|v| v.last()).copied()
    }

    /// Whether the log holds a message with this client act id.
    pub fn has_act(&self, act: &str) -> bool {
        self.acts.get(act).is_some_and(|v| !v.is_empty())
    }

    /// Logs one past message, dated when it was said, with the act id that
    /// keeps it from being logged twice (import, FM-R-71).
    /// A call after a failed one logs only the parts the log lacks, and
    /// none when it holds all of them (FM-R-82).
    pub fn append_imported(
        &mut self,
        text: &str,
        date: SystemTime,
        act: &str,
    ) -> (Vec<i64>, Option<String>) {
        let have = self.acts.get(act).cloned().unwrap_or_default();
        if have.is_empty() {
            return self.append_act_from(KIND_PAST, text, Some(date), act, 0);
        }
        if !self.act_matches(act, text) || self.import_done(text, act) {
            return (have, None);
        }
        self.append_act_from(KIND_PAST, text, Some(date), act, have.len())
    }

    /// Whether the log holds all of an imported message. A message with some
    /// of its parts logged is not done; an id that carries other words is
    /// done, so it is never logged twice under one id (FM-R-82).
    pub fn import_done(&self, text: &str, act: &str) -> bool {
        if !self.has_act(act) {
            return false;
        }
        if !self.act_matches(act, text) {
            return true;
        }
        let n = self.acts.get(act).map_or(0, |v| v.len());
        n >= split(text, self.cfg.split_bytes).len()
    }

    /// Whether text is what the known act id was used for: the parts the
    /// log holds for the id are the first parts of text, split as an append
    /// splits it (a text whose last parts failed to log still matches). A
    /// caller that sends one id for other words gets false and must refuse
    /// (FM-R-65).
    pub fn act_matches(&self, act: &str, text: &str) -> bool {
        let ids = match self.acts.get(act) {
            Some(v) if !v.is_empty() => v,
            _ => return false,
        };
        let parts = split(text, self.cfg.split_bytes);
        if ids.len() > parts.len() {
            return false;
        }
        ids.iter()
            .zip(parts.iter())
            .all(|(i, p)| self.message(*i).is_ok_and(|m| m.text == *p))
    }

    /// Logs the owner's words with a client act id: the id is written in
    /// the line of every part of the text, so a failure part way keeps it.
    /// A later call with a known id is the caller's to refuse (act_msg,
    /// act_matches).
    pub fn append_act(
        &mut self,
        kind: &str,
        text: &str,
        date: Option<SystemTime>,
        act: &str,
    ) -> (Vec<i64>, Option<String>) {
        self.append_act_from(kind, text, date, act, 0)
    }

    /// append_act, where from is how many of the first parts the log holds
    /// already (a retry of a failed append); they are not logged again.
    fn append_act_from(
        &mut self,
        kind: &str,
        text: &str,
        date: Option<SystemTime>,
        act: &str,
        from: usize,
    ) -> (Vec<i64>, Option<String>) {
        if self.read_only || self.lock.is_none() {
            return (
                Vec::new(),
                Some("foreman: the chat is not open for writing".into()),
            );
        }
        if let Some(b) = &self.broken {
            return (
                Vec::new(),
                Some(format!(
                    "foreman: the log takes no more appends until it is opened again: {b}"
                )),
            );
        }
        if !KINDS.contains(&kind) {
            return (
                Vec::new(),
                Some(format!(
                    "foreman: {} is not a message kind",
                    crate::sys::go_quote(kind)
                )),
            );
        }
        let date = date.unwrap_or_else(|| self.cfg.now());
        let stamp = date_text(date);
        let path = self.main_dir().join(format!("{}.jsonl", day_text(date)));
        let mut ids = Vec::new();
        let mut werr = None;
        let parts = split(text, self.cfg.split_bytes);
        for part in parts.into_iter().skip(from) {
            let i = self.msgs.len() as i64;
            let m = Message {
                i,
                kind: kind.to_string(),
                size: part.len() as i64,
                text: part,
                date: stamp.clone(),
                act: act.to_string(),
            };
            let line = m.encode();
            match fsutil::append_line(&path, line.as_bytes()) {
                Ok(off) => {
                    self.msgs.push(MsgRef {
                        file: path.clone(),
                        off,
                        n: line.len() - 1,
                        date: stamp.clone(),
                        past: kind == KIND_PAST,
                    });
                    if !m.act.is_empty() {
                        self.acts.entry(m.act.clone()).or_default().push(i);
                    }
                    self.queue_for(i);
                    ids.push(i);
                }
                Err(e) => {
                    if e.cut_back {
                        self.broken = Some(e.msg.clone());
                    }
                    werr = Some(e.msg);
                    break;
                }
            }
        }
        if ids.is_empty() {
            return (ids, werr);
        }
        // The owner is here: a failed node is asked again at the next Next.
        self.retry_now();
        if let Err(e) = self.settle() {
            return (ids, Some(e));
        }
        (ids, werr)
    }

    /// Reads message i from the log.
    pub fn message(&self, i: i64) -> Result<Message, String> {
        if i < 0 || i >= self.msgs.len() as i64 {
            return Err(format!("foreman: no message {i}"));
        }
        let r = &self.msgs[i as usize];
        let f = fs::File::open(&r.file).map_err(|e| fsutil::path_err("open", &r.file, &e))?;
        let mut buf = vec![0u8; r.n];
        f.read_exact_at(&mut buf, r.off)
            .map_err(|_| "unexpected EOF".to_string())?;
        let m: Message = serde_json::from_slice(&buf).map_err(|e| e.to_string())?;
        if m.i != i {
            return Err(format!(
                "foreman: the log line for message {i} holds message {}",
                m.i
            ));
        }
        Ok(m)
    }

    /// A built node.
    pub fn node(&self, id: NodeId) -> Option<&Node> {
        self.nodes.get(&id)
    }

    /// The date message i was logged, the date(id) tool.
    pub fn date(&self, i: i64) -> Result<String, String> {
        if i < 0 || i >= self.msgs.len() as i64 {
            return Err(format!("foreman: no message {i}"));
        }
        Ok(self.msgs[i as usize].date.clone())
    }

    /// Every message logged on one UTC day, YYYY-MM-DD, in id order.
    pub fn day(&self, day: &str) -> Result<Vec<Message>, String> {
        if !is_day(day) {
            return Err(format!(
                "foreman: {} is not a day like 2026-10-08",
                crate::sys::go_quote(day)
            ));
        }
        let mut out = Vec::new();
        for (i, r) in self.msgs.iter().enumerate() {
            if !r.date.starts_with(&format!("{day}T")) {
                continue;
            }
            out.push(self.message(i as i64)?);
        }
        Ok(out)
    }

    /// The largest whole nodes that cover the chat, oldest first: the ids a
    /// zoom can start from. The binary form of the message count names them.
    pub fn roots(&self) -> Vec<NodeId> {
        let mut out = Vec::new();
        let mut first = 0i64;
        let n = self.msgs.len() as i64;
        for l in (0..=MAX_LEVEL).rev() {
            if (n >> l) & 1 == 1 {
                out.push(NodeId { l, i: first >> l });
                first += 1i64 << l;
            }
        }
        out
    }

    /// The sentence a failed zoom ends with.
    pub fn root_hint(&self) -> String {
        let roots = self.roots();
        if roots.is_empty() {
            return " The chat is empty.".to_string();
        }
        let names: Vec<String> = roots.iter().map(|r| r.name()).collect();
        format!(" The root nodes are {}.", names.join(", "))
    }

    /// Opens a node, the zoom(id, n) tool. For n = 1 it is the message
    /// itself; for a larger node, its two halves as view lines.
    #[allow(dead_code)] // zoom_page is the one the callers use
    pub fn zoom(&self, id: i64, n: i64) -> Result<String, String> {
        self.zoom_page(id, n, 0, 0)
    }

    /// zoom with a page: size bytes from byte offset of the node's text
    /// (size 0 means ZOOM_PAGE_BYTES; more is cut to it). A page that is not
    /// the whole text ends with a line that says which bytes it holds and
    /// where the next page starts. A cut never splits a UTF-8 rune. A bad id
    /// answers with the root ids.
    pub fn zoom_page(&self, id: i64, n: i64, offset: i64, size: i64) -> Result<String, String> {
        let text = match self.zoom_text(id, n) {
            Ok(t) => t,
            Err((e, true)) => return Err(format!("{e}.{}", self.root_hint())),
            Err((e, false)) => return Err(e),
        };
        if offset < 0 || size < 0 {
            return Err("foreman: offset and size must not be negative".to_string());
        }
        let size = if size == 0 || size > ZOOM_PAGE_BYTES as i64 {
            ZOOM_PAGE_BYTES
        } else {
            size as usize
        };
        if offset == 0 && text.len() <= size {
            return Ok(text);
        }
        let mut start = (offset as u64).min(text.len() as u64) as usize;
        while start < text.len() && !text.is_char_boundary(start) {
            start += 1;
        }
        if offset != 0 && start >= text.len() {
            return Err(format!(
                "foreman: the offset is past the end: the node has {} bytes",
                text.len()
            ));
        }
        let mut end = start + size;
        if end >= text.len() {
            end = text.len();
        } else {
            while end > start && !text.is_char_boundary(end) {
                end -= 1;
            }
            if end == start {
                // The size is under one rune: take the rune.
                end = start + text[start..].chars().next().map_or(0, char::len_utf8);
            }
        }
        let tail = if end >= text.len() {
            "End.".to_string()
        } else {
            format!("More: offset {end}")
        };
        Ok(format!(
            "{}\n[bytes {start}-{end} of {}. {tail}]",
            &text[start..end],
            text.len()
        ))
    }

    /// The whole text of a node. The flag is set when the error is a bad
    /// node name, which gets the root hint.
    fn zoom_text(&self, id: i64, n: i64) -> Result<String, (String, bool)> {
        let node = node_of(id, n).map_err(|e| (e, true))?;
        if node.last() >= self.msgs.len() as i64 {
            return Err((
                format!(
                    "foreman: node {}: no such node; the chat has {} messages",
                    node.name(),
                    self.msgs.len()
                ),
                true,
            ));
        }
        if node.l == 0 {
            return self.message(id).map(|m| m.text).map_err(|e| (e, false));
        }
        let (l, r) = node.children();
        Ok(self.line(l) + &self.line(r))
    }

    // The view.

    /// Reads view.json, or None when there is none.
    fn read_view_file(&self) -> Result<Option<ViewFile>, Fault> {
        let p = self.view_path();
        let data = match fs::read(&p) {
            Ok(d) => d,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(Fault::Io(fsutil::path_err("open", &p, &e))),
        };
        serde_json::from_slice::<ViewFile>(&data)
            .map(Some)
            .map_err(|e| {
                corrupt(
                    "view.json is not valid JSON",
                    format!("foreman: {}: {e}", p.display()),
                )
            })
    }

    /// Checks a read view.json against the log and takes it. No file is an
    /// empty view.
    fn apply_view(&mut self, f: Option<ViewFile>) -> Result<(), Fault> {
        let Some(f) = f else {
            self.view = ViewState::default();
            return Ok(());
        };
        let p = self.view_path();
        let mismatch = |m: String| corrupt("view.json does not match the log", m);
        let mut v = ViewState {
            count: f.count,
            batch: f.batch,
            lines: Vec::with_capacity(f.lines.len()),
        };
        let mut next = 0i64;
        for (k, pair) in f.lines.iter().enumerate() {
            let (l, i) = (
                pair.first().copied().unwrap_or(0),
                pair.get(1).copied().unwrap_or(0),
            );
            if l < 0 || l > MAX_LEVEL as i64 || i < 0 || i > (1i64 << (MAX_LEVEL as i64 - l)) {
                return Err(mismatch(format!(
                    "foreman: {}: line {k} is not a node",
                    p.display()
                )));
            }
            let id = NodeId { l: l as i32, i };
            if id.first() != next {
                return Err(mismatch(format!(
                    "foreman: {}: line {k} starts at {}, want {next}",
                    p.display(),
                    id.first()
                )));
            }
            next = id.last() + 1;
            v.lines.push(id);
        }
        if next != v.count || v.count > self.msgs.len() as i64 {
            return Err(mismatch(format!(
                "foreman: {} covers {next} messages but says {}; the log has {}",
                p.display(),
                v.count,
                self.msgs.len()
            )));
        }
        self.view = v;
        Ok(())
    }

    fn save_view(&self) -> Result<(), String> {
        #[cfg(test)]
        if FAIL_VIEW_WRITE.with(|c| c.get()) {
            return Err("injected disk error".to_string());
        }
        fsutil::write_atomic(&self.view_path(), self.view.encode().as_bytes())
    }

    /// view.json as the chat would write it now.
    pub fn view_json(&self) -> String {
        self.view.encode()
    }

    /// The view's nodes, oldest first.
    pub fn view_lines(&self) -> Vec<NodeId> {
        self.view.lines.clone()
    }

    /// One view line: "id+n|text" and a newline.
    fn line(&self, id: NodeId) -> String {
        let text = self.nodes.get(&id).map_or(UNBUILT, |n| n.text.as_str());
        format!("{}|{}\n", id.name(), flat(text))
    }

    /// The view's lines with esc applied to the text of each. The line that
    /// is the single node of message skip is left out; a skip below 0
    /// leaves nothing out.
    pub fn view_text(&self, esc: impl Fn(&str) -> String, skip: i64) -> Vec<ViewLine> {
        let mut out = Vec::with_capacity(self.view.lines.len());
        for &id in &self.view.lines {
            if skip >= 0 && id.l == 0 && id.i == skip {
                continue;
            }
            let text = self.nodes.get(&id).map_or(UNBUILT, |n| n.text.as_str());
            out.push(ViewLine {
                id,
                text: format!("{}|{}\n", id.name(), esc(&flat(text))),
            });
        }
        out
    }

    /// The size of the view's lines in bytes, the number the bounds hold.
    pub fn view_size(&self) -> usize {
        self.view.lines.iter().map(|&id| self.line(id).len()).sum()
    }

    /// The view as a turn sees it: the lines inside chat tags.
    pub fn render(&self) -> String {
        let mut b = String::from("<chat>\n");
        for &id in &self.view.lines {
            b.push_str(&self.line(id));
        }
        b.push_str("</chat>\n");
        b
    }

    /// Adds message t to the view and runs the sawtooth.
    fn view_step(&mut self, t: i64) {
        self.view.lines.push(NodeId { l: 0, i: t });
        self.view.count = t + 1;
        let mut size = self.view_size();
        if size > self.cfg.view_max {
            self.view.batch = true;
        }
        if !self.view.batch {
            return;
        }
        let nodes = &self.nodes;
        let mut p = Planner::new(&self.view.lines, self.view.count, |id| {
            nodes.contains_key(&id)
        });
        let line_len = |id: NodeId| {
            let text = nodes.get(&id).map_or(UNBUILT, |n| n.text.as_str());
            format!("{}|{}\n", id.name(), flat(text)).len()
        };
        while size > self.cfg.view_min {
            let Some(k) = p.pop() else { break };
            size -= line_len(p.id(k)) + line_len(p.id_after(k));
            size += line_len(p.merge(k));
        }
        let lines = p.lines();
        if size <= self.cfg.view_min {
            self.view.batch = false;
        }
        self.view.lines = lines;
    }

    // Building the tree.

    /// Looks at the front node: the line for a node that needs no model, or
    /// the job for one that does.
    fn prepare(&self, id: NodeId) -> Result<(Option<Job>, String), String> {
        let limit = self.cfg.node_bytes;
        if id.l == 0 {
            let m = self.message(id.i)?;
            if m.text.len() <= limit {
                return Ok((None, finish(&m.text, limit)));
            }
            return Ok((
                Some(Job {
                    id,
                    msg: m,
                    left: String::new(),
                    right: String::new(),
                    limit,
                }),
                String::new(),
            ));
        }
        let (lid, rid) = id.children();
        let (Some(l), Some(r)) = (self.nodes.get(&lid), self.nodes.get(&rid)) else {
            return Err(format!("node {} has an input that is not built", id.name()));
        };
        if l.text.len() + 1 + r.text.len() <= limit {
            return Ok((None, finish(&format!("{}\n{}", l.text, r.text), limit)));
        }
        Ok((
            Some(Job {
                id,
                msg: Message::default(),
                left: l.text.clone(),
                right: r.text.clone(),
                limit,
            }),
            String::new(),
        ))
    }

    /// Builds every node at the front that needs no model, steps the view,
    /// and returns the job for the first node that does. The job is None
    /// when nothing is pending, or when the front node waits for a retry;
    /// then the error is the one that put it to wait.
    pub fn next(&mut self) -> (Option<Job>, Option<String>) {
        let mut job = None;
        let mut err = None;
        while let Some(&id) = self.pending.front() {
            if let Some(w) = &self.wait {
                if w.id == id && self.cfg.now() < w.at {
                    err = Some(w.err.clone());
                    break;
                }
            }
            if let Some((hid, text)) = self.held.clone() {
                if hid == id {
                    if let Err(e) = self.write(id, &text) {
                        err = Some(e);
                        break;
                    }
                    self.held = None;
                    continue;
                }
            }
            match self.prepare(id) {
                Err(e) => {
                    err = Some(format!("foreman: {e}"));
                    break;
                }
                Ok((Some(j), _)) => {
                    job = Some(j);
                    break;
                }
                Ok((None, text)) => {
                    if let Err(e) = self.write(id, &text) {
                        err = Some(e);
                        break;
                    }
                }
            }
        }
        if let Err(e) = self.advance() {
            // Only a view save error that next() returns marks the view as
            // unsavable; a tree error that came first is returned as it is.
            if err.is_none() {
                err = Some(e);
                self.view_save_failed = true;
            }
        }
        if err.is_some() {
            return (None, err);
        }
        (job, None)
    }

    /// Writes the line a job got, then builds what follows that needs no
    /// model and steps the view.
    pub fn commit(&mut self, j: &Job, text: &str) -> Result<(), String> {
        if self.pending.front() == Some(&j.id) {
            // Only this job's own retry state ends here: a live job that
            // moved ahead of a failed import job must not reset its backoff.
            if matches!(&self.wait, Some(w) if w.id == j.id) {
                self.wait = None;
            }
            let text = finish(text, j.limit);
            if let Err(e) = self.write(j.id, &text) {
                self.held = Some((j.id, text));
                return Err(e);
            }
        }
        self.settle()
    }

    /// Records that the call for a job failed. Next hands the job out again
    /// only after its wait.
    pub fn fail_err(&mut self, j: &Job, err: &str) {
        let now = self.cfg.now();
        if !matches!(&self.wait, Some(w) if w.id == j.id) {
            self.wait = Some(RetryState {
                id: j.id,
                at: now,
                fails: 0,
                err: String::new(),
            });
        }
        let Some(w) = self.wait.as_mut() else { return };
        w.fails += 1;
        let step = (w.fails - 1).min(RETRY_STEPS.len() - 1);
        w.at = now + RETRY_STEPS[step];
        w.err = err.to_string();
    }

    /// Ends the wait, so the next Next hands the failed node out again. The
    /// count of failures stays.
    pub fn retry_now(&mut self) {
        if let Some(w) = &mut self.wait {
            w.at = SystemTime::UNIX_EPOCH - Duration::from_secs(1 << 40);
        }
    }

    /// How long until the failed front node may be asked again, or None
    /// when no node waits for a retry.
    pub fn retry_in(&self) -> Option<Duration> {
        let w = self.wait.as_ref()?;
        if self.pending.front() != Some(&w.id) {
            return None;
        }
        Some(
            w.at.duration_since(self.cfg.now())
                .unwrap_or(Duration::ZERO),
        )
    }

    fn settle(&mut self) -> Result<(), String> {
        match self.next() {
            (_, Some(e)) => Err(e),
            _ => Ok(()),
        }
    }

    /// Appends the front pending node to the tree file.
    fn write(&mut self, id: NodeId, text: &str) -> Result<(), String> {
        let n = Node {
            l: id.l as i64,
            i: id.i,
            text: text.to_string(),
            size: text.len() as i64,
        };
        let day = day_text(self.cfg.now());
        let path = self.tree_dir().join(format!("{day}.jsonl"));
        fsutil::append_line(&path, n.encode().as_bytes()).map_err(|e| e.msg)?;
        self.nodes.insert(id, n);
        self.pending.pop_front();
        Ok(())
    }

    /// Whether every node queued for message i is written.
    fn complete(&self, i: i64) -> bool {
        chain(i)
            .iter()
            .all(|id| self.nodes.contains_key(id) || self.skip.contains(id))
    }

    /// Steps the view over each message in order while its nodes are all
    /// written, and saves the view when it moved.
    fn advance(&mut self) -> Result<(), String> {
        if !self.step_view() {
            return Ok(());
        }
        self.save_view()
    }

    /// Steps the view over each message in order while its nodes are all
    /// written. It reports whether the view moved.
    fn step_view(&mut self) -> bool {
        let mut moved = false;
        while self.view.count < self.msgs.len() as i64 && self.complete(self.view.count) {
            self.view_step(self.view.count);
            moved = true;
        }
        moved
    }

    /// Rebuilds the view in memory from the tree, from an empty view. It
    /// writes nothing.
    fn replay_view(&mut self) {
        self.view = ViewState::default();
        self.step_view();
    }

    /// The messages the view does not cover yet, word for word, oldest
    /// first.
    pub fn tail(&self) -> Result<Vec<Message>, String> {
        (self.view.count..self.msgs.len() as i64)
            .map(|i| self.message(i))
            .collect()
    }

    /// Whether node id is an import node: the message that completes it is
    /// a past message. A node that merges imported lines into a live message
    /// is a live node.
    pub fn import_node(&self, id: NodeId) -> bool {
        let last = id.last();
        last >= 0 && (last as usize) < self.msgs.len() && self.msgs[last as usize].past
    }

    /// Moves the live nodes whose inputs are built ahead of the first import
    /// node, so a live message is compacted before any import job queued
    /// earlier (FM-R-72). A live node that waits for an import node as its
    /// input stays behind it. The order among live nodes, and among the
    /// rest, does not change.
    pub fn prioritize(&mut self) {
        if self.held.is_some() || self.pending.len() < 2 {
            return;
        }
        let Some(first) = self.pending.iter().position(|id| self.import_node(*id)) else {
            return;
        };
        let (mut live, mut rest) = (Vec::new(), Vec::new());
        for id in self.pending.iter().skip(first).copied() {
            let ready = id.l == 0 || {
                let (l, r) = id.children();
                self.nodes.contains_key(&l) && self.nodes.contains_key(&r)
            };
            if ready && !self.import_node(id) {
                live.push(id);
            } else {
                rest.push(id);
            }
        }
        if live.is_empty() {
            return;
        }
        let mut out: VecDeque<NodeId> = self.pending.iter().take(first).copied().collect();
        out.extend(live);
        out.extend(rest);
        self.pending = out;
    }

    /// Builds the pending nodes with s, one at a time, until none can be
    /// built now. See drain_with for the form that takes a lock.
    pub fn drain(&mut self, ctx: &Ctx, s: &dyn Summarizer) -> Result<(), String> {
        loop {
            self.prioritize();
            let (job, err) = self.next();
            if let Some(e) = err {
                return Err(e);
            }
            let Some(job) = job else { return Ok(()) };
            if let Some(e) = ctx.err() {
                return Err(e.text().into());
            }
            match run_job(ctx, s, &job) {
                Err(e) => {
                    if ctx.err().is_none() {
                        self.fail_err(&job, &e);
                    }
                    return Err(e);
                }
                Ok(text) => self.commit(&job, &text)?,
            }
        }
    }
}

/// Asks s for a job's line.
pub fn run_job(ctx: &Ctx, s: &dyn Summarizer, j: &Job) -> Result<String, String> {
    if j.id.l == 0 {
        return s.compress(ctx, &j.msg, j.limit);
    }
    s.merge(ctx, j.id, &j.left, &j.right, j.limit)
}

/// Builds the pending nodes of the chat in slot with s, one at a time,
/// until none can be built now. It holds the lock around each step on the
/// chat and never around a model call. A node whose call fails waits and
/// stays pending; drain returns that error, or the error that put the
/// front node to wait.
pub fn drain_locked(
    slot: &std::sync::Mutex<Option<Chat>>,
    ctx: &Ctx,
    s: &dyn Summarizer,
) -> Result<(), String> {
    drain_locked_gated(slot, ctx, s, &|| false)
}

/// drain_locked with a gate: a live node always goes before an import node,
/// and when hold says true no model call starts for an import node; the
/// node stays pending and drain returns Ok. hold runs outside the lock.
pub fn drain_locked_gated(
    slot: &std::sync::Mutex<Option<Chat>>,
    ctx: &Ctx,
    s: &dyn Summarizer,
    hold: &dyn Fn() -> bool,
) -> Result<(), String> {
    let lock = || slot.lock().unwrap_or_else(|e| e.into_inner());
    loop {
        let (job, imp) = {
            let mut g = lock();
            let Some(c) = g.as_mut() else { return Ok(()) };
            c.prioritize();
            match c.next() {
                (_, Some(e)) => return Err(e),
                (None, None) => return Ok(()),
                (Some(j), None) => {
                    let imp = c.import_node(j.id);
                    (j, imp)
                }
            }
        };
        if let Some(e) = ctx.err() {
            return Err(e.text().into());
        }
        if imp && hold() {
            return Ok(());
        }
        let res = run_job(ctx, s, &job);
        let mut g = lock();
        let Some(c) = g.as_mut() else { return Ok(()) };
        match res {
            Err(e) => {
                if ctx.err().is_none() {
                    c.fail_err(&job, &e);
                }
                return Err(e);
            }
            Ok(text) => c.commit(&job, &text)?,
        }
    }
}

/// Renames cfg.dir to a free <dir>.broken-<UTC time> name in the same
/// parent, mode 0700, and returns the new path.
fn move_aside(cfg: &Config) -> Result<String, String> {
    let base = format!(
        "{}.broken-{}",
        cfg.dir.to_string_lossy(),
        stamp_text(cfg.now())
    );
    let mut target = base.clone();
    let mut n = 2;
    while fs::symlink_metadata(&target).is_ok() {
        target = format!("{base}-{n}");
        n += 1;
    }
    fs::rename(&cfg.dir, &target).map_err(|e| {
        format!(
            "the history could not be moved aside: rename {} {target}: {}",
            cfg.dir.display(),
            crate::sys::go_errno_text(&e)
        )
    })?;
    crate::broken::prune(Path::new(&target));
    use std::os::unix::fs::PermissionsExt;
    #[cfg(test)]
    if FAIL_CHMOD_ASIDE.with(|c| c.get()) {
        return Err(format!(
            "the history was moved to {target}, but injected disk error"
        ));
    }
    fs::set_permissions(&target, fs::Permissions::from_mode(fsutil::DIR_MODE)).map_err(|e| {
        format!(
            "the history was moved to {target}, but {}",
            fsutil::path_err("chmod", Path::new(&target), &e)
        )
    })?;
    let parent = Path::new(&target)
        .parent()
        .map(Path::to_path_buf)
        .unwrap_or_default();
    fsutil::sync_dir(&parent).map_err(|e| format!("the history was moved to {target}, but {e}"))?;
    Ok(target)
}

/// Reads one tree line. A line is a node only when it is a JSON object that
/// holds the keys l, i, text and size in exactly that case. null, a wrong key
/// case (also in an extra key), a missing key, a lone surrogate escape and
/// invalid UTF-8 are bad lines, as in the Go port.
pub(super) fn parse_node(line: &[u8]) -> Option<Node> {
    let v: serde_json::Value = serde_json::from_slice(line).ok()?;
    let o = v.as_object()?;
    for k in ["l", "i", "text", "size"] {
        o.get(k)?;
    }
    // A key that is one of the four only in another case is a bad line, as
    // in the Go port, whose reader would match it without case.
    let fold = |k: &str| -> String {
        k.chars()
            .map(|c| match c {
                '\u{17f}' => 's',
                '\u{212a}' => 'k',
                c => c.to_ascii_lowercase(),
            })
            .collect()
    };
    for k in o.keys() {
        if !["l", "i", "text", "size"].contains(&k.as_str())
            && ["l", "i", "text", "size"].contains(&fold(k).as_str())
        {
            return None;
        }
    }
    serde_json::from_value(v).ok()
}

/// base, or base-2, base-3 ... the first name nothing has.
pub(crate) fn free_name(base: &str) -> String {
    let mut target = base.to_string();
    let mut n = 2;
    while fs::symlink_metadata(&target).is_ok() {
        target = format!("{base}-{n}");
        n += 1;
    }
    target
}

/// Writes data to a new file, 0600, and fsyncs it and its dir. The file
/// must not exist.
fn write_new(path: &Path, data: &[u8]) -> Result<(), String> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(fsutil::FILE_MODE)
        .open(path)
        .map_err(|e| fsutil::path_err("open", path, &e))?;
    f.write_all(data)
        .map_err(|e| fsutil::path_err("write", path, &e))?;
    f.sync_all()
        .map_err(|e| fsutil::path_err("sync", path, &e))?;
    drop(f);
    fsutil::sync_dir(path.parent().unwrap_or(Path::new(".")))
}

/// dir's YYYY-MM-DD.jsonl files in name order.
fn day_files(dir: &Path) -> Result<Vec<String>, String> {
    let rd = match fs::read_dir(dir) {
        Ok(r) => r,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(fsutil::path_err("open", dir, &e)),
    };
    let mut out = Vec::new();
    for e in rd.flatten() {
        let n = e.file_name().to_string_lossy().into_owned();
        let regular = e.file_type().map(|t| t.is_file()).unwrap_or(false);
        if regular {
            if let Some(stem) = n.strip_suffix(".jsonl") {
                if is_day(stem) {
                    out.push(n);
                }
            }
        }
    }
    out.sort();
    Ok(out)
}

/// Takes the chat's lock: one writer per chat. It fails at once if another
/// process holds it.
fn lock_dir(path: &Path) -> Result<fs::File, String> {
    use std::os::unix::fs::OpenOptionsExt;
    let f = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(fsutil::FILE_MODE)
        .open(path)
        .map_err(|e| fsutil::path_err("open", path, &e))?;
    // SAFETY: flock on a descriptor this function owns.
    let rc = unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
    if rc != 0 {
        let e = std::io::Error::last_os_error();
        if e.raw_os_error() == Some(libc::EWOULDBLOCK) {
            return Err(ERR_LOCKED.into());
        }
        return Err(crate::sys::go_errno_text(&e));
    }
    Ok(f)
}

#[cfg(test)]
mod internal_tests {
    use super::*;

    /// A batch that runs out of built pairs stays open and goes on at the
    /// next message, once the parents it needs are built.
    #[test]
    fn a_batch_waits_for_unbuilt_parents() {
        let mut cfg = Config {
            view_max: 1000,
            view_min: 500,
            ..Default::default()
        };
        cfg.fill();
        let mut c = Chat::empty(cfg, false);
        let text = "p".repeat(300);
        for i in 0..4 {
            c.nodes.insert(
                NodeId { l: 0, i },
                Node {
                    l: 0,
                    i,
                    text: text.clone(),
                    size: 300,
                },
            );
            c.view_step(i);
        }
        assert!(c.view.batch && c.view.lines.len() == 4);
        for (l, i, t) in [(1, 0, "a"), (1, 1, "c"), (0, 4, "b")] {
            let n = Node {
                l,
                i,
                text: t.into(),
                size: 1,
            };
            c.nodes.insert(n.id(), n);
        }
        c.view_step(4);
        assert!(!c.view.batch && c.view_size() <= 500, "{:?}", c.view.lines);
    }
}
