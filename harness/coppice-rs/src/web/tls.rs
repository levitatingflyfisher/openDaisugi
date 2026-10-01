//! Where the server's certificate comes from, the refusals that teach the
//! command which makes one, and the rustls config that serves it.

use std::path::Path;
use std::sync::Arc;

use crate::sys;

/// The certificate sources.
pub const TAILSCALE: &str = "tailscale";
pub const LOCALCA: &str = "localca";
pub const FILES: &str = "files";
pub const OFF: &str = "off";

/// What Resolve reads.
pub struct Options<'a> {
    pub source: &'a str,
    pub cert_file: &'a str,
    pub key_file: &'a str,
    pub ca_dir: &'a str,
    pub listen: &'a str,
}

/// Go's statMissing: whether path is missing, and the stat error words.
fn stat_err(path: &str) -> Option<String> {
    match std::fs::metadata(path) {
        Ok(_) => None,
        Err(e) => Some(sys::go_path_err("stat", Path::new(path), &e)),
    }
}

/// The certificate and key to serve, or an error that names the command
/// which makes them. TLS off gives two empty paths.
pub fn resolve(o: &Options) -> Result<(String, String), String> {
    match o.source {
        OFF => require_loopback(o.listen).map(|_| (String::new(), String::new())),
        FILES => {
            if o.cert_file.is_empty() || o.key_file.is_empty() {
                return Err("--tls files needs both --cert and --key".into());
            }
            if let Some(e) = stat_err(o.cert_file) {
                return Err(format!("--cert {}: {e}", o.cert_file));
            }
            if let Some(e) = stat_err(o.key_file) {
                return Err(format!("--key {}: {e}", o.key_file));
            }
            Ok((o.cert_file.into(), o.key_file.into()))
        }
        TAILSCALE => {
            if o.cert_file.is_empty() || o.key_file.is_empty() {
                return Err("--tls tailscale needs --cert and --key. Run tailscale cert --cert-file <path> --key-file <path> <name>.<tailnet>.ts.net".into());
            }
            if let Some(e) = stat_err(o.cert_file) {
                return Err(format!(
                    "no tailscale certificate at {}: {e}. Run tailscale cert --cert-file {} --key-file {} <name>.<tailnet>.ts.net",
                    o.cert_file, o.cert_file, o.key_file
                ));
            }
            if let Some(e) = stat_err(o.key_file) {
                return Err(format!(
                    "no tailscale key at {}: {e}. Run tailscale cert --cert-file {} --key-file {} <name>.<tailnet>.ts.net",
                    o.key_file, o.cert_file, o.key_file
                ));
            }
            Ok((o.cert_file.into(), o.key_file.into()))
        }
        LOCALCA => {
            if o.ca_dir.is_empty() {
                return Err("--tls localca needs --ca-dir".into());
            }
            let (c, k) = super::localca::CaDir::new(o.ca_dir.into()).leaf_paths();
            let (c, k) = (
                c.to_string_lossy().into_owned(),
                k.to_string_lossy().into_owned(),
            );
            if let Some(e) = stat_err(&c) {
                return Err(format!(
                    "no local certificate at {c}: {e}. Run coppice web cert init"
                ));
            }
            if let Some(e) = stat_err(&k) {
                return Err(format!(
                    "no local key at {k}: {e}. Run coppice web cert init"
                ));
            }
            Ok((c, k))
        }
        s => Err(format!(
            "unknown --tls {}. Use tailscale, localca, files, or off",
            sys::go_quote(s)
        )),
    }
}

/// Refuses plain HTTP anywhere a second machine could reach.
pub fn require_loopback(listen: &str) -> Result<(), String> {
    let Ok((host, _)) = super::split_host_port(listen) else {
        return Err(format!(
            "--listen {} is not host:port",
            sys::go_quote(listen)
        ));
    };
    if host.is_empty() {
        return Err("--tls off needs a loopback address. Use --listen 127.0.0.1:8443".into());
    }
    if host == "localhost" {
        return Ok(());
    }
    if super::ip_is_loopback(&host) != Some(true) {
        return Err(format!(
            "--tls off serves plain HTTP, so it only listens on loopback. Use --listen 127.0.0.1:8443, or pick --tls localca to reach {host}"
        ));
    }
    Ok(())
}

/// A rustls server config for a PEM certificate chain and key, as Go's
/// tls.LoadX509KeyPair reads them, with TLS 1.2 at least.
pub fn server_config(cert_file: &str, key_file: &str) -> Result<Arc<rustls::ServerConfig>, String> {
    use rustls::pki_types::{CertificateDer, PrivateKeyDer};
    let cert_pem =
        std::fs::read(cert_file).map_err(|e| sys::go_path_err("open", Path::new(cert_file), &e))?;
    let key_pem =
        std::fs::read(key_file).map_err(|e| sys::go_path_err("open", Path::new(key_file), &e))?;
    let certs: Vec<CertificateDer<'static>> = super::der::pem_blocks(&cert_pem)
        .into_iter()
        .filter(|(k, _)| k == "CERTIFICATE")
        .map(|(_, b)| CertificateDer::from(b))
        .collect();
    if certs.is_empty() {
        return Err("tls: failed to find any PEM data in certificate input".into());
    }
    let key = super::der::pem_blocks(&key_pem)
        .into_iter()
        .find_map(|(k, b)| match k.as_str() {
            "PRIVATE KEY" => Some(PrivateKeyDer::Pkcs8(b.into())),
            "EC PRIVATE KEY" => Some(PrivateKeyDer::Sec1(b.into())),
            "RSA PRIVATE KEY" => Some(PrivateKeyDer::Pkcs1(b.into())),
            _ => None,
        })
        .ok_or_else(|| "tls: failed to find any PEM data in key input".to_string())?;
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let cfg = rustls::ServerConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13, &rustls::version::TLS12])
        .map_err(|e| e.to_string())?
        .with_no_client_auth()
        .with_single_cert(certs, key)
        .map_err(|e| format!("tls: {e}"))?;
    let mut cfg = cfg;
    cfg.alpn_protocols = vec![b"http/1.1".to_vec()];
    Ok(Arc::new(cfg))
}
