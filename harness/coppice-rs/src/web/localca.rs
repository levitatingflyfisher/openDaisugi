//! The local CA: one P-256 root the phone installs once, and one leaf
//! issued again whenever the box changes address. The files and their
//! layout are Go's, so either port reads a CA the other made.

use std::path::{Path, PathBuf};

use ring::rand::SystemRandom;
use ring::signature::{EcdsaKeyPair, KeyPair, ECDSA_P256_SHA256_ASN1_SIGNING};
use serde_json::{json, Value};

use super::der;
use crate::sys;

/// Ten years: the phone installs the CA once.
pub const CA_VALIDITY_SECS: i64 = 10 * 365 * 24 * 3600;
/// 398 days, the browsers' ceiling for a server certificate.
pub const LEAF_VALIDITY_SECS: i64 = 398 * 24 * 3600;

/// What the leaf covers, from meta.json.
#[derive(Clone, Debug, Default)]
pub struct Meta {
    pub names: Vec<String>,
    pub ips: Vec<String>,
    /// not_after as written, RFC 3339 with nanoseconds.
    pub not_after: String,
}

/// One CA directory.
pub struct CaDir {
    pub path: PathBuf,
}

/// Now as Unix seconds and nanoseconds.
pub fn now() -> (i64, u32) {
    let d = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    (d.as_secs() as i64, d.subsec_nanos())
}

/// A time as Go's RFC3339Nano writes it in UTC: trailing zeros of the
/// fraction cut, no fraction when it is zero.
pub fn rfc3339_nano(secs: i64, nanos: u32) -> String {
    let (y, mo, d, h, mi, s) = der::civil(secs);
    let mut out = format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}");
    if nanos > 0 {
        let f = format!("{nanos:09}");
        out.push('.');
        out.push_str(f.trim_end_matches('0'));
    }
    out.push('Z');
    out
}

/// A time as Go's RFC3339 writes it in UTC.
pub fn rfc3339(secs: i64) -> String {
    rfc3339_nano(secs, 0)
}

impl CaDir {
    pub fn new(path: PathBuf) -> CaDir {
        CaDir { path }
    }

    fn file(&self, name: &str) -> PathBuf {
        self.path.join(name)
    }

    /// The server certificate and key --tls localca serves.
    pub fn leaf_paths(&self) -> (PathBuf, PathBuf) {
        (self.file("leaf.crt"), self.file("leaf.key"))
    }

    /// The certificate the phone installs.
    pub fn ca_pem(&self) -> Result<Vec<u8>, String> {
        let p = self.file("ca.crt");
        std::fs::read(&p).map_err(|e| sys::go_path_err("open", &p, &e))
    }

    /// What the current leaf covers. Any failure is an error; the caller
    /// says the CA is missing.
    pub fn meta(&self) -> Result<Meta, String> {
        let raw = std::fs::read(self.file("meta.json")).map_err(|e| e.to_string())?;
        let v: Value = serde_json::from_slice(&raw).map_err(|e| e.to_string())?;
        let list = |k: &str| -> Result<Vec<String>, String> {
            match v.get(k) {
                None | Some(Value::Null) => Ok(Vec::new()),
                Some(Value::Array(a)) => a
                    .iter()
                    .map(|x| {
                        x.as_str()
                            .map(String::from)
                            .ok_or_else(|| "bad".to_string())
                    })
                    .collect(),
                _ => Err("bad".into()),
            }
        };
        Ok(Meta {
            names: list("names")?,
            ips: list("ips")?,
            not_after: v
                .get("not_after")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
        })
    }

    /// Loads the CA, naming the file a failure came from.
    fn load_ca(&self) -> Result<(Vec<u8>, EcdsaKeyPair), String> {
        let cert_file = self.file("ca.crt");
        let key_file = self.file("ca.key");
        let cert_pem = std::fs::read(&cert_file).map_err(|e| {
            format!(
                "{}: {}",
                cert_file.display(),
                sys::go_path_err("open", &cert_file, &e)
            )
        })?;
        let key_pem = std::fs::read(&key_file).map_err(|e| {
            format!(
                "{}: {}",
                key_file.display(),
                sys::go_path_err("open", &key_file, &e)
            )
        })?;
        let Some((_, cert_der)) = der::pem_blocks(&cert_pem).into_iter().next() else {
            return Err(format!("{} is not a PEM file", cert_file.display()));
        };
        let Some((_, key_der)) = der::pem_blocks(&key_pem).into_iter().next() else {
            return Err(format!("{} is not a PEM file", key_file.display()));
        };
        if der::parse_cert(&cert_der).is_none() {
            return Err(format!(
                "{}: x509: malformed certificate",
                cert_file.display()
            ));
        }
        let key = EcdsaKeyPair::from_pkcs8(
            &ECDSA_P256_SHA256_ASN1_SIGNING,
            &key_der,
            &SystemRandom::new(),
        )
        .map_err(|_| format!("{} is not an ECDSA key", key_file.display()))?;
        Ok((cert_der, key))
    }

    /// Mints the root the phone installs once.
    fn create_ca(&self, now: i64) -> Result<(Vec<u8>, EcdsaKeyPair), String> {
        let rng = SystemRandom::new();
        let pkcs8 = EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, &rng)
            .map_err(|_| "cannot make a P-256 key".to_string())?;
        let key = EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, pkcs8.as_ref(), &rng)
            .map_err(|_| "cannot read the new key".to_string())?;
        let subject = der::name("openDaisugi", "openDaisugi coppice local CA");
        let pubkey = key.public_key().as_ref().to_vec();
        // Go derives a CA's subject key id from the key: the first 20
        // bytes of the SHA-256 of the public key bits.
        let skid = ring::digest::digest(&ring::digest::SHA256, &pubkey).as_ref()[..20].to_vec();
        let exts = vec![
            key_usage(0x61),
            basic_constraints(true),
            ext(&[2, 5, 29, 14], false, der::tlv(der::OCTET_STRING, &skid)),
        ];
        let cert = issue(
            &key,
            &subject,
            &subject,
            now - 3600,
            now + CA_VALIDITY_SECS,
            &pubkey,
            exts,
        )?;
        super::config::write_file_mode(
            &self.file("ca.crt"),
            der::pem("CERTIFICATE", &cert).as_bytes(),
            0o644,
        )?;
        super::config::write_file_mode(
            &self.file("ca.key"),
            der::pem("PRIVATE KEY", pkcs8.as_ref()).as_bytes(),
            0o600,
        )?;
        Ok((cert, key))
    }

    /// Makes the CA if there is none, keeps it if there is, and always
    /// issues a new leaf for the names and addresses.
    pub fn init(&self, names: &[String], ips: &[std::net::IpAddr]) -> Result<Meta, String> {
        if names.is_empty() && ips.is_empty() {
            return Err("give at least one name or one address".into());
        }
        super::config::mkdir_private(&self.path)?;
        let (now_s, now_ns) = now();
        let ca_crt = self.file("ca.crt");
        let ca_key = self.file("ca.key");
        let (ca_der, ca_key_pair) = match std::fs::metadata(&ca_crt) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                if std::fs::metadata(&ca_key).is_ok() {
                    return Err(format!(
                        "{} is present but {} is not. A new CA would overwrite that key. \
                         Move {} aside, or restore {}, then run this again",
                        ca_key.display(),
                        ca_crt.display(),
                        ca_key.display(),
                        ca_crt.display()
                    ));
                }
                self.create_ca(now_s)?
            }
            _ => self.load_ca().map_err(|e| {
                format!(
                    "the CA in {} will not load: {e}. The phone already trusts it, so this \
                     command will not replace it. Move {} aside to start a new one, then \
                     install the new CA on every phone",
                    self.path.display(),
                    self.path.display()
                )
            })?,
        };
        let Some(ca) = der::parse_cert(&ca_der) else {
            return Err("x509: malformed certificate".into());
        };
        if ca.spki_key != ca_key_pair.public_key().as_ref() {
            return Err("x509: provided PrivateKey doesn't match parent's PublicKey".into());
        }
        let rng = SystemRandom::new();
        let pkcs8 = EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, &rng)
            .map_err(|_| "cannot make a P-256 key".to_string())?;
        let leaf_key =
            EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_ASN1_SIGNING, pkcs8.as_ref(), &rng)
                .map_err(|_| "cannot read the new key".to_string())?;
        let cn = match names.first() {
            Some(n) => n.clone(),
            None => super::ip_string(&ips[0]),
        };
        let not_before = now_s - 3600;
        let not_after = not_before + LEAF_VALIDITY_SECS;
        let mut exts = vec![
            key_usage(0x01),
            ext(
                &[2, 5, 29, 37],
                false,
                der::seq(&[der::oid(&[1, 3, 6, 1, 5, 5, 7, 3, 1])]),
            ),
            basic_constraints(false),
        ];
        if let Some(akid) = ca.subject_key_id {
            exts.push(ext(
                &[2, 5, 29, 35],
                false,
                der::seq(&[der::tlv(0x80, akid)]),
            ));
        }
        let mut sans = Vec::new();
        for n in names {
            sans.push(der::tlv(0x82, n.as_bytes()));
        }
        for ip in ips {
            let b = match ip {
                std::net::IpAddr::V4(v4) => v4.octets().to_vec(),
                std::net::IpAddr::V6(v6) => match v6.to_ipv4_mapped() {
                    Some(v4) => v4.octets().to_vec(),
                    None => v6.octets().to_vec(),
                },
            };
            sans.push(der::tlv(0x87, &b));
        }
        exts.push(ext(&[2, 5, 29, 17], false, der::seq(&sans)));
        let leaf = issue(
            &ca_key_pair,
            ca.subject,
            &der::name("openDaisugi", &cn),
            not_before,
            not_after,
            leaf_key.public_key().as_ref(),
            exts,
        )?;
        let (cert_file, key_file) = self.leaf_paths();
        super::config::write_file_mode(
            &cert_file,
            der::pem("CERTIFICATE", &leaf).as_bytes(),
            0o644,
        )?;
        super::config::write_file_mode(
            &key_file,
            der::pem("PRIVATE KEY", pkcs8.as_ref()).as_bytes(),
            0o600,
        )?;
        let ip_texts: Vec<String> = ips.iter().map(super::ip_string).collect();
        let list = |v: &[String]| {
            if v.is_empty() {
                Value::Null
            } else {
                json!(v)
            }
        };
        let meta = crate::gojson::obj(vec![
            ("version", json!(1)),
            ("names", list(names)),
            ("ips", list(&ip_texts)),
            ("created", json!(rfc3339_nano(now_s, now_ns))),
            ("not_after", json!(rfc3339_nano(not_after, now_ns))),
        ]);
        super::config::write_file_mode(
            &self.file("meta.json"),
            crate::gojson::marshal_indent(&meta, "  ").as_bytes(),
            0o600,
        )?;
        Ok(Meta {
            names: names.to_vec(),
            ips: ip_texts,
            not_after: rfc3339_nano(not_after, now_ns),
        })
    }
}

fn ext(arcs: &[u64], critical: bool, value: Vec<u8>) -> Vec<u8> {
    let mut parts = vec![der::oid(arcs)];
    if critical {
        parts.push(der::tlv(der::BOOLEAN, &[0xff]));
    }
    parts.push(der::tlv(der::OCTET_STRING, &value));
    der::seq(&parts)
}

/// Go's marshalKeyUsage for usages under one byte: the bits reversed,
/// with the trailing zero bits not counted.
fn key_usage(ku: u8) -> Vec<u8> {
    let b = ku.reverse_bits();
    let unused = b.trailing_zeros().min(7) as u8;
    ext(&[2, 5, 29, 15], true, der::bit_string(&[b], unused))
}

/// Go's marshalBasicConstraints: a CA with path length zero, or an empty
/// sequence for a leaf.
fn basic_constraints(is_ca: bool) -> Vec<u8> {
    let body = if is_ca {
        der::seq(&[der::tlv(der::BOOLEAN, &[0xff]), der::uint(&[0])])
    } else {
        der::seq(&[])
    };
    ext(&[2, 5, 29, 19], true, body)
}

/// A certificate signed by key, laid out as Go's CreateCertificate lays
/// it out, with a random 128-bit serial.
fn issue(
    key: &EcdsaKeyPair,
    issuer: &[u8],
    subject: &[u8],
    not_before: i64,
    not_after: i64,
    pubkey: &[u8],
    exts: Vec<Vec<u8>>,
) -> Result<Vec<u8>, String> {
    let serial = super::token::random_bytes(16)?;
    let alg = der::seq(&[der::oid(&[1, 2, 840, 10045, 4, 3, 2])]);
    let spki = der::seq(&[
        der::seq(&[
            der::oid(&[1, 2, 840, 10045, 2, 1]),
            der::oid(&[1, 2, 840, 10045, 3, 1, 7]),
        ]),
        der::bit_string(pubkey, 0),
    ]);
    let tbs = der::seq(&[
        der::tlv(0xa0, &der::uint(&[2])),
        der::uint(&serial),
        alg.clone(),
        issuer.to_vec(),
        der::seq(&[der::time(not_before), der::time(not_after)]),
        subject.to_vec(),
        spki,
        der::tlv(0xa3, &der::seq(&exts)),
    ]);
    let sig = key
        .sign(&SystemRandom::new(), &tbs)
        .map_err(|_| "cannot sign the certificate".to_string())?;
    Ok(der::seq(&[tbs, alg, der::bit_string(sig.as_ref(), 0)]))
}

/// The not-after of the first certificate in a PEM file, as Unix seconds.
pub fn load_leaf(cert_file: &Path) -> Result<i64, String> {
    let raw = super::tailscale::read_small(cert_file)?;
    for (kind, b) in der::pem_blocks(&raw) {
        if kind == "CERTIFICATE" {
            return der::parse_cert(&b)
                .map(|c| c.not_after)
                .ok_or_else(|| "x509: malformed certificate".to_string());
        }
    }
    Err(format!("{} holds no PEM certificate", cert_file.display()))
}

/// The line to log when a certificate is close to running out, or "".
pub fn expiry_warning(not_after: i64, now: i64) -> String {
    let left = not_after - now;
    if left > 14 * 24 * 3600 {
        return String::new();
    }
    if left <= 0 {
        return "The certificate has expired. Renew it, then restart coppice web serve.".into();
    }
    if left < 24 * 3600 {
        let hours = (left / 3600).max(1);
        let unit = if hours == 1 { "hour" } else { "hours" };
        return format!("The certificate expires in {hours} {unit}. Renew it before then.");
    }
    let days = left / (24 * 3600);
    let unit = if days == 1 { "day" } else { "days" };
    format!("The certificate expires in {days} {unit}. Renew it before then.")
}

/// The URL the terminal QR encodes for the CA hand-off.
pub fn ca_cert_url(host: &str, ca_listen: &str) -> String {
    let port = match super::split_host_port(ca_listen) {
        Ok((_, p)) if !p.is_empty() => p,
        _ => "8080".into(),
    };
    format!("http://{host}:{port}/ca.crt")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn times_print_as_go_prints_them() {
        assert_eq!(rfc3339_nano(0, 0), "1970-01-01T00:00:00Z");
        assert_eq!(rfc3339_nano(0, 120_000_000), "1970-01-01T00:00:00.12Z");
        assert_eq!(rfc3339(4102444800), "2100-01-01T00:00:00Z");
    }

    #[test]
    fn expiry_words() {
        assert_eq!(expiry_warning(100 * 86400, 0), "");
        assert_eq!(
            expiry_warning(86400 + 5, 0),
            "The certificate expires in 1 day. Renew it before then."
        );
        assert_eq!(
            expiry_warning(60, 0),
            "The certificate expires in 1 hour. Renew it before then."
        );
        assert_eq!(
            expiry_warning(0, 0),
            "The certificate has expired. Renew it, then restart coppice web serve."
        );
    }

    #[test]
    fn a_ca_and_a_leaf_chain() {
        let dir = std::env::temp_dir().join(format!("coppice-ca-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let ca = CaDir::new(dir.clone());
        let m = ca
            .init(&["a.test".into()], &["127.0.0.1".parse().unwrap()])
            .unwrap();
        assert_eq!(m.ips, vec!["127.0.0.1"]);
        let (leaf, _) = ca.leaf_paths();
        assert!(load_leaf(&leaf).is_ok());
        // A second init keeps the CA.
        let before = std::fs::read(dir.join("ca.crt")).unwrap();
        ca.init(&["b.test".into()], &[]).unwrap();
        assert_eq!(std::fs::read(dir.join("ca.crt")).unwrap(), before);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
