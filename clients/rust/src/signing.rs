//! The oracle's ed25519 signing (`opendaisugi/signing.py`): base64 keys
//! and signatures read as Python's `base64.b64decode` reads them,
//! `sign_bytes` and `verify_bytes`, the trusted-signer registry, and a
//! contract's canonical bytes.
//!
//! Key format, as `signing.generate_keypair` writes it: the private key
//! is the raw 32-byte seed in base64, the public key the raw 32 bytes in
//! base64. The Go client's `internal/signing` is the reference.

use ring::signature::{Ed25519KeyPair, KeyPair, UnparsedPublicKey, ED25519};

use crate::gate::pyjson::{
    canonical_json_ascii, dumps_indent, loads_py, py_type_name, Object, Value,
};
use crate::lerr::{LErr, PyError, LR};

const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

fn b64_value(c: u8) -> Option<u8> {
    ALPHABET.iter().position(|&a| a == c).map(|i| i as u8)
}

/// `base64.b64decode(s)` on a str: CPython's `binascii.a2b_base64` in its
/// lenient mode. A character outside the alphabet is skipped; a pad
/// sequence that completes a quad ends the input; what follows it is
/// ignored.
pub fn b64decode(s: &str) -> Result<Vec<u8>, PyError> {
    if !s.is_ascii() {
        return Err(PyError {
            typ: "ValueError".into(),
            msg: "string argument should contain only ASCII characters".into(),
            context: "UnicodeEncodeError: 'ascii' codec can't encode a character: ordinal not in range(128)".into(),
        });
    }
    let mut out = vec![];
    let (mut quad, mut pads) = (0, 0);
    let mut left = 0u8;
    for &c in s.as_bytes() {
        if c == b'=' {
            if quad >= 2 {
                pads += 1;
                if quad + pads >= 4 {
                    return Ok(out);
                }
            }
            continue;
        }
        let Some(v) = b64_value(c) else { continue };
        pads = 0;
        match quad {
            0 => {
                quad = 1;
                left = v;
            }
            1 => {
                quad = 2;
                out.push(left << 2 | v >> 4);
                left = v & 0x0f;
            }
            2 => {
                quad = 3;
                out.push(left << 4 | v >> 2);
                left = v & 0x03;
            }
            _ => {
                quad = 0;
                out.push(left << 6 | v);
                left = 0;
            }
        }
    }
    match quad {
        0 => Ok(out),
        1 => Err(PyError::new(
            "binascii.Error",
            format!(
                "Invalid base64-encoded string: number of data characters ({}) cannot be 1 more than a multiple of 4",
                out.len() / 3 * 4 + 1
            ),
        )),
        _ => Err(PyError::new("binascii.Error", "Incorrect padding")),
    }
}

/// `base64.b64encode(b).decode("ascii")`.
pub fn b64encode(b: &[u8]) -> String {
    let mut out = String::with_capacity(b.len().div_ceil(3) * 4);
    for chunk in b.chunks(3) {
        let n = chunk.len();
        let x = (chunk[0] as u32) << 16
            | (*chunk.get(1).unwrap_or(&0) as u32) << 8
            | *chunk.get(2).unwrap_or(&0) as u32;
        for i in 0..4 {
            if i <= n {
                out.push(ALPHABET[(x >> (18 - 6 * i) & 0x3f) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

/// `Ed25519PrivateKey.from_private_bytes(b64decode(text))`.
fn private_key(priv_b64: &str) -> Result<Ed25519KeyPair, PyError> {
    let seed = b64decode(priv_b64)?;
    if seed.len() != 32 {
        return Err(PyError::new(
            "ValueError",
            "An Ed25519 private key is 32 bytes long",
        ));
    }
    Ed25519KeyPair::from_seed_unchecked(&seed)
        .map_err(|e| PyError::new("ValueError", e.to_string()))
}

/// `sign_bytes`: the base64 ed25519 signature over `payload`.
pub fn sign_bytes(payload: &[u8], priv_b64: &str) -> Result<String, PyError> {
    let k = private_key(priv_b64)?;
    Ok(b64encode(k.sign(payload).as_ref()))
}

/// The base64 public key of a base64 private key.
pub fn public_of(priv_b64: &str) -> Result<String, PyError> {
    let k = private_key(priv_b64)?;
    Ok(b64encode(k.public_key().as_ref()))
}

/// `verify_bytes`: false on any failure (bad encoding, a key that is not
/// 32 bytes, a signature that does not verify), never an error.
pub fn verify_bytes(payload: &[u8], sig_b64: &str, pub_b64: &str) -> bool {
    let Ok(pk) = b64decode(pub_b64) else {
        return false;
    };
    if pk.len() != 32 {
        return false;
    }
    let Ok(sig) = b64decode(sig_b64) else {
        return false;
    };
    if sig.len() != 64 {
        return false;
    }
    UnparsedPublicKey::new(&ED25519, &pk)
        .verify(payload, &sig)
        .is_ok()
}

/// `generate_keypair`: (private_b64, public_b64), a fresh seed from the
/// system's random source.
pub fn generate_keypair() -> Result<(String, String), String> {
    use ring::rand::SecureRandom;
    let mut seed = [0u8; 32];
    ring::rand::SystemRandom::new()
        .fill(&mut seed)
        .map_err(|_| "no random key".to_string())?;
    let k = Ed25519KeyPair::from_seed_unchecked(&seed).map_err(|e| e.to_string())?;
    Ok((b64encode(&seed), b64encode(k.public_key().as_ref())))
}

/// `verify_bytes` with a registry value as the key. A value that is not a
/// str raises TypeError in b64decode, which verify_bytes catches: false.
pub fn verify_with(payload: &[u8], sig_b64: &str, pub_key: &Value) -> bool {
    match pub_key {
        Value::Str(s) => verify_bytes(payload, sig_b64, s),
        _ => false,
    }
}

/// `TrustedSignerRegistry`: signer name to base64 public key, the values
/// as the JSON file holds them (a value may be something other than a
/// str; the oracle keeps it as it is).
pub struct Registry {
    pub path: String,
    pub entries: Object,
}

impl Registry {
    /// `TrustedSignerRegistry.load`: an absent file is an empty registry;
    /// a file that is not a JSON object is an error, as the oracle raises.
    pub fn load(path: &str) -> LR<Registry> {
        let raw = match std::fs::read(path) {
            Ok(r) => r,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Registry {
                    path: path.into(),
                    entries: Object::new(),
                })
            }
            Err(e) => return Err(LErr::io(e, path)),
        };
        let text =
            String::from_utf8(raw).map_err(|_| LErr::Unread(format!("{path} is not UTF-8")))?;
        let v =
            loads_py(&text, 900).map_err(|m| PyError::new("json.decoder.JSONDecodeError", m))?;
        match v {
            Value::Obj(o) => Ok(Registry {
                path: path.into(),
                entries: o,
            }),
            other => Err(PyError::new(
                "ValueError",
                format!("{path}: expected dict, got {}", py_type_name(&other)),
            )
            .into()),
        }
    }

    /// `save`: `json.dumps(sort_keys=True, indent=2)`.
    pub fn save(&self) -> std::io::Result<()> {
        if let Some(i) = self.path.rfind('/') {
            if i > 0 {
                std::fs::create_dir_all(&self.path[..i])?;
            }
        }
        let mut sorted = Object::new();
        for k in self.names() {
            sorted.set(&k, self.entries.value(&k).clone());
        }
        std::fs::write(&self.path, dumps_indent(&Value::Obj(sorted), 2, true))
    }

    pub fn add(&mut self, name: &str, pub_b64: &str) {
        self.entries.set(name, pub_b64);
    }

    /// Deletes `name`; whether it was there.
    pub fn remove(&mut self, name: &str) -> bool {
        if self.entries.get(name).is_none() {
            return false;
        }
        self.entries.remove(name);
        true
    }

    /// The names, sorted.
    pub fn names(&self) -> Vec<String> {
        let mut out = self.entries.keys().to_vec();
        out.sort();
        out
    }

    /// `TrustedSignerRegistry.verify`: true when the contract's signature
    /// verifies under any named key; unknown names are skipped.
    pub fn verify_named(&self, c: &Object, names: &[String]) -> bool {
        if c.value("signature").as_str().is_none() {
            return false;
        }
        names.iter().any(|n| match self.entries.get(n) {
            None | Some(Value::Null) => false,
            Some(pk) => verify_contract(c, pk),
        })
    }
}

/// `canonicalize_contract`: the contract's JSON dump without its
/// signature and signer, as sorted compact ASCII JSON.
pub fn canonical_contract(c: &Object) -> Vec<u8> {
    let mut body = Object::new();
    for (k, v) in c.iter() {
        if k != "signature" && k != "signer" {
            body.set(k, crate::pathways::pathway::json_mode(v));
        }
    }
    canonical_json_ascii(&Value::Obj(body)).into_bytes()
}

/// `sign_contract`.
pub fn sign_contract(c: &Object, priv_b64: &str) -> Result<String, PyError> {
    sign_bytes(&canonical_contract(c), priv_b64)
}

/// `verify_signature_raw`: false for a contract with no signature, and on
/// any failure.
pub fn verify_contract(c: &Object, pub_key: &Value) -> bool {
    match c.value("signature") {
        Value::Str(sig) => verify_with(&canonical_contract(c), sig, pub_key),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn b64_reads_as_cpython_reads() {
        assert_eq!(b64decode("aGk=").unwrap(), b"hi");
        assert_eq!(b64decode("a!G k=").unwrap(), b"hi");
        assert_eq!(b64decode("aGk=QUJD").unwrap(), b"hi");
        assert_eq!(b64decode("abc").unwrap_err().msg, "Incorrect padding");
        assert!(b64decode("A")
            .unwrap_err()
            .msg
            .contains("(1) cannot be 1 more"));
        assert_eq!(b64decode("é").unwrap_err().typ, "ValueError");
        assert_eq!(b64encode(b"hi"), "aGk=");
        assert_eq!(b64encode(b""), "");
        assert_eq!(b64encode(b"abc"), "YWJj");
    }

    #[test]
    fn a_signature_verifies_under_its_key_only() {
        let (priv_a, pub_a) = generate_keypair().unwrap();
        let (_, pub_b) = generate_keypair().unwrap();
        assert_eq!(public_of(&priv_a).unwrap(), pub_a);
        let sig = sign_bytes(b"payload", &priv_a).unwrap();
        assert!(verify_bytes(b"payload", &sig, &pub_a));
        assert!(!verify_bytes(b"payload", &sig, &pub_b));
        assert!(!verify_bytes(b"other", &sig, &pub_a));
        assert!(!verify_bytes(b"payload", "@@", &pub_a));
        assert_eq!(
            sign_bytes(b"x", &b64encode(&[1; 31])).unwrap_err().typ,
            "ValueError"
        );
    }

    #[test]
    fn the_rfc8032_vector_signs_byte_for_byte() {
        // RFC 8032, 7.1, TEST 1.
        let seed = [
            0x9d, 0x61, 0xb1, 0x9d, 0xef, 0xfd, 0x5a, 0x60, 0xba, 0x84, 0x4a, 0xf4, 0x92, 0xec,
            0x2c, 0xc4, 0x44, 0x49, 0xc5, 0x69, 0x7b, 0x32, 0x69, 0x19, 0x70, 0x3b, 0xac, 0x03,
            0x1c, 0xae, 0x7f, 0x60,
        ];
        let sig = sign_bytes(b"", &b64encode(&seed)).unwrap();
        assert!(sig.starts_with("5VZDAMNgrHKQhuLMgG6CioSHfx645dl02HPgZSJJAVVfuIIVkKM7rMYeOXAc"));
        assert_eq!(
            public_of(&b64encode(&seed)).unwrap(),
            "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
        );
    }
}
