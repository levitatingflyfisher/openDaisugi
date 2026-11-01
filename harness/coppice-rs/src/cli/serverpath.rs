//! `coppice server install` and the PATH warning of `server start`. The Go
//! `internal/cli/serverpath.go` is the reference.

use super::{errln, out, Cli};
use crate::{config, sys};
use std::path::PathBuf;

/// The lines a starting server prints when it cannot find daisugi or the
/// default harness on its own PATH. A server started over ssh, or by a
/// service manager, often has PATH=/usr/local/bin:/usr/bin:/bin and finds
/// neither, and a later `coppice open` says only that it found no default
/// harness. Each line names the PATH the server has.
pub fn path_warnings(look: &dyn Fn(&str) -> bool, path_env: &str, daisugi: &str, harness: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut miss = |what: &str, name: &str| {
        out.push(format!(
            "{what} {} is not on this server's PATH ({path_env}). Start the server from a shell that has it, or run: coppice server install",
            sys::go_quote(name)
        ));
    };
    if !look(daisugi) {
        miss("daisugi", daisugi);
    }
    if !harness.is_empty() && !look(harness) {
        miss("the default harness", harness);
    }
    out
}

/// The systemd user unit for the server: the PATH it was installed from
/// (with ~/.local/bin added), so the server finds daisugi and the harnesses
/// in a session no login shell started.
///
/// systemd reads % in a unit as a specifier, so % is written as %%. It also
/// expands $VAR in ExecStart, so $ is written as $$ there. A PATH
/// holding a double quote or a newline cannot sit inside Environment="...",
/// so the unit is refused.
pub fn server_unit(exe: &str, path_env: &str, home: &str, args: &[String]) -> Result<String, String> {
    if path_env.contains(['"', '\n', '\r']) {
        return Err("PATH holds a double quote or a newline, which a systemd unit cannot carry; fix PATH and run coppice server install again".into());
    }
    let mut parts: Vec<String> = path_env.split(':').map(String::from).collect();
    let local = format!("{}/.local/bin", home.trim_end_matches('/'));
    if !parts.contains(&local) {
        parts.insert(0, local);
    }
    let mut argv: Vec<String> = vec![exe.to_string()];
    argv.extend(args.iter().cloned());
    argv.extend(["server".into(), "start".into(), "--foreground".into()]);
    for a in argv.iter_mut() {
        let r = a.replace('%', "%%").replace('$', "$$");
        *a = if r.contains([' ', '\t', '"', '\'', '\\']) {
            format!("\"{}\"", r.replace('\\', "\\\\").replace('"', "\\\""))
        } else {
            r
        };
    }
    Ok(format!(
        "[Unit]\nDescription=coppice server\n\n[Service]\nEnvironment=\"PATH={}\"\nExecStart={}\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n",
        parts.join(":").replace('%', "%%"),
        argv.join(" ")
    ))
}

impl Cli {
    /// Writes ~/.config/systemd/user/coppice-server.service (under
    /// XDG_CONFIG_HOME) and says how to start it. It runs no systemctl.
    pub(super) fn server_install(&self) -> i32 {
        let exe = match std::env::current_exe() {
            Ok(e) => std::fs::canonicalize(&e).unwrap_or(e),
            Err(e) => {
                errln(&format!("cannot find the coppice binary: {e}"));
                return 1;
            }
        };
        let Some(home) = sys::home_dir() else {
            errln("cannot find the home dir: $HOME is not defined");
            return 1;
        };
        let cfg = match std::env::var("XDG_CONFIG_HOME") {
            Ok(c) if !c.is_empty() => PathBuf::from(c),
            _ => home.join(".config"),
        };
        let dir = cfg.join("systemd").join("user");
        if let Err(e) = std::fs::create_dir_all(&dir) {
            errln(&sys::go_path_err("mkdir", &dir, &e));
            return 1;
        }
        let mut args: Vec<String> = Vec::new();
        if !self.socket.as_os_str().is_empty() {
            args.extend(["--socket".into(), self.socket.to_string_lossy().into_owned()]);
        }
        if !self.data_dir.as_os_str().is_empty() {
            args.extend(["--data-dir".into(), self.data_dir.to_string_lossy().into_owned()]);
        }
        let path = dir.join("coppice-server.service");
        let unit = match server_unit(
            &exe.to_string_lossy(),
            &std::env::var("PATH").unwrap_or_default(),
            &home.to_string_lossy(),
            &args,
        ) {
            Ok(u) => u,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        if let Err(e) = std::fs::write(&path, unit) {
            errln(&sys::go_path_err("open", &path, &e));
            return 1;
        }
        out(&format!(
            "wrote {}\nStart it: systemctl --user daemon-reload && systemctl --user enable --now coppice-server\n",
            path.display()
        ));
        0
    }

    /// Prints the path_warnings of this process, once, to stderr (the
    /// server log, for a detached server).
    pub(super) fn warn_path(&self) {
        let mut harness = String::new();
        if let Ok(Some(cfg)) = config::load() {
            if !cfg.default.is_empty() {
                harness = cfg.harness.get(&cfg.default).map(|h| h.command.clone()).unwrap_or_default();
                if harness.is_empty() {
                    harness = cfg.default.clone();
                }
            }
        }
        let bin = std::env::var("COPPICE_DAISUGI_BIN").ok().filter(|b| !b.is_empty()).unwrap_or_else(|| "daisugi".into());
        let look = |n: &str| crate::pane::look_path(n).is_some();
        for w in path_warnings(&look, &std::env::var("PATH").unwrap_or_default(), &bin, &harness) {
            errln(&format!("warning: {w}"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_server_without_daisugi_or_the_harness_on_path_warns_and_names_the_path() {
        let got = path_warnings(&|_| false, "/usr/bin:/bin", "daisugi", "claude");
        assert_eq!(got.len(), 2);
        for w in &got {
            assert!(w.contains("/usr/bin:/bin") && w.contains("coppice server install"), "{w}");
        }
        assert!(path_warnings(&|_| true, "/usr/bin", "daisugi", "claude").is_empty());
    }

    #[test]
    fn the_server_unit_carries_the_path_with_local_bin_and_escapes_percent() {
        let u = server_unit("/opt/coppice", "/usr/bin:/bin", "/home/u", &["--socket".into(), "/run/s k".into()]).unwrap();
        for want in [
            "Environment=\"PATH=/home/u/.local/bin:/usr/bin:/bin\"",
            "ExecStart=/opt/coppice --socket \"/run/s k\" server start --foreground",
            "Restart=on-failure",
            "WantedBy=default.target",
        ] {
            assert!(u.contains(want), "{want} in {u}");
        }
        let u2 = server_unit("/c", "/home/u/.local/bin:/bin", "/home/u", &[]).unwrap();
        assert_eq!(u2.matches("/home/u/.local/bin").count(), 1);
        let u = server_unit("/opt/co%ppice", "/a%h/bin:/bin", "/home/u", &["--socket".into(), "/run/100%/s k".into()]).unwrap();
        assert!(u.contains("/a%%h/bin") && u.contains("/opt/co%%ppice") && u.contains("/run/100%%/s k"), "{u}");
        let u = server_unit("/opt/co$HOME", "/bin", "/home/u", &["--data-dir".into(), "/d/$X y".into()]).unwrap();
        assert!(u.contains("ExecStart=/opt/co$$HOME --data-dir \"/d/$$X y\" server"), "{u}");
        for bad in ["/a\"b:/bin", "/a\n/b"] {
            assert!(server_unit("/c", bad, "/home/u", &[]).unwrap_err().contains("PATH"));
        }
    }
}
