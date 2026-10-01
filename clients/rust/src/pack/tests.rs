use super::*;
use std::path::Path;

const REPO: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/../..");

#[test]
fn the_default_catalog_names_the_packs() {
    let cat = Catalog::load(None).unwrap();
    let names: Vec<&str> = cat.packs.iter().map(|p| p.name.as_str()).collect();
    assert_eq!(names, ["train", "train-cuda", "vla-ref", "vla-ref-cuda"]);
    assert_eq!(cat.protocol, PROTOCOL);
    assert_eq!(cat.python.sha256.len(), 64);
    for p in &cat.packs {
        if !p.gpu {
            assert!(!cat.lock_text(p).unwrap().is_empty());
        }
    }
}

#[test]
fn parse_lock_joins_lines_and_keeps_hashes() {
    let (a, b, c) = ("a".repeat(64), "b".repeat(64), "c".repeat(64));
    let text = format!(
        "# a comment\nFoo_Bar==1.0 \\\n    --hash=sha256:{a} \\\n    --hash=sha256:{b}\n    # via baz\ntorch==2.14.1+cpu \\\n    --hash=sha256:{c}\n"
    );
    let reqs = catalog::parse_lock(&text).unwrap();
    assert_eq!(
        reqs,
        vec![
            Req {
                name: "foo-bar".into(),
                version: "1.0".into(),
                hashes: vec![a.clone(), b]
            },
            Req {
                name: "torch".into(),
                version: "2.14.1+cpu".into(),
                hashes: vec![c]
            },
        ]
    );
    for bad in [
        "foo==1.0\n".to_string(),
        format!("foo>=1.0 --hash=sha256:{a}\n"),
        format!("foo==1.0 ; sys_platform == 'win32' --hash=sha256:{a}\n"),
        "foo==1.0 --hash=md5:abc\n".to_string(),
    ] {
        assert!(catalog::parse_lock(&bad).is_err(), "{bad}");
    }
}

#[test]
fn wheel_keys_and_names() {
    assert_eq!(catalog::normalize("Foo_Bar.baz"), "foo-bar-baz");
    assert_eq!(
        catalog::wheel_key("torch-2.14.1+cpu-cp312-cp312-manylinux_2_28_x86_64.whl"),
        Some(("torch".into(), "2.14.1+cpu".into()))
    );
    assert_eq!(
        catalog::wheel_key("Foo_Bar-1.0-1-py3-none-any.whl"),
        Some(("foo-bar".into(), "1.0".into()))
    );
    assert_eq!(catalog::wheel_key("notawheel.tar.gz"), None);
}

#[test]
fn ustar_writes_python_bytes_and_reads_them_back() {
    let files = vec![
        (format!("wheels/{}.whl", "x".repeat(95)), b"one".to_vec()),
        ("b.txt".to_string(), b"two".to_vec()),
    ];
    let b = ustar::write(&files).unwrap();
    assert_eq!(b.len(), 3072);
    assert_eq!(
        crate::gate::sha256::hexdigest(&b),
        "cf85a0b218a0e9bb759e89ad547d576f370f2f0e4b73d4083eb0270a2b5ca3ab"
    );
    let back = ustar::read(&b).unwrap();
    assert_eq!(back[0].0, "b.txt");
    assert_eq!(back[1].1, b"one");
    // A name longer than the ustar fields goes in an extended header.
    let long = format!("wheels/{}-1.0-py3-none-any.whl", "y".repeat(120));
    let pax = ustar::write(&[
        (long.clone(), b"long".to_vec()),
        ("a.txt".into(), b"a".to_vec()),
    ])
    .unwrap();
    assert_eq!(
        crate::gate::sha256::hexdigest(&pax),
        "4fd858c9e651dd835f78a85eff780cabdb930685c815c6a734f6c50551747c08"
    );
    assert_eq!(ustar::read(&pax).unwrap()[1].0, long);
    let mut evil = ustar::write(&[("a".to_string(), vec![])]).unwrap();
    evil[..8].copy_from_slice(b"../evil\0");
    ustar::fix_checksum(&mut evil[..512]);
    assert!(ustar::read(&evil).is_err());
}

fn scratch(tag: &str) -> std::path::PathBuf {
    let base = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
    let d = Path::new(&base).join(format!("pack-rs-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

#[test]
fn unpack_the_fake_cpython() {
    let data = std::fs::read(format!(
        "{REPO}/clients/fixtures/pack/assets/cpython-fake-x86_64-linux.tar.gz"
    ))
    .unwrap();
    let d = scratch("unpack");
    ustar::unpack_tar_gz(&data, &d, "x.tar.gz").unwrap();
    assert_eq!(
        std::fs::read_link(d.join("python/bin/python"))
            .unwrap()
            .to_str(),
        Some("python3")
    );
    let long = d
        .join("python/share/fake")
        .join("d".repeat(60))
        .join(format!("{}.txt", "f".repeat(40)));
    assert!(long.is_file());
    use std::os::unix::fs::PermissionsExt;
    assert!(
        std::fs::metadata(d.join("python/bin/python3"))
            .unwrap()
            .permissions()
            .mode()
            & 0o111
            != 0
    );
    std::fs::remove_dir_all(&d).unwrap();
}

fn cleanup(tag: &str) {
    let base = std::env::var("TMPDIR").unwrap_or_else(|_| "/tmp".into());
    let _ = std::fs::remove_dir_all(
        Path::new(&base).join(format!("pack-rs-{tag}-{}", std::process::id())),
    );
}

fn fake_pack(tag: &str, worker: &str) -> std::path::PathBuf {
    let d = scratch(tag).join("packs/t");
    std::fs::create_dir_all(d.join("venv/bin")).unwrap();
    std::os::unix::fs::symlink("/usr/bin/python3", d.join("venv/bin/python")).unwrap();
    std::fs::create_dir_all(d.join("worker")).unwrap();
    std::fs::write(
        d.join("worker").join(WORKER_FILE),
        if worker.is_empty() { WORKER_PY } else { worker },
    )
    .unwrap();
    d
}

fn env() -> Option<Vec<(String, String)>> {
    Some(vec![
        ("PATH".into(), "/usr/bin:/bin".into()),
        ("DAISUGI_PACK_TEST_JOBS".into(), "1".into()),
    ])
}

#[test]
fn run_job_progress_result_error_and_a_dead_worker() {
    let d = fake_pack("run", "");
    let mut seen = vec![];
    let o = run_job(
        &d,
        "t",
        "echo",
        &["a".into(), "b".into()],
        &mut |s: &str| seen.push(s.to_string()),
        &env(),
    );
    assert_eq!(o.code, 0);
    assert!(o.result.is_some());
    assert_eq!(seen, ["a", "b"]);
    let o = run_job(&d, "t", "nope", &[], &mut |_: &str| {}, &env());
    assert_eq!(
        (o.code, o.error.as_deref()),
        (1, Some("pack t: nope: no job named nope in this worker"))
    );
    let o = run_job(&d, "t", "die", &["5".into()], &mut |_: &str| {}, &env());
    assert_eq!(
        o.error.as_deref(),
        Some("pack t: the worker exited 5: die: asked to exit")
    );
    cleanup("run");
}

#[test]
fn run_job_refuses_bad_workers() {
    let d = fake_pack("bad1", "import json,sys\nprint(json.dumps({\"ready\": \"daisugi-pack-0\"}), flush=True)\nsys.stdin.read()\n");
    let o = run_job(&d, "t", "echo", &[], &mut |_: &str| {}, &env());
    assert_eq!(
        o.error.as_deref(),
        Some("pack t: the worker speaks daisugi-pack-0, not daisugi-pack-1. Install it again: daisugi pack install t --force")
    );
    let d = fake_pack(
        "bad2",
        "import json,sys\nprint(json.dumps({\"ready\": \"daisugi-pack-1\"}), flush=True)\nsys.stdin.buffer.read(4)\nprint('hello', flush=True)\nsys.stdin.read()\n",
    );
    let o = run_job(&d, "t", "echo", &[], &mut |_: &str| {}, &env());
    assert_eq!(
        o.error.as_deref(),
        Some("pack t: the worker wrote a line that is not a reply: hello")
    );
    cleanup("bad1");
    cleanup("bad2");
}
