//! The BASE_URL layer of install (ADR-0013): each harness pointed at the
//! local token-saving gateway, and the reverse. The Go client's
//! `install/gateway.go` is the reference.

use super::gateroot::join;
use super::install::{dump_settings, fail, plan, read_json, Edit, InstErr, ReadState, Runtime, Step, R};
use crate::gate::py::text::is_space;
use crate::gate::pyjson::{loads, py_type_name, Object, Value};

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



/// The content an edit is planned on: an earlier edit of the same file in
/// this run, or the file.
pub(super) fn json_of(p: &str, prior: Option<&Edit>) -> R<(Value, ReadState)> {
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
pub(super) fn read_text(p: &str) -> R<(String, bool)> {
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


/// `plan_codex_base_url` on an earlier edit's content.
pub fn plan_codex_base_url_on(p: &str, url: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (mut text, existed) = match prior {
        Some(e) => (e.content.clone(), true),
        None => read_text(p)?,
    };
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


/// `plan_codex_unpatch` on an earlier edit's content.
pub fn plan_codex_unpatch_on(p: &str, prior: Option<&Edit>) -> R<Option<Edit>> {
    let (mut text, existed) = match prior {
        Some(e) => (e.content.clone(), true),
        None => read_text(p).map_err(|_| InstErr::Unsupported)?,
    };
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



#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json5_comments_and_trailing_commas_go() {
        assert_eq!(strip_json5("{\"a\": \"//x\", // c\n\"b\": [1, /* d */ 2,\n]\n,}"), "{\"a\": \"//x\", \n\"b\": [1,  2\n]\n}");
    }
}
