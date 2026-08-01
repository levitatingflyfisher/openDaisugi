//! Task 3 gate: unit-test every port against the 134 oracle-generated
//! cases in `clients/fixtures/semantics.json` (committed, unlike the
//! corpus).

use daisugi_verify::glob_engine::{head_allowed, path_matches_any};
use daisugi_verify::interpreter_parse::parse_interpreter;
use daisugi_verify::models::resolve_strict_stakes;
use daisugi_verify::verify::{extract_shell_head, shell_metachar_hit};
use serde_json::Value;
use std::fs;
use std::path::PathBuf;

fn fixture() -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/semantics.json");
    let text = fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading {path:?}: {e}"));
    serde_json::from_str(&text).unwrap()
}

#[test]
fn head_allowed_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["head_allowed"].as_array().unwrap() {
        let head = case["head"].as_str().unwrap();
        let allowlist: Vec<String> = case["allowlist"].as_array().unwrap().iter().map(|v| v.as_str().unwrap().to_string()).collect();
        let expected = case["allowed"].as_bool().unwrap();
        let got = head_allowed(head, &allowlist);
        assert_eq!(got, expected, "head_allowed({head:?}, {allowlist:?})");
        n += 1;
    }
    assert_eq!(n, 27);
}

#[test]
fn path_match_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["path_match"].as_array().unwrap() {
        let path = case["path"].as_str().unwrap();
        let globs: Vec<String> = case["globs"].as_array().unwrap().iter().map(|v| v.as_str().unwrap().to_string()).collect();
        let expected = case["matched"].as_bool().unwrap();
        let got = path_matches_any(path, &globs);
        assert_eq!(got, expected, "path_matches_any({path:?}, {globs:?})");
        n += 1;
    }
    assert_eq!(n, 29);
}

#[test]
fn extract_head_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["extract_head"].as_array().unwrap() {
        let line = case["line"].as_str().unwrap();
        let expected = case["head"].as_str().map(|s| s.to_string());
        let got = extract_shell_head(line.trim());
        assert_eq!(got, expected, "extract_shell_head({line:?}.trim())");
        n += 1;
    }
    assert_eq!(n, 16);
}

#[test]
fn metachar_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["metachar"].as_array().unwrap() {
        let command = case["command"].as_str().unwrap();
        let expected = case["hit"].as_bool().unwrap();
        let got = shell_metachar_hit(command);
        assert_eq!(got, expected, "shell_metachar_hit({command:?})");
        n += 1;
    }
    assert_eq!(n, 17);
}

#[test]
fn interpreter_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["interpreter"].as_array().unwrap() {
        let command = case["command"].as_str().unwrap();
        let got = parse_interpreter(command);
        match &case["payload"] {
            Value::Null => assert!(got.is_none(), "parse_interpreter({command:?}) expected None, got {got:?}"),
            payload => {
                let got = got.unwrap_or_else(|| panic!("parse_interpreter({command:?}) expected Some, got None"));
                assert_eq!(got.head, payload["head"].as_str().unwrap(), "head mismatch for {command:?}");
                assert_eq!(got.opaque, payload["opaque"].as_bool().unwrap(), "opaque mismatch for {command:?}");
                let expected_inner: Vec<String> =
                    payload["inner_commands"].as_array().unwrap().iter().map(|v| v.as_str().unwrap().to_string()).collect();
                assert_eq!(got.inner_commands, expected_inner, "inner_commands mismatch for {command:?}");
            }
        }
        n += 1;
    }
    assert_eq!(n, 33);
}

#[test]
fn resolve_strict_matches_fixture() {
    let f = fixture();
    let mut n = 0;
    for case in f["resolve_strict"].as_array().unwrap() {
        let strict = case["strict"].as_bool();
        let stakes = case["stakes"].as_str().unwrap();
        let expected = case["effective"].as_bool().unwrap();
        let got = resolve_strict_stakes(strict, stakes);
        assert_eq!(got, expected, "resolve_strict_stakes({strict:?}, {stakes:?})");
        n += 1;
    }
    assert_eq!(n, 12);
}
