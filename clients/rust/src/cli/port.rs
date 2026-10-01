//! `DAISUGI_PORT` (ruling PK-R-15): a package installs the three ports
//! beside each other as `daisugi` (Go), `daisugi-rs` and `daisugi-py`, and
//! `install` in any of them hands the whole command to the one the
//! variable names, once, at install time. The hook it writes then runs
//! that port's binary directly, so the gate's hot path has no extra exec.

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;

pub const PORT_ENV: &str = "DAISUGI_PORT";
/// Set on the one hand-over, so a binary that turns out to be another
/// port stops instead of handing over again.
pub const PORT_HOP_ENV: &str = "DAISUGI_PORT_HOP";

/// The file name each port's daisugi has beside the others.
pub fn port_binary(port: &str) -> Option<&'static str> {
    match port {
        "go" => Some("daisugi"),
        "rust" => Some("daisugi-rs"),
        "python" => Some("daisugi-py"),
        _ => None,
    }
}

/// Where `install` runs: None for this binary (`own` is its port, `self_path`
/// its path), or the path of the sibling binary `DAISUGI_PORT` names. The
/// error is one line, said with exit 2; nothing is changed.
pub fn port_hop(
    own: &str,
    self_path: &str,
    env: &HashMap<String, String>,
) -> Result<Option<String>, String> {
    let want = env.get(PORT_ENV).map(String::as_str).unwrap_or("");
    if want.is_empty() || want == own {
        return Ok(None);
    }
    let Some(name) = port_binary(want) else {
        return Err(format!(
            "{PORT_ENV} must be go, rust or python, not {want}. Nothing was changed."
        ));
    };
    if env.get(PORT_HOP_ENV).is_some_and(|v| !v.is_empty()) {
        return Err(format!(
            "{PORT_ENV} is {want}, but the daisugi it ran is the {own} port. Nothing was changed."
        ));
    }
    let dir = std::path::Path::new(self_path)
        .parent()
        .map(|p| p.to_string_lossy().into_owned())
        .unwrap_or_default();
    let target = format!("{dir}/{name}");
    let ok = std::fs::metadata(&target)
        .map(|m| !m.is_dir() && m.permissions().mode() & 0o111 != 0)
        .unwrap_or(false);
    if !ok {
        return Err(format!(
            "{PORT_ENV} is {want}, but there is no {name} beside {self_path}. Install it, or unset {PORT_ENV}. Nothing was changed."
        ));
    }
    Ok(Some(target))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn hands_over_to_the_named_port() {
        // Under target/, on disk: /tmp is RAM on some boxes.
        let dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(format!("target/daisugi-port-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let d = dir.to_string_lossy().into_owned();
        for n in ["daisugi", "daisugi-rs"] {
            std::fs::write(dir.join(n), "#!/bin/sh\n").unwrap();
            std::fs::set_permissions(dir.join(n), std::fs::Permissions::from_mode(0o755)).unwrap();
        }
        std::fs::write(dir.join("daisugi-py"), "").unwrap();
        let me = format!("{d}/daisugi-rs");
        assert_eq!(port_hop("rust", &me, &env(&[])), Ok(None));
        assert_eq!(
            port_hop("rust", &me, &env(&[("DAISUGI_PORT", "rust")])),
            Ok(None)
        );
        assert_eq!(
            port_hop(
                "rust",
                &me,
                &env(&[("DAISUGI_PORT", "rust"), ("DAISUGI_PORT_HOP", "1")])
            ),
            Ok(None)
        );
        assert_eq!(
            port_hop("rust", &me, &env(&[("DAISUGI_PORT", "go")])),
            Ok(Some(format!("{d}/daisugi")))
        );
        assert_eq!(
            port_hop("rust", &me, &env(&[("DAISUGI_PORT", "python")])),
            Err(format!("DAISUGI_PORT is python, but there is no daisugi-py beside {me}. Install it, or unset DAISUGI_PORT. Nothing was changed."))
        );
        assert_eq!(
            port_hop("rust", &me, &env(&[("DAISUGI_PORT", "zig")])),
            Err(
                "DAISUGI_PORT must be go, rust or python, not zig. Nothing was changed."
                    .to_string()
            )
        );
        assert_eq!(
            port_hop(
                "rust",
                &me,
                &env(&[("DAISUGI_PORT", "go"), ("DAISUGI_PORT_HOP", "1")])
            ),
            Err(
                "DAISUGI_PORT is go, but the daisugi it ran is the rust port. Nothing was changed."
                    .to_string()
            )
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
