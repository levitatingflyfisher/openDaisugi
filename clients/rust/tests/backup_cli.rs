//! `daisugi backup` and `daisugi restore` through the real binary, in a
//! fake HOME. The Go client's backupcmd_test.go has the same cases.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

/// A directory that goes away on drop, on failure too.
struct Home(PathBuf);

impl Home {
    fn new(tag: &str) -> Home {
        let p = PathBuf::from(env!("CARGO_TARGET_TMPDIR")).join(format!("backup-cli-{}-{tag}", std::process::id()));
        let _ = fs::remove_dir_all(&p);
        fs::create_dir_all(&p).unwrap();
        Home(p)
    }
}

impl Drop for Home {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

fn seed(home: &Path, rel: &str, body: &str) {
    let p = home.join(".opendaisugi").join(rel);
    fs::create_dir_all(p.parent().unwrap()).unwrap();
    fs::write(p, body).unwrap();
}

/// A real SQLite file with one row, which a backup copies with VACUUM INTO.
fn seed_db(home: &Path, rel: &str, val: &str) {
    let p = home.join(".opendaisugi").join(rel);
    fs::create_dir_all(p.parent().unwrap()).unwrap();
    let con = rusqlite::Connection::open(p).unwrap();
    con.execute_batch("CREATE TABLE t (v TEXT)").unwrap();
    con.execute("INSERT INTO t VALUES (?1)", [val]).unwrap();
}

fn read_db(p: &Path) -> String {
    let con = rusqlite::Connection::open_with_flags(p, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY).unwrap();
    con.query_row("SELECT v FROM t", [], |r| r.get(0)).unwrap()
}

fn run(home: &Path, cwd: &Path, args: &[&str]) -> (i32, String, String) {
    let o = Command::new(env!("CARGO_BIN_EXE_daisugi"))
        .args(args)
        .env_clear()
        .env("HOME", home)
        .env("PATH", "/usr/bin:/bin")
        .current_dir(cwd)
        .output()
        .unwrap();
    (
        o.status.code().unwrap_or(-1),
        String::from_utf8_lossy(&o.stdout).into_owned(),
        String::from_utf8_lossy(&o.stderr).into_owned(),
    )
}

#[test]
fn backup_then_restore_into_a_fresh_home() {
    let (home, work, fresh) = (Home::new("h"), Home::new("w"), Home::new("f"));
    seed(&home.0, "config.yaml", "matcher_model: lexical\n");
    seed_db(&home.0, "pathways.db", "p1");
    seed_db(&home.0, "journal/index.db", "idx");
    seed(&home.0, "journal/index.db-wal", "torn");
    seed(&home.0, "pathways.db-shm", "torn");
    seed(&home.0, "gate/envelopes/s.json", "{}");
    seed(&home.0, "gate/audit/2026.jsonl", "{}\n");
    seed(&home.0, "gate/disarmed", "");
    seed(&home.0, "sessions/a.json", "private");
    seed(&home.0, "gate/api-token", "SECRET");

    let (code, out, err) = run(&home.0, &work.0, &["backup"]);
    assert!(code == 0 && out.contains("5 files"), "{code} {out} {err}");
    let names: Vec<_> = fs::read_dir(&work.0).unwrap().map(|e| e.unwrap().file_name().to_string_lossy().into_owned()).collect();
    assert_eq!(names.len(), 1, "{names:?}");
    assert!(names[0].starts_with("daisugi-backup-") && names[0].ends_with(".tgz"), "{names:?}");

    let file = work.0.join(&names[0]);
    let (code, out, err) = run(&fresh.0, &work.0, &["restore", file.to_str().unwrap()]);
    assert!(code == 0 && out.contains("5 files"), "{code} {out} {err}");
    let nd = fresh.0.join(".opendaisugi");
    assert_eq!(read_db(&nd.join("journal/index.db")), "idx");
    assert_eq!(read_db(&nd.join("pathways.db")), "p1");
    for p in ["sessions", "gate/api-token", "gate/disarmed", "journal/index.db-wal", "pathways.db-shm"] {
        assert!(!nd.join(p).exists(), "restored {p}");
    }
}

#[test]
fn restore_refuses_a_full_target_without_force() {
    let (home, work) = (Home::new("h2"), Home::new("w2"));
    seed(&home.0, "config.yaml", "a: 1\n");
    let file = work.0.join("b.tgz");
    let f = file.to_str().unwrap();
    assert_eq!(run(&home.0, &work.0, &["backup", f]).0, 0);
    let (code, _, err) = run(&home.0, &work.0, &["restore", f]);
    assert!(code == 1 && err.contains("--force"), "{code} {err}");
    let (code, out, err) = run(&home.0, &work.0, &["restore", f, "--force"]);
    assert!(code == 0 && out.contains(".broken-"), "{code} {out} {err}");
}

#[test]
fn usage_errors() {
    let (home, work) = (Home::new("h3"), Home::new("w3"));
    assert_eq!(run(&home.0, &work.0, &["restore"]).0, 2);
    let (code, _, err) = run(&home.0, &work.0, &["backup", work.0.join("x.tgz").to_str().unwrap()]);
    assert!(code == 1 && err.contains("nothing to back up"), "{code} {err}");
}

#[test]
fn restore_refuses_while_a_gate_answers() {
    let (home, work, fresh) = (Home::new("h4"), Home::new("w4"), Home::new("f4"));
    seed(&home.0, "config.yaml", "a: 1\n");
    let file = work.0.join("b.tgz");
    let f = file.to_str().unwrap();
    assert_eq!(run(&home.0, &work.0, &["backup", f]).0, 0);
    let sock = fresh.0.join(".opendaisugi/gate/gate.sock");
    fs::create_dir_all(sock.parent().unwrap()).unwrap();
    let _l = std::os::unix::net::UnixListener::bind(&sock).unwrap();
    let (code, _, err) = run(&fresh.0, &work.0, &["restore", f]);
    assert!(code == 1 && err.contains("Stop agent sessions or run `daisugi gate disarm` first"), "{code} {err}");
    assert!(!fresh.0.join(".opendaisugi/config.yaml").exists());
}

#[test]
fn backup_says_how_many_files_it_left_out() {
    let (home, work) = (Home::new("h5"), Home::new("w5"));
    seed(&home.0, "config.yaml", "a: 1\n");
    seed(&home.0, "journal/token-thrift.md", "n");
    let (code, out, err) = run(&home.0, &work.0, &["backup", work.0.join("b.tgz").to_str().unwrap()]);
    assert!(code == 0 && out.contains("Left out 1 files whose names look like secrets or locks."), "{code} {out} {err}");
}

#[test]
fn backup_refuses_a_symlinked_journal() {
    let (home, work) = (Home::new("h6"), Home::new("w6"));
    seed(&home.0, "config.yaml", "a: 1\n");
    std::os::unix::fs::symlink(&work.0, home.0.join(".opendaisugi/journal")).unwrap();
    let (code, _, err) = run(&home.0, &work.0, &["backup", work.0.join("b.tgz").to_str().unwrap()]);
    let want = format!("{} is a symlink", home.0.join(".opendaisugi/journal").display());
    assert!(code == 1 && err.contains(&want), "{code} {err}");
}

#[test]
fn backup_help_does_not_overclaim() {
    let (home, work) = (Home::new("h7"), Home::new("w7"));
    let (_, out, _) = run(&home.0, &work.0, &["backup", "--help"]);
    assert!(out.contains("No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data."), "{out}");
}

#[test]
fn a_failed_restore_puts_the_old_files_back_and_says_so() {
    let (home, work, target) = (Home::new("h8"), Home::new("w8"), Home::new("t8"));
    seed(&home.0, "config.yaml", "new\n");
    seed(&home.0, "gate/envelopes/s.json", "{}");
    let file = work.0.join("b.tgz");
    let f = file.to_str().unwrap();
    assert_eq!(run(&home.0, &work.0, &["backup", f]).0, 0);
    seed(&target.0, "config.yaml", "old\n");
    // "gate" is a file, so gate/envelopes cannot be put in place.
    seed(&target.0, "gate", "a file");
    let (code, out, err) = run(&target.0, &work.0, &["restore", f, "--force"]);
    assert!(code == 1 && err.contains("The old data is back in place") && err.contains(".restore-"), "{code} {out} {err}");
    assert!(!out.contains("Moved aside"), "{out}");
    let nd = target.0.join(".opendaisugi");
    assert_eq!(fs::read_to_string(nd.join("config.yaml")).unwrap(), "old\n");
    assert_eq!(fs::read_to_string(nd.join("gate")).unwrap(), "a file");
}

#[test]
fn restore_moves_a_stale_wal_aside_and_the_database_opens() {
    let (home, work, target) = (Home::new("h9"), Home::new("w9"), Home::new("t9"));
    seed_db(&home.0, "pathways.db", "from the archive");
    let file = work.0.join("b.tgz");
    let f = file.to_str().unwrap();
    assert_eq!(run(&home.0, &work.0, &["backup", f]).0, 0);
    seed_db(&target.0, "pathways.db", "old");
    seed(&target.0, "pathways.db-wal", "stale wal from a crash");
    seed(&target.0, "pathways.db-shm", "stale shm");
    let (code, out, err) = run(&target.0, &work.0, &["restore", f, "--force"]);
    assert_eq!(code, 0, "{out} {err}");
    let nd = target.0.join(".opendaisugi");
    assert_eq!(read_db(&nd.join("pathways.db")), "from the archive");
    for n in ["pathways.db-wal", "pathways.db-shm"] {
        assert!(!nd.join(n).exists(), "{n} stayed beside the restored database");
        assert!(out.contains(&format!("{n}.broken-")), "{n} not in the aside list: {out}");
    }
}
