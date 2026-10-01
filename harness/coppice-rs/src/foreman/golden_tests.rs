//! The Go golden fixtures, played in Rust: every file under out/ must be
//! the same bytes (harness/coppice/internal/foreman/testdata/golden).

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde_json::Value;

use super::chat::{parse_date, Chat, Config};
use super::chat_tests::{temp_dir, Fake, TestClock};
use super::ctx::Ctx;
use super::fsutil::json_str;

fn golden_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../coppice/internal/foreman/testdata/golden")
}

/// Every file under root but the lock, by its path relative to root.
fn files_under(root: &Path) -> BTreeMap<String, Vec<u8>> {
    fn walk(root: &Path, d: &Path, out: &mut BTreeMap<String, Vec<u8>>) {
        for e in std::fs::read_dir(d).unwrap().flatten() {
            let p = e.path();
            if p.is_dir() {
                walk(root, &p, out);
            } else if p.file_name().is_some_and(|n| n != "lock") {
                let rel = p.strip_prefix(root).unwrap().to_string_lossy().into_owned();
                out.insert(rel, std::fs::read(&p).unwrap());
            }
        }
    }
    let mut out = BTreeMap::new();
    walk(root, root, &mut out);
    out
}

#[test]
fn the_go_golden_fixtures_are_the_same_bytes() {
    let script: Value =
        serde_json::from_slice(&std::fs::read(golden_dir().join("script.json")).unwrap()).unwrap();
    let cfgv = &script["config"];
    let num = |v: &Value| v.as_u64().unwrap() as usize;
    let dir = temp_dir();
    let clk = TestClock::new();
    super::fsutil::NO_SYNC.store(true, std::sync::atomic::Ordering::Relaxed);
    let mut c = Chat::open(Config {
        dir: dir.clone(),
        now: Some(clk.clock()),
        node_bytes: num(&cfgv["node_bytes"]),
        view_max: num(&cfgv["view_max"]),
        view_min: num(&cfgv["view_min"]),
        split_bytes: num(&cfgv["split_bytes"]),
    })
    .unwrap();
    let fake = Fake::default();
    let mut views = String::new();
    for (k, m) in script["messages"].as_array().unwrap().iter().enumerate() {
        clk.set(parse_date(m["date"].as_str().unwrap()).unwrap());
        let (_, err) = c.append(m["kind"].as_str().unwrap(), m["text"].as_str().unwrap());
        assert!(err.is_none(), "message {k}: {err:?}");
        c.drain(&Ctx::background(), &fake).unwrap();
        views.push_str(&format!(
            "{{\"step\":{k},\"size\":{},\"view\":{}}}\n",
            c.view_size(),
            c.view_json().trim_end_matches('\n')
        ));
    }
    let mut probes = Vec::new();
    for p in script["probes"].as_array().unwrap() {
        let op = p["op"].as_str().unwrap();
        let id = p["id"].as_i64().unwrap();
        let n = p.get("n").and_then(Value::as_i64).unwrap_or(0);
        let res = match op {
            "zoom" => c.zoom(id, n),
            _ => c.date(id),
        };
        let mut fields = vec![
            format!("    \"op\": {}", json_str(op)),
            format!("    \"id\": {id}"),
        ];
        if n != 0 {
            fields.push(format!("    \"n\": {n}"));
        }
        match res {
            Ok(out) if !out.is_empty() => fields.push(format!("    \"out\": {}", json_str(&out))),
            Ok(_) => {}
            Err(_) => fields.push("    \"err\": true".to_string()),
        }
        probes.push(format!("  {{\n{}\n  }}", fields.join(",\n")));
    }
    let mut got = files_under(&dir);
    got.insert("views.jsonl".into(), views.into_bytes());
    got.insert("view.txt".into(), c.render().into_bytes());
    got.insert(
        "probes.json".into(),
        format!("[\n{}\n]\n", probes.join(",\n")).into_bytes(),
    );
    let want = files_under(&golden_dir().join("out"));
    let names: std::collections::BTreeSet<&String> = want.keys().chain(got.keys()).collect();
    for name in names {
        assert!(
            got.get(name) == want.get(name),
            "golden {name} differs (have {} bytes, want {})",
            got.get(name).map_or(0, Vec::len),
            want.get(name).map_or(0, Vec::len)
        );
    }
}
