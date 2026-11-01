//! `install.COMPONENTS`: the names `daisugi install --only` takes. Each is
//! an install layer under its user-facing name.

/// One component: name, layer, what it writes, the harnesses that carry it.
pub struct Component {
    pub name: &'static str,
    pub layer: &'static str,
    pub what: &'static str,
    pub where_: &'static str,
}

pub const COMPONENTS: &[Component] = &[
    Component { name: "skill", layer: "skill", what: "the opendaisugi-checklist skill directory", where_: "claude codex hermes openclaw" },
    Component { name: "mcp", layer: "mcp", what: "the opendaisugi MCP server entry", where_: "claude codex hermes openclaw" },
    Component { name: "capture", layer: "capture", what: "the passive capture hook (daisugi hook record)", where_: "claude hermes openclaw" },
    Component { name: "instructions", layer: "instructions", what: "the pathway note in the harness instructions file", where_: "claude codex hermes openclaw" },
    Component { name: "warden", layer: "warden", what: "the warden hook, audit mode; --enforce makes it deny", where_: "claude codex" },
    Component { name: "pando", layer: "pando", what: "the Pando memory: MCP server, chat hooks, Hermes plugin, follow service", where_: "claude codex hermes" },
    Component { name: "router", layer: "base_url", what: "the harness base_url pointed at the router", where_: "claude codex openclaw" },
];

pub const DEFAULT_COMPONENTS: &[&str] = &["skill", "mcp", "capture", "instructions", "warden", "pando"];

fn component_names() -> String {
    COMPONENTS.iter().map(|c| c.name).collect::<Vec<_>>().join(", ")
}

/// `install.component_layers`: the layers the names select. Each name may
/// hold several, split on commas.
pub fn component_layers(names: &[String]) -> Result<Vec<String>, String> {
    let mut out: Vec<String> = vec![];
    for raw in names {
        for tok in raw.split(',') {
            let name = crate::warden::pystr::py_strip(tok);
            match COMPONENTS.iter().find(|c| c.name == name) {
                Some(c) => {
                    if !out.iter().any(|l| l == c.layer) {
                        out.push(c.layer.to_string());
                    }
                }
                None => {
                    return Err(format!(
                        "unknown component '{name}'. Components: {}. Run: daisugi install --list",
                        component_names()
                    ))
                }
            }
        }
    }
    Ok(out)
}

/// `install.component_names`: the names of a set of layers, in COMPONENTS order.
pub fn names_of(layers: &[String]) -> Vec<&'static str> {
    COMPONENTS.iter().filter(|c| layers.iter().any(|l| l == c.layer)).map(|c| c.name).collect()
}

/// `install.list_components`: the table `install --list` prints (no
/// trailing newline).
pub fn list_components() -> String {
    let mut lines: Vec<String> = COMPONENTS.iter().map(|c| format!("{:<13}{} ({})", c.name, c.what, c.where_)).collect();
    lines.push(String::new());
    lines.push(format!("Default: {}. Add router with --router or --only router.", DEFAULT_COMPONENTS.join(", ")));
    lines.push("The pi and OpenCode extensions are separate: daisugi install --harness pi|opencode.".to_string());
    lines.join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_unknown_component_names_the_list() {
        let err = component_layers(&["warden,journal".to_string()]).unwrap_err();
        assert_eq!(
            err,
            "unknown component 'journal'. Components: skill, mcp, capture, instructions, warden, pando, router. Run: daisugi install --list"
        );
    }

    #[test]
    fn names_split_on_commas_and_fold() {
        let got = component_layers(&["warden,capture".to_string(), "warden".to_string()]).unwrap();
        assert_eq!(got, vec!["warden".to_string(), "capture".to_string()]);
    }

    #[test]
    fn the_list_matches_the_oracles_text() {
        let t = list_components();
        for s in ["skill        ", "router       ", "Default: skill, mcp, capture, instructions, warden, pando.", "--harness pi|opencode"] {
            assert!(t.contains(s), "missing {s:?}");
        }
    }
}
