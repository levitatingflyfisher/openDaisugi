//! The slot model behind the floor, ported from Go's `internal/tiles`. A
//! tile set is a list of slots, each holding one pane id or nothing, plus a
//! focus index. The floor shows the first slots the width allows. A slot
//! never moves on its own: only swap, rotate and reset rearrange, and a
//! lock refuses all three. The model never closes a pane and never talks
//! to a server. The page's tiles.js keeps the same rules.

#![allow(dead_code)]

/// How the floor gives panes their slots.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Layout {
    /// Every pane has its own slot. Fill appends.
    All,
    /// A fixed number of slots. Fill takes the first empty slot, else the
    /// focused one.
    Focus,
    /// A single slot. Fill replaces it.
    One,
}

impl Layout {
    /// The layout a name reads as: all, focus or one. Any other name reads
    /// as None.
    pub fn parse(name: &str) -> Option<Layout> {
        match name {
            "all" => Some(Layout::All),
            "focus" => Some(Layout::Focus),
            "one" => Some(Layout::One),
            _ => None,
        }
    }

    pub fn name(self) -> &'static str {
        match self {
            Layout::All => "all",
            Layout::Focus => "focus",
            Layout::One => "one",
        }
    }
}

/// The refusal every rearranging call returns while locked.
pub const ERR_LOCKED: &str = "Layout is locked. Unlock first.";

/// One tile set. `slots` holds a pane id per slot, or "" for an empty
/// slot. `focus` is the index of the focused slot. It is signed, as Go's
/// int is, so a caller may set it outside the slots.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Tiles {
    pub slots: Vec<String>,
    pub focus: i64,
    pub layout: Layout,
    pub locked: bool,
}

impl Tiles {
    /// A tile set for layout. Under Focus it makes n empty slots, with n
    /// below 1 read as 1. Under One it makes one slot. Under All none.
    pub fn new(layout: Layout, n: i64) -> Tiles {
        let count = match layout {
            Layout::All => 0,
            Layout::One => 1,
            Layout::Focus => n.max(1) as usize,
        };
        Tiles {
            slots: vec![String::new(); count],
            focus: 0,
            layout,
            locked: false,
        }
    }

    /// Puts pane in a slot and focuses that slot. A pane that already has
    /// a slot keeps it and only gains focus. Otherwise All appends a slot,
    /// One replaces slot 0, and Focus takes the first empty slot, else the
    /// focused slot. An empty name changes nothing.
    pub fn fill(&mut self, pane: &str) {
        if pane.is_empty() {
            return;
        }
        if let Some(i) = self.slot_of(pane) {
            self.focus = i as i64;
            return;
        }
        match self.layout {
            Layout::All => {
                self.slots.push(pane.to_string());
                self.focus = self.slots.len() as i64 - 1;
                return;
            }
            Layout::One => {
                if self.slots.is_empty() {
                    self.slots.push(String::new());
                }
                self.slots[0] = pane.to_string();
                self.focus = 0;
                return;
            }
            Layout::Focus => {}
        }
        if let Some(i) = self.slots.iter().position(|s| s.is_empty()) {
            self.slots[i] = pane.to_string();
            self.focus = i as i64;
            return;
        }
        if self.slots.is_empty() {
            self.slots.push(String::new());
        }
        self.focus = self.clamp(self.focus);
        let f = self.focus as usize;
        self.slots[f] = pane.to_string();
    }

    /// Gives pane a slot of its own and returns the slot index. A pane
    /// that already has a slot gains focus and returns its index. Under One
    /// it acts as fill and returns 0. Otherwise a new slot is appended and
    /// focused. An empty name returns -1.
    pub fn open(&mut self, pane: &str) -> i64 {
        if pane.is_empty() {
            return -1;
        }
        if let Some(i) = self.slot_of(pane) {
            self.focus = i as i64;
            return i as i64;
        }
        if self.layout == Layout::One {
            self.fill(pane);
            return 0;
        }
        self.slots.push(pane.to_string());
        self.focus = self.slots.len() as i64 - 1;
        self.focus
    }

    /// Empties slot i under Focus and One, and removes it under All. Focus
    /// stays inside the slots. An index outside the slots changes nothing.
    pub fn close_slot(&mut self, i: i64) {
        if i < 0 || i >= self.slots.len() as i64 {
            return;
        }
        if self.layout == Layout::All {
            self.slots.remove(i as usize);
        } else {
            self.slots[i as usize].clear();
        }
        self.focus = self.clamp(self.focus);
    }

    /// Exchanges slots i and j. Focus follows its pane. It refuses while
    /// locked, and refuses an index outside the slots.
    pub fn swap(&mut self, i: i64, j: i64) -> Result<(), String> {
        if self.locked {
            return Err(ERR_LOCKED.into());
        }
        let n = self.slots.len() as i64;
        for k in [i, j] {
            if k < 0 || k >= n {
                return Err(format!("Slot {k} is outside the slots 0 to {}.", n - 1));
            }
        }
        self.slots.swap(i as usize, j as usize);
        if self.focus == i {
            self.focus = j;
        } else if self.focus == j {
            self.focus = i;
        }
        Ok(())
    }

    /// Moves every slot one to the right. The last slot wraps to the
    /// front. Focus follows its pane. It refuses while locked.
    pub fn rotate(&mut self) -> Result<(), String> {
        if self.locked {
            return Err(ERR_LOCKED.into());
        }
        let n = self.slots.len() as i64;
        if n < 2 {
            return Ok(());
        }
        self.slots.rotate_right(1);
        // Go's % keeps the sign of the left side.
        self.focus = (self.focus + 1) % n;
        Ok(())
    }

    /// Fills the slots from order and moves focus to 0. Under All the
    /// slots become a copy of order. Under Focus the first slots take the
    /// first entries and the rest stay empty. Under One slot 0 takes the
    /// first entry. It refuses while locked.
    pub fn reset(&mut self, order: &[String]) -> Result<(), String> {
        if self.locked {
            return Err(ERR_LOCKED.into());
        }
        match self.layout {
            Layout::All => self.slots = order.to_vec(),
            Layout::One => {
                self.slots = vec![order.first().cloned().unwrap_or_default()];
            }
            Layout::Focus => {
                for (i, s) in self.slots.iter_mut().enumerate() {
                    *s = order.get(i).cloned().unwrap_or_default();
                }
            }
        }
        self.focus = 0;
        Ok(())
    }

    /// The pane in the focused slot, or "" when that slot is empty or
    /// there are no slots.
    pub fn focused(&self) -> &str {
        if self.focus < 0 || self.focus >= self.slots.len() as i64 {
            return "";
        }
        &self.slots[self.focus as usize]
    }

    /// The slot that holds pane. An empty name never matches an empty
    /// slot.
    pub fn slot_of(&self, pane: &str) -> Option<usize> {
        if pane.is_empty() {
            return None;
        }
        self.slots.iter().position(|s| s == pane)
    }

    /// A copy of the first n slots, fewer when there are fewer.
    pub fn shown(&self, n: i64) -> Vec<String> {
        let n = n.clamp(0, self.slots.len() as i64) as usize;
        self.slots[..n].to_vec()
    }

    /// i moved inside the slots. With no slots it is 0.
    fn clamp(&self, i: i64) -> i64 {
        let mut i = i;
        if i >= self.slots.len() as i64 {
            i = self.slots.len() as i64 - 1;
        }
        i.max(0)
    }
}

#[cfg(test)]
mod tests {
    //! Go's tiles_test.go, case for case.
    use super::*;

    fn count(slots: &[String], pane: &str) -> usize {
        slots.iter().filter(|s| *s == pane).count()
    }

    fn v(xs: &[&str]) -> Vec<String> {
        xs.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn fill_replaces_the_focused_slot_only() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.focus = 1;
        ts.fill("p2");
        ts.focus = 0;
        ts.fill("p3");
        assert_eq!(ts.slots, v(&["p3", "p2"]));
    }

    #[test]
    fn a_pane_has_at_most_one_tile() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.focus = 1;
        ts.fill("p1");
        assert_eq!(count(&ts.slots, "p1"), 1);
    }

    #[test]
    fn swap_keeps_focus_with_the_pane() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.focus = 1;
        ts.fill("p2");
        ts.focus = 0;
        ts.swap(0, 1).unwrap();
        assert_eq!(ts.focused(), "p1");
        assert_eq!(ts.slots[1], "p1");
    }

    #[test]
    fn locked_refuses_swap() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.locked = true;
        assert!(ts.swap(0, 1).is_err());
    }

    #[test]
    fn empty_slot_wins_over_focus() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.fill("p2");
        assert_eq!(ts.slots, v(&["p1", "p2"]));
    }

    #[test]
    fn new_makes_the_slots_each_layout_starts_with() {
        assert_eq!(Tiles::new(Layout::Focus, 3).slots.len(), 3);
        assert_eq!(Tiles::new(Layout::Focus, 0).slots.len(), 1);
        assert_eq!(Tiles::new(Layout::One, 3).slots.len(), 1);
        assert_eq!(Tiles::new(Layout::All, 3).slots.len(), 0);
        assert_eq!(Tiles::new(Layout::Focus, 2).focused(), "");
        // A caller reads a name that is none of the three as Focus, as Go's New does.
        assert_eq!(Layout::parse("grid"), None);
    }

    #[test]
    fn fill_under_all_appends_and_under_one_replaces() {
        let mut all = Tiles::new(Layout::All, 0);
        all.fill("p1");
        all.fill("p2");
        assert_eq!(all.slots, v(&["p1", "p2"]));
        let mut one = Tiles::new(Layout::One, 0);
        one.fill("p1");
        one.fill("p2");
        assert_eq!(one.slots, v(&["p2"]));
    }

    #[test]
    fn fill_focuses_the_slot_it_filled() {
        let mut ts = Tiles::new(Layout::Focus, 3);
        ts.fill("p1");
        ts.fill("p2");
        assert_eq!(ts.focus, 1);
        ts.fill("p1");
        assert_eq!(ts.focus, 0);
    }

    #[test]
    fn fill_ignores_an_empty_name() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("");
        assert_eq!(ts.slots, v(&["", ""]));
        assert_eq!(ts.open(""), -1);
    }

    #[test]
    fn open_appends_and_focuses_a_new_slot() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.fill("p2");
        assert_eq!(ts.open("p3"), 2);
        assert_eq!(ts.focus, 2);
        assert_eq!(ts.slots.len(), 3);
    }

    #[test]
    fn open_on_a_pane_already_in_a_slot_returns_that_slot() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.fill("p2");
        assert_eq!(ts.open("p1"), 0);
        assert_eq!(ts.focus, 0);
        assert_eq!(ts.slots.len(), 2);
    }

    #[test]
    fn open_under_one_replaces_slot_zero() {
        let mut ts = Tiles::new(Layout::One, 0);
        ts.fill("p1");
        assert_eq!(ts.open("p2"), 0);
        assert_eq!(ts.slots, v(&["p2"]));
    }

    #[test]
    fn close_slot_under_all_removes_the_slot() {
        let mut ts = Tiles::new(Layout::All, 0);
        ts.fill("p1");
        ts.fill("p2");
        ts.fill("p3");
        ts.focus = 2;
        ts.close_slot(2);
        assert_eq!((ts.slots.len(), ts.focus), (2, 1));
        ts.close_slot(0);
        assert_eq!((ts.slots.clone(), ts.focus), (v(&["p2"]), 0));
        ts.close_slot(0);
        assert_eq!((ts.slots.len(), ts.focus, ts.focused()), (0, 0, ""));
    }

    #[test]
    fn close_slot_under_focus_empties_the_slot() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.fill("p2");
        ts.close_slot(0);
        assert_eq!(ts.slots, v(&["", "p2"]));
        ts.close_slot(5);
        assert_eq!(ts.slots.len(), 2);
    }

    #[test]
    fn close_slot_clamps_focus_under_all() {
        let mut ts = Tiles::new(Layout::All, 0);
        ts.fill("p1");
        ts.fill("p2");
        ts.focus = 1;
        ts.close_slot(1);
        assert_eq!((ts.focus, ts.focused()), (0, "p1"));
    }

    #[test]
    fn swap_refuses_an_index_outside_the_slots() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        assert_eq!(
            ts.swap(0, 2).unwrap_err(),
            "Slot 2 is outside the slots 0 to 1."
        );
        assert!(ts.swap(-1, 0).is_err());
    }

    #[test]
    fn locked_refuses_rotate_and_reset() {
        let mut ts = Tiles::new(Layout::Focus, 2);
        ts.fill("p1");
        ts.locked = true;
        assert_eq!(ts.rotate().unwrap_err(), ERR_LOCKED);
        assert_eq!(ts.reset(&v(&["p2"])).unwrap_err(), ERR_LOCKED);
        assert_eq!(ts.swap(0, 1).unwrap_err(), ERR_LOCKED);
        assert_eq!(ts.slots[0], "p1");
    }

    #[test]
    fn rotate_moves_every_slot_right_and_keeps_focus_with_its_pane() {
        let mut ts = Tiles::new(Layout::Focus, 3);
        ts.fill("p1");
        ts.fill("p2");
        ts.fill("p3");
        ts.focus = 1;
        ts.rotate().unwrap();
        assert_eq!(ts.slots, v(&["p3", "p1", "p2"]));
        assert_eq!((ts.focus, ts.focused()), (2, "p2"));
        let mut empty = Tiles::new(Layout::All, 0);
        assert!(empty.rotate().is_ok());
    }

    #[test]
    fn reset_under_each_layout() {
        let mut order = v(&["a", "b", "c"]);
        let mut all = Tiles::new(Layout::All, 0);
        all.fill("z");
        all.focus = 0;
        all.reset(&order).unwrap();
        assert_eq!(
            (all.slots.len(), all.slots[2].as_str(), all.focus),
            (3, "c", 0)
        );
        order[0] = "changed".into();
        assert_eq!(all.slots[0], "a");

        let mut focus = Tiles::new(Layout::Focus, 2);
        focus.fill("z");
        focus.focus = 1;
        focus.reset(&v(&["a", "b", "c"])).unwrap();
        assert_eq!((focus.slots.clone(), focus.focus), (v(&["a", "b"]), 0));
        let mut wide = Tiles::new(Layout::Focus, 3);
        wide.fill("z");
        wide.reset(&v(&["a"])).unwrap();
        assert_eq!(wide.slots, v(&["a", "", ""]));

        let mut one = Tiles::new(Layout::One, 0);
        one.fill("z");
        one.reset(&v(&["a", "b"])).unwrap();
        assert_eq!((one.slots.clone(), one.focus), (v(&["a"]), 0));
        one.reset(&[]).unwrap();
        assert_eq!(one.slots, v(&[""]));
    }

    #[test]
    fn slot_of_and_shown() {
        let mut ts = Tiles::new(Layout::Focus, 3);
        ts.fill("p1");
        ts.fill("p2");
        assert_eq!(ts.slot_of("p2"), Some(1));
        assert_eq!(ts.slot_of("p9"), None);
        assert_eq!(ts.slot_of(""), None);
        assert_eq!(ts.shown(2), v(&["p1", "p2"]));
        assert_eq!(ts.shown(5).len(), 3);
        assert!(ts.shown(0).is_empty());
        assert!(ts.shown(-1).is_empty());
        let mut got = ts.shown(1);
        got[0] = "changed".into();
        assert_eq!(ts.slots[0], "p1");
    }
}
