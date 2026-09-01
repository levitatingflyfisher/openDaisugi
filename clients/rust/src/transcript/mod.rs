//! Agent transcripts read into episodes as the oracle's parsers read them
//! (`opendaisugi/parsers`): `ClaudeCodeParser` for Claude Code .jsonl
//! sessions and `CodexParser` for Codex rollouts. An episode is the work
//! between two real user turns, with each tool call as a typed step; small
//! episodes merge into the one before, and large ones are split by a model
//! (the caller supplies the split).
//!
//! The oracle reads each value with Python's duck typing. Values are kept
//! as the transcript holds them until the steps are built, and an
//! exception the parse raises is `Fail::Parse` with Python's text. Input
//! this module does not read the way the oracle does is `Unreadable`, for
//! the caller to refuse. The Go client's `internal/transcript` is the
//! reference.

mod codex;

pub use codex::codex_messages;

use std::sync::OnceLock;

use regex::Regex;

use crate::gate::check::split_compound_shell;
use crate::gate::py::text::{head, split, strip};
use crate::gate::pyjson::{py_str, py_type_name, Object, Value};
use crate::gate::pymodel::decode_stream;
use crate::pathways::pmodel::{self, Id, Mode, ValidationError};

/// Why a transcript was not read, or a parse failed.
#[derive(Debug)]
pub enum Fail {
    /// Input this module does not read the way the oracle does.
    Unreadable(String),
    /// An exception the parse raises: the CLI prints "Parse error: <text>".
    Parse(String),
}

pub type R<T> = Result<T, Fail>;

fn unreadable(why: impl Into<String>) -> Fail {
    Fail::Unreadable(format!("a transcript value this binary does not read yet: {}", why.into()))
}

/// One flat {role, content} message.
#[derive(Debug, Clone)]
pub struct Message {
    pub role: Value,
    pub content: Value,
    /// Whether the row had a content key (a flat row may not).
    pub has_content: bool,
}

/// A step dict before `_finalize`: its type and the fields that type
/// reads, each as the transcript holds it.
#[derive(Debug, Clone)]
struct Step {
    typ: String,
    path: Value,
    command: Value,
    url: Value,
    query: Value,
    prompt: Value,
    skill_id: Value,
    skill_input: Option<Object>,
    server: String,
    tool: String,
    arguments: Option<Object>,
    prev_hint: bool,
    prev_index: Option<usize>,
}

impl Step {
    fn new(typ: &str) -> Step {
        Step {
            typ: typ.into(),
            path: Value::Null,
            command: Value::Null,
            url: Value::Null,
            query: Value::Null,
            prompt: Value::Null,
            skill_id: Value::Null,
            skill_input: None,
            server: String::new(),
            tool: String::new(),
            arguments: None,
            prev_hint: false,
            prev_index: None,
        }
    }
}

/// `parsers.claude_code._RawEpisode`.
#[derive(Debug, Clone)]
pub struct RawEpisode {
    /// A str, or for a split episode the model's value as it is.
    pub task: Value,
    pub first_message: usize,
    pub last_message: usize,
    steps: Vec<Step>,
    /// The split's start_index and end_index, None for an episode that was
    /// not split.
    step_range: Option<(Value, Value)>,
}

impl RawEpisode {
    /// The number of steps.
    pub fn steps(&self) -> usize {
        self.steps.len()
    }
}

/// `dict.get(k)` on a JSON object: the value, or None.
fn get(o: &Object, k: &str) -> Value {
    o.get(k).cloned().unwrap_or(Value::Null)
}

/// `a or b`.
fn or2(a: Value, b: Value) -> Value {
    if a.truthy() {
        a
    } else {
        b
    }
}

/// `str(v)` for a value `json.loads` makes.
fn pystr_of(v: &Value) -> R<String> {
    py_str(v).map_err(|_| unreadable("a value whose str() this binary does not write"))
}

/// The TypeError a set or dict lookup of `v` raises: a list or a dict is
/// unhashable.
fn unhashable(v: &Value) -> R<()> {
    match v {
        Value::List(_) | Value::Obj(_) => Err(Fail::Parse(format!("unhashable type: '{}'", py_type_name(v)))),
        _ => Ok(()),
    }
}

/// The AttributeError of `v.<attr>` on a value that has none.
fn no_attr(v: &Value, attr: &str) -> Fail {
    Fail::Parse(format!("'{}' object has no attribute '{attr}'", py_type_name(v)))
}

/// Python's text-mode iteration of a file: universal newlines.
pub fn lines(text: &str) -> Vec<&str> {
    let b = text.as_bytes();
    let mut out = vec![];
    let mut start = 0;
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'\n' => {
                out.push(&text[start..=i]);
                start = i + 1;
            }
            b'\r' => {
                if i + 1 < b.len() && b[i + 1] == b'\n' {
                    out.push(&text[start..i + 2]);
                    i += 1;
                } else {
                    out.push(&text[start..=i]);
                }
                start = i + 1;
            }
            _ => {}
        }
        i += 1;
    }
    if start < text.len() {
        out.push(&text[start..]);
    }
    out
}

/// `json.loads` of a stripped line: None when the line is not JSON. The
/// ValueError of an int past the digit limit ends the parse; a line nested
/// past the decoder's stack is unreadable (where Python's RecursionError
/// comes depends on its C stack).
fn load_line(line: &str) -> R<Option<Value>> {
    use crate::gate::pyjson::{loads, py_decode_error, PyDecodeFail};
    match py_decode_error(line, 900) {
        Some(PyDecodeFail::Raised(m)) if m.starts_with("Exceeds the limit ") => return Err(Fail::Parse(m)),
        Some(PyDecodeFail::Raised(_)) => return Ok(None),
        Some(PyDecodeFail::TooDeep) => {
            return Err(unreadable("a transcript line nested deeper than this binary decodes"))
        }
        None => {}
    }
    match loads(line) {
        Ok(v) => Ok(Some(v)),
        Err(_) => Err(unreadable("a transcript line this binary does not decode")),
    }
}

/// `parse()` up to the merge: the transcript's bytes read as
/// `open(path, encoding="utf-8")` reads them, the messages, and the raw
/// episodes.
pub fn read(raw: &[u8], format: &str) -> R<Vec<RawEpisode>> {
    let (text, dec) = decode_stream(raw);
    let msgs = if format == "codex" { codex_messages(&text) } else { claude_messages(&text) };
    // The lines read before a byte that is not UTF-8 are parsed first, so
    // an error in them comes first.
    let msgs = msgs?;
    if let Some(m) = dec {
        return Err(Fail::Parse(m));
    }
    identify(&msgs)
}

/// `ClaudeCodeParser._read_messages`.
pub fn claude_messages(text: &str) -> R<Vec<Message>> {
    let mut out = vec![];
    for raw in lines(text) {
        let line = strip(raw);
        if line.is_empty() {
            continue;
        }
        let Some(v) = load_line(line)? else { continue };
        let Value::Obj(d) = v else { continue };
        let row_type = d.get("type").cloned();
        let rt = row_type.as_ref().and_then(|v| v.as_str()).map(|s| s.to_string());
        let is_turn = matches!(rt.as_deref(), Some("user") | Some("assistant"));
        if is_turn {
            if let Value::Obj(msg) = get(&d, "message") {
                let content = msg.get("content").cloned().unwrap_or(Value::Str(String::new()));
                let role = or2(get(&msg, "role"), row_type.clone().unwrap_or(Value::Null));
                out.push(Message { role, content, has_content: true });
                continue;
            }
        }
        // `row_type not in ("user", "assistant")` on an unhashable value
        // compares by ==: never a member.
        if row_type.as_ref().is_some_and(|v| v.truthy()) && !is_turn {
            continue;
        }
        if d.get("role").is_some() {
            let content = d.get("content").cloned();
            out.push(Message {
                role: get(&d, "role"),
                has_content: content.is_some(),
                content: content.unwrap_or(Value::Null),
            });
        }
    }
    Ok(out)
}

/// `_MAX_TASK_CHARS`.
const MAX_TASK_CHARS: usize = 2000;

fn res() -> &'static [Regex; 4] {
    static RES: OnceLock<[Regex; 4]> = OnceLock::new();
    RES.get_or_init(|| {
        [
            Regex::new(r"(?s)<system-reminder>.*?</system-reminder>").expect("a fixed pattern"),
            Regex::new(r"(?s)<local-command-[^>]*>.*?</local-command-[^>]*>").expect("a fixed pattern"),
            Regex::new(r"(?s)<command-message>.*?</command-message>").expect("a fixed pattern"),
            Regex::new(r"</?command-(name|args)>").expect("a fixed pattern"),
        ]
    })
}

/// `_clean_task`.
fn clean_task(text: &str) -> String {
    let t = strip(text);
    if t.starts_with("Base directory for this skill:") {
        let first = t.split('\n').next().unwrap_or("");
        let path_part = match first.split_once(':') {
            Some((_, after)) => strip(after),
            None => "",
        };
        let trimmed = path_part.trim_end_matches('/');
        let name = trimmed.rsplit('/').next().unwrap_or("");
        return format!("skill: {}", if name.is_empty() { "unknown" } else { name });
    }
    if t.starts_with("This session is being continued") {
        return "session continuation".into();
    }
    let mut t = t.to_string();
    for re in res() {
        t = re.replace_all(&t, " ").into_owned();
    }
    split(&t).join(" ")
}

/// `_user_text`: the task label of a real user turn, or None.
fn user_text(m: &Message) -> Option<String> {
    match m.role.as_str() {
        Some("user") | Some("human") => {}
        _ => return None,
    }
    let text = match &m.content {
        Value::Str(s) => s.clone(),
        Value::List(l) => {
            if l.len() != 1 {
                return None;
            }
            let Value::Obj(b) = &l[0] else { return None };
            if get(b, "type").as_str() != Some("text") {
                return None;
            }
            match get(b, "text") {
                Value::Str(s) => s,
                _ => return None,
            }
        }
        _ => return None,
    };
    let mut cleaned = clean_task(&text);
    if cleaned.is_empty() {
        cleaned = head(strip(&text), 80).to_string();
    }
    Some(head(&cleaned, MAX_TASK_CHARS).to_string())
}

/// `_extract_tool_uses`: the tool_use blocks of an assistant message.
fn tool_uses(m: &Message) -> R<Vec<Object>> {
    let content = if m.has_content { m.content.clone() } else { Value::List(vec![]) };
    match content {
        Value::Str(_) => Ok(vec![]),
        Value::List(l) => Ok(l
            .into_iter()
            .filter_map(|b| match b {
                Value::Obj(o) if get(&o, "type").as_str() == Some("tool_use") => Some(o),
                _ => None,
            })
            .collect()),
        // Iterating a dict yields its keys: never a dict.
        Value::Obj(_) => Ok(vec![]),
        other => Err(Fail::Parse(format!("'{}' object is not iterable", py_type_name(&other)))),
    }
}

fn tool_type(name: &str) -> Option<&'static str> {
    Some(match name {
        "Edit" | "Write" => "file_write",
        "Read" | "Glob" | "Grep" => "file_read",
        "Bash" => "shell",
        "WebFetch" | "WebSearch" => "network",
        _ => return None,
    })
}

/// `_extract_step`: None for a tool that is not work. Values are kept as
/// the transcript holds them; `_finalize` validates them.
fn extract_step(tu: &Object) -> R<Option<Step>> {
    let name_v = tu.get("name").cloned().unwrap_or(Value::Str(String::new()));
    let inp_v = or2(get(tu, "input"), Value::Obj(Object::new()));
    let inp = inp_v.as_obj();
    unhashable(&name_v)?;
    let name = name_v.as_str();
    let t = name.and_then(tool_type);
    let need_inp = || inp.ok_or_else(|| no_attr(&inp_v, "get"));
    match t {
        Some("network") => {
            let inp = need_inp()?;
            let mut s = Step::new("network");
            s.url = get(inp, "url");
            s.query = get(inp, "query");
            return Ok(Some(s));
        }
        Some(t) => {
            let inp = need_inp()?;
            let mut s = Step::new(t);
            s.path = or2(or2(get(inp, "file_path"), get(inp, "path")), get(inp, "pattern"));
            s.command = or2(get(inp, "command"), get(inp, "query"));
            return Ok(Some(s));
        }
        None => {}
    }
    let Some(name) = name else { return Err(no_attr(&name_v, "startswith")) };
    if name == "Agent" || name == "Task" {
        let inp = need_inp()?;
        let mut s = Step::new("task");
        s.prompt = or2(get(inp, "prompt"), Value::Str(String::new()));
        return Ok(Some(s));
    }
    if name == "Skill" {
        let inp = need_inp()?;
        let args = get(inp, "args");
        let si = match args {
            Value::Obj(a) => a,
            other => {
                let mut o = Object::new();
                if other.truthy() {
                    o.set("args", other);
                }
                o
            }
        };
        let mut s = Step::new("skill");
        s.skill_id = or2(or2(get(inp, "skill"), get(inp, "command")), Value::Str(String::new()));
        s.skill_input = Some(si);
        return Ok(Some(s));
    }
    if let Some(rest) = name.strip_prefix("mcp__") {
        let (server, tool) = rest.split_once("__").unwrap_or((rest, ""));
        let mut s = Step::new("mcp");
        s.server = server.into();
        s.tool = tool.into();
        s.arguments = Some(inp.cloned().unwrap_or_default());
        return Ok(Some(s));
    }
    Ok(None)
}

/// `_split_compound_shell` on a command of any JSON type: a str is split
/// as the oracle splits it; a falsy value gives no parts; a list is walked
/// item by item, as Python walks it.
fn split_compound(v: &Value) -> R<Vec<String>> {
    if !v.truthy() {
        return Ok(vec![]);
    }
    match v {
        Value::Str(c) => Ok(split_compound_shell(c)),
        Value::List(items) => split_items(items),
        // command[0] on a dict: KeyError(0).
        Value::Obj(_) => Err(Fail::Parse("0".into())),
        other => Err(Fail::Parse(format!("object of type '{}' has no len()", py_type_name(other)))),
    }
}

/// `_split_compound_shell` over a list: each item stands where a character
/// would, and `"".join` raises on an item that is not a str.
fn split_items(items: &[Value]) -> R<Vec<String>> {
    fn join(cur: &[&Value]) -> R<String> {
        let mut b = String::new();
        for (k, c) in cur.iter().enumerate() {
            match c {
                Value::Str(s) => b.push_str(s),
                other => {
                    return Err(Fail::Parse(format!(
                        "sequence item {k}: expected str instance, {} found",
                        py_type_name(other)
                    )))
                }
            }
        }
        Ok(b)
    }
    let is = |v: &Value, s: &str| v.as_str() == Some(s);
    let mut parts: Vec<String> = vec![];
    let mut cur: Vec<&Value> = vec![];
    let (mut in_s, mut in_d) = (false, false);
    let mut i = 0;
    while i < items.len() {
        let c = &items[i];
        if is(c, "'") && !in_d {
            in_s = !in_s;
            cur.push(c);
            i += 1;
            continue;
        }
        if is(c, "\"") && !in_s {
            in_d = !in_d;
            cur.push(c);
            i += 1;
            continue;
        }
        if !in_s && !in_d {
            let step = if is(c, ";") {
                1
            } else if (is(c, "&") && i + 1 < items.len() && is(&items[i + 1], "&"))
                || (is(c, "|") && i + 1 < items.len() && is(&items[i + 1], "|"))
            {
                2
            } else {
                0
            };
            if step > 0 {
                parts.push(strip(&join(&cur)?).to_string());
                cur.clear();
                i += step;
                continue;
            }
        }
        cur.push(c);
        i += 1;
    }
    let tail = join(&cur)?;
    let tail = strip(&tail);
    if !tail.is_empty() {
        parts.push(tail.to_string());
    }
    Ok(parts.into_iter().filter(|p| !p.is_empty()).collect())
}

/// `_extract_step_maybe_multiple`.
fn extract_steps(tu: &Object) -> R<Vec<Step>> {
    if get(tu, "name").as_str() == Some("Bash") {
        let inp_v = tu.get("input").cloned().unwrap_or(Value::Obj(Object::new()));
        let Value::Obj(inp) = &inp_v else { return Err(no_attr(&inp_v, "get")) };
        let cmd = inp.get("command").cloned().unwrap_or(Value::Str(String::new()));
        let parts = split_compound(&cmd)?;
        if parts.len() > 1 {
            return Ok(parts
                .into_iter()
                .map(|p| {
                    let mut s = Step::new("shell");
                    s.command = Value::Str(p);
                    s.prev_hint = true;
                    s
                })
                .collect());
        }
    }
    Ok(extract_step(tu)?.into_iter().collect())
}

/// `_identify_episodes`: episodes cut at real user turns.
pub fn identify(messages: &[Message]) -> R<Vec<RawEpisode>> {
    let mut eps: Vec<RawEpisode> = vec![];
    for (idx, m) in messages.iter().enumerate() {
        if let Some(task) = user_text(m) {
            if let Some(cur) = eps.last_mut() {
                cur.last_message = idx - 1;
            }
            eps.push(RawEpisode {
                task: Value::Str(task),
                first_message: idx,
                last_message: idx,
                steps: vec![],
                step_range: None,
            });
            continue;
        }
        let Some(cur) = eps.last_mut() else { continue };
        cur.last_message = idx;
        if m.role.as_str() != Some("assistant") {
            continue;
        }
        for tu in tool_uses(m)? {
            let mut prev: Option<usize> = None;
            for mut s in extract_steps(&tu)? {
                if s.prev_hint {
                    s.prev_index = prev;
                }
                s.prev_hint = false;
                cur.steps.push(s);
                prev = Some(cur.steps.len() - 1);
            }
        }
    }
    Ok(eps)
}

/// `_merge_small`.
pub fn merge_small(eps: Vec<RawEpisode>, min_tools: i128) -> Vec<RawEpisode> {
    let mut out: Vec<RawEpisode> = vec![];
    for ep in eps {
        match out.last_mut() {
            Some(last) if (ep.steps.len() as i128) < min_tools => {
                last.steps.extend(ep.steps);
                last.last_message = ep.last_message;
            }
            _ => out.push(ep),
        }
    }
    out
}

/// The user content `_llm_split` sends for one episode.
pub fn split_content(ep: &RawEpisode) -> R<String> {
    let mut lines = Vec::with_capacity(ep.steps.len());
    for (i, s) in ep.steps.iter().enumerate() {
        let v = or2(s.path.clone(), s.command.clone());
        let label = if v.truthy() { pystr_of(&v)? } else { String::new() };
        lines.push(format!("{i}: {} {label}", s.typ));
    }
    Ok(format!("User message: {}\n\nTool calls ({} total):\n{}", pystr_of(&ep.task)?, ep.steps.len(), lines.join("\n")))
}

/// An int value, bool included (bool is an int in Python).
fn int_of(v: &Value) -> Option<i64> {
    match v {
        Value::Bool(b) => Some(*b as i64),
        Value::Int(t) => t.parse().ok(),
        _ => None,
    }
}

/// The [lo, hi) that `seq[a:b]` takes of n items.
fn slice_bounds(n: i64, a: i64, b: i64) -> (usize, usize) {
    let norm = |i: i64| -> i64 {
        let i = if i < 0 { (i + n).max(0) } else { i };
        i.min(n)
    };
    let (lo, hi) = (norm(a), norm(b));
    (lo as usize, hi.max(lo) as usize)
}

/// `_validate_boundaries`: the boundaries, in their own order, when sorted
/// by start they cover [0, n) with no gap or overlap.
fn valid_boundaries(b: &Value, n: usize) -> Option<Vec<Object>> {
    if !b.truthy() {
        return None;
    }
    // A dict iterates its keys and a str its characters: indexing a str
    // by "start_index" raises TypeError, which is caught.
    let Value::List(list) = b else { return None };
    let mut subs = vec![];
    let mut starts = vec![];
    for x in list {
        let Value::Obj(o) = x else { return None };
        // Sorting on keys of mixed types raises TypeError, and a single
        // non-int start fails the isinstance check.
        let s = int_of(o.get("start_index")?)?;
        subs.push(o.clone());
        starts.push(s);
    }
    let mut order: Vec<usize> = (0..subs.len()).collect();
    // sorted() is stable.
    order.sort_by_key(|&i| starts[i]);
    let first = starts[order[0]];
    let last = int_of(&get(&subs[order[order.len() - 1]], "end_index"))?;
    if first != 0 || last != n as i64 - 1 {
        return None;
    }
    for k in 1..order.len() {
        let prev_end = int_of(&get(&subs[order[k - 1]], "end_index"))?;
        if starts[order[k]] != prev_end + 1 {
            return None;
        }
    }
    // Each sub is sliced by its end_index too, which must be an int.
    for s in &subs {
        int_of(&get(s, "end_index"))?;
    }
    Some(subs)
}

/// `_split_large`: each episode over max_tools is cut where `split` says.
/// `split` returns the boundaries (the "subtasks" value), or None on a
/// failed call.
pub fn split_large(
    eps: Vec<RawEpisode>,
    max_tools: i128,
    split: &mut dyn FnMut(&str) -> R<Option<Value>>,
) -> R<Vec<RawEpisode>> {
    let mut out = vec![];
    for ep in eps {
        if ep.steps.len() as i128 <= max_tools {
            out.push(ep);
            continue;
        }
        let b = split(&split_content(&ep)?)?.unwrap_or(Value::Null);
        let Some(subs) = valid_boundaries(&b, ep.steps.len()) else {
            out.push(ep);
            continue;
        };
        for sub in subs {
            let Some(task) = sub.get("task").cloned() else { return Err(Fail::Parse("'task'".into())) };
            let start = int_of(&get(&sub, "start_index")).unwrap_or(0);
            let end = int_of(&get(&sub, "end_index")).unwrap_or(0);
            let (lo, hi) = slice_bounds(ep.steps.len() as i64, start, end + 1);
            out.push(RawEpisode {
                task,
                first_message: ep.first_message,
                last_message: ep.last_message,
                steps: ep.steps[lo..hi].to_vec(),
                step_range: Some((get(&sub, "start_index"), get(&sub, "end_index"))),
            });
        }
    }
    Ok(out)
}

/// `urllib.parse.quote(s)` with the default safe "/".
fn quote(s: &str) -> String {
    let mut b = String::new();
    for c in s.bytes() {
        if c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b'-' | b'~' | b'/') {
            b.push(c as char);
        } else {
            b.push_str(&format!("%{c:02X}"));
        }
    }
    b
}

/// `_network_url`.
fn network_url(s: &Step) -> R<String> {
    if s.url.truthy() {
        return pystr_of(&s.url);
    }
    if s.query.truthy() {
        return Ok(format!("https://web-search.invalid/?q={}", quote(&pystr_of(&s.query)?)));
    }
    Ok(String::new())
}

/// A value as `model_dump(mode="json")` writes it: a float that is nan or
/// an infinity becomes None.
fn json_mode(v: Value) -> Value {
    match v {
        Value::Float(f) if !f.is_finite() => Value::Null,
        Value::List(l) => Value::List(l.into_iter().map(json_mode).collect()),
        Value::Obj(o) => {
            let mut out = Object::with_capacity(o.len());
            for (k, x) in o.iter() {
                out.set(k, json_mode(x.clone()));
            }
            Value::Obj(out)
        }
        other => other,
    }
}

/// One step model constructed from keyword values, as `_finalize`
/// constructs it: pydantic's error when a value does not validate.
fn build(tag: &str, kw: &Object) -> R<()> {
    let Some(i) = pmodel::steps().order.iter().position(|t| *t == tag) else {
        return Err(Fail::Unreadable(format!("a step type {tag} this binary does not carry")));
    };
    match pmodel::validate_model(Id::Step(i), &Value::Obj(kw.clone()), Mode::Python) {
        Ok(_) => Ok(()),
        Err(e) => Err(Fail::Parse(e.text())),
    }
}

/// `_finalize`: the episodes as Episode dumps (mode="json", exclude_none),
/// with sequential step ids across the whole transcript. A value a step or
/// an episode does not take is pydantic's error.
pub fn finalize(eps: &[RawEpisode]) -> R<Vec<Value>> {
    let mut out = vec![];
    let mut counter = 0usize;
    for (i, ep) in eps.iter().enumerate() {
        let mut id_of: Vec<String> = vec![];
        let mut steps = vec![];
        for s in &ep.steps {
            let id = format!("s{counter}");
            id_of.push(id.clone());
            let deps = match s.prev_index.and_then(|p| id_of.get(p)) {
                Some(d) => vec![Value::Str(d.clone())],
                None => vec![],
            };
            let empty = || Value::Str(String::new());
            let mut kw = Object::new().with("id", id.as_str());
            let mut tag = s.typ.as_str();
            match tag {
                "shell" => {
                    kw.set("command", or2(s.command.clone(), empty()));
                }
                "file_read" => {
                    kw.set("path", or2(s.path.clone(), empty()));
                }
                "file_write" => {
                    kw.set("path", or2(s.path.clone(), empty()));
                    kw.set("content", "");
                }
                "network" => {
                    kw.set("url", network_url(s)?);
                }
                "task" => {
                    kw.set("prompt", or2(s.prompt.clone(), empty()));
                }
                "skill" => {
                    kw.set("skill_id", or2(s.skill_id.clone(), empty()));
                    kw.set("skill_input", s.skill_input.clone().unwrap_or_default());
                }
                "mcp" => {
                    kw.set("server", s.server.as_str());
                    kw.set("tool", s.tool.as_str());
                    kw.set("arguments", s.arguments.clone().unwrap_or_default());
                }
                _ => {
                    tag = "shell";
                    kw.set("command", "");
                }
            }
            kw.set("depends_on", Value::List(deps.clone()));
            build(tag, &kw)?;
            let mut o = Object::new()
                .with("id", id.as_str())
                .with("depends_on", Value::List(deps))
                .with("metadata", Object::new())
                .with("type", tag);
            for (k, v) in kw.iter() {
                if k != "id" && k != "depends_on" {
                    o.set(k, v.clone());
                }
            }
            if tag == "network" {
                o.set("method", "GET");
                o.set("headers", Object::new());
            }
            steps.push(json_mode(Value::Obj(o)));
            counter += 1;
        }
        let mut rng = Object::new()
            .with("first_message", Value::Int(ep.first_message.to_string()))
            .with("last_message", Value::Int(ep.last_message.to_string()));
        if let Some((a, b)) = &ep.step_range {
            rng.set("step_start", a.clone());
            rng.set("step_end", b.clone());
        }
        let ep_id = format!("ep_{i:02}");
        let kw = Object::new()
            .with("id", ep_id.as_str())
            .with("task", ep.task.clone())
            .with("steps", Value::List(vec![]))
            .with("source_range", rng.clone());
        if let Err(e) = pmodel::validate_model(Id::Episode, &Value::Obj(kw), Mode::Python) {
            return Err(Fail::Parse(e.text()));
        }
        out.push(Value::Obj(
            Object::new()
                .with("id", ep_id)
                .with("task", ep.task.clone())
                .with("steps", Value::List(steps))
                .with("source_range", rng),
        ));
    }
    Ok(out)
}

/// `ParseResult(**raw)` on a mapping: the validated dump, or pydantic's
/// error.
pub fn validate(raw: &Object) -> Result<Value, ValidationError> {
    pmodel::validate_model(Id::ParseResult, &Value::Obj(raw.clone()), Mode::Python)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lines_split_as_python_text_mode_does() {
        assert_eq!(lines("a\nb\r\nc\rd"), vec!["a\n", "b\r\n", "c\r", "d"]);
        assert_eq!(lines(""), Vec::<&str>::new());
    }

    #[test]
    fn a_task_is_cleaned_as_the_oracle_cleans_it() {
        assert_eq!(clean_task("  hi <system-reminder>x\ny</system-reminder> there  "), "hi there");
        assert_eq!(clean_task("Base directory for this skill: /a/b/tdd/\nmore"), "skill: tdd");
        assert_eq!(clean_task("This session is being continued from"), "session continuation");
    }

    #[test]
    fn a_query_is_quoted_as_urllib_does() {
        assert_eq!(quote("a b/c?é"), "a%20b/c%3F%C3%A9");
    }

    fn parse_err(r: R<Vec<RawEpisode>>) -> String {
        match r {
            Err(Fail::Parse(m)) => m,
            Err(Fail::Unreadable(m)) => panic!("unreadable: {m}"),
            Ok(_) => panic!("no error"),
        }
    }

    #[test]
    fn an_int_past_the_digit_limit_raises_as_json_loads_does() {
        let big = "7".repeat(5000);
        let t = format!("{{\"type\": \"user\", \"message\": {{\"role\": \"user\", \"content\": \"t\"}}}}\n{{\"type\": \"summary\", \"n\": {big}}}\n");
        assert_eq!(
            parse_err(read(t.as_bytes(), "claude-code")),
            "Exceeds the limit (4300 digits) for integer string conversion: value has 5000 digits; use sys.set_int_max_str_digits() to increase the limit"
        );
        let args = format!("{{\\\"n\\\": {}}}", "1".repeat(4400));
        let c = format!(
            "{{\"type\": \"event_msg\", \"payload\": {{\"type\": \"user_message\", \"message\": \"t\"}}}}\n{{\"type\": \"response_item\", \"payload\": {{\"type\": \"function_call\", \"name\": \"shell\", \"arguments\": \"{args}\"}}}}\n"
        );
        assert!(parse_err(read(c.as_bytes(), "codex")).contains("value has 4400 digits"));
    }

    #[test]
    fn bytes_that_are_not_utf8_raise_after_the_lines_before_them() {
        let t = b"{\"type\": \"user\", \"message\": {\"role\": \"user\", \"content\": \"t\"}}\n\xff\n";
        assert_eq!(
            parse_err(read(t, "claude-code")),
            "'utf-8' codec can't decode byte 0xff in position 62: invalid start byte"
        );
    }

    #[test]
    fn nan_and_infinity_are_written_as_null() {
        let t = "{\"type\": \"user\", \"message\": {\"role\": \"user\", \"content\": \"t\"}}\n{\"type\": \"assistant\", \"message\": {\"role\": \"assistant\", \"content\": [{\"type\": \"tool_use\", \"name\": \"mcp__a__b\", \"input\": {\"x\": NaN, \"y\": [Infinity]}}]}}\n";
        let eps = read(t.as_bytes(), "claude-code").unwrap();
        let out = finalize(&eps).unwrap();
        let dumped = crate::gate::pyjson::dumps(&out[0], true);
        assert!(dumped.contains("\"arguments\": {\"x\": null, \"y\": [null]}"), "{dumped}");
    }

    #[test]
    fn a_split_task_that_is_not_a_str_is_pydantics_error() {
        let t = "{\"type\": \"user\", \"message\": {\"role\": \"user\", \"content\": \"t\"}}\n{\"type\": \"assistant\", \"message\": {\"role\": \"assistant\", \"content\": [{\"type\": \"tool_use\", \"name\": \"Read\", \"input\": {\"file_path\": \"/a\"}}, {\"type\": \"tool_use\", \"name\": \"Read\", \"input\": {\"file_path\": \"/b\"}}]}}\n";
        let eps = read(t.as_bytes(), "claude-code").unwrap();
        for (task, shown, typ) in [(Value::Int("5".into()), "5", "int"), (Value::Null, "None", "NoneType")] {
            let sub = Value::List(vec![Value::Obj(
                Object::new()
                    .with("start_index", Value::Int("0".into()))
                    .with("end_index", Value::Int("1".into()))
                    .with("task", task.clone()),
            )]);
            let split = split_large(eps.clone(), 1, &mut |_| Ok(Some(sub.clone()))).unwrap();
            match finalize(&split) {
                Err(Fail::Parse(m)) => assert_eq!(
                    m,
                    format!(
                        "1 validation error for Episode\ntask\n  Input should be a valid string [type=string_type, input_value={shown}, input_type={typ}]\n    For further information visit https://errors.pydantic.dev/2.13/v/string_type"
                    )
                ),
                _ => panic!("no parse error"),
            }
        }
    }
}
