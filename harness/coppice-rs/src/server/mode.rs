//! The per-agent daisugi mode (F11), the Rust twin of mode.go. The owner
//! lowers one agent from enforcing to watching from its stack bar; coppice
//! writes a marker the gate reads on that agent's next tool call:
//! `<gate root>/agents/<floor>/<pane>.mode`. The gate lowers an enforce hook to
//! audit only when the marker holds exactly "watching" (F11-R-1). Turning
//! the gate off stays a typed command, so this verb never offers off.

use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::PathBuf;
use std::sync::Arc;

use serde_json::json;

use super::{role, Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::sys;

/// What a pane, a plugin or an unplaced peer gets for agent.set_mode.
pub const MODE_REFUSAL: &str = "only the operator sets an agent's daisugi mode.";

/// What agent.set_mode says to a mode it does not take.
pub const MODE_WORDS_REFUSAL: &str =
    "mode must be enforcing or watching. Turning the gate off stays a command: daisugi gate disarm";

/// The marker's whole text.
const MODE_WATCHING: &str = "watching\n";

/// The shape of a pane id a marker may be named for, the shape the gate
/// checks before it reads one: `w<1-9 digits>:p<1-9 digits>`.
pub fn marker_pane_id(id: &str) -> bool {
    fn digits(s: &str) -> bool {
        (1..=9).contains(&s.len()) && s.bytes().all(|c| c.is_ascii_digit())
    }
    match id.strip_prefix('w').and_then(|r| r.split_once(":p")) {
        Some((w, p)) => digits(w) && digits(p),
        None => false,
    }
}

/// The largest marker read, as the gate's AGENT_MODE_MAX_BYTES.
const MARKER_MAX: u64 = 64;

/// One marker read as the gate reads it (F11-R-7, F11-R-9): agents/ and
/// the floor's directory opened as directories without following a
/// symlink, the marker opened by name in the floor's directory without
/// following a symlink and without blocking, and only a regular file this
/// user owns, with one link, writable by no group and no other user, of
/// at most MARKER_MAX bytes. None for anything else.
fn read_marker(agents: &std::path::Path, floor: &str, name: &str) -> Option<Vec<u8>> {
    use std::ffi::CString;
    use std::io::Read;
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
    use std::os::unix::ffi::OsStrExt;
    let dir_flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
    let open_at = |dirfd: libc::c_int, name: &[u8], flags: libc::c_int| -> Option<OwnedFd> {
        let c = CString::new(name).ok()?;
        let fd = unsafe { libc::openat(dirfd, c.as_ptr(), flags) };
        (fd >= 0).then(|| unsafe { OwnedFd::from_raw_fd(fd) })
    };
    let a = open_at(libc::AT_FDCWD, agents.as_os_str().as_bytes(), dir_flags)?;
    let f = open_at(a.as_raw_fd(), floor.as_bytes(), dir_flags)?;
    let m = open_at(
        f.as_raw_fd(),
        name.as_bytes(),
        libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC,
    )?;
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(m.as_raw_fd(), &mut st) } != 0 {
        return None;
    }
    if st.st_mode & libc::S_IFMT != libc::S_IFREG
        || st.st_uid != unsafe { libc::geteuid() }
        || st.st_nlink != 1
        || st.st_mode & 0o022 != 0
        || st.st_size as u64 > MARKER_MAX
    {
        return None;
    }
    let mut file = std::fs::File::from(m);
    let mut buf = Vec::new();
    (&mut file)
        .take(MARKER_MAX + 1)
        .read_to_end(&mut buf)
        .ok()?;
    Some(buf)
}

/// The error for a symlink or a file where a marker directory belongs.
fn not_a_dir(p: &std::path::Path) -> std::io::Error {
    std::io::Error::other(format!(
        "{} is not a directory: it is a symlink or a file. Move it aside",
        p.display()
    ))
}

/// True when p is a directory, false when nothing is there, and an error
/// when something else is: a symlink (even to a directory) or a file.
/// coppice makes and sweeps markers only in real directories (F11-R-8).
fn real_dir(p: &std::path::Path) -> std::io::Result<bool> {
    match std::fs::symlink_metadata(p) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e),
        Ok(m) if m.file_type().is_dir() => Ok(true),
        Ok(_) => Err(not_a_dir(p)),
    }
}

impl Server {
    /// This floor's directory under agents/: the first 16 hex digits of
    /// the SHA-256 of the socket path every pane gets as COPPICE_SOCK
    /// (F11-R-6).
    pub fn floor_key(&self) -> String {
        use std::os::unix::ffi::OsStrExt;
        crate::sha256::hex(self.cfg.socket_path.as_os_str().as_bytes())[..16].to_string()
    }

    /// This floor's marker directory.
    fn floor_dir(&self) -> PathBuf {
        self.gate_root().join("agents").join(self.floor_key())
    }

    /// True when agents/ and this floor's directory are both real
    /// directories, false when either is missing, and an error when either
    /// is something else (F11-R-8).
    fn floor_dir_real(&self) -> std::io::Result<bool> {
        let floor = self.floor_dir();
        for d in [floor.parent().map(PathBuf::from).unwrap_or_default(), floor] {
            if !real_dir(&d)? {
                return Ok(false);
            }
        }
        Ok(true)
    }

    /// Makes agents/ and this floor's directory, 0700, each only when
    /// nothing is there, and refuses a symlink or a file in their place.
    fn make_floor_dir(&self) -> std::io::Result<()> {
        let floor = self.floor_dir();
        let agents = floor.parent().map(PathBuf::from).unwrap_or_default();
        if let Some(root) = agents.parent() {
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(root)?;
        }
        for d in [agents, floor] {
            if !real_dir(&d)? {
                match std::fs::DirBuilder::new().mode(0o700).create(&d) {
                    Err(e) if e.kind() != std::io::ErrorKind::AlreadyExists => return Err(e),
                    _ => {}
                }
                if !real_dir(&d)? {
                    return Err(not_a_dir(&d));
                }
            }
        }
        Ok(())
    }

    /// The marker of pane id, or None when id has not the shape.
    fn marker_path(&self, id: &str) -> Option<PathBuf> {
        marker_pane_id(id).then(|| self.floor_dir().join(format!("{id}.mode")))
    }

    /// "watching" when pane id's marker reads as the gate reads it
    /// (read_marker) and holds the marker text, and "" otherwise. The read
    /// is by fd from the directory down, never a path read after a check,
    /// so the stack shows a marker only when the gate would lower the agent
    /// (F11-R-9).
    pub fn mode_marker(&self, id: &str) -> &'static str {
        if self.marker_path(id).is_none() {
            return "";
        }
        let agents = self.gate_root().join("agents");
        match read_marker(&agents, &self.floor_key(), &format!("{id}.mode")) {
            Some(b) if b == MODE_WATCHING.as_bytes() || b == b"watching" => "watching",
            _ => "",
        }
    }

    /// Writes the watching marker for pane id: a new file 0600 in the
    /// agents directory (0700), renamed over the old one, so the gate reads
    /// either the old marker or the whole new one.
    fn write_mode_marker(&self, id: &str) -> std::io::Result<()> {
        let p = self.marker_path(id).ok_or_else(|| {
            std::io::Error::other(format!("no marker for pane {}", sys::go_quote(id)))
        })?;
        let dir = p.parent().map(PathBuf::from).unwrap_or_default();
        self.make_floor_dir()?;
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let tmp = dir.join(format!(".{id}.mode.{}-{nanos}", std::process::id()));
        let res = (|| {
            let mut f = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&tmp)?;
            f.write_all(MODE_WATCHING.as_bytes())?;
            drop(f);
            std::fs::rename(&tmp, &p)
        })();
        if res.is_err() {
            let _ = std::fs::remove_file(&tmp);
        }
        res
    }

    /// Removes pane id's marker. No marker is no error.
    pub fn clear_mode_marker(&self, id: &str) -> std::io::Result<()> {
        let Some(p) = self.marker_path(id) else {
            return Ok(());
        };
        if !self.floor_dir_real()? {
            return Ok(());
        }
        match std::fs::remove_file(&p) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        }
    }

    /// Removes every marker in this floor's directory. A server start runs
    /// it: no agent of an earlier run is live, so every agent starts
    /// enforcing again (F11-R-4). Another floor's directory and other files
    /// stay (F11-R-6).
    pub fn clear_all_mode_markers(&self) {
        let dir = self.floor_dir();
        match self.floor_dir_real() {
            Ok(true) => {}
            Ok(false) => return,
            Err(e) => {
                // Never remove anything through a symlink (F11-R-8).
                eprintln!("mode markers: {e}");
                self.note(&format!("daisugi mode markers left alone: {e}"), "");
                return;
            }
        }
        let Ok(ents) = std::fs::read_dir(&dir) else {
            return;
        };
        for e in ents.flatten() {
            let name = e.file_name().to_string_lossy().into_owned();
            if let Some(id) = name.strip_suffix(".mode") {
                if marker_pane_id(id) {
                    let _ = std::fs::remove_file(dir.join(&name));
                }
            }
        }
    }

    /// agent.set_mode {pane, mode}: watching writes the pane's marker,
    /// enforcing removes it. The role check has already refused a pane, a
    /// plugin and an unplaced peer.
    pub(super) fn handle_set_mode(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        // A view-only attach is refused before anything else is read
        // (F11-R-10).
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let mode = r.s("mode");
        if mode != "enforcing" && mode != "watching" {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, MODE_WORDS_REFUSAL);
        }
        let rec = match self.tree.pane(&id) {
            Some(rec) if self.marker_path(&id).is_some() => rec,
            _ => {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_NO_SUCH_PANE,
                    &format!("no pane {}. Run: coppice agent list", sys::go_quote(&id)),
                )
            }
        };
        if rec.closed {
            return proto::err_resp(
                &r.id,
                proto::ERR_PANE_CLOSED,
                &format!("{} has ended, so no gate checks it.", self.label_of(&id)),
            );
        }
        if rec.harness.is_empty() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!(
                    "{} runs no harness, so no gate checks it.",
                    self.label_of(&id)
                ),
            );
        }
        let res = if mode == "watching" {
            self.write_mode_marker(&id)
        } else {
            self.clear_mode_marker(&id)
        };
        if let Err(e) = res {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!(
                    "could not set the daisugi mode of {}: {e}",
                    self.label_of(&id)
                ),
            );
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![("pane", json!(id)), ("mode", json!(mode))]),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::super::{Config, Server};
    use super::marker_pane_id;

    fn scratch_server(name: &str) -> (std::path::PathBuf, std::sync::Arc<Server>) {
        let d = std::env::temp_dir().join(format!("coppice-rs-mode-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(d.join("data")).unwrap();
        let s = Server::new(Config {
            socket_path: d.join("s.sock"),
            data_dir: d.join("data"),
            start_dir: d.clone(),
            voice: Default::default(),
            foreman: Default::default(),
        });
        (d, s)
    }

    /// F11-R-8: nothing is made, written or removed through a symlink where
    /// agents/ or the floor's directory belongs.
    #[test]
    fn markers_go_only_into_real_directories() {
        for which in ["agents", "floor"] {
            let (d, s) = scratch_server(which);
            let floor = s.floor_dir();
            let link = if which == "agents" {
                floor.parent().unwrap().to_path_buf()
            } else {
                floor.clone()
            };
            let target = d.join("elsewhere");
            std::fs::create_dir_all(&target).unwrap();
            std::fs::write(target.join("w1:p1.mode"), b"watching\n").unwrap();
            std::fs::create_dir_all(link.parent().unwrap()).unwrap();
            std::os::unix::fs::symlink(&target, &link).unwrap();
            let e = s.write_mode_marker("w1:p2").unwrap_err();
            assert!(e.to_string().contains("not a directory"), "{e}");
            assert!(s.clear_mode_marker("w1:p1").is_err());
            s.clear_all_mode_markers();
            assert!(
                target.join("w1:p1.mode").exists(),
                "{which}: removed through the link"
            );
            assert!(
                !target.join("w1:p2.mode").exists(),
                "{which}: written through the link"
            );
            let _ = std::fs::remove_dir_all(&d);
        }
    }

    /// F11-R-9 (M2): the stack shows a marker only when it reads as the
    /// gate reads it.
    #[test]
    fn the_marker_shows_only_when_the_gate_would_read_it() {
        use std::os::unix::fs::PermissionsExt;
        type Spoil = fn(&std::path::Path, &std::path::Path);
        let spoils: &[(&str, Spoil)] = &[
            ("symlink", |d, p| {
                std::fs::rename(p, d.join("real")).unwrap();
                std::os::unix::fs::symlink(d.join("real"), p).unwrap();
            }),
            ("hard link", |d, p| {
                std::fs::hard_link(p, d.join("second")).unwrap()
            }),
            ("group writable", |_, p| {
                std::fs::set_permissions(p, std::fs::Permissions::from_mode(0o620)).unwrap()
            }),
            ("floor dir symlink", |d, p| {
                let dir = p.parent().unwrap();
                std::fs::rename(dir, d.join("floor")).unwrap();
                std::os::unix::fs::symlink(d.join("floor"), dir).unwrap();
            }),
        ];
        for (name, spoil) in spoils {
            let (d, s) = scratch_server(&format!("m2-{}", name.replace(' ', "-")));
            s.write_mode_marker("w1:p1").unwrap();
            assert_eq!(
                s.mode_marker("w1:p1"),
                "watching",
                "{name}: not read before"
            );
            spoil(&d, &s.marker_path("w1:p1").unwrap());
            assert_eq!(s.mode_marker("w1:p1"), "", "{name}: still read");
            let _ = std::fs::remove_dir_all(&d);
        }
    }

    #[test]
    fn a_marker_names_only_a_pane_id_of_coppice_shape() {
        for ok in ["w1:p1", "w123456789:p987654321"] {
            assert!(marker_pane_id(ok), "{ok}");
        }
        for bad in [
            "",
            "../w1:p1",
            "w1:p1/../w1:p1",
            "w1:p1\n",
            "W1:P1",
            "w1:p1234567890",
            "w:p1",
            "w1:p",
            "%3",
        ] {
            assert!(!marker_pane_id(bad), "{bad:?}");
        }
    }
}
