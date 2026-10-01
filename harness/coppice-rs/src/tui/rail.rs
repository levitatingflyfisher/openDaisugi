//! What each key and click does on the rail, the prompt line, the picker
//! and the tree.

use std::time::Instant;

use serde_json::{json, Value};

use crate::textwidth;
use crate::tiles::Tiles;

use super::keys::{bind, hit_row, hit_tile, on_tiles, Action, Click, Key};
use super::model::{
    answerable, boolean, clamp, last_rune_len, obj, s, Item, ItemKind, Picker, Project, Row,
    CHILD_MESSAGE,
};
use super::prompt::{
    classify_with, parse_layout, task_promotion_line, tiles_message, write_floor_state, Kind,
    FLOOR_FILE, FOREMAN_LABEL, HARNESSES, LAYOUT_NEEDS_MESSAGE, LOCKED_MESSAGE, NO_FOREMAN_ANSWER,
    SWAP_NEEDS_MESSAGE, VERBS,
};
use super::render::window_count;
use super::run::{
    call, call_within, list_of, params, Async, Floor, FloorErr, CALL_DEADLINE, DOUBLE_CLICK,
};

/// What the floor says when a key that means nothing on the rail is
/// pressed.
pub const RAIL_HINT: &str = "That key does nothing on the rail. ctrl-t types a line.";

/// Applies one key to a line being typed. It reports whether it used the
/// key.
pub fn edit_line(line: &mut Vec<u8>, k: &Key) -> bool {
    let Key::Byte(b) = k else { return false };
    let b = *b;
    if b == 0x7f || b == 0x08 {
        let n = last_rune_len(line);
        line.truncate(line.len() - n);
        return true;
    }
    if b >= 0x20 {
        line.push(b);
        return true;
    }
    false
}

/// The counts of one verb sent for many agents.
#[derive(Default)]
struct Batch {
    done: i64,
    failed: i64,
    first: Option<String>,
}

impl Batch {
    fn add(&mut self, r: Result<(), FloorErr>) -> Result<(), FloorErr> {
        match r {
            Ok(()) => {
                self.done += 1;
                Ok(())
            }
            Err(e @ FloorErr::Gone(_)) => Err(e),
            Err(FloorErr::Other(m)) => {
                self.failed += 1;
                if self.first.is_none() {
                    self.first = Some(m);
                }
                Ok(())
            }
        }
    }

    fn say(&self, format: &str) -> String {
        let mut msg = format.replace("%d", &self.done.to_string());
        if self.failed > 0 {
            msg.push_str(&format!(
                " {} failed: {}",
                self.failed,
                self.first.clone().unwrap_or_default()
            ));
        }
        msg
    }
}

/// How long promote waits for the new foreman pane to go idle.
pub const PROMOTE_WAIT: std::time::Duration = std::time::Duration::from_secs(30);

/// Waits for pane id's first idle state after it drew something, then
/// sends it line with Enter.
pub fn promote(
    socket: std::path::PathBuf,
    id: String,
    label: String,
    line: String,
    finished: String,
    wait: std::time::Duration,
) -> String {
    let secs = wait.as_secs();
    let end = Instant::now() + wait;
    let mut err = false;
    for until in ["working", "idle"] {
        let left = end.saturating_duration_since(Instant::now());
        if left.is_zero() {
            err = true;
            break;
        }
        if call_within(
            &socket,
            "agent.wait",
            params(vec![
                ("pane", json!(id)),
                ("until", json!(until)),
                ("timeout_ms", json!(left.as_millis() as i64)),
            ]),
            left + CALL_DEADLINE,
        )
        .is_err()
        {
            err = true;
            break;
        }
    }
    if err {
        return format!(
            "{label} did not go idle in {secs} s, so nothing was sent. Answer it in its tile, then type there: {line}"
        );
    }
    if let Err(e) = call(
        &socket,
        "pane.send_text",
        params(vec![
            ("pane", json!(id)),
            ("text", json!(line)),
            ("enter", json!(true)),
        ]),
    ) {
        return format!("cannot send the page to {label}: {}", e.text());
    }
    finished
}

/// What the floor says once pane label has the page.
pub fn promoted_message(label: &str, task: &str) -> String {
    if task.is_empty() {
        return format!("{label} has its page. Type a sentence and it goes there.");
    }
    format!("{label} is the foreman of {task}. The task's undoable asks go to it first.")
}

/// The one line the floor shows for a floor.talk reply.
pub fn talk_message(res: &super::model::Obj) -> String {
    let mut label = s(res, "label");
    if label.is_empty() {
        label = "the foreman".into();
    }
    let note = s(res, "note");
    if !note.is_empty() {
        return note;
    }
    if boolean(res, "started") == Some(true) {
        return format!("{label} starts and gets its page. Your words go once it is ready.");
    }
    if let Some(n) = super::model::num(res, "queued") {
        if n > 1.0 {
            return format!(
                "Sent to {label}. {} sentences wait for it, yours last.",
                n as i64
            );
        }
    }
    format!("Sent to {label}.")
}

impl Floor<'_> {
    /// Applies one keystroke while no window has the keys. Ok(true) quits.
    pub fn handle(&mut self, k: Key) -> Result<bool, FloorErr> {
        if k == Key::Byte(0x03) {
            return Ok(true);
        }
        if !self.m.confirm.is_empty() || self.m.clear_all {
            // A terminal sends a release after the press that asked, and
            // motion is not an answer either. Only a key or a new press is.
            if let Key::Mouse(c) = &k {
                if c.release || c.button & 32 != 0 {
                    return Ok(false);
                }
            }
            self.answer_confirm(&k)?;
            return Ok(false);
        }
        if self.m.picker.is_some() {
            self.picker_key(&k)?;
            return Ok(false);
        }
        if !self.m.renaming.is_empty() {
            self.rename_key(&k)?;
            return Ok(false);
        }
        if self.m.talking {
            self.talk_key(&k)?;
            return Ok(false);
        }
        if self.m.tree.is_some() {
            self.tree_key(&k);
            return Ok(false);
        }
        if let Key::Byte(b) = k {
            if self.m.peek.as_ref().is_some_and(|p| p.trust) && (b == b'y' || b == b'n') {
                self.answer_trust(b == b'y')?;
                return Ok(false);
            }
            if answerable(&self.m.peek) && matches!(b, b'y' | b't' | b'n') {
                self.answer_peek(b)?;
                return Ok(false);
            }
        }
        match bind(&k) {
            Action::Talk => {
                self.m.talking = true;
                return Ok(false);
            }
            Action::NextNeed => {
                self.m.next_need();
                return Ok(false);
            }
            Action::Stop => {
                self.stop_key()?;
                return Ok(false);
            }
            Action::Peek => {
                self.space()?;
                return Ok(false);
            }
            Action::GoIn => {
                self.enter()?;
                return Ok(false);
            }
            Action::Up => {
                self.up();
                return Ok(false);
            }
            Action::New => {
                self.new_near()?;
                return Ok(false);
            }
            Action::NewIn => {
                self.open_picker()?;
                return Ok(false);
            }
            Action::Rename => {
                self.start_rename();
                return Ok(false);
            }
            Action::Window => {
                if let Key::Byte(b) = k {
                    self.put_in_window((b - b'0') as i64)?;
                }
                return Ok(false);
            }
            Action::None => {}
        }
        match k {
            Key::Up => self.m.move_by(-1),
            Key::Down => self.m.move_by(1),
            Key::Mouse(c) => self.click(c)?,
            Key::Byte(b) if b >= 0x20 && b != 0x7f => self.m.message = RAIL_HINT.into(),
            _ => {}
        }
        Ok(false)
    }

    fn talk_key(&mut self, k: &Key) -> Result<(), FloorErr> {
        match k {
            Key::Byte(b'\r') | Key::Byte(b'\n') => return self.enter(),
            Key::Esc => {
                self.up();
                return Ok(());
            }
            _ => {}
        }
        if edit_line(&mut self.m.prompt, k) {
            return Ok(());
        }
        match k {
            Key::Up => self.m.move_by(-1),
            Key::Down => self.m.move_by(1),
            Key::Mouse(c) => return self.click(*c),
            _ => {}
        }
        Ok(())
    }

    fn start_rename(&mut self) {
        let Some(r) = self.m.selected() else { return };
        if !r.parent.is_empty() {
            self.m.message = CHILD_MESSAGE.into();
            return;
        }
        self.m.renaming = r.id.clone();
        self.m.prompt = textwidth::printable(&r.label, 0).into_bytes();
    }

    fn rename_key(&mut self, k: &Key) -> Result<(), FloorErr> {
        match k {
            Key::Esc => {
                self.m.renaming.clear();
                self.m.prompt.clear();
            }
            Key::Byte(b'\r') | Key::Byte(b'\n') => {
                let id = std::mem::take(&mut self.m.renaming);
                let label = self.m.prompt_text().trim().to_string();
                self.m.prompt.clear();
                if label.is_empty() {
                    self.m.message = "A label cannot be empty. The name stays.".into();
                    return Ok(());
                }
                if let Err(e) = call(
                    &self.o.socket,
                    "pane.rename",
                    params(vec![("pane", json!(id)), ("label", json!(label))]),
                ) {
                    return self.fail(e);
                }
                return self.refresh();
            }
            _ => {
                edit_line(&mut self.m.prompt, k);
            }
        }
        Ok(())
    }

    fn stop_key(&mut self) -> Result<(), FloorErr> {
        let Some(it) = self.m.selected_item() else {
            return Ok(());
        };
        match it.kind {
            ItemKind::Row => {
                if !it.row.parent.is_empty() {
                    self.m.message = CHILD_MESSAGE.into();
                    return Ok(());
                }
                self.m.confirm = it.row.id;
            }
            ItemKind::Ended => return self.forget(&it.row.id),
            ItemKind::ClearAll => self.m.clear_all = true,
            _ => {}
        }
        Ok(())
    }

    fn answer_confirm(&mut self, k: &Key) -> Result<(), FloorErr> {
        let id = std::mem::take(&mut self.m.confirm);
        let all = self.m.clear_all;
        self.m.clear_all = false;
        if !matches!(k, Key::Byte(b'\r') | Key::Byte(b'\n')) {
            return Ok(());
        }
        if all {
            return self.clear_all();
        }
        if let Err(e) = call(
            &self.o.socket,
            "pane.close",
            params(vec![("pane", json!(id))]),
        ) {
            return self.fail(e);
        }
        self.m.message = format!("{} stopped.", self.m.label_of(&id));
        self.refresh()
    }

    fn forget(&mut self, id: &str) -> Result<(), FloorErr> {
        self.ended_due = true;
        if let Err(e) = call(
            &self.o.socket,
            "pane.forget",
            params(vec![("pane", json!(id))]),
        ) {
            return self.fail(e);
        }
        self.refresh()
    }

    fn clear_all(&mut self) -> Result<(), FloorErr> {
        self.ended_due = true;
        let mut tally = Batch::default();
        for r in self.m.recent() {
            let res = call(
                &self.o.socket,
                "pane.forget",
                params(vec![("pane", json!(r.id))]),
            )
            .map(|_| ());
            tally.add(res)?;
        }
        self.m.message = tally.say("Forgot %d ended agents.");
        self.refresh()
    }

    fn resume(&mut self, r: &Row) -> Result<(String, bool), FloorErr> {
        let (cols, rows) = self.new_size();
        self.ended_due = true;
        let res = call(
            &self.o.socket,
            "pane.resume",
            params(vec![
                ("pane", json!(r.id)),
                ("cols", json!(cols)),
                ("rows", json!(rows)),
            ]),
        )?;
        Ok((s(&res, "pane"), boolean(&res, "resumed") == Some(true)))
    }

    fn enter_recent(&mut self, it: Item) -> Result<(), FloorErr> {
        match it.kind {
            ItemKind::Group => {
                self.m.toggle_group(&it.group.key);
                return Ok(());
            }
            ItemKind::Fold => {
                self.m.toggle_recent();
                return Ok(());
            }
            ItemKind::ClearAll => {
                self.m.clear_all = true;
                return Ok(());
            }
            ItemKind::ResumeAll => {
                let mut tally = Batch::default();
                for r in self.m.recent() {
                    let res = self.resume(&r).map(|_| ());
                    tally.add(res)?;
                }
                self.m.message = tally.say("Started %d agents again.");
                return self.refresh();
            }
            _ => {}
        }
        let (id, resumed) = match self.resume(&it.row) {
            Ok(x) => x,
            Err(e) => return self.fail(e),
        };
        let label = self.m.label_of_ended(&it.row);
        if resumed {
            self.m.message = format!("{label} resumed its session.");
        } else {
            self.m.message = format!("{label} started fresh. It had no session to resume.");
        }
        self.refresh()?;
        self.show_new(&id);
        Ok(())
    }

    /// Enter: runs the prompt line, acts on a Recent item, or goes in.
    pub fn enter(&mut self) -> Result<(), FloorErr> {
        self.m.message.clear();
        if self.m.talking {
            self.m.talking = false;
            let line = self.m.prompt_text();
            self.m.prompt.clear();
            if line.is_empty() {
                return Ok(());
            }
            if let Some(p) = self.m.peek.clone() {
                if !p.ask_id.is_empty() && p.permanent() {
                    if line.trim() != p.name() {
                        self.m.message = format!(
                            "That is not the pane name. Type {} and enter to allow, or n to deny.",
                            p.name()
                        );
                        return Ok(());
                    }
                    let mut extra = serde_json::Map::new();
                    extra.insert("confirm".into(), json!(p.name()));
                    return self.send_answer("agent.allow", Some(extra), "Allowed.");
                }
            }
            return self.run_prompt(&line);
        }
        if let Some(it) = self.m.selected_item() {
            if it.kind != ItemKind::Row {
                return self.enter_recent(it);
            }
            if !it.row.parent.is_empty() {
                self.m.message = CHILD_MESSAGE.into();
                return Ok(());
            }
            if self.type_in_tile(&it.row.id) {
                return Ok(());
            }
            return self.attach_to(&it.row.id);
        }
        let def = self.o.default.clone();
        let id = match self.create_harness_pane(&def, &[]) {
            Ok(id) => id,
            Err(e) => return self.fail(e),
        };
        self.attach_to(&id)
    }

    fn new_near(&mut self) -> Result<(), FloorErr> {
        let (cols, rows) = self.new_size();
        let mut p = serde_json::Map::new();
        p.insert("kind".into(), json!("pty"));
        p.insert("cols".into(), json!(cols));
        p.insert("rows".into(), json!(rows));
        if let Some(r) = self.m.selected() {
            let near = if r.parent.is_empty() { r.id } else { r.parent };
            p.insert("near".into(), json!(near));
        }
        let res = match call(&self.o.socket, "pane.create", Some(p)) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        let id = s(&res, "pane");
        self.refresh()?;
        self.show_new(&id);
        Ok(())
    }

    fn open_picker(&mut self) -> Result<(), FloorErr> {
        let res = match call(&self.o.socket, "project.list", None) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        let mut p = Picker::default();
        for pm in list_of(&res, "projects") {
            if s(&pm, "path").is_empty() {
                continue;
            }
            p.projects.push(Project {
                path: s(&pm, "path"),
                name: s(&pm, "name"),
                pinned: boolean(&pm, "pinned") == Some(true),
            });
        }
        self.m.picker = Some(p);
        Ok(())
    }

    fn picker_key(&mut self, k: &Key) -> Result<(), FloorErr> {
        let n = self
            .m
            .picker
            .as_ref()
            .map(|p| p.projects.len())
            .unwrap_or(0) as i64;
        match k {
            Key::Esc => self.m.picker = None,
            Key::Up => {
                if let Some(p) = &mut self.m.picker {
                    p.cursor = clamp(p.cursor - 1, n);
                }
            }
            Key::Down => {
                if let Some(p) = &mut self.m.picker {
                    p.cursor = clamp(p.cursor + 1, n);
                }
            }
            Key::Byte(b'\r') | Key::Byte(b'\n') => {
                if n == 0 {
                    self.m.picker = None;
                    return Ok(());
                }
                let pr = self
                    .m
                    .picker
                    .as_ref()
                    .map(|p| p.projects[p.cursor as usize].clone());
                if let Some(pr) = pr {
                    return self.start_in(pr);
                }
            }
            Key::Byte(b) if (b'1'..=b'9').contains(b) => {
                let i = (b - b'1') as i64;
                if i >= n {
                    self.m.message = format!("There is no project {}.", i + 1);
                    return Ok(());
                }
                let pr = self
                    .m
                    .picker
                    .as_ref()
                    .map(|p| p.projects[i as usize].clone());
                if let Some(pr) = pr {
                    return self.start_in(pr);
                }
            }
            Key::Mouse(c) => return self.click(*c),
            _ => {}
        }
        Ok(())
    }

    fn start_in(&mut self, pr: Project) -> Result<(), FloorErr> {
        self.m.picker = None;
        let mut extra = serde_json::Map::new();
        extra.insert("harness".into(), json!(self.o.default));
        extra.insert("cwd".into(), json!(pr.path));
        let id = match self.create(extra) {
            Ok(id) => id,
            Err(e) => return self.fail(e),
        };
        self.refresh()?;
        self.show_new(&id);
        Ok(())
    }

    /// Puts a new agent where the owner sees it.
    fn show_new(&mut self, id: &str) {
        if id.is_empty() {
            return;
        }
        self.m.cursor = self.m.index_of(id, self.m.cursor);
        if self.type_in_tile(id) {
            return;
        }
        if self.m.message.is_empty() {
            self.m.message = format!("{} started. Enter goes in.", self.m.label_of(id));
        }
    }

    /// Applies one mouse report.
    pub fn click(&mut self, c: Click) -> Result<(), FloorErr> {
        if c.release {
            self.release(c);
            return Ok(());
        }
        if c.button >= 64 {
            return Ok(());
        }
        self.drag_from.clear();
        let cols = self.cols();
        if c.button == 0 && self.m.ended_y >= 0 && c.y - 1 == self.m.ended_y {
            self.set_typing("");
            self.m.clear_ended();
            if !self.m.recent_open {
                self.m.toggle_recent();
            }
            return Ok(());
        }
        if c.button == 0 && self.m.tiles.is_some() && on_tiles(&self.m, c.y) {
            if let Some(slot) = hit_tile(&self.m, c.x) {
                let shown = self
                    .m
                    .tiles
                    .as_ref()
                    .map(|t| t.shown(window_count(cols)))
                    .unwrap_or_default();
                let id = shown.get(slot as usize).cloned().unwrap_or_default();
                if !id.is_empty()
                    && c.y - 1 == self.m.header_y
                    && (slot as usize) < self.m.tile_x.len()
                    && c.x > self.m.tile_x[slot as usize] + self.m.tile_w[slot as usize] - 2
                {
                    self.set_typing("");
                    self.m.confirm = id;
                    return Ok(());
                }
                if let Some(ts) = &mut self.m.tiles {
                    ts.focus = slot;
                }
                let now = Instant::now();
                let double = !id.is_empty()
                    && id == self.last_pane
                    && slot == self.last_slot
                    && matches!(self.last_press, Some(t) if now.saturating_duration_since(t) < DOUBLE_CLICK);
                self.last_press = Some(now);
                self.last_slot = slot;
                self.last_pane = id.clone();
                if double {
                    self.last_press = None;
                    return self.attach_to(&id);
                }
                self.set_typing(&id);
                return Ok(());
            }
        }
        self.last_press = None;
        if c.button == 0 {
            self.set_typing("");
        }
        let Some(i) = hit_row(&self.m, c.y) else {
            return Ok(());
        };
        self.m.cursor = i;
        self.m.landed();
        let Some(it) = self.m.selected_item() else {
            return Ok(());
        };
        let on_close = c.x > self.m.rail_w - 2;
        if it.kind != ItemKind::Row {
            if c.button != 0 {
                return Ok(());
            }
            if it.kind == ItemKind::Ended && on_close {
                return self.forget(&it.row.id);
            }
            if it.kind != ItemKind::Ended {
                return self.enter_recent(it);
            }
            return Ok(());
        }
        let r = it.row;
        if !r.parent.is_empty() {
            self.m.message = CHILD_MESSAGE.into();
            return Ok(());
        }
        if c.button == 0 && on_close {
            self.m.confirm = r.id;
            return Ok(());
        }
        let Some(ts) = &mut self.m.tiles else {
            return Ok(());
        };
        match c.button {
            0 => self.drag_from = r.id,
            1 => {
                ts.open(&r.id);
            }
            _ => {}
        }
        Ok(())
    }

    fn release(&mut self, c: Click) {
        let from = std::mem::take(&mut self.drag_from);
        if from.is_empty() || c.button != 0 || self.m.tiles.is_none() || !self.m.has(&from) {
            return;
        }
        if on_tiles(&self.m, c.y) {
            if let Some(slot) = hit_tile(&self.m, c.x) {
                self.put_in_slot(&from, slot);
                return;
            }
        }
        if let Some(i) = hit_row(&self.m, c.y) {
            let all = self.m.items();
            if (i as usize) < all.len()
                && all[i as usize].kind == ItemKind::Row
                && all[i as usize].row.id == from
            {
                self.type_in_tile(&from);
            }
        }
    }

    fn put_in_slot(&mut self, id: &str, i: i64) {
        let Some(ts) = &mut self.m.tiles else { return };
        put_in_slot(ts, id, i);
    }

    fn put_in_window(&mut self, n: i64) -> Result<(), FloorErr> {
        let Some(r) = self.m.selected() else {
            return Ok(());
        };
        if !r.parent.is_empty() {
            self.m.message = CHILD_MESSAGE.into();
            return Ok(());
        }
        let shown = window_count(self.cols());
        if shown == 0 || self.m.tiles.is_none() {
            self.m.message = "This screen is too narrow for windows. Enter goes in.".into();
            return Ok(());
        }
        if n > shown {
            self.m.message = format!("This screen shows {shown} windows. Press 1 to {shown}.");
            return Ok(());
        }
        self.grow_slots(n);
        self.put_in_slot(&r.id, n - 1);
        Ok(())
    }

    /// y, t or n on a peek that shows an ask.
    fn answer_peek(&mut self, b: u8) -> Result<(), FloorErr> {
        let Some(p) = self.m.peek.clone() else {
            return Ok(());
        };
        if b == b'n' {
            return self.send_answer("agent.deny", None, "Denied.");
        }
        if p.permanent() {
            self.m.message = "type the pane name to allow".into();
            return Ok(());
        }
        if b == b'y' {
            return self.send_answer("agent.allow", None, "Allowed once.");
        }
        let mut extra = serde_json::Map::new();
        extra.insert("scope".into(), json!("task"));
        self.send_answer(
            "agent.allow",
            Some(extra),
            "Allowed for this task. The gate records it as a proposal.",
        )
    }

    fn answer_trust(&mut self, yes: bool) -> Result<(), FloorErr> {
        let pane = self
            .m
            .peek
            .as_ref()
            .map(|p| p.pane.clone())
            .unwrap_or_default();
        if let Err(e) = call(
            &self.o.socket,
            "pane.trust",
            params(vec![("pane", json!(pane)), ("trust", json!(yes))]),
        ) {
            return self.fail(e);
        }
        self.m.close_peek();
        self.m.message = if yes {
            "Trusted the folder. Claude starts.".into()
        } else {
            "Not now. Claude ends.".into()
        };
        Ok(())
    }

    fn send_answer(
        &mut self,
        verb: &str,
        extra: Option<serde_json::Map<String, Value>>,
        done: &str,
    ) -> Result<(), FloorErr> {
        let Some(p) = self.m.peek.clone() else {
            return Ok(());
        };
        let mut prm = serde_json::Map::new();
        prm.insert("pane".into(), json!(p.pane));
        prm.insert("ask".into(), json!(p.ask_id));
        for (k, v) in extra.unwrap_or_default() {
            prm.insert(k, v);
        }
        if let Err(e) = call(&self.o.socket, verb, Some(prm)) {
            return self.fail(e);
        }
        self.m.close_peek();
        self.m.message = done.into();
        Ok(())
    }

    fn tree_key(&mut self, k: &Key) {
        let Some(t) = &mut self.m.tree else { return };
        match k {
            Key::Up => t.move_by(-1),
            Key::Down => t.move_by(1),
            Key::Esc => self.m.tree = None,
            Key::Byte(b'\r') | Key::Byte(b'\n') => {
                let id = t.selected();
                if !id.is_empty() {
                    self.m.push(&id);
                }
                self.m.tree = None;
            }
            _ => {}
        }
    }

    /// Esc: goes up one level.
    pub fn up(&mut self) {
        let m = &mut self.m;
        if m.tree.is_some() {
            m.tree = None;
        } else if m.peek.is_some() {
            m.close_peek();
        } else if m.talking || !m.prompt.is_empty() {
            m.talking = false;
            m.prompt.clear();
        } else if !m.path.is_empty() {
            m.pop();
        }
    }

    /// Fills pane id into the tiles and gives that tile the keyboard.
    pub fn type_in_tile(&mut self, id: &str) -> bool {
        let n = window_count(self.cols());
        let Some(ts) = &mut self.m.tiles else {
            return false;
        };
        if n == 0 {
            return false;
        }
        ts.fill(id);
        if ts.shown(n).iter().any(|x| x == id) {
            self.set_typing(id);
            return true;
        }
        false
    }

    /// Space: fills the row's pane into the tiles, or opens the peek.
    fn space(&mut self) -> Result<(), FloorErr> {
        if let Some(it) = self.m.selected_item() {
            if it.kind == ItemKind::Group {
                self.m.toggle_group(&it.group.key);
                return Ok(());
            }
        }
        if let Some(r) = self.m.selected() {
            if !r.parent.is_empty() {
                self.m.message = CHILD_MESSAGE.into();
                return Ok(());
            }
        }
        if window_count(self.cols()) == 0 || self.m.tiles.is_none() {
            return self.peek();
        }
        if let Some(r) = self.m.selected() {
            if let Some(ts) = &mut self.m.tiles {
                ts.fill(&r.id);
            }
        }
        Ok(())
    }

    fn create_pane(&mut self, argv: &[String]) -> Result<String, FloorErr> {
        let mut extra = serde_json::Map::new();
        extra.insert("cmd_argv".into(), json!(argv));
        self.create(extra)
    }

    fn create_harness_pane(&mut self, name: &str, args: &[String]) -> Result<String, FloorErr> {
        let mut extra = serde_json::Map::new();
        extra.insert("harness".into(), json!(name));
        if !args.is_empty() {
            extra.insert("args".into(), json!(args));
        }
        self.create(extra)
    }

    /// The size a new agent starts at.
    pub fn new_size(&mut self) -> (i64, i64) {
        let m = &self.m;
        if let Some(ts) = &m.tiles {
            if !m.tile_w.is_empty() && m.win_rows > 0 {
                let mut slot: i64 = -1;
                let mut i = 0;
                while i < m.tile_w.len() && i < ts.slots.len() {
                    if ts.slots[i].is_empty() {
                        slot = i as i64;
                        break;
                    }
                    i += 1;
                }
                if slot < 0 {
                    slot = ts.focus.max(0).min(m.tile_w.len() as i64 - 1);
                }
                return (m.tile_w[slot as usize], m.win_rows);
            }
        }
        let (cols, mut rows) = (self.o.size)();
        if rows > 1 {
            rows -= 1;
        }
        (cols, rows)
    }

    /// Sends pane.create for a pty pane in Cwd and returns its id.
    fn create(&mut self, extra: serde_json::Map<String, Value>) -> Result<String, FloorErr> {
        let (cols, rows) = self.new_size();
        let mut p = serde_json::Map::new();
        p.insert("cwd".into(), json!(self.o.cwd));
        p.insert("kind".into(), json!("pty"));
        p.insert("cols".into(), json!(cols));
        p.insert("rows".into(), json!(rows));
        for (k, v) in extra {
            p.insert(k, v);
        }
        let res = call(&self.o.socket, "pane.create", Some(p))?;
        let id = s(&res, "pane");
        if id.is_empty() {
            return Err(FloorErr::Other(
                "the server created no pane. Run: coppice pane list".into(),
            ));
        }
        Ok(id)
    }

    /// Sends one sentence to the floor's foreman.
    fn talk(&mut self, line: &str) -> Result<(), FloorErr> {
        let res = match call(
            &self.o.socket,
            "floor.talk",
            params(vec![("text", json!(line))]),
        ) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        let id = s(&res, "pane");
        self.m.message = talk_message(&res);
        self.refresh()?;
        self.fill_shown(&id);
        Ok(())
    }

    fn fill_shown(&mut self, id: &str) {
        let n = window_count(self.cols());
        if id.is_empty() || n == 0 {
            return;
        }
        if let Some(ts) = &mut self.m.tiles {
            if ts.slot_of(id).is_none() {
                ts.fill(id);
            }
        }
    }

    /// Runs one prompt line.
    fn run_prompt(&mut self, line: &str) -> Result<(), FloorErr> {
        if classify_with(line, HARNESSES, &self.o.views) == Kind::TalkLine {
            return self.talk(line);
        }
        let words: Vec<String> = line.split_whitespace().map(String::from).collect();
        if self.view_word(&words) {
            return Ok(());
        }
        match words[0].as_str() {
            "list" => return self.refresh(),
            "read" => {
                if words.len() < 2 {
                    self.m.message = "read needs a pane id. Run: list".into();
                    return Ok(());
                }
                if !self.m.has(&words[1]) {
                    self.m.message = format!("no pane {}. Run: list", words[1]);
                    return Ok(());
                }
                self.m.cursor = self.m.index_of(&words[1], self.m.cursor);
                return self.peek();
            }
            "close" => {
                if words.len() < 2 {
                    self.m.message = "close needs a pane id. Run: list".into();
                    return Ok(());
                }
                if let Err(e) = call(
                    &self.o.socket,
                    "pane.close",
                    params(vec![("pane", json!(words[1]))]),
                ) {
                    return self.fail(e);
                }
                return self.refresh();
            }
            "open" => {
                if words.len() < 2 {
                    self.m.message =
                        format!("open needs a harness name, one of {}", HARNESSES.join(" "));
                    return Ok(());
                }
                if let Err(e) = self.create_harness_pane(&words[1], &words[2..]) {
                    return self.fail(e);
                }
                return self.refresh();
            }
            "swap" => {
                if words.len() != 3 {
                    self.m.message = SWAP_NEEDS_MESSAGE.into();
                    return Ok(());
                }
                let (Some(i), Some(j)) = (
                    super::keys::go_atoi(words[1].as_bytes()),
                    super::keys::go_atoi(words[2].as_bytes()),
                ) else {
                    self.m.message = SWAP_NEEDS_MESSAGE.into();
                    return Ok(());
                };
                let n = self.m.tiles.as_ref().map(|t| t.slots.len()).unwrap_or(0) as i64;
                if i < 1 || i > n || j < 1 || j > n {
                    self.m.message = format!("swap needs slot numbers from 1 to {n}");
                    return Ok(());
                }
                if let Some(ts) = &mut self.m.tiles {
                    self.m.message = tiles_message(ts.swap(i - 1, j - 1));
                }
            }
            "rotate" => {
                if let Some(ts) = &mut self.m.tiles {
                    self.m.message = tiles_message(ts.rotate());
                }
            }
            "reset" => {
                let order = self.m.roster_order();
                if let Some(ts) = &mut self.m.tiles {
                    self.m.message = tiles_message(ts.reset(&order));
                }
            }
            "lock" | "unlock" => {
                if let Some(ts) = &mut self.m.tiles {
                    ts.locked = words[0] == "lock";
                }
                return self.save_floor();
            }
            "layout" => return self.set_layout(&words[1..]),
            "zoom" => {
                let mut id = self.m.typing.clone();
                if id.is_empty() {
                    if let Some(r) = self.m.selected() {
                        if !r.parent.is_empty() {
                            self.m.message = CHILD_MESSAGE.into();
                            return Ok(());
                        }
                        id = r.id;
                    }
                }
                if id.is_empty() {
                    self.m.message = "zoom needs a pane. Run: list".into();
                    return Ok(());
                }
                return self.attach_to(&id);
            }
            "foreman" => return self.foreman_word(&words[1..]),
            "tree" => {
                self.refresh()?;
                let cols = self.cols();
                self.m.tree = Some(self.m.new_tree(cols));
            }
            _ => {
                if let Err(e) = self.create_pane(&words) {
                    return self.fail(e);
                }
                return self.refresh();
            }
        }
        Ok(())
    }

    /// A view id typed alone names the floor page.
    fn view_word(&mut self, words: &[String]) -> bool {
        if words.len() != 1 {
            return false;
        }
        if VERBS.contains(&words[0].as_str()) {
            return false;
        }
        for id in &self.o.views {
            if words[0] != *id {
                continue;
            }
            if self.o.web_url.is_empty() {
                self.m.message = "views live on the floor page, and the web server is off. Run: coppice web serve --persist".into();
                return true;
            }
            self.m.message = format!(
                "views live on the floor page: {}/#/view/{id}",
                self.o.web_url.trim_end_matches('/')
            );
            return true;
        }
        false
    }

    fn foreman_word(&mut self, args: &[String]) -> Result<(), FloorErr> {
        if !self.m.path.is_empty() {
            return self.task_foreman_word(args);
        }
        if args.is_empty() {
            return self.name_foreman();
        }
        if args.len() > 1 {
            self.m.message = "foreman takes one harness name, as in foreman claude".into();
            return Ok(());
        }
        let res = match call(
            &self.o.socket,
            "floor.foreman",
            params(vec![("harness", json!(args[0]))]),
        ) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        let id = s(&res, "pane");
        self.m.message = format!(
            "{FOREMAN_LABEL} opens. It gets the page once it is idle, and then your sentences."
        );
        self.refresh()?;
        self.fill_shown(&id);
        Ok(())
    }

    fn name_foreman(&mut self) -> Result<(), FloorErr> {
        let res = match call(&self.o.socket, "floor.facts", None) {
            Ok(r) => r,
            Err(e) => return self.fail(e),
        };
        if let Some(fm) = obj(&res, "foreman") {
            let id = s(fm, "pane");
            if !id.is_empty() {
                self.m.message = format!("foreman: {} ({id})", s(fm, "label"));
                return Ok(());
            }
        }
        self.m.message = NO_FOREMAN_ANSWER.into();
        Ok(())
    }

    fn task_foreman_word(&mut self, args: &[String]) -> Result<(), FloorErr> {
        let sc = self.m.scope();
        let Some(tk) = self.m.task(&sc) else {
            self.m.message = format!("task {sc} is gone");
            return Ok(());
        };
        let label = if tk.label.is_empty() {
            tk.id.clone()
        } else {
            tk.label.clone()
        };
        if args.is_empty() {
            if tk.foreman.is_empty() {
                self.m.message = format!(
                    "no foreman for {label}. Type: foreman {}",
                    self.m.default_harness()
                );
                return Ok(());
            }
            self.m.message = format!("foreman of {label}: {}", self.m.label_of(&tk.foreman));
            return Ok(());
        }
        if args.len() > 1 {
            self.m.message = "foreman takes one harness name, as in foreman claude".into();
            return Ok(());
        }
        let name = format!("{label}-foreman");
        self.refresh()?;
        for r in &self.m.rows {
            if r.label == name && r.parent.is_empty() {
                self.m.message =
                    format!("a pane labelled {name} runs already. Close {} first.", r.id);
                return Ok(());
            }
        }
        let mut extra = serde_json::Map::new();
        extra.insert("harness".into(), json!(args[0]));
        extra.insert("label".into(), json!(name));
        extra.insert("task".into(), json!(sc));
        if !tk.worktree.is_empty() {
            extra.insert("cwd".into(), json!(tk.worktree));
        } else if !tk.cwd.is_empty() {
            extra.insert("cwd".into(), json!(tk.cwd));
        }
        let id = match self.create(extra) {
            Ok(id) => id,
            Err(e) => return self.fail(e),
        };
        if let Err(e) = call(
            &self.o.socket,
            "task.set_foreman",
            params(vec![("task", json!(sc)), ("pane", json!(id))]),
        ) {
            self.fail(e)?;
            return self.refresh();
        }
        let tx = self.async_tx.clone();
        let (socket, line, fin) = (
            self.o.socket.clone(),
            task_promotion_line(&label),
            promoted_message(&name, &label),
        );
        let (pid, plabel) = (id.clone(), name.clone());
        std::thread::spawn(move || {
            let msg = promote(socket, pid, plabel, line, fin, PROMOTE_WAIT);
            let _ = tx.send(Async::Promoted(msg));
        });
        self.m.message = format!("{name} opens. It gets the page once it is idle.");
        self.refresh()
    }

    fn set_layout(&mut self, words: &[String]) -> Result<(), FloorErr> {
        if words.len() != 1 {
            self.m.message = LAYOUT_NEEDS_MESSAGE.into();
            return Ok(());
        }
        let Some(layout) = parse_layout(&words[0]) else {
            self.m.message = LAYOUT_NEEDS_MESSAGE.into();
            return Ok(());
        };
        if self.m.tiles.as_ref().is_some_and(|t| t.locked) {
            self.m.message = LOCKED_MESSAGE.into();
            return Ok(());
        }
        let mut order: Vec<String> = Vec::new();
        if let Some(ts) = &self.m.tiles {
            for id in &ts.slots {
                if !id.is_empty() && !order.contains(id) {
                    order.push(id.clone());
                }
            }
        }
        for id in self.m.roster_order() {
            if !order.contains(&id) {
                order.push(id);
            }
        }
        let mut ts = Tiles::new(layout, window_count(self.cols()));
        if let Err(e) = ts.reset(&order) {
            self.m.message = tiles_message(Err(e));
            return Ok(());
        }
        self.m.tiles = Some(ts);
        self.save_floor()
    }

    fn save_floor(&mut self) -> Result<(), FloorErr> {
        let Some(ts) = &self.m.tiles else {
            return Ok(());
        };
        if let Err(e) = write_floor_state(&self.o.data_dir, ts.layout, ts.locked) {
            self.m.message = format!(
                "cannot write {}: {e}",
                std::path::Path::new(&self.o.data_dir)
                    .join(FLOOR_FILE)
                    .display()
            );
        }
        Ok(())
    }
}

/// Puts pane id in slot i; when it is in another slot the two trade.
pub fn put_in_slot(ts: &mut Tiles, id: &str, i: i64) {
    while (ts.slots.len() as i64) <= i {
        ts.slots.push(String::new());
    }
    if let Some(from) = ts.slot_of(id) {
        ts.slots[from] = ts.slots[i as usize].clone();
    }
    ts.slots[i as usize] = id.to_string();
    ts.focus = i;
}
