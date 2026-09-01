//! The oracle's pathway bundle (`opendaisugi/pathway_bundle.py`): a
//! compiled pathway as a content-addressed, ed25519-signed unit that
//! travels between instances through a git registry.
//!
//! Bundles and pathways are `model_dump(mode="json")` values, so the
//! bytes hashed and signed are the oracle's bytes. The Go client's
//! `internal/bundle` is the reference.

use crate::gate::pyjson::{canonical_json_ascii, Object, Value};
use crate::gate::sha256::hexdigest;
use crate::lerr::PyError;
use crate::pathways::pathway::json_mode;
use crate::pathways::pmodel::{validate_model, Id, Mode, ValidationError};
use crate::signing;

/// `_canonical_payload`: the sorted, compact, ASCII JSON of the pathway's
/// JSON dump, the publisher and the publication time, which is hashed and
/// signed. `published_at` is written as the caller holds it (an int stays
/// an int, as in Python).
pub fn canonical_payload(pathway: &Object, publisher: &str, published_at: &Value) -> Vec<u8> {
    let body = Object::new()
        .with("pathway", json_mode(&Value::Obj(pathway.clone())))
        .with("publisher", publisher)
        .with("published_at", published_at.clone());
    canonical_json_ascii(&Value::Obj(body)).into_bytes()
}

/// `compute_bundle_hash`.
pub fn hash(pathway: &Object, publisher: &str, published_at: &Value) -> String {
    hexdigest(&canonical_payload(pathway, publisher, published_at))
}

fn as_float(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse::<f64>().unwrap_or(0.0),
        _ => 0.0,
    }
}

/// `pathway_to_bundle`: the bundle's model dump (mode="json"). `pathway`
/// is a validated CompiledPathway dump. A private key with no public key
/// is the oracle's ValueError.
pub fn to_bundle(
    pathway: &Object,
    publisher: &str,
    published_at: &Value,
    private: Option<&str>,
    public: Option<&str>,
) -> Result<Object, PyError> {
    let sig = match pathway.value("structure_signature") {
        Value::Str(s) if !s.is_empty() => s.clone(),
        _ => match pathway.value("plan_template") {
            Value::Obj(t) => crate::tracejournal::dag::structure_signature(t).unwrap_or_default(),
            _ => String::new(),
        },
    };
    let h = hash(pathway, publisher, published_at);
    let mut sig_b64 = Value::Null;
    if let Some(k) = private {
        if public.is_none() {
            return Err(PyError::new(
                "ValueError",
                "pathway_to_bundle: signing requires both private_key_b64 and public_key_b64 (the latter goes into \
                 the bundle for consumer verification)",
            ));
        }
        sig_b64 = Value::Str(signing::sign_bytes(
            &canonical_payload(pathway, publisher, published_at),
            k,
        )?);
    }
    let signer = if sig_b64.is_null() {
        Value::Null
    } else {
        Value::Str(public.unwrap_or("").to_string())
    };
    Ok(Object::new()
        .with("bundle_format_version", Value::Int("1".into()))
        .with("pathway", json_mode(&Value::Obj(pathway.clone())))
        .with("structure_signature", sig)
        .with("publisher", publisher)
        .with("published_at", as_float(published_at))
        .with("bundle_hash", h)
        .with("signature_b64", sig_b64)
        .with("signer_pubkey_b64", signer))
}

fn short(s: &str, n: usize) -> String {
    s.chars().take(n).collect()
}

const MODULE: &str = "opendaisugi.pathway_bundle.";

fn refusal(kind: &str, msg: String) -> PyError {
    PyError::new(format!("{MODULE}{kind}"), msg)
}

/// `bundle_to_pathway` on a validated bundle dump: the pathway, or the
/// oracle's refusal. `trusted` None is None (no trust set).
pub fn from_bundle(
    b: &Object,
    trusted: Option<&[String]>,
    require_signed: bool,
) -> Result<Object, PyError> {
    let h = b.value("bundle_hash").as_str().unwrap_or("");
    let pathway = || b.value("pathway").as_obj().cloned().unwrap_or_default();
    let Value::Str(sig) = b.value("signature_b64") else {
        if require_signed {
            return Err(refusal(
                "UnsignedBundleError",
                format!("bundle {} is unsigned; refusing", short(h, 12)),
            ));
        }
        return Ok(pathway());
    };
    let Value::Str(public) = b.value("signer_pubkey_b64") else {
        return Err(refusal(
            "InvalidSignatureError",
            format!(
                "bundle {} carries a signature but no signer pubkey to verify against",
                short(h, 12)
            ),
        ));
    };
    let Some(trusted) = trusted else {
        return Err(refusal(
            "UntrustedSignerError",
            format!(
                "bundle {} is signed but no trusted_pubkey_b64s was supplied to verify against; refusing (a \
                 self-supplied key proves nothing)",
                short(h, 12)
            ),
        ));
    };
    if !trusted.iter().any(|t| t == public) {
        return Err(refusal(
            "UntrustedSignerError",
            format!(
                "bundle {} signed by {}…; not in trusted-signers list",
                short(h, 12),
                short(public, 16)
            ),
        ));
    }
    let pw = pathway();
    let payload = canonical_payload(
        &pw,
        b.value("publisher").as_str().unwrap_or(""),
        b.value("published_at"),
    );
    if !signing::verify_bytes(&payload, sig, public) {
        return Err(refusal(
            "InvalidSignatureError",
            format!("bundle {} signature does not verify", short(h, 12)),
        ));
    }
    Ok(pw)
}

/// `PathwayBundle.model_validate(v)`.
pub fn validate(v: &Value) -> Result<Object, ValidationError> {
    match validate_model(Id::PathwayBundle, v, Mode::Python)? {
        Value::Obj(o) => Ok(o),
        _ => unreachable!("a model validates to an object"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gate::pyjson::loads;

    fn pathway() -> Object {
        let v = loads(
            r#"{"id": "pw_1", "task_description": "deploy", "task_embedding": [0.5, 0.0],
            "envelope": {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}},
            "plan_template": {"id": "plan_00000001", "source": "script", "task": "t",
                "steps": [{"id": "s1", "type": "shell", "command": "make"}]},
            "source_trace_ids": [], "distilled_at": 1.5}"#,
        )
        .unwrap();
        match validate_model(Id::CompiledPathway, &v, Mode::Python).unwrap() {
            Value::Obj(o) => o,
            _ => unreachable!(),
        }
    }

    #[test]
    fn a_signed_bundle_comes_back_only_under_trust() {
        let (private, public) = signing::generate_keypair().unwrap();
        let p = pathway();
        let at = Value::Float(1600000000.25);
        let b = to_bundle(&p, "team", &at, Some(&private), Some(&public)).unwrap();
        assert_eq!(
            b.value("bundle_hash").as_str().unwrap(),
            hash(&p, "team", &at)
        );
        assert_eq!(b.value("structure_signature").as_str().unwrap(), "shell");
        let b = validate(&Value::Obj(b)).unwrap();
        let trusted = vec![public.clone()];
        assert_eq!(
            from_bundle(&b, Some(&trusted), true)
                .unwrap()
                .value("id")
                .as_str(),
            Some("pw_1")
        );
        assert!(from_bundle(&b, Some(&[]), true)
            .unwrap_err()
            .typ
            .ends_with("UntrustedSignerError"));
        assert!(from_bundle(&b, None, true)
            .unwrap_err()
            .typ
            .ends_with("UntrustedSignerError"));
        let mut tampered = b.clone();
        tampered.set("publisher", "mallory");
        assert!(from_bundle(&tampered, Some(&trusted), true)
            .unwrap_err()
            .typ
            .ends_with("InvalidSignatureError"));
    }

    #[test]
    fn an_unsigned_bundle_passes_only_when_signing_is_not_required() {
        let p = pathway();
        let b = to_bundle(&p, "team", &Value::Int("7".into()), None, None).unwrap();
        assert!(b.value("signature_b64").is_null());
        assert_eq!(b.value("published_at"), &Value::Float(7.0));
        assert!(from_bundle(&b, None, true)
            .unwrap_err()
            .typ
            .ends_with("UnsignedBundleError"));
        assert!(from_bundle(&b, None, false).is_ok());
        let e = to_bundle(&p, "team", &Value::Float(1.0), Some("x"), None).unwrap_err();
        assert_eq!(e.typ, "ValueError");
    }
}
