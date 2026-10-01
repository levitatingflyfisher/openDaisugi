//! The one table every verb's flags and shape come from, and the parser
//! that turns a verb's arguments into wire parameters.

use serde_json::{json, Map, Value};

use crate::sys;

#[derive(Clone, Copy, PartialEq)]
enum Kind {
    Str,
    Int,
    Bool(bool),
}

#[derive(Clone, Copy, PartialEq)]
enum Trailing {
    None,
    Join,
    List,
}

struct Spec {
    pane: bool,
    task: bool,
    flags: &'static [(&'static str, &'static str, Kind)],
    trailing: Trailing,
    trailing_key: &'static str,
    argv: bool,
}

const fn plain(flags: &'static [(&'static str, &'static str, Kind)]) -> Spec {
    Spec {
        pane: false,
        task: false,
        flags,
        trailing: Trailing::None,
        trailing_key: "",
        argv: false,
    }
}

const fn on_pane(flags: &'static [(&'static str, &'static str, Kind)]) -> Spec {
    Spec {
        pane: true,
        ..plain(flags)
    }
}

const fn on_task(flags: &'static [(&'static str, &'static str, Kind)]) -> Spec {
    Spec {
        task: true,
        ..plain(flags)
    }
}

const fn joined(mut s: Spec, key: &'static str) -> Spec {
    s.trailing = Trailing::Join;
    s.trailing_key = key;
    s
}

use Kind::*;

/// Each group's verbs.
fn specs(group: &str) -> Option<Vec<(&'static str, Spec)>> {
    Some(match group {
        "workspace" => vec![
            (
                "create",
                plain(&[("cwd", "cwd", Str), ("label", "label", Str)]),
            ),
            ("list", plain(&[])),
        ],
        "tab" => vec![
            (
                "create",
                plain(&[("label", "label", Str), ("workspace", "workspace", Str)]),
            ),
            ("list", plain(&[("workspace", "workspace", Str)])),
        ],
        "pane" => vec![
            (
                "create",
                Spec {
                    argv: true,
                    ..plain(&[
                        ("cwd", "cwd", Str),
                        ("label", "label", Str),
                        ("kind", "kind", Str),
                        ("harness", "harness", Str),
                        ("cols", "cols", Int),
                        ("rows", "rows", Int),
                        ("workspace", "workspace", Str),
                        ("tab", "tab", Str),
                        ("task", "task", Str),
                    ])
                },
            ),
            ("list", plain(&[("ended", "ended", Bool(true))])),
            (
                "send-text",
                joined(on_pane(&[("no-enter", "enter", Bool(false))]), "text"),
            ),
            (
                "send-keys",
                Spec {
                    trailing: Trailing::List,
                    trailing_key: "keys",
                    ..on_pane(&[])
                },
            ),
            ("trust", on_pane(&[("not-now", "trust", Bool(false))])),
            ("run", joined(on_pane(&[]), "line")),
            ("read", on_pane(&[("source", "source", Str)])),
            ("close", on_pane(&[])),
            (
                "forget",
                joined(plain(&[("ended", "ended", Bool(true))]), "pane"),
            ),
            ("resume", on_pane(&[])),
            (
                "wait-output",
                on_pane(&[
                    ("contains", "contains", Str),
                    ("state", "state", Str),
                    ("timeout", "timeout_ms", Int),
                ]),
            ),
            (
                "resize",
                on_pane(&[("cols", "cols", Int), ("rows", "rows", Int)]),
            ),
            ("explain", on_pane(&[])),
            ("fork", on_pane(&[("label", "label", Str)])),
            ("rename", joined(on_pane(&[]), "label")),
        ],
        "task" => vec![
            (
                "create",
                plain(&[
                    ("label", "label", Str),
                    ("parent", "parent", Str),
                    ("cwd", "cwd", Str),
                    ("worktree", "worktree", Bool(true)),
                    ("model", "model", Str),
                ]),
            ),
            ("list", plain(&[])),
            (
                "close",
                on_task(&[("keep-worktree", "keep_worktree", Bool(true))]),
            ),
            ("move", on_task(&[("parent", "parent", Str)])),
            ("set-foreman", on_task(&[("pane", "pane", Str)])),
        ],
        "agent" => vec![
            ("list", plain(&[])),
            ("get", on_pane(&[])),
            (
                "prompt",
                joined(
                    on_pane(&[
                        ("wait", "wait", Bool(true)),
                        ("until", "until", Str),
                        ("timeout", "timeout_ms", Int),
                    ]),
                    "text",
                ),
            ),
            (
                "wait",
                on_pane(&[("until", "until", Str), ("timeout", "timeout_ms", Int)]),
            ),
            ("read", on_pane(&[("region", "region", Str)])),
            (
                "allow",
                joined(
                    on_pane(&[
                        ("reason", "reason", Str),
                        ("confirm", "confirm", Str),
                        ("scope", "scope", Str),
                    ]),
                    "ask",
                ),
            ),
            ("deny", joined(on_pane(&[("reason", "reason", Str)]), "ask")),
        ],
        "floor" => vec![
            ("note", joined(plain(&[]), "text")),
            ("talk", joined(plain(&[]), "text")),
        ],
        "session" => vec![("list", plain(&[])), ("stop", on_pane(&[]))],
        "project" => vec![("list", plain(&[]))],
        _ => return None,
    })
}

/// Go's strconv.Atoi: an optional sign and decimal digits that fit an int.
pub fn atoi(s: &str) -> Option<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    s.parse::<i64>().ok()
}

/// Turns a verb's own arguments into wire parameters, and says whether
/// --json was given. Err is the one line to print.
pub fn parse(
    group: &str,
    verb: &str,
    rest: &[String],
) -> Result<(Map<String, Value>, bool), String> {
    let Some(table) = specs(group) else {
        return Err(format!("unknown command group {}", sys::go_quote(group)));
    };
    let Some(spec) = table.iter().find(|(v, _)| *v == verb).map(|(_, s)| s) else {
        let mut names: Vec<&str> = table.iter().map(|(v, _)| *v).collect();
        names.sort();
        return Err(format!(
            "unknown {group} verb {}. Known verbs: {}",
            sys::go_quote(verb),
            names.join(", ")
        ));
    };
    let known_flags = || {
        let mut out: Vec<String> = spec.flags.iter().map(|f| format!("--{}", f.0)).collect();
        out.sort();
        out.push("--json".into());
        out.join(", ")
    };
    let mut params = Map::new();
    let mut as_json = false;
    let mut positional: Vec<String> = Vec::new();
    let mut cmd_argv: Option<Vec<String>> = None;
    let mut i = 0;
    while i < rest.len() {
        let a = &rest[i];
        if a == "--" {
            if !spec.argv {
                return Err(format!("{group} {verb} does not take a command after --"));
            }
            cmd_argv = Some(rest[i + 1..].to_vec());
            break;
        } else if a == "--json" {
            as_json = true;
        } else if let Some(name) = a.strip_prefix("--") {
            let Some(f) = spec.flags.iter().find(|f| f.0 == name) else {
                return Err(format!(
                    "unknown flag --{name} for {group} {verb}. Known flags: {}",
                    known_flags()
                ));
            };
            match f.2 {
                Bool(v) => {
                    params.insert(f.1.into(), json!(v));
                    if let Some(n) = rest.get(i + 1).and_then(|x| atoi(x)) {
                        if spec.flags.iter().any(|f| f.0 == "timeout") {
                            return Err(format!(
                                "--{name} takes no value. Did you mean --timeout {n}?"
                            ));
                        }
                        return Err(format!(
                            "--{name} takes no value, but {n} looks like one for {group} {verb}"
                        ));
                    }
                }
                Str | Int => {
                    let Some(val) = rest.get(i + 1) else {
                        return Err(format!("--{name} needs a value"));
                    };
                    i += 1;
                    if f.2 == Int {
                        let Some(n) = atoi(val) else {
                            return Err(format!(
                                "--{name} needs a number, got {}",
                                sys::go_quote(val)
                            ));
                        };
                        params.insert(f.1.into(), json!(n));
                    } else {
                        params.insert(f.1.into(), json!(val));
                    }
                }
            }
        } else {
            positional.push(a.clone());
        }
        i += 1;
    }
    if let Some(a) = cmd_argv {
        params.insert("cmd_argv".into(), json!(a));
    }
    let mut pos = positional.as_slice();
    if spec.pane {
        let Some(p) = pos.first() else {
            return Err(format!(
                "{group} {verb} needs a pane id. Run: coppice pane list"
            ));
        };
        params.insert("pane".into(), json!(p));
        pos = &pos[1..];
    }
    if spec.task {
        let Some(t) = pos.first() else {
            return Err(format!(
                "{group} {verb} needs a task id. Run: coppice task list"
            ));
        };
        params.insert("task".into(), json!(t));
        pos = &pos[1..];
    }
    match spec.trailing {
        Trailing::Join => {
            params.insert(spec.trailing_key.into(), json!(pos.join(" ")));
        }
        Trailing::List => {
            if !pos.is_empty() {
                params.insert(spec.trailing_key.into(), json!(pos));
            }
        }
        Trailing::None => {}
    }
    Ok((params, as_json))
}

/// "pane send-text" as the wire command "pane.send_text".
pub fn socket_command(group: &str, verb: &str) -> String {
    format!("{group}.{}", verb.replace('-', "_"))
}

/// The list a verb's reply carries, for a table.
pub fn plural_key(group: &str, verb: &str) -> &'static str {
    if verb != "list" {
        return "";
    }
    match group {
        "pane" | "session" => "panes",
        "agent" => "agents",
        "workspace" => "workspaces",
        "tab" => "tabs",
        "task" => "tasks",
        "project" => "projects",
        _ => "",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn args(a: &[&str]) -> Vec<String> {
        a.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn a_verb_parses_as_go_parses_it() {
        let (p, j) = parse(
            "pane",
            "send-text",
            &args(&["w1:p1", "a", "b", "--no-enter", "--json"]),
        )
        .unwrap();
        assert!(j);
        assert_eq!(
            serde_json::to_string(&p).unwrap(),
            r#"{"enter":false,"pane":"w1:p1","text":"a b"}"#
        );
        assert_eq!(
            parse("agent", "prompt", &args(&["w1:p1", "--wait", "5000"])).unwrap_err(),
            "--wait takes no value. Did you mean --timeout 5000?"
        );
        assert_eq!(
            parse("pane", "read", &args(&["p", "--x"])).unwrap_err(),
            "unknown flag --x for pane read. Known flags: --source, --json"
        );
        assert_eq!(atoi("+3"), Some(3));
        assert_eq!(atoi("3x"), None);
    }
}
