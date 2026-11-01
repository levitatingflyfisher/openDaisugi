//! `write_paths.shell_accesses` against the oracle: the cases in
//! `clients/fixtures/dialect/accesses.json`, and the tree-coverage rule.

use daisugi_verify::dialect::shell_accesses;
use daisugi_verify::glob_engine::{covers_everything, covers_tree};
use serde_json::Value;
use std::fs;
use std::path::PathBuf;

fn fixture() -> Value {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/dialect/accesses.json");
    serde_json::from_str(&fs::read_to_string(p).unwrap()).unwrap()
}

fn key(path: &str, tree: bool, via: &str) -> String {
    format!("{path}\0{}\0{via}", if tree { "tree" } else { "path" })
}

fn want(v: &Value) -> Vec<String> {
    let mut out: Vec<String> =
        v.as_array().unwrap().iter().map(|t| key(t[0].as_str().unwrap(), t[1].as_bool().unwrap(), t[2].as_str().unwrap())).collect();
    out.sort();
    out
}

#[test]
fn accesses_match_the_oracle() {
    let fx = fixture();
    for c in fx["cases"].as_array().unwrap() {
        let command = c["command"].as_str().unwrap();
        let base = c["base"].as_str();
        let got = shell_accesses(command, base);
        let mut reads: Vec<String> = got.reads.iter().map(|a| key(&a.path, a.tree, a.via)).collect();
        let mut writes: Vec<String> = got.writes.iter().map(|a| key(&a.path, a.tree, a.via)).collect();
        reads.sort();
        writes.sort();
        assert_eq!(reads, want(&c["reads"]), "{command:?} base {base:?} reads");
        assert_eq!(writes, want(&c["writes"]), "{command:?} base {base:?} writes");
        assert_eq!(got.reads_unknown.as_deref(), c["reads_unknown"].as_str(), "{command:?} base {base:?} reads_unknown");
        assert_eq!(got.writes_unknown.as_deref(), c["writes_unknown"].as_str(), "{command:?} base {base:?} writes_unknown");
    }
}

fn g(globs: &[&str]) -> Vec<String> {
    globs.iter().map(|s| s.to_string()).collect()
}

#[test]
fn a_tree_needs_a_covering_glob() {
    for (dir, globs, ok) in [
        ("/work/out", vec!["/work/**"], true),
        ("/work", vec!["/work/**"], true),
        ("/work/a/out", vec!["/work/*/out/**"], true),
        ("/work/out", vec!["/work/*.py"], false),
        ("/work/out", vec!["/work/out"], false),
        ("/work/out", vec!["/work/out/*"], false),
        ("/", vec!["/**"], true),
        ("/", vec!["**"], true),
        ("/etc", vec!["/work/**", "/home/**"], false),
        ("out", vec!["out/**"], true),
    ] {
        assert_eq!(covers_tree(dir, &g(&globs)), ok, "{dir} {globs:?}");
    }
    assert!(covers_everything(&g(&["x", "**"])) && covers_everything(&g(&["/**"])));
    assert!(!covers_everything(&g(&["/**/*"])) && !covers_everything(&g(&["/home/**"])));
}
