//! The phone server's paths under the data dir, and web.json, the saved
//! settings whose `enabled` makes the coppice server start it.

use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
use std::path::{Path, PathBuf};

use serde_json::json;

use crate::godec::{self, StructTy, Ty};
use crate::sys;

/// The bearer token: a "web" directory under the data dir, holding
/// "token".
pub fn token_path(data_dir: &Path) -> PathBuf {
    data_dir.join("web").join("token")
}

/// Where coppice web cert tailscale writes its pair.
pub fn tls_dir(data_dir: &Path) -> PathBuf {
    data_dir.join("web").join("tls")
}

/// The pair coppice web cert tailscale writes and --tls tailscale reads.
pub fn tailscale_paths(data_dir: &Path) -> (PathBuf, PathBuf) {
    let d = tls_dir(data_dir);
    (d.join("tailscale.crt"), d.join("tailscale.key"))
}

/// Where coppice web cert init writes the CA.
pub fn local_ca_dir(data_dir: &Path) -> PathBuf {
    data_dir.join("web").join("ca")
}

/// The saved settings.
pub fn config_path(data_dir: &Path) -> PathBuf {
    data_dir.join("web").join("web.json")
}

/// The gate directory beside the data dir: its parent, plus "gate",
/// cleaned as Go's filepath.Join cleans it.
pub fn gate_root(data_dir: &Path) -> PathBuf {
    PathBuf::from(sys::clean(&format!("{}/../gate", data_dir.display())))
}

/// The persisted phone server.
#[derive(Clone, Debug, Default)]
pub struct Config {
    pub enabled: bool,
    pub listen: String,
    pub tls: String,
    pub cert_file: String,
    pub key_file: String,
    pub ca_dir: String,
    pub ca_listen: String,
    pub ntfy: String,
    pub ntfy_topic: String,
    pub ntfy_token_env: String,
    pub external_url: String,
    pub gate_root: String,
    pub voice_url: String,
    pub voice_token_file: String,
}

static CONFIG_TY: StructTy = StructTy {
    name: "Config",
    text: "web.Config",
    fields: &[
        ("enabled", Ty::PtrBool),
        ("listen", Ty::Str),
        ("tls", Ty::Str),
        ("cert_file", Ty::Str),
        ("key_file", Ty::Str),
        ("ca_dir", Ty::Str),
        ("ca_listen", Ty::Str),
        ("ntfy", Ty::Str),
        ("ntfy_topic", Ty::Str),
        ("ntfy_token_env", Ty::Str),
        ("external_url", Ty::Str),
        ("gate_root", Ty::Str),
        ("voice_url", Ty::Str),
        ("voice_token_file", Ty::Str),
    ],
};

/// Reads the file at path. A missing file is the phone server off.
pub fn load(path: &Path) -> Result<Config, String> {
    let raw = match std::fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Config::default()),
        Err(e) => return Err(sys::go_path_err("open", path, &e)),
    };
    let (g, err) = godec::decode(&raw, &Ty::Struct(&CONFIG_TY));
    if let Some(e) = err {
        return Err(e);
    }
    Ok(Config {
        enabled: g.f("enabled").boolean().unwrap_or(false),
        listen: g.f("listen").s(),
        tls: g.f("tls").s(),
        cert_file: g.f("cert_file").s(),
        key_file: g.f("key_file").s(),
        ca_dir: g.f("ca_dir").s(),
        ca_listen: g.f("ca_listen").s(),
        ntfy: g.f("ntfy").s(),
        ntfy_topic: g.f("ntfy_topic").s(),
        ntfy_token_env: g.f("ntfy_token_env").s(),
        external_url: g.f("external_url").s(),
        gate_root: g.f("gate_root").s(),
        voice_url: g.f("voice_url").s(),
        voice_token_file: g.f("voice_token_file").s(),
    })
}

/// c as Go's json.MarshalIndent(c, "", "  ") writes it.
pub fn marshal(c: &Config) -> String {
    let mut pairs = vec![
        ("enabled", json!(c.enabled)),
        ("listen", json!(c.listen)),
        ("tls", json!(c.tls)),
        ("cert_file", json!(c.cert_file)),
        ("key_file", json!(c.key_file)),
        ("ca_dir", json!(c.ca_dir)),
        ("ca_listen", json!(c.ca_listen)),
        ("ntfy", json!(c.ntfy)),
        ("ntfy_topic", json!(c.ntfy_topic)),
        ("ntfy_token_env", json!(c.ntfy_token_env)),
        ("external_url", json!(c.external_url)),
        ("gate_root", json!(c.gate_root)),
        ("voice_url", json!(c.voice_url)),
    ];
    if !c.voice_token_file.is_empty() {
        pairs.push(("voice_token_file", json!(c.voice_token_file)));
    }
    crate::gojson::marshal_indent(&crate::gojson::obj(pairs), "  ")
}

/// Writes c to path, mode 0600 inside a directory mode 0700.
pub fn save(path: &Path, c: &Config) -> Result<(), String> {
    let dir = path.parent().map(PathBuf::from).unwrap_or_default();
    mkdir_private(&dir)?;
    write_file_mode(path, marshal(c).as_bytes(), 0o600)
}

/// Go's os.MkdirAll(dir, 0700) then os.Chmod(dir, 0700), with Go's words.
pub fn mkdir_private(dir: &Path) -> Result<(), String> {
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|e| sys::go_path_err("mkdir", dir, &e))?;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| sys::go_path_err("chmod", dir, &e))
}

/// Go's os.WriteFile(path, body, mode) then os.Chmod(path, mode).
pub fn write_file_mode(path: &Path, body: &[u8], mode: u32) -> Result<(), String> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(mode)
        .open(path)
        .map_err(|e| sys::go_path_err("open", path, &e))?;
    f.write_all(body)
        .map_err(|e| sys::go_path_err("write", path, &e))?;
    drop(f);
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
        .map_err(|e| sys::go_path_err("chmod", path, &e))
}

/// The floor page's address on this box, or "" when the phone server is
/// off or its listen address does not parse.
#[allow(dead_code)]
pub fn local_url(c: &Config) -> String {
    if !c.enabled {
        return String::new();
    }
    let listen = if c.listen.is_empty() {
        ":8443"
    } else {
        &c.listen
    };
    let Ok((mut host, port)) = super::split_host_port(listen) else {
        return String::new();
    };
    if port.is_empty() {
        return String::new();
    }
    let scheme = if c.tls == "off" { "http" } else { "https" };
    if host.is_empty() || host == "0.0.0.0" || host == "::" {
        host = "127.0.0.1".into();
    }
    format!("{scheme}://{}", super::join_host_port(&host, &port))
}
