//! How the floor draws: the bar, the rail, the windows, the lines under
//! them, and the prompt.

use crate::config;
use crate::textwidth;

use super::facts::{facts_line, gate_hint_shown, FACTS_MIN_COLS, GATE_HINT};
use super::keys::{join_keys, table, Action};
use super::model::{answerable, ItemKind, Model, PeekView, Row, Size};
use super::railtree::group_line;
use super::recent::recent_line;
use super::tree::TreeView;

pub const SGR_AMBER: &str = "\x1b[33m";
pub const SGR_BLUE: &str = "\x1b[34m";
pub const SGR_GREEN: &str = "\x1b[32m";
pub const SGR_FAINT: &str = "\x1b[2m";
pub const SGR_REVERSE: &str = "\x1b[7m";
pub const SGR_ACCENT: &str = "\x1b[1;36m";
pub const SGR_RESET: &str = "\x1b[0m";

/// Leads the prompt line and marks the cursor row.
pub const PROMPT_MARK: &str = "› ";

pub const DIVIDER: &str = "│";
pub const ACCENT_DIVIDER: &str = "┃";

/// Ends every live row and window header.
pub const CLOSE_MARK: &str = "×";

pub const RAIL_COLS: i64 = 38;
pub const MIN_WINDOW: i64 = 80;

pub const EMPTY_SLOT_TEXT: &str = "Empty. Select a row and press this window's number.";

pub const HEADLESS_PLACEHOLDER: &str = "This agent takes whole messages. Type and press Enter.";
pub const HEADLESS_MARK: &str = "> ";

/// s at exactly w cells.
pub fn pad(s: &str, w: i64) -> String {
    if w <= 0 {
        return String::new();
    }
    let mut out = textwidth::truncate(s, w);
    let n = w - textwidth::width(&out) as i64;
    if n > 0 {
        out.push_str(&" ".repeat(n as usize));
    }
    out
}

fn width(s: &str) -> i64 {
    textwidth::width(s) as i64
}

fn typing_words(leave: u8) -> String {
    format!("typing here · {} leaves", config::key_name(leave))
}

/// The key line for what has the keys now.
pub fn footer_text(m: &mut Model) -> String {
    if !m.typing.is_empty() {
        return format!(
            "{} back to the rail, then ctrl-c quits  click another window to type there  {} speak",
            config::key_name(m.leave),
            m.talk_name()
        );
    }
    if !m.confirm.is_empty() || m.clear_all {
        return "Enter yes  Esc no".into();
    }
    if !m.renaming.is_empty() {
        return "Enter renames  Esc keeps the name".into();
    }
    if m.talking {
        return "Enter runs the line  Esc lets it go".into();
    }
    if m.picker.is_some() {
        return format!(
            "1-9 or Enter starts {} there  Up Down move  Esc closes",
            m.default_harness()
        );
    }
    if m.tree.is_some() {
        return "Enter goes into the task  Up Down move  Esc closes".into();
    }
    if let Some(it) = m.selected_item() {
        match it.kind {
            ItemKind::Fold => {
                if m.recent_open {
                    return "Enter closes Recent  n new  N new in project  ctrl-t talk".into();
                }
                return "Enter opens Recent  n new  N new in project  ctrl-t talk".into();
            }
            ItemKind::Group => {
                if m.folded.get(&it.group.key).copied().unwrap_or(false) {
                    return "Enter or Space opens the group  n new  N new in project  shift-tab next need  ctrl-t talk".into();
                }
                return "Enter or Space folds the group  n new  N new in project  shift-tab next need  ctrl-t talk".into();
            }
            ItemKind::ResumeAll => return "Enter resumes every ended agent  Esc up".into(),
            ItemKind::ClearAll => return "Enter forgets every ended agent  Esc up".into(),
            ItemKind::Ended => return "Enter resume  ctrl-w forget  Esc up".into(),
            ItemKind::Row => {}
        }
    }
    let mut t = table();
    if answerable(&m.peek) {
        for b in t.iter_mut() {
            if b.action == Action::New {
                b.help = "deny";
            }
        }
    }
    join_keys(&t)
}

/// Wraps the footer into at most three lines of at most cols cells.
pub fn footer_lines(m: &mut Model, cols: i64) -> Vec<String> {
    let text = footer_text(m);
    let mut lines = wrap_footer(&text, cols);
    if m.typing.is_empty() && text.ends_with("Esc up") {
        let more = wrap_footer(&format!("{text}  {} speak", m.talk_name()), cols);
        if more.len() == lines.len() {
            lines = more;
        }
    }
    lines
}

/// Wraps text at its spaces into at most max lines of at most cols cells.
pub fn wrap_words(text: &str, cols: i64, max: usize) -> Vec<String> {
    let mut lines = Vec::new();
    let mut cur = String::new();
    for w in text.split_whitespace() {
        if cur.is_empty() {
            cur = w.to_string();
        } else if lines.len() + 1 < max && width(&format!("{cur} {w}")) > cols {
            lines.push(std::mem::take(&mut cur));
            cur = w.to_string();
        } else {
            cur.push(' ');
            cur.push_str(w);
        }
    }
    if !cur.is_empty() {
        lines.push(cur);
    }
    lines
}

/// Wraps text at its two-space gaps into lines of at most cols cells.
pub fn wrap_footer(text: &str, cols: i64) -> Vec<String> {
    let mut lines: Vec<String> = Vec::new();
    let mut cur = String::new();
    for p in text.split("  ") {
        if cur.is_empty() {
            cur = p.to_string();
        } else if width(&format!("{cur}  {p}")) <= cols {
            cur.push_str("  ");
            cur.push_str(p);
        } else {
            lines.push(std::mem::take(&mut cur));
            cur = p.to_string();
        }
    }
    lines.push(cur);
    if lines.len() > 3 {
        let rest = lines[2..].join("  ");
        lines.truncate(2);
        lines.push(rest);
    }
    lines.iter().map(|l| pad(l, cols)).collect()
}

/// How many windows a screen of cols columns shows.
pub fn window_count(cols: i64) -> i64 {
    let n = (cols - RAIL_COLS) / (MIN_WINDOW + 1);
    n.max(0)
}

/// The width of the rail and of each of n windows.
pub fn split_widths(cols: i64, n: i64) -> (i64, Vec<i64>) {
    let rail = RAIL_COLS.min(cols);
    let n = n.max(1);
    let avail = (cols - rail - n).max(0);
    let each = avail / n;
    let mut widths = vec![each; n as usize];
    widths[n as usize - 1] += avail - each * n;
    (rail, widths)
}

/// Reads an SGR sequence at s[i..]: its end, or None.
fn sgr_at(s: &[u8], i: usize) -> Option<usize> {
    if s[i] == 0x1b && i + 1 < s.len() && s[i + 1] == b'[' {
        let mut j = i + 2;
        while j < s.len() && (s[j] == b';' || s[j].is_ascii_digit()) {
            j += 1;
        }
        if j < s.len() && s[j] == b'm' {
            return Some(j + 1);
        }
    }
    None
}

/// Cuts a colored line to exactly w cells.
pub fn cut_sgr(s: &str, w: i64) -> String {
    if w <= 0 {
        return String::new();
    }
    let b = s.as_bytes();
    let mut pieces: Vec<(String, char)> = Vec::new();
    let mut plain = String::new();
    let mut pending = String::new();
    let mut i = 0;
    while i < b.len() {
        if let Some(end) = sgr_at(b, i) {
            pending.push_str(&s[i..end]);
            i = end;
            continue;
        }
        let r = s[i..].chars().next().unwrap_or('\u{FFFD}');
        pieces.push((std::mem::take(&mut pending), r));
        plain.push(r);
        i += r.len_utf8();
    }
    let kept = textwidth::truncate(&plain, w);
    let (mut n, mut tail) = (pieces.len(), "");
    if kept != plain {
        tail = textwidth::ELLIPSIS;
        n = kept.strip_suffix(tail).unwrap_or(&kept).chars().count();
    }
    let mut out = String::new();
    for (codes, r) in &pieces[..n] {
        out.push_str(codes);
        out.push(*r);
    }
    out.push_str(tail);
    out.push_str(SGR_RESET);
    let padn = w - width(&kept);
    if padn > 0 {
        out.push_str(&" ".repeat(padn as usize));
    }
    out
}

/// The SGR code for one pane state.
pub fn state_color(state: &str) -> &'static str {
    match state {
        "blocked" => SGR_AMBER,
        "working" => SGR_BLUE,
        "done" => SGR_GREEN,
        _ => SGR_FAINT,
    }
}

/// s with every SGR sequence removed.
#[cfg(test)]
pub fn strip_sgr(s: &str) -> String {
    if !s.contains("\x1b[") {
        return s.to_string();
    }
    let b = s.as_bytes();
    let mut out = Vec::new();
    let mut i = 0;
    while i < b.len() {
        if let Some(end) = sgr_at(b, i) {
            i = end;
            continue;
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// A row's age in the coarsest unit that is not zero.
pub fn age_text(age: f64) -> String {
    if age < 1.0 {
        "now".into()
    } else if age < 60.0 {
        format!("{}s", age as i64)
    } else if age < 3600.0 {
        format!("{}m", (age / 60.0) as i64)
    } else {
        format!("{}h", (age / 3600.0) as i64)
    }
}

/// Line 0: the program name, the task path, and who needs the operator.
pub fn bar(m: &Model) -> String {
    let mut head = "coppice".to_string();
    for id in &m.path {
        head.push_str(" › ");
        head.push_str(&textwidth::printable(&m.task_label(id), 0));
    }
    match m.need_you {
        0 => format!("{head} · quiet"),
        1 => format!("{head} · 1 needs you"),
        n => format!("{head} · {n} need you"),
    }
}

fn looking_text(names: &[String]) -> String {
    if names.is_empty() {
        return String::new();
    }
    let shown: Vec<String> = names.iter().map(|n| textwidth::printable(n, 0)).collect();
    format!("· {} looking", shown.join(", "))
}

/// n entries of -1.
pub fn no_rows(n: usize) -> Vec<i64> {
    vec![-1; n]
}

fn fit<T>(mut lines: Vec<T>, height: i64) -> Vec<T> {
    let h = height.max(0) as usize;
    lines.truncate(h);
    lines
}

impl Model {
    /// True while the rail has the keys.
    pub fn rail_focused(&self) -> bool {
        self.typing.is_empty()
            && !self.talking
            && self.renaming.is_empty()
            && self.confirm.is_empty()
            && !self.clear_all
            && self.picker.is_none()
            && self.tree.is_none()
    }

    /// The window number a row shows.
    pub fn window_tag(&self, id: &str) -> String {
        let Some(ts) = &self.tiles else {
            return String::new();
        };
        if self.shown == 0 {
            return String::new();
        }
        if let Some(slot) = ts.slot_of(id) {
            if (slot as i64) < self.shown {
                return format!("{} ", slot + 1);
            }
        }
        "  ".into()
    }

    /// The harness Enter opens on an empty floor.
    pub fn default_harness(&self) -> String {
        if self.default.is_empty() {
            return "claude".into();
        }
        self.default.clone()
    }

    /// One line for the stack when the server sent one.
    pub fn stack_lines(&self, id: &str) -> i64 {
        match self.row(id) {
            Some(r) if !r.stack.is_empty() => 1,
            _ => 0,
        }
    }
}

/// One pane's line.
pub fn row_line(r: &Row, cursor: bool, dim: bool, focused: bool, tag: &str, cols: i64) -> String {
    let mark = if cursor { PROMPT_MARK } else { "  " };
    let mut label = textwidth::printable(&r.label, 0);
    if label.is_empty() {
        label = r.id.clone();
    }
    let mut indent = "";
    if !r.parent.is_empty() {
        indent = "  ";
        if label == r.id {
            label = r.child.clone();
        }
    }
    let mut state = r.state.clone();
    let mut line = textwidth::printable(&r.line, 0);
    let mut color = state_color(&r.state).to_string();
    if let Some(h) = &r.held {
        state = "held".into();
        line = format!(
            "waiting on {}'s foreman · {}",
            textwidth::printable(&h.task_label, 0),
            age_text(h.waited)
        );
        color = state_color("working").into();
    }
    let fields = [
        label,
        textwidth::printable(&r.harness, 0),
        state,
        age_text(r.age),
        line,
        looking_text(&r.looking),
    ];
    let kept: Vec<&str> = fields
        .iter()
        .filter(|f| !f.is_empty())
        .map(|f| f.as_str())
        .collect();
    let text = format!("  {mark}{tag}{indent}{}", kept.join("  "));
    let mut plain = pad(&text, cols);
    if r.parent.is_empty() && cols > 4 {
        plain = format!("{} {CLOSE_MARK}", pad(&text, cols - 2));
    }
    if dim {
        color = SGR_FAINT.into();
    }
    if cursor && focused {
        color.push_str(SGR_REVERSE);
    }
    format!("{color}{plain}{SGR_RESET}")
}

/// The rail when no peek is open.
pub fn roster(m: &mut Model, cols: i64, height: i64) -> (Vec<String>, Vec<i64>) {
    let rows = m.visible();
    let items = m.items();
    m.norm(&items);
    let focused = m.rail_focused();
    let mut cursor_at: i64 = -1;
    let mut lines: Vec<String> = Vec::new();
    let mut at: Vec<i64> = Vec::new();
    if rows.is_empty() {
        let mut text = format!("Nothing running. Enter opens {} here.", m.default_harness());
        if !m.scope().is_empty() {
            text = format!(
                "No panes in {}. Esc goes back.",
                textwidth::printable(&m.task_label(&m.scope()), 0)
            );
        } else if !items.is_empty() {
            text = format!("Nothing running. n starts {}.", m.default_harness());
        }
        lines.push(pad(&text, cols));
        at.push(-1);
    }
    for (i, it) in items.iter().enumerate() {
        let i = i as i64;
        if it.kind == ItemKind::Fold {
            lines.push(pad("", cols));
            at.push(-1);
        }
        if i == m.cursor {
            cursor_at = lines.len() as i64;
        }
        match it.kind {
            ItemKind::Group => lines.push(group_line(m, &it.group, i == m.cursor, focused, cols)),
            ItemKind::Row => {
                let r = &it.row;
                let mut tag = m.window_tag(&r.id);
                if !r.parent.is_empty() && !tag.is_empty() {
                    tag = "  ".into();
                }
                lines.push(row_line(
                    r,
                    i == m.cursor,
                    !m.typing.is_empty(),
                    focused,
                    &tag,
                    cols,
                ));
            }
            _ => lines.push(recent_line(m, it, i == m.cursor, focused, cols)),
        }
        at.push(i);
    }
    if height > 0 && cursor_at >= height {
        let cut = (cursor_at - height + 1) as usize;
        lines.drain(..cut);
        at.drain(..cut);
        cursor_at = height - 1;
    }
    m.body_cur = cursor_at;
    (fit(lines, height), fit(at, height))
}

/// The open tree in height lines, and the cursor's line.
pub fn tree_body(v: &TreeView, cols: i64, height: i64) -> (Vec<String>, i64) {
    if v.lines.is_empty() {
        return (
            fit(
                vec![pad("No tasks. Run: coppice task create --label NAME", cols)],
                height,
            ),
            -1,
        );
    }
    let mut lines: Vec<String> = Vec::new();
    for (i, l) in v.lines.iter().enumerate() {
        let mark = if i as i64 == v.cursor {
            PROMPT_MARK
        } else {
            "  "
        };
        lines.push(pad(
            &textwidth::truncate(&format!("  {mark}{l}"), cols),
            cols,
        ));
    }
    let mut cur = v.cursor;
    if height > 0 && v.cursor >= height {
        lines.drain(..(v.cursor - height + 1) as usize);
        cur = height - 1;
    }
    (fit(lines, height), cur)
}

/// The open project picker in height lines, and the cursor's line.
pub fn picker_body(m: &Model, cols: i64, height: i64) -> (Vec<String>, i64) {
    let Some(p) = &m.picker else {
        return (Vec::new(), -1);
    };
    let mut lines = vec![pad(
        &format!("Start {} in a project", m.default_harness()),
        cols,
    )];
    if p.projects.is_empty() {
        lines.push(pad(
            "  No projects yet. Pin one with: coppice project add DIR",
            cols,
        ));
        return (fit(lines, height), -1);
    }
    let mut cur: i64 = -1;
    for (i, pr) in p.projects.iter().enumerate() {
        let mut mark = "  ";
        if i as i64 == p.cursor {
            mark = PROMPT_MARK;
            cur = lines.len() as i64;
        }
        let num = if i < 9 {
            format!("{} ", i + 1)
        } else {
            "  ".into()
        };
        let pin = if pr.pinned { "  pinned" } else { "" };
        let text = format!(
            "  {mark}{num}{}  {}{pin}",
            textwidth::printable(&pr.name, 0),
            textwidth::printable(&pr.path, 0)
        );
        lines.push(pad(&text, cols));
    }
    if height > 0 && cur >= height {
        lines.drain(..(cur - height + 1) as usize);
        cur = height - 1;
    }
    (fit(lines, height), cur)
}

/// Everything between the bar and the lines under it.
pub fn body(m: &mut Model, cols: i64, height: i64) -> (Vec<String>, Vec<i64>) {
    m.body_cur = -1;
    if height <= 0 {
        return (Vec::new(), Vec::new());
    }
    if m.picker.is_some() {
        let (lines, cur) = picker_body(m, cols, height);
        m.body_cur = cur;
        let n = lines.len();
        return (lines, no_rows(n));
    }
    if let Some(t) = &m.tree {
        let (lines, cur) = tree_body(t, cols, height);
        m.body_cur = cur;
        let n = lines.len();
        return (lines, no_rows(n));
    }
    let Some(peek) = m.peek.clone() else {
        return roster(m, cols, height);
    };
    let peek_h = height / 2;
    let (mut lines, mut at) = roster(m, cols, height - peek_h);
    while (lines.len() as i64) < height - peek_h {
        lines.push(pad("", cols));
    }
    lines.extend(peek_lines(&peek, cols, peek_h));
    let more = lines.len() - at.len();
    at.extend(no_rows(more));
    (lines, at)
}

/// The width of headless_mark+line to show and the cursor's column.
pub fn headless_visible(line: &str, cur: usize, w: i64) -> (String, i64) {
    if w <= 0 {
        return (String::new(), 0);
    }
    let cur = if cur > line.len() || !line.is_char_boundary(cur) {
        line.len()
    } else {
        cur
    };
    let full = format!("{HEADLESS_MARK}{line}");
    let cursor_w = width(&format!("{HEADLESS_MARK}{}", &line[..cur]));
    let want = (cursor_w - (w - 1)).max(0);
    let (shown, dropped) = drop_cells(&full, want);
    (pad(&textwidth::truncate(&shown, w), w), cursor_w - dropped)
}

/// Removes the first n cells of s at rune boundaries.
pub fn drop_cells(s: &str, n: i64) -> (String, i64) {
    let mut w = 0;
    for (i, r) in s.char_indices() {
        if w >= n {
            return (s[i..].to_string(), w);
        }
        w += textwidth::width(r.encode_utf8(&mut [0; 4])) as i64;
    }
    (String::new(), w)
}

fn headless_foot_line(m: &Model, id: &str, w: i64) -> String {
    let (line, cur) = m.headless_buf(id);
    let shown = if line.is_empty() {
        pad(&textwidth::truncate(HEADLESS_PLACEHOLDER, w), w)
    } else {
        headless_visible(&line, cur, w).0
    };
    if id == m.typing {
        return shown;
    }
    format!("{SGR_FAINT}{shown}{SGR_RESET}")
}

/// One window of w by h cells for slot i holding pane id.
pub fn tile_lines(m: &mut Model, id: &str, i: usize, w: i64, h: i64) -> Vec<String> {
    if h <= 0 {
        return Vec::new();
    }
    let mark = match &m.tiles {
        Some(ts) if i as i64 == ts.focus => PROMPT_MARK,
        _ => "  ",
    };
    let mut color = SGR_FAINT.to_string();
    let mut text = EMPTY_SLOT_TEXT.to_string();
    if !id.is_empty() {
        text = id.to_string();
        if let Some(r) = m.row(id) {
            color = state_color(&r.state).into();
            let mut label = textwidth::printable(&r.label, 0);
            if label.is_empty() {
                label = r.id.clone();
            }
            let parts: Vec<String> = [label, textwidth::printable(&r.harness, 0), r.state.clone()]
                .into_iter()
                .filter(|f| !f.is_empty())
                .collect();
            text = parts.join("  ");
        }
    }
    let mut head = format!("{}{mark}{text}", i + 1);
    let end = if !id.is_empty() && w > 4 {
        format!(" {CLOSE_MARK}")
    } else {
        String::new()
    };
    let room = w - width(&end);
    if !id.is_empty() && id == m.typing {
        color = format!("{SGR_ACCENT}{SGR_REVERSE}");
        let words = format!("  {}", typing_words(m.leave));
        head = format!(
            "{}{words}",
            textwidth::truncate(&head, room - width(&words))
        );
    }
    let mut lines = vec![format!("{color}{}{end}{SGR_RESET}", pad(&head, room))];
    if let Some(r) = m.row(id) {
        if !id.is_empty() && !r.stack.is_empty() && (lines.len() as i64) < h {
            lines.push(format!(
                "{SGR_FAINT}{}{SGR_RESET}",
                pad(&textwidth::truncate(&format!("  {}", r.stack), w), w)
            ));
        }
    }
    let headless = !id.is_empty() && m.is_headless(id) && h >= 2;
    let budget = if headless { h - 1 } else { h };
    if !id.is_empty() {
        if let Some(sc) = m.screens.get(id) {
            for l in sc.lines() {
                if lines.len() as i64 >= budget {
                    break;
                }
                lines.push(cut_sgr(&l, w));
            }
        }
    }
    while (lines.len() as i64) < budget {
        lines.push(pad("", w));
    }
    if headless {
        lines.push(headless_foot_line(m, id, w));
    }
    lines
}

/// The rail on the left and the windows on the right.
pub fn split(m: &mut Model, cols: i64, height: i64, n: i64) -> (Vec<String>, Vec<i64>, Vec<i64>) {
    let shown = m.tiles.as_ref().map(|t| t.shown(n)).unwrap_or_default();
    m.shown = shown.len() as i64;
    let n = (shown.len() as i64).max(1);
    let (rail_w, widths) = split_widths(cols, n);
    m.rail_w = rail_w;
    let mut tile_at = no_rows(cols.max(0) as usize);
    let mut x = rail_w;
    for (i, w) in widths.iter().enumerate() {
        x += 1;
        m.tile_x.push(x);
        m.tile_w.push(*w);
        let mut k = 0;
        while k < *w && x + k < cols {
            if i < shown.len() {
                tile_at[(x + k) as usize] = i as i64;
            }
            k += 1;
        }
        x += w;
    }
    if height <= 0 {
        return (Vec::new(), Vec::new(), tile_at);
    }
    let (mut left, mut at) = body(m, rail_w, height);
    while (left.len() as i64) < height {
        left.push(pad("", rail_w));
    }
    let more = (height as usize).saturating_sub(at.len());
    at.extend(no_rows(more));
    let mut ids = vec![String::new(); n as usize];
    let mut tiles: Vec<Vec<String>> = Vec::new();
    for i in 0..n as usize {
        if i < shown.len() {
            ids[i] = shown[i].clone();
        }
        let id = ids[i].clone();
        tiles.push(tile_lines(m, &id, i, widths[i], height));
        let mut inner = height - 1 - m.stack_lines(&id);
        if !id.is_empty() && m.is_headless(&id) && height >= 2 {
            inner -= 1;
        }
        if !id.is_empty() && !m.tile_sizes.contains_key(&id) && widths[i] > 0 && inner > 0 {
            m.tile_sizes.insert(
                id.clone(),
                Size {
                    cols: widths[i],
                    rows: inner,
                },
            );
        }
    }
    let mut lines = Vec::with_capacity(height as usize);
    for y in 0..height as usize {
        let mut b = left[y].clone();
        for i in 0..tiles.len() {
            let typing = !ids[i].is_empty() && ids[i] == m.typing;
            let prev_typing = i > 0 && !ids[i - 1].is_empty() && ids[i - 1] == m.typing;
            if typing || prev_typing {
                b.push_str(SGR_ACCENT);
                b.push_str(ACCENT_DIVIDER);
                b.push_str(SGR_RESET);
            } else {
                b.push_str(DIVIDER);
            }
            b.push_str(&tiles[i][y]);
        }
        lines.push(b);
    }
    (lines, at, tile_at)
}

/// Lays the floor out as exactly rows lines of exactly cols cells.
pub fn render(m: &mut Model, cols: i64, rows: i64) -> Vec<String> {
    m.row_at.clear();
    m.tile_at.clear();
    m.tile_y.clear();
    m.tile_sizes.clear();
    m.tile_x.clear();
    m.tile_w.clear();
    m.header_y = -1;
    m.ended_y = -1;
    m.shown = 0;
    m.rail_w = cols;
    m.cursor_x = 0;
    m.cursor_y = 0;
    m.cursor_hidden = false;
    m.win_rows = 0;
    if rows <= 0 || cols <= 0 {
        return Vec::new();
    }
    let mut head = vec![pad(&bar(m), cols)];
    if m.facts.is_some() && cols >= FACTS_MIN_COLS {
        {
            head.push(format!(
                "{SGR_FAINT}{}{SGR_RESET}",
                pad(&textwidth::truncate(&facts_line(m), cols), cols)
            ));
        }
    }
    if gate_hint_shown(&m.facts) && cols >= width(GATE_HINT) {
        head.push(pad(GATE_HINT, cols));
    }
    let mut tail: Vec<String> = Vec::new();
    for n in &m.notes {
        let n = textwidth::printable(n, 0);
        tail.push(format!(
            "{SGR_FAINT}{}{SGR_RESET}",
            pad(&textwidth::truncate(&format!("  {n}"), cols), cols)
        ));
    }
    if !m.paused.is_empty() {
        tail.push(format!(
            "{SGR_AMBER}{}{SGR_RESET}",
            pad(&textwidth::printable(&m.paused, 0), cols)
        ));
    }
    for l in wrap_words(&textwidth::printable(&m.voice, 0), cols, 3) {
        tail.push(format!(
            "{SGR_AMBER}{}{SGR_RESET}",
            pad(&textwidth::truncate(&l, cols), cols)
        ));
    }
    let mut ended_at: i64 = -1;
    if !m.ended_line.is_empty() {
        let color = if m.ended_sticky { SGR_AMBER } else { "" };
        ended_at = tail.len() as i64;
        tail.push(format!(
            "{color}{}{SGR_RESET}",
            pad(&textwidth::printable(&m.ended_line, 0), cols)
        ));
    }
    if !m.message.is_empty() {
        tail.push(pad(&textwidth::printable(&m.message, 0), cols));
    }
    tail.extend(footer_lines(m, cols));
    let prompt = format!("{PROMPT_MARK}{}", m.prompt_line());
    if !m.confirm.is_empty() || m.clear_all {
        tail.push(format!("{SGR_AMBER}{}{SGR_RESET}", pad(&prompt, cols)));
    } else {
        tail.push(pad(&prompt, cols));
    }
    let body_h = rows - head.len() as i64 - tail.len() as i64;
    let mut tiled = false;
    let (mid, at, tile_at) = {
        let n = window_count(cols);
        if n > 0 && m.tiles.is_some() {
            tiled = true;
            split(m, cols, body_h, n)
        } else {
            let (mid, at) = body(m, cols, body_h);
            (mid, at, no_rows(cols as usize))
        }
    };
    let mut lines: Vec<String> = Vec::new();
    lines.extend(head.iter().cloned());
    lines.extend(mid);
    let mut at_all = no_rows(head.len());
    at_all.extend(at);
    let mut on_tiles = vec![false; lines.len()];
    for v in on_tiles.iter_mut().skip(head.len()) {
        *v = tiled;
    }
    while (lines.len() as i64) < rows - tail.len() as i64 {
        lines.push(pad("", cols));
    }
    let tail_top = lines.len() as i64;
    lines.extend(tail);
    let more = lines.len().saturating_sub(at_all.len());
    at_all.extend(no_rows(more));
    let more = lines.len().saturating_sub(on_tiles.len());
    on_tiles.extend(vec![false; more]);
    let mut drop: i64 = 0;
    if lines.len() as i64 > rows {
        drop = lines.len() as i64 - rows;
        let d = drop as usize;
        lines.drain(..d);
        at_all.drain(..d.min(at_all.len()));
        on_tiles.drain(..d.min(on_tiles.len()));
    }
    m.row_at = at_all;
    m.tile_at = tile_at;
    m.tile_y = on_tiles;
    let body_top = head.len() as i64 - drop;
    if tiled && body_h > 0 {
        m.header_y = body_top;
        m.win_rows = body_h - 1;
    }
    if ended_at >= 0 {
        m.ended_y = tail_top + ended_at - drop;
    }
    place_cursor(m, cols, rows, body_top, body_h, &prompt);
    lines
}

fn place_cursor(m: &mut Model, cols: i64, rows: i64, body_top: i64, body_h: i64, prompt: &str) {
    let (mut x, mut y) = (0, body_top);
    let slot_of_typing = |m: &Model| -> i64 {
        let mut slot = -1;
        if let Some(ts) = &m.tiles {
            for (i, id) in ts.shown(m.tile_x.len() as i64).iter().enumerate() {
                if *id == m.typing {
                    slot = i as i64;
                }
            }
        }
        slot
    };
    if m.talking || !m.renaming.is_empty() || !m.confirm.is_empty() || m.clear_all {
        x = width(prompt);
        y = rows - 1;
    } else if !m.typing.is_empty() && m.is_headless(&m.typing.clone()) && !m.tile_x.is_empty() {
        let slot = slot_of_typing(m);
        if slot >= 0 && body_h >= 2 {
            let (line, cur) = m.headless_buf(&m.typing.clone());
            let w = m.tile_w[slot as usize];
            let mut cx = if line.is_empty() {
                width(HEADLESS_MARK)
            } else {
                headless_visible(&line, cur, w).1
            };
            cx = cx.max(0).min(w - 1);
            x = m.tile_x[slot as usize] + cx;
            y = body_top + body_h - 1;
        }
    } else if !m.typing.is_empty() && !m.tile_x.is_empty() {
        let slot = slot_of_typing(m);
        if slot >= 0 {
            let (mut cx, mut cy) = (0, 0);
            if let Some(sc) = m.screens.get(&m.typing) {
                let c = sc.cursor();
                cx = c[0];
                cy = c[1];
            }
            let top = 1 + m.stack_lines(&m.typing.clone());
            let w = m.tile_w[slot as usize];
            if cx < 0 || cy < 0 || cx >= w || cy >= body_h - top {
                m.cursor_hidden = true;
            }
            cx = cx.max(0).min(w - 1);
            cy = cy.max(0).min(body_h - top - 1);
            x = m.tile_x[slot as usize] + cx;
            y = body_top + top + cy;
        }
    } else if m.body_cur >= 0 {
        x = 2;
        y = body_top + m.body_cur;
    }
    m.cursor_x = x.max(0).min(cols - 1);
    m.cursor_y = y.max(0).min(rows - 1);
}

/// The peek in height lines.
pub fn peek_lines(p: &PeekView, cols: i64, height: i64) -> Vec<String> {
    if height <= 0 {
        return Vec::new();
    }
    let mut head = vec![pad(
        format!("peek {} {}", p.pane, textwidth::printable(&p.label, 0)).trim_end_matches(' '),
        cols,
    )];
    if !p.ask.is_empty() {
        head.push(pad(&format!("asks: {}", p.ask), cols));
    }
    if p.has_gate {
        head.push(pad(&verdict_text(&p.verdict, p.rule), cols));
    }
    for line in trust_lines(p) {
        head.push(pad(&line, cols));
    }
    for line in tier_lines(p) {
        head.push(pad(&line, cols));
    }
    let head = fit(head, height);
    let room = (height - head.len() as i64).max(0) as usize;
    let trimmed = p.text.trim_end_matches('\n');
    let mut text: Vec<&str> = trimmed.split('\n').collect();
    if text.len() > room {
        text = text[text.len() - room..].to_vec();
    }
    let mut lines = head;
    for t in text {
        lines.push(pad(t, cols));
    }
    while (lines.len() as i64) < height {
        lines.push(pad("", cols));
    }
    lines
}

/// The lines that say how to answer the peek's ask.
pub fn tier_lines(p: &PeekView) -> Vec<String> {
    if p.ask_id.is_empty() {
        return Vec::new();
    }
    if !p.permanent() {
        return vec![
            "Deny is the default.".into(),
            "y allow once  t allow for this task  n deny".into(),
        ];
    }
    let name = textwidth::printable(&p.name(), 0);
    vec![
        "Deny is the default.".into(),
        format!("n deny   to allow, press ctrl-t, type {name} and enter"),
    ]
}

/// The lines a peek on Claude's folder trust screen shows.
pub fn trust_lines(p: &PeekView) -> Vec<String> {
    if !p.trust {
        return Vec::new();
    }
    vec![
        "Claude asks to trust this folder.".into(),
        TRUST_KEYS.into(),
    ]
}

/// The peek's line of keys for the trust screen.
pub const TRUST_KEYS: &str = "y trust this folder  n not now, which ends Claude";

/// The one line that says what the gate decided.
pub fn verdict_text(verdict: &str, rule: i64) -> String {
    if verdict == "allow" {
        return "The gate says yes".into();
    }
    format!("The gate says no, rule {rule}")
}
