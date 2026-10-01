//! The prompt line: plumbing the floor runs itself, or talk meant for a
//! foreman; and floor.json, the layout and the lock between runs.

use std::io::Write;
use std::path::Path;

use serde_json::Value;

use crate::tiles::{Layout, ERR_LOCKED};

/// What a prompt line is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Plumbing,
    TalkLine,
}

/// The harness names a prompt line may open by name.
pub const HARNESSES: &[&str] = &["claude", "codex", "pi", "sprig", "opencode"];

/// The plumbing verbs the prompt line answers to.
pub const VERBS: &[&str] = &[
    "list", "read", "close", "open", "swap", "rotate", "reset", "lock", "unlock", "layout", "tree",
    "zoom", "foreman",
];

/// The label of the pane "foreman <harness>" opens.
pub const FOREMAN_LABEL: &str = "foreman";

/// The one line that makes a pane the foreman.
#[cfg(test)]
pub const PROMOTION_LINE: &str =
    "Run: coppice skill foreman. Follow it. You are the foreman of this floor. Say ready.";

/// The line that makes a pane the foreman of one task.
pub fn task_promotion_line(task: &str) -> String {
    format!(
        "Run: coppice skill foreman. Follow it. You are the foreman of task {task}. Its panes' asks come to you first. Say ready."
    )
}

pub const NO_FOREMAN_ANSWER: &str =
    "no foreman yet. Say a sentence and one starts, or type: foreman claude";

pub const LOCKED_MESSAGE: &str = "Layout is locked. Type unlock first.";
pub const SWAP_NEEDS_MESSAGE: &str = "swap needs two slot numbers, as in swap 1 2";
pub const LAYOUT_NEEDS_MESSAGE: &str = "layout is all, focus, or one";

/// The file under the data dir that keeps the layout and the lock.
pub const FLOOR_FILE: &str = "floor.json";

/// Reads a layout name.
pub fn parse_layout(name: &str) -> Option<Layout> {
    Layout::parse(name)
}

/// Reads the layout and the lock from floor.json under data_dir. A
/// missing or unreadable file, or no data dir, is focus and unlocked.
pub fn read_floor_state(data_dir: &str) -> (Layout, bool) {
    if data_dir.is_empty() {
        return (Layout::Focus, false);
    }
    let Ok(b) = std::fs::read(Path::new(data_dir).join(FLOOR_FILE)) else {
        return (Layout::Focus, false);
    };
    let Ok(v) = serde_json::from_slice::<Value>(&b) else {
        return (Layout::Focus, false);
    };
    let (mut layout, mut locked) = (String::new(), false);
    match &v {
        Value::Null => {}
        Value::Object(m) => {
            match m.get("layout") {
                None | Some(Value::Null) => {}
                Some(Value::String(x)) => layout = x.clone(),
                Some(_) => return (Layout::Focus, false),
            }
            match m.get("locked") {
                None | Some(Value::Null) => {}
                Some(Value::Bool(x)) => locked = *x,
                Some(_) => return (Layout::Focus, false),
            }
        }
        _ => return (Layout::Focus, false),
    }
    (parse_layout(&layout).unwrap_or(Layout::Focus), locked)
}

/// Writes the layout and the lock to floor.json under data_dir, through a
/// temp file renamed into place.
pub fn write_floor_state(data_dir: &str, layout: Layout, locked: bool) -> Result<(), String> {
    if data_dir.is_empty() {
        return Ok(());
    }
    let b = format!("{{\"layout\":\"{}\",\"locked\":{locked}}}", layout.name());
    let dir = Path::new(data_dir);
    let (name, mut f) = create_temp(dir)?;
    if let Err(e) = f.write_all(b.as_bytes()) {
        let _ = std::fs::remove_file(&name);
        return Err(e.to_string());
    }
    drop(f);
    if let Err(e) = std::fs::rename(&name, dir.join(FLOOR_FILE)) {
        let _ = std::fs::remove_file(&name);
        return Err(format!(
            "rename {} {}: {}",
            name.display(),
            dir.join(FLOOR_FILE).display(),
            crate::sys::go_errno_text(&e)
        ));
    }
    Ok(())
}

/// Go's os.CreateTemp(dir, "floor.json.*.tmp"): a new file of mode 0600.
fn create_temp(dir: &Path) -> Result<(std::path::PathBuf, std::fs::File), String> {
    use std::os::unix::fs::OpenOptionsExt;
    let mut last = String::new();
    for _ in 0..10000 {
        let n = crate::web::token::random_bytes(4)
            .map(|b| u32::from_le_bytes([b[0], b[1], b[2], b[3]]))
            .unwrap_or(0);
        let name = dir.join(format!("{FLOOR_FILE}.{n}.tmp"));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&name)
        {
            Ok(f) => return Ok((name, f)),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => {
                last = crate::sys::go_path_err(
                    "createtemp",
                    &dir.join(format!("{FLOOR_FILE}.*.tmp")),
                    &e,
                );
                break;
            }
        }
    }
    Err(last)
}

/// A refusal from the tiles model as the message line.
pub fn tiles_message(err: Result<(), String>) -> String {
    match err {
        Ok(()) => String::new(),
        Err(e) if e == ERR_LOCKED => LOCKED_MESSAGE.into(),
        Err(e) => e,
    }
}

/// Caps a verb-led plumbing line.
const PLUMBING_WORDS: usize = 4;

/// Tells plumbing from talk.
pub fn classify(line: &str, harnesses: &[&str]) -> Kind {
    let words: Vec<&str> = line.split_whitespace().collect();
    if words.is_empty() {
        return Kind::TalkLine;
    }
    if harnesses.contains(&words[0]) {
        return Kind::Plumbing;
    }
    if VERBS.contains(&words[0]) && words.len() <= PLUMBING_WORDS {
        return Kind::Plumbing;
    }
    Kind::TalkLine
}

/// classify that also knows the view ids.
pub fn classify_with(line: &str, harnesses: &[&str], views: &[String]) -> Kind {
    let words: Vec<&str> = line.split_whitespace().collect();
    if words.len() == 1 && views.iter().any(|v| v == words[0]) {
        return Kind::Plumbing;
    }
    classify(line, harnesses)
}
