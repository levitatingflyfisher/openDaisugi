//! Screen detection: Herdr's TOML agent manifests, read the way the Go
//! coppice's internal/detect reads them. The same 21 manifest files are
//! compiled in from the Go tree, so both ports judge a screen by the same
//! rules. Patterns are rewritten to Go's RE2 meaning before they compile:
//! \d, \s, \w and \b are ASCII there, and Unicode in Rust's regex crate.

use std::collections::{BTreeMap, HashMap};
use std::path::{Path, PathBuf};
use std::sync::RwLock;

use regex::Regex;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::gojson;
use crate::sys;

/// What a manifest's min_engine_version is checked against.
pub const ENGINE_VERSION: i64 = 3;

const MAX_RULES: usize = 128;
const MAX_GATE_DEPTH: usize = 8;
const MAX_TOTAL_GATES: usize = 512;
const MAX_MATCHERS_PER_GATE: usize = 32;
const MAX_TOTAL_MATCHERS: usize = 1024;
const MAX_MATCHER_CHARS: usize = 512;
const TOP_REGION_MIN_VER: i64 = 3;
const MAX_TOP_REGION_LINES: usize = 65535;
const PREVIEW_MAX: usize = 240;

pub const STATE_IDLE: &str = "idle";
pub const STATE_WORKING: &str = "working";
pub const STATE_BLOCKED: &str = "blocked";
pub const STATE_UNKNOWN: &str = "unknown";

/// One bundled manifest: its file name and its text, from the Go tree.
macro_rules! manifest {
    ($name:literal) => {
        (
            $name,
            include_str!(concat!("../../coppice/internal/detect/manifests/", $name)),
        )
    };
}

/// The manifests the Go coppice embeds, in file name order.
const BUNDLED: [(&str, &str); 21] = [
    manifest!("amp.toml"),
    manifest!("antigravity.toml"),
    manifest!("claude.toml"),
    manifest!("cline.toml"),
    manifest!("codex.toml"),
    manifest!("cursor.toml"),
    manifest!("devin.toml"),
    manifest!("droid.toml"),
    manifest!("gemini.toml"),
    manifest!("github-copilot.toml"),
    manifest!("grok.toml"),
    manifest!("hermes.toml"),
    manifest!("kilo.toml"),
    manifest!("kimi.toml"),
    manifest!("kiro.toml"),
    manifest!("maki.toml"),
    manifest!("muse.toml"),
    manifest!("opencode.toml"),
    manifest!("pi.toml"),
    manifest!("qodercli.toml"),
    manifest!("qwen.toml"),
];

/// A nested matcher table.
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Gate {
    pub all: Vec<Gate>,
    pub any: Vec<Gate>,
    pub not: Vec<Gate>,
    pub contains: Vec<String>,
    pub regex: Vec<String>,
    pub line_regex: Vec<String>,
}

/// One rule. A rule takes the six matcher keys a gate takes.
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Rule {
    pub id: String,
    pub state: Option<String>,
    pub priority: i64,
    pub region: String,
    pub visible_idle: bool,
    pub visible_blocker: bool,
    pub visible_working: bool,
    pub skip_state_update: bool,
    pub all: Vec<Gate>,
    pub any: Vec<Gate>,
    pub not: Vec<Gate>,
    pub contains: Vec<String>,
    pub regex: Vec<String>,
    pub line_regex: Vec<String>,
}

impl Rule {
    /// unknown when the rule declares no state, as Herdr reads it.
    pub fn effective_state(&self) -> &str {
        self.state.as_deref().unwrap_or(STATE_UNKNOWN)
    }

    fn gate(&self) -> Gate {
        Gate {
            all: self.all.clone(),
            any: self.any.clone(),
            not: self.not.clone(),
            contains: self.contains.clone(),
            regex: self.regex.clone(),
            line_regex: self.line_regex.clone(),
        }
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Manifest {
    pub id: String,
    pub version: String,
    pub min_engine_version: i64,
    pub updated_at: String,
    pub aliases: Vec<String>,
    pub rules: Vec<Rule>,
}

/// A gate with its patterns already built.
#[derive(Debug, Default)]
struct CompiledGate {
    all: Vec<CompiledGate>,
    any: Vec<CompiledGate>,
    not: Vec<CompiledGate>,
    contains: Vec<String>,
    regex: Vec<Regex>,
    line_regex: Vec<Regex>,
}

/// A manifest ready to evaluate.
#[derive(Debug)]
pub struct Compiled {
    pub manifest: Manifest,
    gates: Vec<CompiledGate>,
}

/// What the evaluator sees: the screen unwrapped, the terminal title, and
/// the last OSC progress payload.
#[derive(Clone, Debug, Default)]
pub struct Input {
    pub screen: String,
    pub osc_title: String,
    pub osc_progress: String,
}

/// One rule the evaluator visited.
#[derive(Clone, Debug)]
pub struct Evaluated {
    pub id: String,
    pub priority: i64,
    pub region: String,
    pub state: String,
    pub matched: bool,
    pub region_bytes: usize,
    pub region_preview: String,
}

impl Evaluated {
    pub fn to_value(&self) -> Value {
        gojson::obj(vec![
            ("id", json!(self.id)),
            ("priority", json!(self.priority)),
            ("region", json!(self.region)),
            ("state", json!(self.state)),
            ("matched", json!(self.matched)),
            ("region_bytes", json!(self.region_bytes)),
            ("region_preview", json!(self.region_preview)),
        ])
    }
}

/// The verdict on one screen.
#[derive(Clone, Debug)]
pub struct Verdict {
    pub agent: String,
    pub matched: bool,
    pub state: String,
    pub rule_id: String,
    pub priority: i64,
    pub region: String,
    pub skip: bool,
    pub visible_idle: bool,
    pub visible_blocker: bool,
    pub visible_working: bool,
    pub evaluated: Vec<Evaluated>,
}

impl Default for Verdict {
    fn default() -> Verdict {
        Verdict {
            agent: String::new(),
            matched: false,
            state: STATE_UNKNOWN.into(),
            rule_id: String::new(),
            priority: 0,
            region: String::new(),
            skip: false,
            visible_idle: false,
            visible_blocker: false,
            visible_working: false,
            evaluated: Vec::new(),
        }
    }
}

/// Go's strings.ToLower: each rune by its simple lower case. Rust's
/// str::to_lowercase maps U+0130 to two runes and a final sigma by its
/// context, and Go does neither.
pub fn go_lower(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c == '\u{130}' {
                'i'
            } else {
                c.to_lowercase().next().unwrap_or(c)
            }
        })
        .collect()
}

/// Rewrites the Rust-only constructs the Go port also rewrites: \uXXXX
/// and \u{XXXX} become \x{XXXX}, and \p{Alphabetic} becomes \pL. Only a
/// live backslash starts one: an even run of backslashes is literal.
fn translate_rust_regex(p: &str) -> String {
    let b: Vec<char> = p.chars().collect();
    let mut out = String::new();
    let mut i = 0;
    while i < b.len() {
        if b[i] != '\\' {
            out.push(b[i]);
            i += 1;
            continue;
        }
        let start = i;
        while i < b.len() && b[i] == '\\' {
            i += 1;
        }
        let run = i - start;
        let rest: String = b[i..].iter().collect();
        let mut construct: Option<(usize, String)> = None;
        if let Some(r) = rest.strip_prefix("u{") {
            if let Some(end) = r.find('}') {
                let hex = &r[..end];
                if !hex.is_empty() && hex.chars().all(|c| c.is_ascii_hexdigit()) {
                    construct = Some((2 + end + 1, format!("\\x{{{hex}}}")));
                }
            }
        }
        if construct.is_none() {
            if let Some(r) = rest.strip_prefix('u') {
                let hex: String = r.chars().take(4).collect();
                if hex.chars().count() == 4 && hex.chars().all(|c| c.is_ascii_hexdigit()) {
                    construct = Some((5, format!("\\x{{{hex}}}")));
                }
            }
        }
        if construct.is_none() && rest.starts_with("p{Alphabetic}") {
            construct = Some(("p{Alphabetic}".len(), "\\pL".into()));
        }
        match construct {
            Some((len, rep)) if run % 2 == 1 => {
                out.extend(std::iter::repeat_n('\\', run - 1));
                out.push_str(&rep);
                i += len;
            }
            _ => out.extend(std::iter::repeat_n('\\', run)),
        }
    }
    out
}

/// Rewrites a pattern to Go's RE2 meaning for the Rust regex crate: the
/// Perl classes \d, \s and \w are ASCII in Go, \b and \B are ASCII word
/// boundaries, and a '[' or a doubled '&', '~' or '-' inside a class is a
/// literal in Go and an operator in Rust.
fn go_compat(p: &str) -> String {
    let c: Vec<char> = p.chars().collect();
    let mut out = String::new();
    let mut i = 0;
    let mut in_class = false;
    // True right after '[' or '[^': a ']' there is a literal.
    let mut class_start = false;
    while i < c.len() {
        let ch = c[i];
        if ch == '\\' {
            let Some(&n) = c.get(i + 1) else {
                out.push('\\');
                break;
            };
            let rep = match (n, in_class) {
                ('d', false) => Some("[0-9]"),
                ('D', false) => Some("[^0-9]"),
                ('s', false) => Some("[\\t\\n\\f\\r ]"),
                ('S', false) => Some("[^\\t\\n\\f\\r ]"),
                ('w', false) => Some("[0-9A-Za-z_]"),
                ('W', false) => Some("[^0-9A-Za-z_]"),
                ('b', false) => Some("(?-u:\\b)"),
                ('B', false) => Some("(?-u:\\B)"),
                ('d', true) => Some("0-9"),
                ('D', true) => Some("[^0-9]"),
                ('s', true) => Some("\\t\\n\\f\\r "),
                ('S', true) => Some("[^\\t\\n\\f\\r ]"),
                ('w', true) => Some("0-9A-Za-z_"),
                ('W', true) => Some("[^0-9A-Za-z_]"),
                _ => None,
            };
            match rep {
                Some(r) => out.push_str(r),
                None => {
                    out.push('\\');
                    out.push(n);
                }
            }
            i += 2;
            class_start = false;
            continue;
        }
        if !in_class {
            out.push(ch);
            i += 1;
            if ch == '[' {
                in_class = true;
                class_start = true;
                if c.get(i) == Some(&'^') {
                    out.push('^');
                    i += 1;
                }
            }
            continue;
        }
        // Inside a class.
        if ch == ']' && !class_start {
            in_class = false;
            out.push(ch);
            i += 1;
            continue;
        }
        class_start = false;
        if ch == '[' {
            if c.get(i + 1) == Some(&':') {
                // A POSIX class, [:name:], copied whole.
                let rest: String = c[i..].iter().collect();
                if let Some(end) = rest.find(":]") {
                    out.push_str(&rest[..end + 2]);
                    i += rest[..end + 2].chars().count();
                    continue;
                }
            }
            out.push_str("\\[");
            i += 1;
            continue;
        }
        if matches!(ch, '&' | '~' | '-') && c.get(i + 1) == Some(&ch) {
            out.push('\\');
            out.push(ch);
            i += 1;
            continue;
        }
        out.push(ch);
        i += 1;
    }
    out
}

/// Compiles one manifest pattern with Go's meaning.
pub fn compile_pattern(p: &str) -> Result<Regex, String> {
    Regex::new(&go_compat(&translate_rust_regex(p))).map_err(|e| e.to_string())
}

#[derive(Default)]
struct Totals {
    gates: usize,
    matchers: usize,
}

fn compile_gate(g: &Gate, ctx: &str, depth: usize, t: &mut Totals) -> Result<CompiledGate, String> {
    if depth > MAX_GATE_DEPTH {
        return Err(format!("{ctx} exceeds max gate depth {MAX_GATE_DEPTH}"));
    }
    t.gates += 1;
    if t.gates > MAX_TOTAL_GATES {
        return Err(format!("manifest exceeds max gate count {MAX_TOTAL_GATES}"));
    }
    let direct = g.contains.len() + g.regex.len() + g.line_regex.len();
    if direct > MAX_MATCHERS_PER_GATE {
        return Err(format!(
            "{ctx} has {direct} direct matchers, max is {MAX_MATCHERS_PER_GATE}"
        ));
    }
    t.matchers += direct;
    if t.matchers > MAX_TOTAL_MATCHERS {
        return Err(format!(
            "manifest exceeds max matcher count {MAX_TOTAL_MATCHERS}"
        ));
    }
    if direct + g.all.len() + g.any.len() == 0 {
        return Err(format!("{ctx} must contain a positive matcher"));
    }
    let mut out = CompiledGate::default();
    for s in &g.contains {
        if s.chars().count() > MAX_MATCHER_CHARS {
            return Err(format!(
                "{ctx} matcher exceeds max length {MAX_MATCHER_CHARS}"
            ));
        }
        out.contains.push(go_lower(s));
    }
    let compile_all = |pats: &[String], kind: &str| -> Result<Vec<Regex>, String> {
        let mut res = Vec::new();
        for p in pats {
            if p.chars().count() > MAX_MATCHER_CHARS {
                return Err(format!(
                    "{ctx} matcher exceeds max length {MAX_MATCHER_CHARS}"
                ));
            }
            let re = compile_pattern(p)
                .map_err(|e| format!("{ctx} has an invalid {kind} {}: {e}", sys::go_quote(p)))?;
            res.push(re);
        }
        Ok(res)
    };
    out.regex = compile_all(&g.regex, "regex")?;
    out.line_regex = compile_all(&g.line_regex, "line_regex")?;
    for n in &g.all {
        out.all
            .push(compile_gate(n, &format!("{ctx} > all"), depth + 1, t)?);
    }
    for n in &g.any {
        out.any
            .push(compile_gate(n, &format!("{ctx} > any"), depth + 1, t)?);
    }
    for n in &g.not {
        out.not
            .push(compile_gate(n, &format!("{ctx} > not"), depth + 1, t)?);
    }
    Ok(out)
}

/// Reads one manifest and validates it fully. Every failure is a load
/// error: a manifest that half-loads would make detections nobody can
/// explain.
pub fn parse(name: &str, data: &str) -> Result<Compiled, String> {
    let mut m: Manifest = toml::from_str(data).map_err(|e| {
        let msg = e.message().to_string();
        format!("{name}: {msg}")
    })?;
    if m.id.trim().is_empty() {
        return Err(format!("{name}: id must not be empty"));
    }
    if m.min_engine_version > ENGINE_VERSION {
        return Err(format!(
            "{name}: needs detection engine {}, this build is engine {ENGINE_VERSION}. Update coppice.",
            m.min_engine_version
        ));
    }
    if m.rules.is_empty() {
        return Err(format!("{name}: manifest must contain at least one rule"));
    }
    if m.rules.len() > MAX_RULES {
        return Err(format!(
            "{name}: {} rules, the cap is {MAX_RULES}",
            m.rules.len()
        ));
    }
    for r in m.rules.iter_mut() {
        if r.region.is_empty() {
            r.region = "whole_recent".into();
        }
    }
    let mut t = Totals::default();
    let mut gates = Vec::new();
    for r in &m.rules {
        if r.id.trim().is_empty() {
            return Err(format!("{name}: a rule has an empty id"));
        }
        if r.skip_state_update {
            if r.effective_state() != STATE_UNKNOWN {
                return Err(format!(
                    "{name}: rule {} uses skip_state_update without state = \"unknown\"",
                    r.id
                ));
            }
            if r.visible_idle || r.visible_blocker || r.visible_working {
                return Err(format!(
                    "{name}: rule {} uses skip_state_update with visible state evidence",
                    r.id
                ));
            }
        }
        let st = r.effective_state();
        if ![STATE_IDLE, STATE_WORKING, STATE_BLOCKED, STATE_UNKNOWN].contains(&st) {
            return Err(format!(
                "{name}: rule {} has state {}",
                r.id,
                sys::go_quote(st)
            ));
        }
        if !valid_region(&r.region) {
            return Err(format!(
                "{name}: rule {} uses invalid region: {}",
                r.id, r.region
            ));
        }
        if r.region.trim().starts_with("top_non_empty_lines(")
            && m.min_engine_version != 0
            && m.min_engine_version < TOP_REGION_MIN_VER
        {
            return Err(format!(
                "{name}: rule {} uses top_non_empty_lines but min_engine_version is {}, need {TOP_REGION_MIN_VER}",
                r.id, m.min_engine_version
            ));
        }
        let cg = compile_gate(&r.gate(), &format!("rule {}", r.id), 0, &mut t)
            .map_err(|e| format!("{name}: {e}"))?;
        gates.push(cg);
    }
    Ok(Compiled { manifest: m, gates })
}

const FIXED_REGIONS: [&str; 12] = [
    "whole_recent",
    "after_last_prompt_marker",
    "before_current_prompt_marker",
    "whole_recent_without_current_prompt_marker",
    "current_prompt_block_marker",
    "after_current_prompt_block_marker",
    "prompt_box_body",
    "above_prompt_box",
    "last_non_empty_above_prompt_box",
    "after_last_horizontal_rule",
    "osc_title",
    "osc_progress",
];

/// Whether a region name is one Herdr's validate_region_name accepts.
pub fn valid_region(spec: &str) -> bool {
    let s = spec.trim();
    FIXED_REGIONS.contains(&s)
        || region_count(s, "bottom_lines").is_some()
        || region_count(s, "bottom_non_empty_lines").is_some()
        || top_region_count(s).is_some()
}

/// The count of name(N): ASCII digits only, a leading zero allowed.
fn region_count(spec: &str, name: &str) -> Option<usize> {
    let rest = spec.strip_prefix(name)?;
    let digits = rest.strip_prefix('(')?.strip_suffix(')')?;
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    digits.parse::<usize>().ok()
}

/// The count of top_non_empty_lines(N): a positive decimal with no
/// leading zero, at most 65535.
fn top_region_count(spec: &str) -> Option<usize> {
    let rest = spec.strip_prefix("top_non_empty_lines")?;
    let digits = rest.strip_prefix('(')?.strip_suffix(')')?;
    if digits.is_empty() || digits.starts_with('0') || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let n = digits.parse::<usize>().ok()?;
    (n <= MAX_TOP_REGION_LINES).then_some(n)
}

/// Slices the input the way a rule asked for.
pub fn region(input: &Input, spec: &str) -> String {
    let s = spec.trim();
    match s {
        "osc_title" => return input.osc_title.clone(),
        "osc_progress" => return input.osc_progress.clone(),
        _ => {}
    }
    let c = input.screen.as_str();
    match s {
        "whole_recent" => return c.to_string(),
        "after_last_prompt_marker" => return after_last_prompt_marker(c),
        "before_current_prompt_marker" => return before_current_prompt_marker(c),
        "whole_recent_without_current_prompt_marker" => {
            return if current_prompt_index(&lines(c)).is_some() {
                String::new()
            } else {
                c.to_string()
            }
        }
        "current_prompt_block_marker" => return current_prompt_block_marker(c),
        "after_current_prompt_block_marker" => return after_current_prompt_block_marker(c),
        "prompt_box_body" => return prompt_box_body(c),
        "above_prompt_box" => return above_prompt_box(c),
        "last_non_empty_above_prompt_box" => return last_non_empty_line(&above_prompt_box(c)),
        "after_last_horizontal_rule" => return after_last_horizontal_rule(c),
        _ => {}
    }
    if let Some(n) = region_count(s, "bottom_lines") {
        return bottom_lines(c, n);
    }
    if let Some(n) = region_count(s, "bottom_non_empty_lines") {
        return bottom_non_empty(c, n);
    }
    if let Some(n) = top_region_count(s) {
        return top_non_empty(c, n);
    }
    String::new()
}

/// The screen's lines, with only the one final line end dropped.
fn lines(s: &str) -> Vec<&str> {
    s.strip_suffix('\n').unwrap_or(s).split('\n').collect()
}

fn join_from(ls: &[&str], i: usize) -> String {
    if i >= ls.len() {
        return String::new();
    }
    ls[i..].join("\n")
}

fn bottom_lines(c: &str, n: usize) -> String {
    let ls = lines(c);
    join_from(&ls, ls.len().saturating_sub(n))
}

fn bottom_non_empty(c: &str, n: usize) -> String {
    let ls = lines(c);
    let (mut seen, mut start) = (0, None);
    for i in (0..ls.len()).rev() {
        if !ls[i].trim().is_empty() {
            seen += 1;
            start = Some(i);
            if seen == n {
                break;
            }
        }
    }
    match start {
        Some(s) => join_from(&ls, s),
        None => String::new(),
    }
}

fn top_non_empty(c: &str, n: usize) -> String {
    let ls = lines(c);
    let (mut seen, mut end) = (0, None);
    for (i, l) in ls.iter().enumerate() {
        if !l.trim().is_empty() {
            seen += 1;
            end = Some(i);
            if seen == n {
                break;
            }
        }
    }
    match end {
        Some(e) => ls[..=e].join("\n"),
        None => String::new(),
    }
}

fn codex_prompt_line(l: &str) -> bool {
    l == "›" || l.starts_with("› ")
}

fn codex_block_marker_line(l: &str) -> bool {
    l.starts_with('•') || l.starts_with('■') || l.starts_with('✗') || l.starts_with('✓')
}

fn last_index(ls: &[&str], f: impl Fn(&str) -> bool) -> Option<usize> {
    (0..ls.len()).rev().find(|&i| f(ls[i]))
}

/// The last prompt marker, when no block marker follows it.
fn current_prompt_index(ls: &[&str]) -> Option<usize> {
    let i = last_index(ls, codex_prompt_line)?;
    if ls[i + 1..].iter().any(|l| codex_block_marker_line(l)) {
        return None;
    }
    Some(i)
}

fn after_last_prompt_marker(c: &str) -> String {
    let ls = lines(c);
    match last_index(&ls, codex_prompt_line) {
        Some(i) => join_from(&ls, i + 1),
        None => c.to_string(),
    }
}

fn before_current_prompt_marker(c: &str) -> String {
    let ls = lines(c);
    match current_prompt_index(&ls) {
        Some(i) => ls[..i].join("\n"),
        None => c.to_string(),
    }
}

fn current_prompt_block_marker(c: &str) -> String {
    let ls = lines(c);
    let Some(i) = current_prompt_index(&ls) else {
        return String::new();
    };
    match last_index(&ls[..i], codex_block_marker_line) {
        Some(j) => ls[j].to_string(),
        None => String::new(),
    }
}

fn after_current_prompt_block_marker(c: &str) -> String {
    let ls = lines(c);
    let Some(i) = current_prompt_index(&ls) else {
        return String::new();
    };
    match last_index(&ls[..i], codex_block_marker_line) {
        Some(j) => join_from(&ls, j),
        None => String::new(),
    }
}

/// A line of box-drawing dashes, with a label after it only when the run
/// is at least three long.
fn is_horizontal_rule(l: &str) -> bool {
    let t = l.trim();
    if t.is_empty() {
        return false;
    }
    let n = t.chars().take_while(|&r| r == '─').count();
    if n == 0 {
        return false;
    }
    let rest: String = t.chars().skip(n).collect();
    rest.trim_start_matches([' ', '\t']).is_empty() || n >= 3
}

/// The second horizontal rule counting up from the bottom.
fn prompt_box_top_index(ls: &[&str]) -> Option<usize> {
    let mut count = 0;
    for i in (0..ls.len()).rev() {
        if is_horizontal_rule(ls[i]) {
            count += 1;
            if count == 2 {
                return Some(i);
            }
        }
    }
    None
}

fn prompt_box_body(c: &str) -> String {
    let ls = lines(c);
    let Some(top) = prompt_box_top_index(&ls) else {
        return String::new();
    };
    let end = (top + 1..ls.len())
        .find(|&i| is_horizontal_rule(ls[i]))
        .unwrap_or(ls.len());
    if top + 1 >= end {
        return String::new();
    }
    ls[top + 1..end].join("\n")
}

fn above_prompt_box(c: &str) -> String {
    let ls = lines(c);
    match prompt_box_top_index(&ls) {
        Some(top) => ls[..top].join("\n"),
        None => c.to_string(),
    }
}

fn after_last_horizontal_rule(c: &str) -> String {
    let ls = lines(c);
    let last = ls.iter().rposition(|l| is_horizontal_rule(l));
    join_from(&ls, last.map(|i| i + 1).unwrap_or(0))
}

fn last_non_empty_line(c: &str) -> String {
    let ls = lines(c);
    ls.iter()
        .rev()
        .find(|l| !l.trim().is_empty())
        .map(|l| l.to_string())
        .unwrap_or_default()
}

fn preview(s: &str) -> String {
    if s.chars().count() <= PREVIEW_MAX {
        return s.to_string();
    }
    let mut out: String = s.chars().take(PREVIEW_MAX).collect();
    out.push_str("...");
    out
}

fn gate_matches(g: &CompiledGate, text: &str, lower: &str) -> bool {
    if !g.contains.iter().all(|n| lower.contains(n.as_str())) {
        return false;
    }
    if !g.regex.iter().all(|re| re.is_match(text)) {
        return false;
    }
    if !g.line_regex.is_empty() {
        let ls: Vec<&str> = text.split('\n').collect();
        if !g
            .line_regex
            .iter()
            .all(|re| ls.iter().any(|l| re.is_match(l)))
        {
            return false;
        }
    }
    if !g.all.iter().all(|n| gate_matches(n, text, lower)) {
        return false;
    }
    if !g.any.is_empty() && !g.any.iter().any(|n| gate_matches(n, text, lower)) {
        return false;
    }
    !g.not.iter().any(|n| gate_matches(n, text, lower))
}

impl Compiled {
    /// Runs every rule in file order and keeps the highest priority match.
    /// A tie keeps the earlier rule, as Herdr does.
    pub fn evaluate(&self, input: &Input) -> Verdict {
        let mut res = Verdict {
            agent: self.manifest.id.clone(),
            ..Default::default()
        };
        let mut best: Option<usize> = None;
        for (i, rule) in self.manifest.rules.iter().enumerate() {
            let text = region(input, &rule.region);
            let matched = gate_matches(&self.gates[i], &text, &go_lower(&text));
            res.evaluated.push(Evaluated {
                id: rule.id.clone(),
                priority: rule.priority,
                region: rule.region.clone(),
                state: rule.effective_state().to_string(),
                matched,
                region_bytes: text.len(),
                region_preview: preview(&text),
            });
            if !matched {
                continue;
            }
            if let Some(b) = best {
                if self.manifest.rules[b].priority >= rule.priority {
                    continue;
                }
            }
            best = Some(i);
        }
        let Some(b) = best else {
            return res;
        };
        let r = &self.manifest.rules[b];
        let st = r.effective_state().to_string();
        res.matched = true;
        res.rule_id = r.id.clone();
        res.priority = r.priority;
        res.region = r.region.clone();
        res.skip = r.skip_state_update;
        res.visible_idle = r.visible_idle && st == STATE_IDLE;
        res.visible_blocker = r.visible_blocker && st == STATE_BLOCKED;
        res.visible_working = r.visible_working && st == STATE_WORKING;
        res.state = st;
        res
    }

    /// Whether this manifest can ever read a screen as idle.
    pub fn has_idle_rule(&self) -> bool {
        self.manifest
            .rules
            .iter()
            .any(|r| r.effective_state() == STATE_IDLE)
    }
}

/// Every manifest coppice knows, bundled plus the operator's overrides.
#[derive(Default)]
pub struct Set {
    inner: RwLock<SetInner>,
}

#[derive(Default)]
struct SetInner {
    by_id: HashMap<String, std::sync::Arc<Compiled>>,
    aliases: HashMap<String, String>,
    sources: HashMap<String, String>,
    warnings: Vec<String>,
}

/// Where an operator's own manifests live.
pub fn override_dir() -> Option<PathBuf> {
    match std::env::var_os("XDG_CONFIG_HOME") {
        Some(x) if !x.is_empty() => Some(PathBuf::from(x).join("coppice").join("agent-detection")),
        _ => sys::home_dir().map(|h| h.join(".config").join("coppice").join("agent-detection")),
    }
}

impl Set {
    /// Reads the bundled manifests, then the overrides. A manifest that
    /// fails to load is skipped with a warning.
    pub fn load(override_dir: Option<&Path>) -> Set {
        let s = Set::default();
        for (name, data) in BUNDLED {
            s.add(name, "bundled", data);
        }
        if let Some(dir) = override_dir {
            match std::fs::read_dir(dir) {
                Ok(rd) => {
                    let mut files: BTreeMap<String, PathBuf> = BTreeMap::new();
                    for e in rd.flatten() {
                        let name = e.file_name().to_string_lossy().into_owned();
                        let is_dir = e.file_type().map(|t| t.is_dir()).unwrap_or(false);
                        if !is_dir && name.ends_with(".toml") {
                            files.insert(name, e.path());
                        }
                    }
                    for (name, p) in files {
                        let ps = p.to_string_lossy().into_owned();
                        match std::fs::read(&p) {
                            Ok(b) => s.add(&name, &ps, &String::from_utf8_lossy(&b)),
                            Err(e) => s.warn(format!(
                                "cannot read {ps}: {}",
                                sys::go_path_err("open", &p, &e)
                            )),
                        }
                    }
                }
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => s.warn(format!(
                    "cannot read override directory {}: {}",
                    dir.display(),
                    sys::go_path_err("open", dir, &e)
                )),
            }
        }
        s
    }

    fn lock_w(&self) -> std::sync::RwLockWriteGuard<'_, SetInner> {
        self.inner.write().unwrap_or_else(|e| e.into_inner())
    }

    fn lock_r(&self) -> std::sync::RwLockReadGuard<'_, SetInner> {
        self.inner.read().unwrap_or_else(|e| e.into_inner())
    }

    fn warn(&self, w: String) {
        self.lock_w().warnings.push(w);
    }

    fn add(&self, name: &str, source: &str, data: &str) {
        match parse(name, data) {
            Ok(c) => {
                let mut g = self.lock_w();
                let id = c.manifest.id.clone();
                for a in &c.manifest.aliases {
                    g.aliases.insert(a.clone(), id.clone());
                }
                g.sources.insert(id.clone(), source.to_string());
                g.by_id.insert(id, std::sync::Arc::new(c));
            }
            Err(e) => self.warn(format!("{source} did not load: {e}. Fix it or delete it.")),
        }
    }

    /// Every agent id, sorted.
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn agents(&self) -> Vec<String> {
        let mut out: Vec<String> = self.lock_r().by_id.keys().cloned().collect();
        out.sort();
        out
    }

    /// Resolves an agent name or alias to its manifest.
    pub fn get(&self, agent: &str) -> Option<std::sync::Arc<Compiled>> {
        let g = self.lock_r();
        if let Some(c) = g.by_id.get(agent) {
            return Some(c.clone());
        }
        let id = g.aliases.get(agent)?;
        g.by_id.get(id).cloned()
    }

    /// Where an agent's manifest came from: "bundled" or the override path.
    pub fn source(&self, agent: &str) -> String {
        self.lock_r()
            .sources
            .get(agent)
            .cloned()
            .unwrap_or_default()
    }

    pub fn warnings(&self) -> Vec<String> {
        self.lock_r().warnings.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn screens() -> PathBuf {
        PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../coppice/testdata/screens")
    }

    /// The fixture's headers and its input, read the way the format page
    /// says: header lines only at the top.
    fn fixture(path: &Path) -> (Option<String>, Input) {
        let text = std::fs::read_to_string(path).unwrap();
        let ls: Vec<&str> = text.split('\n').collect();
        let mut i = 0;
        let mut rule = None;
        let mut input = Input::default();
        while i < ls.len() {
            let l = ls[i];
            if let Some(v) = l.strip_prefix("#rule:") {
                rule = Some(v.trim().to_string());
            } else if let Some(v) = l.strip_prefix("#osc_title:") {
                input.osc_title = v.trim().to_string();
            } else if let Some(v) = l.strip_prefix("#osc_progress:") {
                input.osc_progress = v.trim().to_string();
            } else {
                break;
            }
            i += 1;
        }
        input.screen = ls[i..].join("\n");
        (rule, input)
    }

    #[test]
    fn every_bundled_manifest_loads() {
        let s = Set::load(None);
        assert_eq!(s.warnings(), Vec::<String>::new());
        assert_eq!(s.agents().len(), 21);
    }

    #[test]
    fn every_fixture_gets_its_declared_verdict() {
        let s = Set::load(None);
        let mut n = 0;
        for agent in std::fs::read_dir(screens()).unwrap().flatten() {
            if !agent.file_type().unwrap().is_dir() {
                continue;
            }
            let id = agent.file_name().to_string_lossy().into_owned();
            let c = s.get(&id).unwrap_or_else(|| panic!("no manifest {id}"));
            for f in std::fs::read_dir(agent.path()).unwrap().flatten() {
                let name = f.file_name().to_string_lossy().into_owned();
                let want = name.split('-').next().unwrap().to_string();
                let (rule, input) = fixture(&f.path());
                let v = c.evaluate(&input);
                assert_eq!(v.state, want, "{id}/{name}: {v:?}");
                if let Some(r) = rule {
                    assert_eq!(v.rule_id, r, "{id}/{name}");
                }
                n += 1;
            }
        }
        assert!(n >= 28, "only {n} fixtures");
    }

    #[test]
    fn every_manifest_is_fixtured_or_declared_unfixtured() {
        let s = Set::load(None);
        let text = std::fs::read_to_string(screens().join("unfixtured.txt")).unwrap();
        let mut known: Vec<String> = text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
            .map(String::from)
            .collect();
        for e in std::fs::read_dir(screens()).unwrap().flatten() {
            if e.file_type().unwrap().is_dir() {
                known.push(e.file_name().to_string_lossy().into_owned());
            }
        }
        for id in s.agents() {
            assert!(
                known.contains(&id),
                "{id} has no fixture and is not declared"
            );
        }
    }

    #[test]
    fn perl_classes_are_ascii_as_in_go() {
        let re = compile_pattern(r"^\s*\w+\d\b").unwrap();
        assert!(re.is_match(" ab1"));
        assert!(
            !re.is_match("\u{a0}ab1"),
            "a no-break space is not \\s in Go"
        );
        assert!(!re.is_match(" é1"), "é is not \\w in Go");
        assert!(
            !re.is_match(" a١"),
            "an Arabic-Indic digit is not \\d in Go"
        );
        let re = compile_pattern(r"[-\w.]+").unwrap();
        assert_eq!(re.find("x-y.z é").unwrap().as_str(), "x-y.z");
        let re = compile_pattern(r"[^\S]").unwrap();
        assert!(re.is_match(" ") && !re.is_match("\u{b}"));
        let re = compile_pattern(r"[[]").unwrap();
        assert!(re.is_match("["));
        assert!(compile_pattern(r"[[:alpha:]]").unwrap().is_match("q"));
    }

    #[test]
    fn rust_only_constructs_are_rewritten_like_go() {
        assert_eq!(
            translate_rust_regex(r"[\u2800-\u28FF]"),
            r"[\x{2800}-\x{28FF}]"
        );
        assert_eq!(translate_rust_regex(r"\u{fe0f}"), r"\x{fe0f}");
        assert_eq!(translate_rust_regex(r"\p{Alphabetic}"), r"\pL");
        assert_eq!(translate_rust_regex(r"\\u2800"), r"\\u2800");
        assert_eq!(translate_rust_regex(r"\\\u2800"), r"\\\x{2800}");
    }

    #[test]
    fn lower_case_is_go_simple_mapping() {
        assert_eq!(go_lower("İΣ"), "iσ");
        assert_eq!(go_lower("ΟΔΟΣ"), "οδοσ");
    }

    #[test]
    fn regions_slice_as_herdr_does() {
        let input = Input {
            screen: "a\n\nb\n──────\nbox\n──────\n".into(),
            ..Default::default()
        };
        assert_eq!(region(&input, "bottom_lines(2)"), "box\n──────");
        assert_eq!(region(&input, "bottom_non_empty_lines(2)"), "box\n──────");
        assert_eq!(region(&input, "top_non_empty_lines(2)"), "a\n\nb");
        assert_eq!(region(&input, "prompt_box_body"), "box");
        assert_eq!(region(&input, "above_prompt_box"), "a\n\nb");
        assert_eq!(region(&input, "last_non_empty_above_prompt_box"), "b");
        assert_eq!(region(&input, "after_last_horizontal_rule"), "");
        assert!(!valid_region("top_non_empty_lines(0)"));
        assert!(valid_region("bottom_lines(0)"));
        assert!(!valid_region("bottom_lines(-1)"));
    }

    #[test]
    fn a_bad_manifest_is_refused_with_its_reason() {
        let e = parse("x.toml", "id = \"x\"\n").unwrap_err();
        assert_eq!(e, "x.toml: manifest must contain at least one rule");
        let e = parse(
            "x.toml",
            "id = \"x\"\n[[rules]]\nid = \"r\"\nskip_state_update = true\nstate = \"idle\"\ncontains = [\"a\"]\n",
        )
        .unwrap_err();
        assert!(e.contains("skip_state_update without state"), "{e}");
        let e = parse("x.toml", "id = \"x\"\n[[rules]]\nid = \"r\"\n").unwrap_err();
        assert!(e.contains("must contain a positive matcher"), "{e}");
        assert!(parse(
            "x.toml",
            "id = \"x\"\nbogus = 1\n[[rules]]\nid = \"r\"\ncontains = [\"a\"]\n"
        )
        .is_err());
    }
}
