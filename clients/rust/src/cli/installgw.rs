//! The BASE_URL layer of install (ADR-0013): each harness pointed at the
//! local token-saving gateway, and the reverse. The Go client's
//! `install/gateway.go` is the reference.

use super::gateroot::join;
use super::install::{dump_settings, fail, is_dir, plan, plan_apply, read_json, Change, Edit, InstErr, ReadState, Runtime, Step, R};
use crate::gate::py::text::{is_space, repr};
use crate::gate::pyjson::{loads, py_type_name, LoadError, Object, Value};

/// DEFAULT_GATEWAY_BASE_URL.
pub const DEFAULT_BASE_URL: &str = "http://127.0.0.1:8787";

/// The gateway gap Hermes reports.
pub const HERMES_GAP: &str = "Not wired: gateway speaks Anthropic Messages; this harness's custom endpoint expects OpenAI \
                              \u{2014} not wired (needs an OpenAI-wire adapter)";

/// `Runtime.plan(home, layers)` for the gate and base_url layers: the
/// supported steps in layer order, then the gap notes.
pub fn plan_steps(rt: &Runtime, home: &str, gate: bool, gateway: bool, enforce: bool, ask: bool, url: &str) -> Vec<Step> {
    let (mut steps, mut gaps) = (vec![], vec![]);
    if gate {
        for s in plan(rt, home, enforce, ask) {
            if s.supported {
                steps.push(s);
            } else {
                gaps.push(s);
            }
        }
    }
    if gateway {
        let step = |description: String, target: String| Step { layer: "base_url", description, target, supported: true };
        match rt.key {
            "claude" => steps.push(step(format!("Point ANTHROPIC_BASE_URL at {url}"), join(home, ".claude/settings.json"))),
            "codex" => steps.push(step(
                format!("Register gateway model provider ({url}/v1, wire_api=chat) and select it"),
                join(home, ".codex/config.toml"),
            )),
            "openclaw" => steps.push(step(
                format!("Register opendaisugi gateway provider (anthropic-messages) at {url} \u{2014} select it as the active provider manually"),
                join(home, ".openclaw/openclaw.json"),
            )),
            "hermes" => gaps.push(Step { layer: "base_url", description: HERMES_GAP.into(), target: String::new(), supported: false }),
            _ => {}
        }
    }
    steps.extend(gaps);
    steps
}

/// `plan_apply` with the base_url layer after the gate layer. A base_url
/// edit Python's apply raises on fails the runtime after the gate edit
/// before it was written: `partial` keeps that edit.
#[allow(clippy::too_many_arguments)]
pub fn plan_apply_gateway(
    rt: &Runtime,
    home: &str,
    self_path: &str,
    root: &str,
    gate: bool,
    gateway: bool,
    enforce: bool,
    ask: bool,
    url: &str,
) -> R<Change> {
    let mut ch = Change::new(rt);
    if gate {
        ch = plan_apply(rt, home, self_path, root, enforce, ask)?;
        if ch.failed {
            return Ok(ch);
        }
    } else {
        match rt.key {
            "codex" => ch.mkdirs = vec![join(home, ".codex")],
            "hermes" => ch.mkdirs = vec![join(home, ".hermes")],
            "openclaw" => ch.mkdirs = vec![join(home, ".openclaw/workspace")],
            _ => {}
        }
    }
    if !gateway {
        return Ok(ch);
    }
    let prior = ch.edits.last().cloned();
    let res = match rt.key {
        "claude" => {
            let p = join(home, ".claude/settings.json");
            let prior = prior.filter(|e| e.path == p && e.warning.is_empty());
            match plan_claude_base_url(&p, url, prior.as_ref()) {
                Ok(Some(e)) if e.warning.is_empty() && !is_dir(&join(home, ".claude")) => {
                    Err(InstErr::Fail(format!("[Errno 2] No such file or directory: {}", repr(&p))))
                }
                r => r,
            }
        }
        "codex" => plan_codex_base_url(&join(home, ".codex/config.toml"), url),
        "openclaw" => plan_openclaw_base_url(&join(home, ".openclaw/openclaw.json"), url),
        _ => Ok(None),
    };
    match res {
        Err(InstErr::Fail(why)) => {
            ch.failed = true;
            ch.why = why;
            ch.partial = true;
        }
        Err(e) => return Err(e),
        Ok(Some(e)) => ch.edits.push(e),
        Ok(None) => {}
    }
    Ok(ch)
}

fn load_or_unsupported(text: &str) -> Result<Value, bool> {
    match loads(text) {
        Ok(v) => Ok(v),
        Err(LoadError::Syntax(_)) => Err(false),
        Err(_) => Err(true),
    }
}

/// The content an edit is planned on: an earlier edit of the same file in
/// this run, or the file.
fn json_of(p: &str, prior: Option<&Edit>) -> R<(Value, ReadState)> {
    if let Some(e) = prior.filter(|e| !e.content.is_empty()) {
        return match loads(&e.content) {
            Ok(v) => Ok((v, ReadState::Ok)),
            Err(_) => Err(InstErr::Unsupported),
        };
    }
    read_json(p)
}

/// `_patch_claude_base_url`.
pub fn plan_claude_base_url(settings_path: &str, url: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (mut v, st) = json_of(settings_path, prior)?;
    if st == ReadState::Bad {
        return Ok(Some(Edit {
            path: settings_path.into(),
            warning: format!(
                "{settings_path} is not valid JSON; skipping ANTHROPIC_BASE_URL to avoid overwriting your Claude Code \
                 settings (permissions/env). Fix the file and re-run `daisugi install`."
            ),
            ..Edit::default()
        }));
    }
    let existed = st != ReadState::Missing;
    if !existed {
        v = Value::Obj(Object::new());
    }
    let Value::Obj(s) = &mut v else {
        return fail(format!("'{}' object has no attribute 'setdefault'", py_type_name(&v)));
    };
    if s.get("env").is_none() {
        s.set("env", Object::new());
    }
    let Some(Value::Obj(env)) = s.get_mut("env") else {
        return fail(format!("'{}' object has no attribute 'get'", py_type_name(s.value("env"))));
    };
    if env.get("ANTHROPIC_BASE_URL").and_then(|c| c.as_str()) == Some(url) {
        return Ok(None);
    }
    env.set("ANTHROPIC_BASE_URL", url);
    Ok(Some(Edit { path: settings_path.into(), existed, content: dump_settings(&v), ..Edit::default() }))
}

const CODEX_SELECT: &str = "model_provider = \"opendaisugi\"";

fn codex_block(url: &str) -> String {
    format!("\n[model_providers.opendaisugi]\nname = \"openDaisugi gateway\"\nbase_url = \"{url}/v1\"\nwire_api = \"chat\"\n")
}

/// `path.read_text()`: "" and false when the file is absent; a file that
/// is not UTF-8 fails the runtime.
fn read_text(p: &str) -> R<(String, bool)> {
    if std::fs::metadata(p).is_err() {
        return Ok((String::new(), false));
    }
    let raw = match std::fs::read(p) {
        Ok(r) => r,
        Err(e) => return fail(e.to_string()),
    };
    match String::from_utf8(raw) {
        Ok(t) => Ok((t, true)),
        Err(_) => fail(format!("{p} is not valid UTF-8")),
    }
}

/// `_patch_codex_base_url`: the provider table appended and the selector
/// prepended, both once.
pub fn plan_codex_base_url(p: &str, url: &str) -> R<Option<Edit>> {
    let (mut text, existed) = read_text(p)?;
    if text.contains("[model_providers.opendaisugi]") {
        return Ok(None);
    }
    if !text.contains(CODEX_SELECT) {
        text = format!("{CODEX_SELECT}\n{text}");
    }
    let content = format!("{}\n{}", text.trim_end_matches('\n'), codex_block(url));
    Ok(Some(Edit { path: p.into(), existed, content, ..Edit::default() }))
}

/// `_strip_json5_comments`: // and /* */ comments outside strings, then a
/// comma before a closing brace or bracket (anywhere, as the regex does).
pub fn strip_json5(text: &str) -> String {
    let rs: Vec<char> = text.chars().collect();
    let n = rs.len();
    let find = |sub: &[char], from: usize| -> Option<usize> {
        if from > n || sub.len() > n {
            return None;
        }
        (from..=n - sub.len()).find(|&i| rs[i..i + sub.len()] == *sub)
    };
    let mut out: Vec<char> = Vec::with_capacity(n);
    let mut in_str = false;
    let mut i = 0;
    while i < n {
        let c = rs[i];
        if in_str {
            out.push(c);
            if c == '\\' && i + 1 < n {
                out.push(rs[i + 1]);
                i += 2;
                continue;
            }
            if c == '"' {
                in_str = false;
            }
            i += 1;
            continue;
        }
        if c == '"' {
            in_str = true;
            out.push(c);
            i += 1;
            continue;
        }
        if c == '/' && i + 1 < n && rs[i + 1] == '/' {
            i = find(&['\n'], i).unwrap_or(n);
            continue;
        }
        if c == '/' && i + 1 < n && rs[i + 1] == '*' {
            i = find(&['*', '/'], i + 2).map(|j| j + 2).unwrap_or(n);
            continue;
        }
        out.push(c);
        i += 1;
    }
    // re.sub(r",(\s*[}\]])", r"\1", ...)
    let mut res = String::with_capacity(out.len());
    for (k, &c) in out.iter().enumerate() {
        if c == ',' {
            let mut j = k + 1;
            while j < out.len() && is_space(out[j]) {
                j += 1;
            }
            if j < out.len() && (out[j] == '}' || out[j] == ']') {
                continue;
            }
        }
        res.push(c);
    }
    res
}

/// Walks `keys` with setdefault({}) on each dict.
fn setdefault_path<'a>(v: &'a mut Value, keys: &[&str]) -> R<&'a mut Value> {
    let mut leaf = v;
    for k in keys {
        let Value::Obj(o) = leaf else {
            return fail(format!("'{}' object has no attribute 'setdefault'", py_type_name(leaf)));
        };
        if o.get(k).is_none() {
            o.set(k, Object::new());
        }
        leaf = o.get_mut(k).expect("just set");
    }
    Ok(leaf)
}

/// `_patch_openclaw_base_url`: `_patch_mcp` with the provider block under
/// models.providers.opendaisugi.
pub fn plan_openclaw_base_url(p: &str, url: &str) -> R<Option<Edit>> {
    let warn_bad = format!(
        "{p} is not valid; skipping gateway provider registration to avoid overwriting user state. Fix the file and \
         re-run `daisugi install`."
    );
    let (text, existed) = read_text(p)?;
    let mut cfg = Value::Obj(Object::new());
    if existed {
        cfg = match load_or_unsupported(&text) {
            Ok(v) => v,
            Err(true) => return Err(InstErr::Unsupported),
            Err(false) => match load_or_unsupported(&strip_json5(&text)) {
                Ok(v) => v,
                Err(true) => return Err(InstErr::Unsupported),
                Err(false) => return Ok(Some(Edit { path: p.into(), warning: warn_bad, ..Edit::default() })),
            },
        };
    }
    let leaf = setdefault_path(&mut cfg, &["models", "providers"])?;
    let Value::Obj(lo) = leaf else {
        // `"opendaisugi" in leaf` then leaf[...] = entry: only a dict takes it.
        return fail(format!("'{}' object does not support item assignment", py_type_name(leaf)));
    };
    if lo.get("opendaisugi").is_some() {
        return Ok(None);
    }
    let mut entry = Object::new();
    entry.set("baseUrl", url);
    entry.set("api", "anthropic-messages");
    lo.set("opendaisugi", entry);
    let mut e = Edit { path: p.into(), existed, content: dump_settings(&cfg), ..Edit::default() };
    if existed && (text.contains("//") || text.contains("/*")) {
        e.pre_warning = format!(
            "{p} contains JSON5 comments which will not survive the rewrite (the writer emits plain JSON). The \
             pre-write backup at {p}.bak* preserves the original text \u{2014} restore from it if you need the comments \
             back. Tracked as M7 in REVIEW_FINDINGS.md."
        );
    }
    Ok(Some(e))
}

/// `_pop_json_env_key`.
pub fn plan_pop_env_key(p: &str, key: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (mut v, st) = json_of(p, prior)?;
    if st != ReadState::Ok {
        return Ok(None);
    }
    let Value::Obj(s) = &mut v else { return Err(InstErr::Unsupported) };
    let Some(Value::Obj(env)) = s.get_mut("env") else { return Ok(None) };
    if env.get(key).is_none() {
        return Ok(None);
    }
    env.remove(key);
    if env.is_empty() {
        s.remove("env");
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: dump_settings(&v), ..Edit::default() }))
}

/// `_unpatch_codex_base_url`.
pub fn plan_codex_unpatch(p: &str) -> R<Option<Edit>> {
    let (mut text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    if !existed || (!text.contains("[model_providers.opendaisugi]") && !text.contains(CODEX_SELECT)) {
        return Ok(None);
    }
    let re = regex::Regex::new(r"\n?\[model_providers\.opendaisugi\][^\[]*").expect("a fixed pattern");
    if let Some(m) = re.find(&text) {
        text = format!("{}\n{}", &text[..m.start()], &text[m.end()..]);
    }
    text = text.replacen(&format!("{CODEX_SELECT}\n"), "", 1);
    let mut cleaned = text.trim_matches('\n').to_string();
    if !cleaned.is_empty() {
        cleaned.push('\n');
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: cleaned, ..Edit::default() }))
}

/// The provider half of OpenClaw's reverse.
pub fn plan_openclaw_unprovider(p: &str) -> R<Option<Edit>> {
    let (text, existed) = read_text(p).map_err(|_| InstErr::Unsupported)?;
    if !existed {
        return Ok(None);
    }
    let mut v = match load_or_unsupported(&text) {
        Ok(v) => v,
        Err(_) => match load_or_unsupported(&strip_json5(&text)) {
            Ok(v) => v,
            Err(true) => return Err(InstErr::Unsupported),
            Err(false) => return Ok(None),
        },
    };
    let Value::Obj(cfg) = &mut v else { return Ok(None) };
    let Some(models) = cfg.get_mut("models") else { return Ok(None) };
    let Value::Obj(mo) = models else { return Err(InstErr::Unsupported) };
    let Some(provs) = mo.get_mut("providers") else { return Ok(None) };
    let Value::Obj(po) = provs else { return Err(InstErr::Unsupported) };
    if po.get("opendaisugi").is_none() {
        return Ok(None);
    }
    po.remove("opendaisugi");
    if po.is_empty() {
        mo.remove("providers");
        if mo.is_empty() {
            cfg.remove("models");
        }
    }
    Ok(Some(Edit { path: p.into(), existed: true, content: dump_settings(&v), ..Edit::default() }))
}

/// The base_url half of `Runtime.reverse`, after the gate half's edits of
/// the same files.
pub fn plan_reverse_gateway(rt: &Runtime, home: &str, ch: &mut Change) -> R<()> {
    let e = match rt.key {
        "claude" => {
            let p = join(home, ".claude/settings.json");
            let prior = ch.edits.iter().rev().find(|x| x.path == p).cloned();
            plan_pop_env_key(&p, "ANTHROPIC_BASE_URL", prior.as_ref())?
        }
        "codex" => plan_codex_unpatch(&join(home, ".codex/config.toml"))?,
        "openclaw" => plan_openclaw_unprovider(&join(home, ".openclaw/openclaw.json"))?,
        _ => None,
    };
    if let Some(e) = e {
        ch.edits.push(e);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json5_comments_and_trailing_commas_go() {
        assert_eq!(strip_json5("{\"a\": \"//x\", // c\n\"b\": [1, /* d */ 2,\n]\n,}"), "{\"a\": \"//x\", \n\"b\": [1,  2\n]\n}");
    }
}
