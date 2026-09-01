//! `daisugi release keygen|sign|verify` (`opendaisugi/release.py` and the
//! CLI over it): ed25519 release-signing keys and signed SHA-256
//! manifests, on the one key system the pathway bundles use. The Go
//! client's `cli/releasecmd.go` is the reference.

use super::gateroot::{join, path_str};
use super::lprobe::sort_keys;
use super::registrycmd::{mkdir_parents_py, read_text_strip};
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::pyjson::{
    canonical_json_ascii, dumps_indent, loads_py, py_str, py_type_name, Object, Value,
};
use crate::gate::sha256::hexdigest;
use crate::lerr::{LErr, PyError};
use crate::signing::{self, Registry};

const RELEASE_HELP: &str = "Usage: daisugi release [OPTIONS] COMMAND [ARGS]...

  Signed release manifests: an ed25519 keypair, a signed SHA-256 manifest
  over the artifacts, and its verification against trusted signers.

Commands:
  keygen  Generate an ed25519 release-signing keypair.
  sign    Build a SHA-256 manifest over ARTIFACTS and sign it.
  verify  Verify a release: trusted signature AND intact artifacts.
";

/// `Path.cwd()`.
fn cwd() -> String {
    std::env::current_dir()
        .map(|d| d.to_string_lossy().into_owned())
        .unwrap_or_else(|_| ".".into())
}

/// `Path(p).name`.
fn path_name(p: &str) -> String {
    let p = path_str(p);
    if p == "." {
        return String::new();
    }
    p.rsplit('/').next().unwrap_or(&p).to_string()
}

/// `release.duplicate_names`: the file names more than one artifact has,
/// sorted.
fn duplicate_names(arts: &[String]) -> Vec<String> {
    let names: Vec<String> = arts.iter().map(|a| path_name(a)).collect();
    let mut out: Vec<String> = names
        .iter()
        .filter(|n| names.iter().filter(|m| m == n).count() > 1)
        .cloned()
        .collect();
    out.sort();
    out.dedup();
    out
}

/// `datetime.now(timezone.utc).isoformat()` with "+00:00" as "Z": the
/// microseconds only when they are not zero.
fn iso_now() -> String {
    let d = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    let mut s = crate::tracejournal::iso_seconds(d.as_secs() as i64);
    let us = d.subsec_micros();
    if us != 0 {
        s.push_str(&format!(".{us:06}"));
    }
    s + "Z"
}

/// `release.sha256_file`: the hex digest and the size.
fn sha256_file(path: &str) -> Result<(String, u64), LErr> {
    let b = std::fs::read(path).map_err(|e| LErr::io(e, path))?;
    Ok((hexdigest(&b), b.len() as u64))
}

/// `canonicalize_manifest`: the manifest without its signature, as sorted
/// compact ASCII JSON.
fn canonical_manifest(m: &Object) -> Vec<u8> {
    let mut body = Object::new();
    for (k, v) in m.iter() {
        if k != "signature" {
            body.set(k, v.clone());
        }
    }
    canonical_json_ascii(&Value::Obj(body)).into_bytes()
}

fn type_error(msg: &str) -> LErr {
    PyError::new("TypeError", msg).into()
}

/// `verify_artifacts`: the names missing or changed under `dir`.
fn verify_artifacts(m: &Object, dir: &str) -> Result<Vec<String>, LErr> {
    let Some(raw) = m.get("artifacts") else {
        return Ok(vec![]);
    };
    let list = match raw {
        Value::List(l) => l,
        // Iterating a dict gives its keys; entry["name"] on a str raises
        // TypeError.
        Value::Obj(o) if o.is_empty() => return Ok(vec![]),
        Value::Str(s) if s.is_empty() => return Ok(vec![]),
        Value::Obj(_) | Value::Str(_) => {
            return Err(type_error("string indices must be integers, not 'str'"))
        }
        other => {
            return Err(type_error(&format!(
                "'{}' object is not iterable",
                py_type_name(other)
            )))
        }
    };
    let mut bad = vec![];
    for it in list {
        let Value::Obj(en) = it else {
            return Err(type_error("an artifact entry is not a dict"));
        };
        let Some(nv) = en.get("name") else {
            return Err(PyError::new("KeyError", "'name'").into());
        };
        let Value::Str(name) = nv else {
            return Err(type_error(&format!(
                "unsupported operand type(s) for /: 'PosixPath' and '{}'",
                py_type_name(nv)
            )));
        };
        let path = if name.starts_with('/') {
            name.clone()
        } else {
            join(dir, name)
        };
        if std::fs::metadata(&path).is_err() {
            bad.push(name.clone());
            continue;
        }
        let (sum, _) = sha256_file(&path)?;
        let Some(want) = en.get("sha256") else {
            return Err(PyError::new("KeyError", "'sha256'").into());
        };
        if want.as_str() != Some(sum.as_str()) {
            bad.push(name.clone());
        }
    }
    Ok(bad)
}

impl Env {
    pub(super) fn release_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(RELEASE_HELP);
            return Ok(());
        }
        let rest = &args[1..];
        match args[0].as_str() {
            "keygen" => self.release_keygen(rest),
            "sign" => self.release_sign(rest),
            "verify" => self.release_verify(rest),
            other => {
                self.errf(&format!(
                    "Usage: daisugi release [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi release --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    fn release_keygen(&mut self, args: &[String]) -> Res {
        const CMD: &str = "release keygen";
        let opts = [
            Opt::val(
                &["--out-dir"],
                "PATH",
                "Directory to write the keypair into.",
            ),
            Opt::val(&["--name"], "TEXT", "Key file basename."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Generate an ed25519 release-signing keypair.",
                &opts,
            );
        }
        let out_dir = path_str(&p.str("--out-dir", &cwd()));
        let name = p.str("--name", "release_signing");
        if let Err(e) = mkdir_parents_py(&out_dir) {
            return self.raise(CMD, e);
        }
        let (private, public) = match signing::generate_keypair() {
            Ok(k) => k,
            Err(why) => return self.fail(CMD, &why),
        };
        let priv_path = path_str(&join(&out_dir, &format!("{name}.key")));
        let pub_path = path_str(&join(&out_dir, &format!("{name}.pub")));
        let write =
            |path: &str, text: String| std::fs::write(path, text).map_err(|e| LErr::io(e, path));
        if let Err(e) = write(&priv_path, format!("{private}\n")) {
            return self.raise(CMD, e);
        }
        {
            use std::os::unix::fs::PermissionsExt;
            if let Err(e) =
                std::fs::set_permissions(&priv_path, std::fs::Permissions::from_mode(0o600))
            {
                return self.raise(CMD, LErr::io(e, &priv_path));
            }
        }
        if let Err(e) = write(&pub_path, format!("{public}\n")) {
            return self.raise(CMD, e);
        }
        self.out(&format!(
            "private key → {priv_path} (chmod 600 — keep it offline)\n"
        ));
        self.out(&format!("public key  → {pub_path}\n"));
        self.out(&format!("public key (b64): {public}\n"));
        Ok(())
    }

    fn release_sign(&mut self, args: &[String]) -> Res {
        const CMD: &str = "release sign";
        let opts = [
            Opt::val(&["--version"], "TEXT", "Release version string."),
            Opt::val(
                &["--key"],
                "PATH",
                "Path to the base64 ed25519 private key.",
            ),
            Opt::val(
                &["--signer"],
                "TEXT",
                "Signer name to bind into the manifest.",
            ),
            Opt::val(
                &["--out", "-o"],
                "PATH",
                "Where to write the signed manifest.",
            ),
        ];
        let p = match parse_args(args, &opts, usize::MAX) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ARTIFACTS...", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " ARTIFACTS...",
                "Build a SHA-256 manifest over ARTIFACTS and sign it.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "ARTIFACTS...", "Missing argument 'artifacts'.");
        }
        for o in ["--version", "--key", "--signer"] {
            if !p.has(o) {
                return self.usage_args(CMD, "ARTIFACTS...", &format!("Missing option '{o}'."));
            }
        }
        let arts: Vec<String> = p.args.iter().map(|a| path_str(a)).collect();
        let missing: Vec<String> = arts
            .iter()
            .filter(|a| std::fs::metadata(a).is_err())
            .cloned()
            .collect();
        if !missing.is_empty() {
            self.errf(&format!("no such artifact(s): {}\n", missing.join(", ")));
            return exit(2);
        }
        let dup = duplicate_names(&arts);
        if !dup.is_empty() {
            self.errf(&format!(
                "two artifacts share a name: {} (the manifest names each artifact by its file name)\n",
                dup.join(", ")
            ));
            return exit(2);
        }
        let created = iso_now();
        let mut entries = vec![];
        for a in &arts {
            match sha256_file(a) {
                Ok((sum, n)) => entries.push((path_name(a), sum, n)),
                Err(e) => return self.raise(CMD, e),
            }
        }
        entries.sort_by(|x, y| x.0.cmp(&y.0));
        let list: Vec<Value> = entries
            .iter()
            .map(|(n, s, z)| {
                Value::Obj(
                    Object::new()
                        .with("name", n.as_str())
                        .with("sha256", s.as_str())
                        .with("size", Value::Int(z.to_string())),
                )
            })
            .collect();
        let private = match read_text_strip(&path_str(&p.str("--key", ""))) {
            Ok(t) => t,
            Err(e) => return self.raise(CMD, e),
        };
        let mut m = Object::new()
            .with("manifest_version", Value::Int("1".into()))
            .with("version", p.str("--version", ""))
            .with("created_at", created)
            .with("artifacts", Value::List(list))
            .with("signer", p.str("--signer", ""))
            .with("signature", Value::Null);
        let sig = match signing::sign_bytes(&canonical_manifest(&m), &private) {
            Ok(s) => s,
            Err(e) => return self.raise(CMD, e.into()),
        };
        m.set("signature", sig);
        let out = path_str(&p.str("--out", "release-manifest.json"));
        let text = dumps_indent(&sort_keys(&Value::Obj(m)), 2, true) + "\n";
        if let Err(e) = std::fs::write(&out, text) {
            return self.raise(CMD, LErr::io(e, &out));
        }
        self.out(&format!(
            "signed manifest ({} artifacts) → {out}\n",
            entries.len()
        ));
        Ok(())
    }

    fn release_verify(&mut self, args: &[String]) -> Res {
        const CMD: &str = "release verify";
        let opts = [
            Opt::val(
                &["--artifact-dir"],
                "PATH",
                "Directory holding the artifacts to check.",
            ),
            Opt::many(
                &["--signer"],
                "TEXT",
                "Trusted signer name(s) to accept. Repeatable.",
            ),
            Opt::val(&["--registry"], "PATH", "Trusted-signer registry JSON."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "MANIFEST_PATH", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " MANIFEST_PATH",
                "Verify a release: trusted signature AND intact artifacts. Fails closed.",
                &opts,
            );
        }
        let Some(manifest_path) = p.args.first().cloned() else {
            return self.usage_args(CMD, "MANIFEST_PATH", "Missing argument 'manifest_path'.");
        };
        let reg_path = path_str(&p.str(
            "--registry",
            &join(&self.home, ".opendaisugi/trusted_signers.json"),
        ));
        let reg = match Registry::load(&reg_path) {
            Ok(r) => r,
            Err(e) => return self.raise(CMD, e),
        };
        let mut names = p.list("--signer");
        if names.is_empty() {
            names = reg.names();
        }
        if names.is_empty() {
            self.errf(&format!(
                "no trusted signers: add the release pubkey to {reg_path} or pass --signer\n"
            ));
            return exit(1);
        }
        let mp = path_str(&manifest_path);
        let raw = match std::fs::read(&mp) {
            Ok(r) => r,
            Err(e) => return self.raise(CMD, LErr::io(e, &mp)),
        };
        let Ok(text) = String::from_utf8(raw) else {
            return self.refuse(CMD, "the manifest is not UTF-8");
        };
        let m = match loads_py(&text, 900) {
            Ok(Value::Obj(o)) => o,
            Ok(other) => {
                let e = PyError::new(
                    "AttributeError",
                    format!("'{}' object has no attribute 'get'", py_type_name(&other)),
                );
                return self.raise(CMD, e.into());
            }
            Err(msg) => {
                return self.raise(
                    CMD,
                    PyError::new("json.decoder.JSONDecodeError", msg).into(),
                )
            }
        };
        let signer = m.value("signer").clone();
        let mut sig_ok = false;
        for n in &names {
            let Some(public) = reg.entries.get(n) else {
                continue;
            };
            if public.is_null() {
                continue;
            }
            let sig = m.value("signature");
            if !sig.truthy() {
                continue;
            }
            if let Value::Str(s) = sig {
                if signing::verify_with(&canonical_manifest(&m), s, public) {
                    sig_ok = true;
                    break;
                }
            }
        }
        let bad = match verify_artifacts(&m, &path_str(&p.str("--artifact-dir", &cwd()))) {
            Ok(b) => b,
            Err(e) => return self.raise(CMD, e),
        };
        let reason = if !sig_ok {
            "signature not verifiable under any trusted signer".to_string()
        } else if !bad.is_empty() {
            format!("artifact hash mismatch: {}", bad.join(", "))
        } else {
            "signature valid and all artifacts intact".to_string()
        };
        if sig_ok && bad.is_empty() {
            self.out(&format!(
                "OK — {reason} (signer: {})\n",
                py_str(&signer).unwrap_or_default()
            ));
            return Ok(());
        }
        self.errf(&format!("FAILED — {reason}\n"));
        exit(1)
    }
}
