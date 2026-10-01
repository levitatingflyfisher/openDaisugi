//! Tree node ids and the merge order of the view.

use std::cmp::Ordering;

/// Keeps first and count inside an i64.
pub const MAX_LEVEL: i32 = 62;

/// One tree node: level l covers the 2^l messages that start at message
/// i*2^l. A message is the level-0 node with its own id.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct NodeId {
    pub l: i32,
    pub i: i64,
}

impl NodeId {
    /// The id of the first message the node covers.
    pub fn first(self) -> i64 {
        self.i << self.l
    }

    /// The number of messages the node covers.
    pub fn count(self) -> i64 {
        1i64 << self.l
    }

    /// The id of the last message the node covers.
    pub fn last(self) -> i64 {
        self.first() + self.count() - 1
    }

    /// The node's public name, "id+n": the first message and the count.
    pub fn name(self) -> String {
        format!("{}+{}", self.first(), self.count())
    }

    /// The node one level up that holds this one.
    pub fn parent(self) -> NodeId {
        NodeId {
            l: self.l + 1,
            i: self.i >> 1,
        }
    }

    /// The two nodes one level down. Only for l > 0.
    pub fn children(self) -> (NodeId, NodeId) {
        (
            NodeId {
                l: self.l - 1,
                i: self.i << 1,
            },
            NodeId {
                l: self.l - 1,
                i: (self.i << 1) + 1,
            },
        )
    }
}

/// Reads "id+n". n must be a power of two and id a multiple of n.
pub fn parse_name(s: &str) -> Result<NodeId, String> {
    let bad = || format!("node name {} is not id+n", crate::sys::go_quote(s));
    let (a, b) = s.split_once('+').ok_or_else(bad)?;
    let (id, n) = match (go_parse_int(a), go_parse_int(b)) {
        (Some(id), Some(n)) => (id, n),
        _ => return Err(bad()),
    };
    node_of(id, n)
}

/// strconv.ParseInt(s, 10, 64): an optional sign, then decimal digits.
pub fn go_parse_int(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
        return None;
    }
    s.parse::<i64>().ok()
}

/// The node that covers the n messages from id.
pub fn node_of(id: i64, n: i64) -> Result<NodeId, String> {
    if id < 0 || n <= 0 || n & (n - 1) != 0 {
        return Err(format!(
            "node {id}+{n}: n must be a power of two and id at least 0"
        ));
    }
    let mut l = 0;
    while (1i64 << l) < n {
        l += 1;
    }
    if l > MAX_LEVEL || id % n != 0 {
        return Err(format!("node {id}+{n}: id must be a multiple of n"));
    }
    Ok(NodeId { l, i: id / n })
}

/// Whether a and b are the two halves of one parent, in order.
pub fn siblings(a: NodeId, b: NodeId) -> bool {
    a.l == b.l && a.i % 2 == 0 && b.i == a.i + 1
}

/// Compares a/2^la with b/2^lb for a, b >= 0, exactly, with no overflow.
pub fn due_cmp(a: i64, la: i32, b: i64, lb: i32) -> Ordering {
    if la > lb {
        return due_cmp(b, lb, a, la).reverse();
    }
    let d = lb - la;
    if d >= 63 {
        if a > 0 {
            return Ordering::Greater;
        }
        if b > 0 {
            return Ordering::Less;
        }
        return Ordering::Equal;
    }
    let hi = b >> d;
    if a < hi {
        Ordering::Less
    } else if a > hi {
        Ordering::Greater
    } else if b & ((1i64 << d) - 1) != 0 {
        Ordering::Less
    } else {
        Ordering::Equal
    }
}

/// Whether a/2^la < b/2^lb, exactly.
pub fn due_less(a: i64, la: i32, b: i64, lb: i32) -> bool {
    due_cmp(a, la, b, lb) == Ordering::Less
}

/// The pair of view lines to merge next, by the index of the pair's left
/// line, or None when no pair can merge. For two sibling lines at level l
/// the pair is due by (total - last) / 2^l. The most due pair whose parent
/// is built merges first; among equals, the oldest wins.
pub fn most_due(view: &[NodeId], total: i64, built: impl Fn(NodeId) -> bool) -> Option<usize> {
    let mut best: Option<usize> = None;
    let (mut best_num, mut best_l) = (0i64, 0i32);
    for k in 0..view.len().saturating_sub(1) {
        let (a, b) = (view[k], view[k + 1]);
        if !siblings(a, b) || !built(a.parent()) {
            continue;
        }
        let num = total - b.last();
        if best.is_none() || due_less(best_num, best_l, num, a.l) {
            best = Some(k);
            best_num = num;
            best_l = a.l;
        }
    }
    best
}

/// Replaces lines k and k+1 with their parent.
pub fn merge_at(view: &mut Vec<NodeId>, k: usize) {
    view[k] = view[k].parent();
    view.remove(k + 1);
}

/// The nodes queued for message i: its own and each parent it completes, a
/// binary carry up the tree.
pub fn chain(i: i64) -> Vec<NodeId> {
    let mut id = NodeId { l: 0, i };
    let mut out = Vec::new();
    loop {
        out.push(id);
        if id.i % 2 == 0 || id.l >= MAX_LEVEL {
            return out;
        }
        id = id.parent();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One entry of the rollback list, newest first: the oldest tick it
    /// keeps and its one-bit flag.
    struct Rb {
        keep: bool,
        state: i64,
        older: Option<Box<Rb>>,
    }

    /// The rollback push, a binary counter, written from the recipe's prose
    /// as the reference the merge rule must match.
    fn rb_push(tick: i64, l: Option<Box<Rb>>) -> Option<Box<Rb>> {
        match l {
            None => Some(Box::new(Rb {
                keep: false,
                state: tick,
                older: None,
            })),
            Some(e) if !e.keep => Some(Box::new(Rb {
                keep: true,
                state: e.state,
                older: e.older,
            })),
            Some(e) => Some(Box::new(Rb {
                keep: false,
                state: tick,
                older: rb_push(e.state, e.older),
            })),
        }
    }

    fn rb_lines(l: &Option<Box<Rb>>, tick: i64) -> Vec<NodeId> {
        let mut starts = Vec::new();
        let mut e = l.as_deref();
        while let Some(x) = e {
            starts.insert(0, x.state);
            e = x.older.as_deref();
        }
        let mut out = Vec::new();
        for (k, &s) in starts.iter().enumerate() {
            let end = starts.get(k + 1).copied().unwrap_or(tick + 1);
            let n = end - s;
            assert!(n & (n - 1) == 0 && s % n == 0, "tick {tick}: {s}+{n}");
            let mut lv = 0;
            while (1i64 << lv) < n {
                lv += 1;
            }
            out.push(NodeId { l: lv, i: s / n });
        }
        out
    }

    #[test]
    fn merge_order_equals_rollback_push() {
        let mut list = None;
        let mut view: Vec<NodeId> = Vec::new();
        for tick in 0..=20000i64 {
            list = rb_push(tick, list);
            let want = rb_lines(&list, tick);
            view.push(NodeId { l: 0, i: tick });
            while view.len() > want.len() {
                let k = most_due(&view, tick + 1, |_| true).expect("a pair to merge");
                merge_at(&mut view, k);
            }
            assert_eq!(view, want, "tick {tick}");
        }
    }

    #[test]
    fn the_first_message_rule_diverges() {
        let mut list = None;
        let mut view: Vec<NodeId> = Vec::new();
        let mut matched = 0;
        for tick in 0..=2000i64 {
            list = rb_push(tick, list);
            let want = rb_lines(&list, tick);
            view.push(NodeId { l: 0, i: tick });
            while view.len() > want.len() {
                let mut best: Option<usize> = None;
                let (mut bn, mut bl) = (0i64, 0i32);
                for k in 0..view.len() - 1 {
                    if !siblings(view[k], view[k + 1]) {
                        continue;
                    }
                    let num = tick + 1 - view[k].first();
                    if best.is_none() || due_less(bn, bl, num, view[k].l) {
                        best = Some(k);
                        bn = num;
                        bl = view[k].l;
                    }
                }
                merge_at(&mut view, best.unwrap());
            }
            if view == want {
                matched += 1;
            }
        }
        assert_ne!(matched, 2001, "the test cannot tell the rules apart");
    }

    #[test]
    fn due_less_is_exact() {
        let cases: &[(i64, i32, i64, i32, bool)] = &[
            (1, 0, 1, 0, false),
            (1, 1, 1, 0, true),
            (2, 1, 1, 0, false),
            (3, 1, 1, 0, false),
            (1, 0, 3, 1, true),
            (1, 62, 0, 0, false),
            (0, 0, 1, 62, true),
            (1 << 62, 62, 1, 0, false),
            ((1 << 62) + 1, 62, 1, 0, false),
            ((1 << 62) - 1, 62, 1, 0, true),
        ];
        for &(a, la, b, lb, want) in cases {
            assert_eq!(due_less(a, la, b, lb), want, "{a}/2^{la} vs {b}/2^{lb}");
        }
    }

    #[test]
    fn most_due_skips_unbuilt_parents() {
        let n = |l, i| NodeId { l, i };
        let view = vec![n(1, 0), n(1, 1), n(0, 4), n(0, 5)];
        assert_eq!(most_due(&view, 6, |_| true), Some(0));
        assert_eq!(most_due(&view, 6, |id| id != n(2, 0)), Some(2));
        assert_eq!(most_due(&view, 6, |_| false), None);
    }

    #[test]
    fn node_id_names() {
        let id = NodeId { l: 3, i: 5 };
        assert_eq!((id.first(), id.count(), id.last()), (40, 8, 47));
        assert_eq!(id.name(), "40+8");
        assert_eq!(parse_name("40+8").unwrap(), id);
        for bad in [
            "",
            "40",
            "40+0",
            "40+3",
            "41+8",
            "-8+8",
            "x+1",
            "8+x",
            "0+9223372036854775807",
        ] {
            assert!(parse_name(bad).is_err(), "{bad}");
        }
        assert_eq!(id.parent(), NodeId { l: 4, i: 2 });
        assert_eq!(
            id.children(),
            (NodeId { l: 2, i: 10 }, NodeId { l: 2, i: 11 })
        );
    }
}
