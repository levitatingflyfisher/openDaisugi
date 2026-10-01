//! `config.load_config` as the gate reads it: the YAML beside the gate
//! root, validated by pydantic's `Config` model, for its `gate_mode` and
//! `verifier_client`.
//!
//! The YAML is read by `crate::pyyaml`, as PyYAML's SafeLoader reads it.
//! A value its result model does not hold is not decided by the port.

use super::paths::{path_join, path_parent};
use super::{catch, undecided, Runner, R};

/// `gate.AGENT_MODE_MAX_BYTES`: a larger marker is not read at all.
const AGENT_MODE_MAX_BYTES: u64 = 64;

#[cfg(test)]
thread_local! {
    /// A test's stand-in for this process's euid, to name another owner.
    pub static TEST_EUID: std::cell::Cell<Option<u32>> = const { std::cell::Cell::new(None) };
}

fn euid() -> u32 {
    #[cfg(test)]
    if let Some(u) = TEST_EUID.with(|c| c.get()) {
        return u;
    }
    unsafe { libc::geteuid() }
}

/// The marker's bytes (at most AGENT_MODE_MAX_BYTES + 1), read as
/// `gate.agent_mode_lowered` reads them (F11-R-7): agents/ and the floor's
/// directory opened as directories without following a symlink, the
/// marker opened by name in the floor's directory without following a
/// symlink and without blocking, and only a regular file this user owns,
/// with one link, writable by no group and no other user, and small
/// enough. None for anything else.
fn read_marker(agents: &std::path::Path, floor: &str, name: &str) -> Option<Vec<u8>> {
    use std::ffi::CString;
    use std::io::Read;
    use std::os::fd::{FromRawFd, OwnedFd};
    use std::os::unix::ffi::OsStrExt;
    let dir_flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
    let open_at = |dirfd: libc::c_int, name: &[u8], flags: libc::c_int| -> Option<OwnedFd> {
        let c = CString::new(name).ok()?;
        let fd = unsafe { libc::openat(dirfd, c.as_ptr(), flags) };
        (fd >= 0).then(|| unsafe { OwnedFd::from_raw_fd(fd) })
    };
    use std::os::fd::AsRawFd;
    let a = open_at(libc::AT_FDCWD, agents.as_os_str().as_bytes(), dir_flags)?;
    let f = open_at(a.as_raw_fd(), floor.as_bytes(), dir_flags)?;
    let m = open_at(f.as_raw_fd(), name.as_bytes(), libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC)?;
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(m.as_raw_fd(), &mut st) } != 0 {
        return None;
    }
    if st.st_mode & libc::S_IFMT != libc::S_IFREG
        || st.st_uid != euid()
        || st.st_nlink != 1
        || st.st_mode & 0o022 != 0
        || st.st_size as u64 > AGENT_MODE_MAX_BYTES
    {
        return None;
    }
    let mut file = std::fs::File::from(m);
    let mut buf = Vec::new();
    (&mut file).take(AGENT_MODE_MAX_BYTES + 1).read_to_end(&mut buf).ok()?;
    Some(buf)
}

/// `gate.agent_mode_floor`: the first 16 hex digits of the SHA-256 of the
/// floor's socket path as the OS spells it (F11-R-6). None when the path
/// cannot be encoded.
pub fn agent_mode_floor(sock: &str) -> Option<String> {
    let b = super::py::text::fsencode(sock).ok()?;
    Some(super::sha256::hexdigest(&b)[..16].to_string())
}

/// A YAML value, as `yaml.safe_load` gives it, as far as the gate reads it.
#[derive(Debug, Clone, PartialEq)]
pub enum Y {
    Null,
    Bool(bool),
    /// An int, saturated to the i128 range (validity is all that matters).
    Int(i128),
    Float(f64),
    Str(String),
    List(Vec<Y>),
    Map(Vec<(Y, Y)>),
    /// A date or a datetime: no field takes one.
    Date,
}

/// `yaml.safe_load(text)`: the value, or None where it raises (a YAML
/// error, which load_config does not catch). A value the port does not
/// model is not decided.
pub fn safe_load(text: &str) -> R<Option<Y>> {
    match crate::pyyaml::load(text) {
        Ok(v) => Ok(Some(from_val(&v))),
        Err(crate::pyyaml::Fail::Exc(_)) => Ok(None),
        Err(crate::pyyaml::Fail::Unsupported(why)) => undecided(&format!("config.yaml holds YAML the port does not read ({why})")),
    }
}

fn from_val(v: &crate::pyyaml::Val) -> Y {
    use crate::pyyaml::Val;
    match v {
        Val::Null => Y::Null,
        Val::Bool(b) => Y::Bool(*b),
        Val::Int(t) => Y::Int(saturate(t)),
        Val::Float(f) => Y::Float(*f),
        Val::Str(s) => Y::Str(s.clone()),
        Val::Timestamp(_) => Y::Date,
        Val::List(xs) => Y::List(xs.iter().map(from_val).collect()),
        Val::Map(m) => Y::Map(
            m.iter()
                .map(|(k, x)| {
                    let key = match k.strip_prefix('\0') {
                        None => Y::Str(k.clone()),
                        Some("None") => Y::Null,
                        Some("True") => Y::Bool(true),
                        Some("False") => Y::Bool(false),
                        Some(rest) => Y::Int(saturate(rest.trim_start_matches("int:"))),
                    };
                    (key, from_val(x))
                })
                .collect(),
        ),
    }
}

/// An int's text, saturated to the i128 range.
fn saturate(t: &str) -> i128 {
    t.parse::<i128>().unwrap_or(if t.starts_with('-') { i128::MIN } else { i128::MAX })
}

/// pydantic's lax `int` from a YAML value.
fn valid_int(v: &Y) -> bool {
    match v {
        Y::Bool(_) | Y::Int(_) => true,
        Y::Float(f) => f.is_finite() && f.fract() == 0.0,
        Y::Str(s) => {
            let t = s.trim();
            let t = match t.find('.') {
                Some(d) if t[d + 1..].bytes().all(|c| c == b'0') => &t[..d],
                Some(_) => return false,
                None => t,
            };
            let body = t.strip_prefix(['+', '-']).unwrap_or(t);
            !body.is_empty()
                && body.bytes().all(|c| c.is_ascii_digit() || c == b'_')
                && !body.starts_with('_')
                && !body.ends_with('_')
                && !body.contains("__")
        }
        _ => false,
    }
}

/// pydantic's lax `bool` from a YAML value.
fn valid_bool(v: &Y) -> bool {
    match v {
        Y::Bool(_) => true,
        Y::Int(i) => *i == 0 || *i == 1,
        Y::Float(f) => *f == 0.0 || *f == 1.0,
        Y::Str(s) => matches!(
            s.to_ascii_lowercase().as_str(),
            "0" | "off" | "f" | "false" | "n" | "no" | "1" | "on" | "t" | "true" | "y" | "yes"
        ),
        _ => false,
    }
}

fn is_str(v: &Y) -> bool {
    matches!(v, Y::Str(_))
}

/// `Config(**filtered)` accepts these values.
fn valid_config(pairs: &[(Y, Y)]) -> bool {
    for (k, v) in pairs {
        let key = match k {
            Y::Str(s) => s.as_str(),
            _ => continue,
        };
        let ok = match key {
            "model" | "gateway_router" | "switchyard_route_id" | "switchyard_capable_model" | "gate_mode"
            | "verifier_client" | "matcher_model" | "envelope_source" | "pathway_store_backend" | "voice_engine"
            | "voice_model" | "voice_device" | "voice_compute_type" | "voice_server_url" => is_str(v),
            "max_task_chars" | "z3_timeout_ms" => valid_int(v),
            "data_dir" => is_str(v),
            "auto_tend" => *v == Y::Null || valid_bool(v),
            "shell_allow_decomposition" | "gate_ask" | "voice_cleanup" => valid_bool(v),
            "gateway_local_model" | "switchyard_efficient_model" | "switchyard_api_key_env" | "llm_backend"
            | "llm_base_url" | "llm_host_kind" | "llm_host_model" | "floor_report" | "voice_cleanup_model"
            | "voice_cleanup_base_url" | "dialect_enforce" => *v == Y::Null || is_str(v),
            "llm_context_window" => *v == Y::Null || valid_int(v),
            "floor" => match v {
                Y::Map(inner) => inner.iter().all(|(k2, v2)| match k2 {
                    Y::Str(s) => match s.as_str() {
                        "backend" => is_str(v2),
                        "notify_cmd" | "tmux_socket" | "coppice_socket" => *v2 == Y::Null || is_str(v2),
                        _ => true,
                    },
                    _ => true,
                }),
                _ => false,
            },
            _ => true,
        };
        if !ok {
            return false;
        }
    }
    true
}

/// A field of `load_config(path)`, or None where `load_config` raises.
pub(crate) fn load_field(r: &Runner, path: &str, field: &str, default: &str) -> R<Option<String>> {
    let _f = super::frames::frame();
    if !super::paths::exists(path)? {
        return Ok(Some(default.to_string()));
    }
    let bytes = match std::fs::read(super::paths::os_path("open", path)?) {
        Ok(b) => b,
        Err(e) => return Err(super::paths::os_error(&e, path).into()),
    };
    let text = match String::from_utf8(bytes) {
        Ok(t) => t,
        Err(_) => return Ok(None),
    };
    let _ = r;
    // read_text reads with universal newlines.
    let text = text.replace("\r\n", "\n").replace('\r', "\n");
    let Some(raw) = safe_load(&text)? else {
        return Ok(None);
    };
    let pairs = match raw {
        Y::Null | Y::Bool(false) => vec![],
        Y::Int(0) => vec![],
        Y::Float(f) if f == 0.0 => vec![],
        Y::Str(ref s) if s.is_empty() => vec![],
        Y::List(ref l) if l.is_empty() => vec![],
        Y::Map(p) => p,
        // Any other document: raw.items() raises.
        _ => return Ok(None),
    };
    if !valid_config(&pairs) {
        return Ok(None);
    }
    for (k, v) in &pairs {
        if *k == Y::Str(field.into()) {
            if let Y::Str(s) = v {
                return Ok(Some(s.clone()));
            }
        }
    }
    Ok(Some(default.to_string()))
}

impl Runner {
    /// The config file beside the gate root: `Path(root).parent / "config.yaml"`.
    pub fn config_path(&self) -> String {
        path_join(&path_parent(&self.root), "config.yaml")
    }

    /// `gate.resolve_gate_mode(None, root=...)`: config's gate_mode when it
    /// is audit or enforce, else audit, and audit on any error.
    pub fn config_gate_mode(&self) -> R<String> {
        let _f = super::frames::frame();
        let got = catch(load_field(self, &self.config_path(), "gate_mode", "audit"))?;
        Ok(match got {
            Ok(Some(m)) if m == "audit" || m == "enforce" => m,
            _ => "audit".into(),
        })
    }

    /// `gate.effective_gate_mode`: `mode`, except that an enforce hook
    /// becomes audit when the calling pane's marker says watching
    /// (F11-R-1). The marker never raises a mode.
    pub fn effective_mode(&self, mode: String) -> String {
        if mode == "enforce" && self.agent_mode_lowered() {
            return "audit".into();
        }
        mode
    }

    /// `gate.agent_mode_lowered`: true when `<root>/agents/<floor>/<pane>.mode` is
    /// a regular file of at most AGENT_MODE_MAX_BYTES bytes that holds
    /// exactly `watching`, with at most one newline after it. The pane and
    /// floor are COPPICE_PANE and COPPICE_SOCK, or a resident call's caller
    /// pane and socket, and only an id of coppice's shape is read
    /// (F11-R-2, F11-R-6). It is read as read_marker reads it (F11-R-7).
    /// Anything else, an error included, is false.
    pub fn agent_mode_lowered(&self) -> bool {
        let (pane, sock) = if self.resident {
            (self.caller.pane.as_deref(), self.caller.sock.as_deref())
        } else {
            (self.env.get("COPPICE_PANE").map(String::as_str), self.env.get("COPPICE_SOCK").map(String::as_str))
        };
        let Some(pane) = pane.filter(|p| super::state_report::valid_coppice_pane_id(p)) else {
            return false;
        };
        let Some(floor) = sock.filter(|s| !s.is_empty()).and_then(agent_mode_floor) else {
            return false;
        };
        let agents = super::paths::path_join(&self.root, "agents");
        let Ok(agents) = super::paths::os_path("open", &agents) else {
            return false;
        };
        let Some(buf) = read_marker(&agents, &floor, &format!("{pane}.mode")) else {
            return false;
        };
        buf == b"watching" || buf == b"watching\n"
    }

    /// `gate._configured_dialect_pin(root)`: config's dialect_enforce, or
    /// None (the words in audit) when it names none or cannot be read.
    pub fn dialect_pin(&self) -> R<Option<String>> {
        let _f = super::frames::frame();
        Ok(match catch(load_field(self, &self.config_path(), "dialect_enforce", ""))? {
            Ok(Some(p)) if !p.is_empty() => Some(p),
            _ => None,
        })
    }

    /// `gate._configured_verifier_client(root)[1]`: config's
    /// verifier_client, or python on any error.
    pub fn verifier_client(&self) -> R<String> {
        let _f = super::frames::frame();
        Ok(match catch(load_field(self, &self.config_path(), "verifier_client", "python"))? {
            Ok(Some(c)) if !c.is_empty() => c,
            _ => "python".into(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn map(text: &str) -> Vec<(Y, Y)> {
        match safe_load(text).unwrap().unwrap() {
            Y::Map(p) => p,
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn the_file_save_config_writes_reads_back() {
        let p = map(
            "auto_tend: null\ndata_dir: /home/u/.opendaisugi\nfloor:\n  backend: auto\n  notify_cmd: null\ngate_ask: false\n\
             gate_mode: audit\nmax_task_chars: 4000\nvoice_server_url: http://127.0.0.1:7477\nz3_timeout_ms: 500\n",
        );
        assert!(valid_config(&p));
        assert!(p.contains(&(Y::Str("gate_mode".into()), Y::Str("audit".into()))));
        assert!(p.contains(&(Y::Str("gate_ask".into()), Y::Bool(false))));
    }

    #[test]
    fn scalars_resolve_as_pyyaml_resolves_them() {
        for (t, want) in [
            ("yes", Y::Bool(true)),
            ("Off", Y::Bool(false)),
            ("0x1F", Y::Int(31)),
            ("010", Y::Int(8)),
            ("1_000", Y::Int(1000)),
            ("1e3", Y::Str("1e3".into())),
            ("1.5", Y::Float(1.5)),
            ("~", Y::Null),
            ("'x y'", Y::Str("x y".into())),
            ("\"a\\tb\"", Y::Str("a\tb".into())),
            ("python # comment", Y::Str("python".into())),
        ] {
            assert_eq!(map(&format!("a: {t}\n"))[0].1, want, "{t}");
        }
        assert!(!valid_config(&map("max_task_chars: 1.5\n")));
        assert!(!valid_config(&map("gate_mode: 5\n")));
        assert!(valid_config(&map("max_task_chars: '1_000'\nshell_allow_decomposition: 'Yes'\n")));
        assert_eq!(map("a: &x 1\nb: *x\n")[1].1, Y::Int(1));
        assert_eq!(safe_load("a: [\n").unwrap(), None);
        assert!(safe_load("a: &r [*r]\n").is_err());
    }
}
