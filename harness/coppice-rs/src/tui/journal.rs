//! The journal on the floor: the gate verdicts the server was told,
//! newest first, drawn over the windows. j on the rail opens it.

use crate::textwidth;

use super::keys::Key;
use super::model::{num, s, Model, Obj};
use super::render::{pad, SGR_ACCENT, SGR_AMBER, SGR_RESET};
use super::run::{call, list_of, Floor};

/// What the journal lists, in the words the web page uses.
pub const JOURNAL_TITLE: &str =
    "Journal · the verdicts the gate and each agent's loop report, newest first. 100 per agent, 500 in all, oldest first";

/// One verdict from floor.journal.
#[derive(Clone, Debug, PartialEq)]
pub struct JournalRow {
    pub at: f64,
    pub pane: String,
    pub decision: String,
    pub tool: String,
    pub clause: String,
}

/// The open journal: the verdicts newest first, how far the owner
/// scrolled, and the words of a read that failed.
#[derive(Clone, Debug, Default)]
pub struct JournalView {
    pub rows: Vec<JournalRow>,
    /// The first row drawn. Down moves toward older verdicts.
    pub top: usize,
    /// The server's refusal when the last read failed, or empty.
    pub err: String,
}

/// Reads a floor.journal reply. A row that is not shaped as a verdict is
/// dropped.
pub fn decode_journal(res: &Obj) -> Vec<JournalRow> {
    let mut out = Vec::new();
    for e in list_of(res, "entries") {
        let (Some(at), pane, decision) = (num(&e, "at"), s(&e, "pane"), s(&e, "decision")) else {
            continue;
        };
        if pane.is_empty() || decision.is_empty() {
            continue;
        }
        out.push(JournalRow {
            at,
            pane,
            decision,
            tool: s(&e, "tool"),
            clause: s(&e, "clause"),
        });
    }
    out
}

/// A unix time as HH:MM:SS, local time.
fn clock(at: f64) -> String {
    let t = at.floor() as libc::time_t;
    // SAFETY: localtime_r writes only into tm, which lives on this frame.
    unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        if libc::localtime_r(&t, &mut tm).is_null() {
            return "--:--:--".into();
        }
        format!("{:02}:{:02}:{:02}", tm.tm_hour, tm.tm_min, tm.tm_sec)
    }
}

/// Draws the journal in height lines of cols cells: a title, then one line
/// per verdict from the scroll point.
pub fn journal_lines(m: &Model, cols: i64, height: i64) -> Vec<String> {
    let Some(v) = &m.journal else {
        return Vec::new();
    };
    let mut lines = vec![format!(
        "{SGR_ACCENT}{}{SGR_RESET}",
        pad(&textwidth::truncate(JOURNAL_TITLE, cols), cols)
    )];
    if !v.err.is_empty() {
        let text = format!(
            "The journal did not load: {}",
            textwidth::printable(&v.err, 0)
        );
        lines.push(format!(
            "{SGR_AMBER}{}{SGR_RESET}",
            pad(&textwidth::truncate(&text, cols), cols)
        ));
    } else if v.rows.is_empty() {
        lines.push(pad("No gate verdicts yet.", cols));
    }
    for r in v.rows.iter().skip(v.top) {
        if lines.len() as i64 >= height {
            break;
        }
        let label = match m.row(&r.pane) {
            Some(row) if !row.label.is_empty() => row.label,
            _ => r.pane.clone(),
        };
        let glyph = match r.decision.as_str() {
            "allow" => "✓",
            "deny" => "✕",
            _ => "?",
        };
        let mut what = vec![r.decision.clone()];
        if !r.tool.is_empty() {
            what.push(r.tool.clone());
        }
        if !r.clause.is_empty() {
            what.push(format!("clause {}", r.clause));
        }
        let text = format!(
            " {} {}  {}  {}",
            clock(r.at),
            glyph,
            textwidth::printable(&label, 0),
            textwidth::printable(&what.join(" · "), 0)
        );
        let color = if r.decision == "allow" { "" } else { SGR_AMBER };
        lines.push(format!(
            "{color}{}{SGR_RESET}",
            pad(&textwidth::truncate(&text, cols), cols)
        ));
    }
    while (lines.len() as i64) < height {
        lines.push(pad("", cols));
    }
    lines.truncate(height.max(0) as usize);
    lines
}

impl Floor<'_> {
    /// Asks floor.journal and keeps the rows. A failed read keeps the last
    /// rows and says why.
    pub fn read_journal(&mut self) {
        if self.m.journal.is_none() {
            return;
        }
        let res = call(&self.o.socket, "floor.journal", None);
        let Some(j) = &mut self.m.journal else {
            return;
        };
        match res {
            Err(e) => j.err = e.text(),
            Ok(res) => {
                j.err.clear();
                j.rows = decode_journal(&res);
                if j.top >= j.rows.len() {
                    j.top = j.rows.len().saturating_sub(1);
                }
            }
        }
    }

    /// Opens the journal over the windows and reads it.
    pub fn open_journal(&mut self) {
        self.m.journal = Some(JournalView::default());
        self.read_journal();
    }

    /// Applies one keystroke while the journal is open. Up and Down
    /// scroll. Esc, j or q closes it. Every other key does nothing.
    pub fn journal_key(&mut self, k: &Key) {
        let Some(j) = &mut self.m.journal else {
            return;
        };
        match k {
            Key::Up => j.top = j.top.saturating_sub(1),
            Key::Down => j.top = (j.top + 1).min(j.rows.len().saturating_sub(1)),
            Key::Esc | Key::Byte(b'j') | Key::Byte(b'q') => self.m.journal = None,
            _ => {}
        }
    }
}
