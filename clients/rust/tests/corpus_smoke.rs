//! Task 1 gate: every corpus line's `plan`/`envelope` must deserialize
//! without error. The corpus is never committed (it embeds real local
//! paths — see `docs/spec/conformance.md`), so this test reads its path
//! from `DAISUGI_CORPUS` and skips cleanly when unset, keeping the
//! committed test suite corpus-independent.

use daisugi_verify::models::{parse_plan, Envelope};
use serde_json::Value;
use std::env;
use std::fs;

#[test]
fn every_corpus_verify_case_deserializes() {
    let Ok(path) = env::var("DAISUGI_CORPUS") else {
        eprintln!("DAISUGI_CORPUS not set; skipping corpus smoke test");
        return;
    };
    let text = fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading {path}: {e}"));

    let mut total = 0usize;
    let mut verify_cases = 0usize;
    let mut decompose_cases = 0usize;
    for (lineno, line) in text.lines().enumerate() {
        if line.trim().is_empty() {
            continue;
        }
        total += 1;
        let v: Value = serde_json::from_str(line).unwrap_or_else(|e| panic!("line {lineno}: invalid JSON: {e}"));
        let kind = v["kind"].as_str().unwrap_or_else(|| panic!("line {lineno}: missing 'kind'"));
        match kind {
            "verify" => {
                verify_cases += 1;
                let plan = v.get("plan").unwrap_or_else(|| panic!("line {lineno}: missing 'plan'"));
                parse_plan(plan).unwrap_or_else(|e| panic!("line {lineno} plan: {e}"));
                let envelope = v.get("envelope").unwrap_or_else(|| panic!("line {lineno}: missing 'envelope'")).clone();
                let _: Envelope = serde_json::from_value(envelope).unwrap_or_else(|e| panic!("line {lineno} envelope: {e}"));
            }
            "decompose" => {
                decompose_cases += 1;
                assert!(v.get("command").and_then(|c| c.as_str()).is_some(), "line {lineno}: missing 'command'");
            }
            other => panic!("line {lineno}: unknown kind {other:?}"),
        }
    }
    eprintln!("corpus smoke: {total} cases ({verify_cases} verify, {decompose_cases} decompose) all deserialized");
    assert!(total > 0, "corpus at {path} was empty");
}
