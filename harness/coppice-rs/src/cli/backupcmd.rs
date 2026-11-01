//! `coppice backup [FILE]` and `coppice restore FILE [--force]`. The archive
//! code is in `crate::backup`; this file holds the allow-list and the words.

use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};

use super::{errln, out, Cli};
use crate::backup::{self, Spec};
use crate::config;

const BACKUP_HELP: &str = "Usage: coppice backup [FILE]

  Write the steward's chat and memory, the layout, coppice.toml and
  pools.toml to one .tgz (default ./coppice-backup-DATE.tgz). No key, token
  or credential files go in. Your chat and logs do, so keep the file as
  private as the data. The config files always come from the XDG config
  dir, whatever --data-dir says.";

const RESTORE_HELP: &str = "Usage: coppice restore FILE [--force]

  Put a coppice backup back. Stop the server first. It refuses to overwrite
  data unless --force. Then it moves every path a backup can hold aside to
  NAME.broken-TIME, including paths this backup does not hold, and never
  deletes. The config files always go to the XDG config dir, whatever
  --data-dir says.";

/// The directories a restore writes into: the roots of the spec, without a
/// root that lies inside another one.
fn root_list(spec: &Spec) -> Vec<String> {
    let mut all: Vec<&Path> = spec.roots.iter().map(|(_, p)| p.as_path()).collect();
    all.sort();
    all.dedup();
    all.iter()
        .filter(|r| !all.iter().any(|o| o != *r && r.starts_with(o)))
        .map(|r| r.display().to_string())
        .collect()
}

fn now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

impl Cli {
    /// What `coppice backup` holds: the steward's conversation memory, the
    /// layout, coppice.toml and pools.toml. It is an allow-list. The web
    /// token, certificates, sockets, locks and the server log stay out, and
    /// so does anything new until it is added here.
    fn backup_spec(&self) -> Spec {
        self.backup_spec_in(Self::cfg_dir())
    }

    fn cfg_dir() -> PathBuf {
        config::path().parent().map(Path::to_path_buf).unwrap_or_default()
    }

    fn backup_spec_in(&self, cfg_dir: PathBuf) -> Spec {
        self.backup_spec_for(cfg_dir, &[])
    }

    /// The memory is every Pando R2 store in <data home>/pando (ADR-0023),
    /// with the parts the pando crate names (`backup_rels`), and a chat that
    /// has not been moved yet. A restore passes the paths its archive holds:
    /// a store the target does not have yet is put back, and an archive made
    /// before the move (area "foreman", the old chat) goes into the work
    /// store.
    fn backup_spec_for(&self, cfg_dir: PathBuf, archived: &[String]) -> Spec {
        let home = crate::steward::store_root(&self.data_dir);
        let pando_dir = home.join("pando");
        let old = crate::steward::DIR_NAME;
        let in_pando: Vec<String> = archived
            .iter()
            .filter_map(|p| p.strip_prefix("datahome/pando/").map(str::to_string))
            .collect();
        let legacy = archived.iter().any(|p| p.starts_with(&format!("{old}/")));
        let mut roots: Vec<(String, PathBuf)> = vec![
            ("data".into(), self.data_dir.clone()),
            ("datahome".into(), home.clone()),
            // A chat that has not been moved yet is still saved.
            ("steward-old".into(), crate::steward::log_dir(&self.data_dir)),
            ("config".into(), cfg_dir),
        ];
        let mut units: Vec<(String, String)> = vec![
            ("data".into(), "layout.json".into()),
            ("data".into(), "recent-dirs.json".into()),
        ];
        let work = pando::store::DEFAULT_STORE;
        for r in pando::store::backup_rels(&pando::store::backup_stores(&pando_dir, &in_pando)) {
            // An old archive's chat is the work store; one area holds it.
            if legacy && r.starts_with(&format!("{work}/")) {
                continue;
            }
            units.push(("datahome".into(), format!("pando/{r}")));
        }
        if legacy {
            roots.push((old.into(), pando_dir.join(work)));
            for part in pando::store::BACKUP_PARTS {
                units.push((old.into(), part.into()));
            }
        }
        for part in ["main", "tree", "view.json", pando::import::IMPORT_FILE] {
            units.push(("steward-old".into(), part.into()));
        }
        units.push(("config".into(), "coppice.toml".into()));
        units.push(("config".into(), "pools.toml".into()));
        Spec {
            tool: "coppice".into(),
            version: env!("COPPICE_VERSION").into(),
            roots,
            units,
            ..Spec::default()
        }
    }

    /// The paths an archive holds, as its manifest lists them. The manifest
    /// is read with the whole pando dir and the old chat's area allowed,
    /// only to learn which stores it names; the restore itself checks every
    /// path against the real list. An archive that does not read gives
    /// none, and the restore then says what is wrong with it.
    fn archived_paths(&self, cfg_dir: PathBuf, file: &Path) -> Vec<String> {
        let mut peek = self.backup_spec_in(cfg_dir);
        let old = crate::steward::DIR_NAME;
        let pando_dir = crate::steward::store_root(&self.data_dir).join("pando");
        peek.roots.push((old.into(), pando_dir.join(pando::store::DEFAULT_STORE)));
        peek.units.push(("datahome".into(), "pando".into()));
        for part in pando::store::BACKUP_PARTS {
            peek.units.push((old.into(), part.into()));
        }
        backup::manifest_paths(&peek, file).unwrap_or_default()
    }

    /// coppice backup [FILE]. It reads files only and dials no socket.
    pub(super) fn run_backup(&self, argv: &[String]) -> i32 {
        self.backup_with(&self.backup_spec(), argv)
    }

    fn backup_with(&self, spec: &Spec, argv: &[String]) -> i32 {
        if argv.len() == 1 && (argv[0] == "-h" || argv[0] == "--help") {
            out(&format!("{BACKUP_HELP}\n"));
            return 0;
        }
        if argv.len() > 1 || argv.first().is_some_and(|a| a.starts_with('-')) {
            errln("backup takes one optional FILE");
            return 1;
        }
        let t = now();
        let file = argv
            .first()
            .cloned()
            .unwrap_or_else(|| format!("coppice-backup-{}.tgz", backup::date(t)));
        match backup::create(spec, Path::new(&file), t) {
            Ok(r) => {
                if r.left > 0 {
                    out(&format!("Left out {} files whose names look like secrets or locks.\n", r.left));
                }
                let abs = std::path::absolute(&file).map(|a| a.display().to_string()).unwrap_or(file);
                out(&format!("Backed up {} files ({} bytes) to {abs}.\n", r.files, r.bytes));
                0
            }
            Err(e) => {
                errln(&format!("coppice backup: {e}"));
                1
            }
        }
    }

    /// coppice restore FILE [--force]. The server must be stopped: it holds
    /// the layout and the steward files open and writes them back.
    pub(super) fn run_restore(&self, argv: &[String]) -> i32 {
        self.restore_with(&self.backup_spec(), argv)
    }

    fn restore_with(&self, spec: &Spec, argv: &[String]) -> i32 {
        if argv.iter().any(|a| a == "-h" || a == "--help") {
            out(&format!("{RESTORE_HELP}\n"));
            return 0;
        }
        if let Some(a) = argv.iter().find(|a| a.starts_with('-') && *a != "--force") {
            errln(&format!("restore does not take {a}. It takes one FILE and --force"));
            return 1;
        }
        let force = argv.iter().any(|a| a == "--force");
        let rest: Vec<&String> = argv.iter().filter(|a| *a != "--force").collect();
        if rest.len() != 1 {
            errln("restore needs one FILE, and takes --force");
            return 1;
        }
        if UnixStream::connect(&self.socket).is_ok() {
            errln("coppice restore: the server is running. Run coppice server stop, then restore.");
            return 1;
        }
        let file: PathBuf = PathBuf::from(rest[0]);
        // The archive's own paths widen the list to the stores it holds.
        let cfg_dir = spec.root("config").map(Path::to_path_buf).unwrap_or_else(Self::cfg_dir);
        let spec = &self.backup_spec_for(cfg_dir.clone(), &self.archived_paths(cfg_dir, &file));
        match backup::restore(spec, &file, force, now()) {
            Ok(r) => {
                for a in &r.aside {
                    out(&format!("Moved aside: {a}\n"));
                }
                out(&format!(
                    "Restored {} files ({} bytes) into {}. Start the server again with coppice server start.\n",
                    r.files,
                    r.bytes,
                    root_list(spec).join(" and ")
                ));
                0
            }
            Err(f) => {
                for a in &f.aside {
                    out(&format!("Moved aside: {a}\n"));
                }
                errln(&format!("coppice restore: {}", f.msg));
                1
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    /// A temp dir that goes away on drop, on failure too.
    struct Tmp(PathBuf);
    impl Tmp {
        fn new(tag: &str) -> Tmp {
            let p = std::env::temp_dir().join(format!("cb-{tag}-{}", std::process::id()));
            let _ = fs::remove_dir_all(&p);
            fs::create_dir_all(&p).unwrap();
            Tmp(p)
        }
    }
    impl Drop for Tmp {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn put(dir: &Path, rel: &str, body: &str) {
        let p = dir.join(rel);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::write(p, body).unwrap();
    }

    fn cli(data: &Path, sock: &Path) -> Cli {
        Cli { socket: sock.to_path_buf(), data_dir: data.to_path_buf() }
    }

    #[test]
    fn backup_and_restore_carry_history_and_layout_not_secrets() {
        let (a, b, o) = (Tmp::new("a"), Tmp::new("b"), Tmp::new("o"));
        let (ca, cb) = (a.0.join("cfg"), b.0.join("cfg"));
        let sock = o.0.join("none.sock");
        put(&a.0, "layout.json", "{}");
        put(&a.0, "recent-dirs.json", "[]");
        put(&a.0, "foreman/main/2026-10-24.jsonl", "{}\n"); // names-ok: the log dir name on disk
        put(&a.0, "foreman/tree/2026-10-24.jsonl", "{}\n"); // names-ok: the log dir name on disk
        put(&a.0, "foreman/view.json", "{}"); // names-ok: the log dir name on disk
        put(&a.0, "foreman/lock", ""); // names-ok: the log dir name on disk
        put(&a.0, "foreman/tools.sock", ""); // names-ok
        put(&a.0, "web/token", "SECRET");
        put(&a.0, "server.log", "log");
        put(&ca, "coppice.toml", "fence = \"auto\"\n");
        put(&ca, "api-key", "SECRET");
        let file = o.0.join("b.tgz");
        let (sa, sb) = (cli(&a.0, &sock).backup_spec_in(ca.clone()), cli(&b.0, &sock).backup_spec_in(cb.clone()));
        let f = file.display().to_string();
        assert_eq!(cli(&a.0, &sock).backup_with(&sa, &[f.clone()]), 0);
        assert_eq!(cli(&b.0, &sock).restore_with(&sb, &[f.clone()]), 0);
        assert!(cb.join("coppice.toml").exists());
        assert!(!cb.join("api-key").exists());
        assert!(b.0.join("layout.json").exists());
        assert!(b.0.join("foreman/tree/2026-10-24.jsonl").exists()); // names-ok
        for p in ["web", "server.log", "foreman/lock", "foreman/tools.sock"] { // names-ok
            assert!(!b.0.join(p).exists(), "restored {p}");
        }
        // A full target is refused without --force and kept aside with it.
        assert_eq!(cli(&b.0, &sock).restore_with(&sb, &[f.clone()]), 1);
        assert_eq!(cli(&b.0, &sock).restore_with(&sb, &[f, "--force".into()]), 0);
        let aside = fs::read_dir(&b.0).unwrap().flatten().any(|e| e.file_name().to_string_lossy().contains(".broken-"));
        assert!(aside);
    }

    #[test]
    fn restore_with_a_running_server_changes_nothing() {
        let (a, b, o) = (Tmp::new("c"), Tmp::new("d"), Tmp::new("e"));
        put(&a.0, "layout.json", "{}");
        let file = o.0.join("b.tgz");
        let ca = cli(&a.0, &o.0.join("none.sock"));
        assert_eq!(ca.backup_with(&ca.backup_spec_in(a.0.join("cfg")), &[file.display().to_string()]), 0);
        let sock = o.0.join("live.sock");
        let _l = std::os::unix::net::UnixListener::bind(&sock).unwrap();
        let cb = cli(&b.0, &sock);
        assert_eq!(cb.restore_with(&cb.backup_spec_in(b.0.join("cfg")), &[file.display().to_string()]), 1);
        assert_eq!(fs::read_dir(&b.0).unwrap().count(), 0);
    }

    #[test]
    fn backup_and_restore_refuse_remote() {
        let o = Tmp::new("g");
        let file = o.0.join("b.tgz");
        for verb in ["backup", "restore"] {
            let argv: Vec<String> = ["--remote", "ssh://example.invalid", verb, file.to_str().unwrap()].iter().map(|s| s.to_string()).collect();
            assert_eq!(crate::cli::run(argv), 1, "{verb}");
        }
        assert_eq!(fs::read_dir(&o.0).unwrap().count(), 0);
    }

    #[test]
    fn help_is_help_and_other_flags_are_refused() {
        let o = Tmp::new("h");
        let c = cli(&o.0, &o.0.join("none.sock"));
        let s = c.backup_spec_in(o.0.join("cfg"));
        for a in ["--help", "-h"] {
            assert_eq!(c.backup_with(&s, &[a.into()]), 0, "backup {a}");
            assert_eq!(c.restore_with(&s, &[a.into()]), 0, "restore {a}");
        }
        assert_eq!(c.backup_with(&s, &["--out".into()]), 1);
        assert_eq!(c.restore_with(&s, &["--frce".into(), "x.tgz".into()]), 1);
        assert_eq!(c.restore_with(&s, &["-x".into()]), 1);
        let names: Vec<_> = fs::read_dir(&o.0).unwrap().flatten().map(|e| e.file_name()).collect();
        assert!(names.is_empty(), "wrote files: {names:?}");
    }

    #[test]
    fn restore_names_the_roots_it_writes_without_a_nested_one() {
        let o = Tmp::new("i");
        let c = cli(&o.0.join("d"), &o.0.join("none.sock"));
        let roots = root_list(&c.backup_spec_in(o.0.join("c")));
        assert_eq!(roots, vec![o.0.join("c").display().to_string(), o.0.join("d").display().to_string()]);
    }

    #[test]
    fn a_failed_restore_leaves_the_old_files_and_exits_1() {
        let (a, b, o) = (Tmp::new("j"), Tmp::new("k"), Tmp::new("l"));
        put(&a.0, "layout.json", "new");
        put(&a.0, "foreman/view.json", "{}"); // names-ok: the log dir name on disk
        let file = o.0.join("b.tgz");
        let ca = cli(&a.0, &o.0.join("none.sock"));
        assert_eq!(ca.backup_with(&ca.backup_spec_in(a.0.join("cfg")), &[file.display().to_string()]), 0);
        put(&b.0, "layout.json", "old");
        // "foreman" is a file, so foreman/view.json cannot be put in place. // names-ok
        put(&b.0, "foreman", "a file"); // names-ok
        let cb = cli(&b.0, &o.0.join("none.sock"));
        assert_eq!(cb.restore_with(&cb.backup_spec_in(b.0.join("cfg")), &[file.display().to_string(), "--force".into()]), 1);
        assert_eq!(fs::read_to_string(b.0.join("layout.json")).unwrap(), "old");
        assert_eq!(fs::read_to_string(b.0.join("foreman")).unwrap(), "a file"); // names-ok
    }

    /// Every Pando store in the data home is in the backup, with its
    /// children, view and import queue, and never a lock. A restore puts
    /// back a store the target does not have.
    #[test]
    fn backup_holds_every_pando_store_and_its_children() {
        let (a, b, o) = (Tmp::new("pa"), Tmp::new("pb"), Tmp::new("po"));
        for s in ["work", "notes"] {
            put(&a.0, &format!("pando/{s}/main/2026-10-24.jsonl"), "{\"id\":0}\n");
            put(&a.0, &format!("pando/{s}/tree/2026-10-24.jsonl"), "{}\n");
            put(&a.0, &format!("pando/{s}/view.json"), "{}");
            put(&a.0, &format!("pando/{s}/lock"), "");
        }
        put(&a.0, "pando/work/import.json", "{}");
        put(&a.0, "pando/work/children/w1/main/2026-10-24.jsonl", "{\"id\":0}\n");
        put(&a.0, "pando/work/children/w1/lock", "");
        let file = o.0.join("b.tgz").display().to_string();
        let sock = o.0.join("none.sock");
        let ca = cli(&a.0, &sock);
        assert_eq!(ca.backup_with(&ca.backup_spec_in(a.0.join("cfg")), &[file.clone()]), 0);
        let cb = cli(&b.0, &sock);
        assert_eq!(cb.restore_with(&cb.backup_spec_in(b.0.join("cfg")), &[file]), 0);
        for p in [
            "pando/work/main/2026-10-24.jsonl",
            "pando/notes/view.json",
            "pando/notes/tree/2026-10-24.jsonl",
            "pando/work/import.json",
            "pando/work/children/w1/main/2026-10-24.jsonl",
        ] {
            assert!(b.0.join(p).exists(), "not restored: {p}");
        }
        for p in ["pando/work/lock", "pando/notes/lock", "pando/work/children/w1/lock"] {
            assert!(!b.0.join(p).exists(), "restored a lock: {p}");
        }
    }

    /// An archive made before the move holds the chat in area "foreman" (the
    /// old log dir). It restores into the work store.
    #[test]
    fn an_old_archive_restores_into_the_work_store() {
        let (old, b, o) = (Tmp::new("oa"), Tmp::new("ob"), Tmp::new("oo"));
        put(&old.0, "main/2026-10-24.jsonl", "{\"id\":0}\n");
        put(&old.0, "view.json", "{}");
        let file = o.0.join("old.tgz");
        let area = crate::steward::DIR_NAME;
        let spec = Spec {
            tool: "coppice".into(),
            version: "old".into(),
            roots: vec![(area.into(), old.0.clone())],
            units: vec![(area.into(), "main".into()), (area.into(), "view.json".into())],
            ..Spec::default()
        };
        backup::create(&spec, &file, now()).unwrap();
        let cb = cli(&b.0, &o.0.join("none.sock"));
        assert_eq!(cb.restore_with(&cb.backup_spec_in(b.0.join("cfg")), &[file.display().to_string()]), 0);
        for p in ["pando/work/main/2026-10-24.jsonl", "pando/work/view.json"] {
            assert!(b.0.join(p).exists(), "not restored: {p}");
        }
    }

    #[test]
    fn usage_errors() {
        let o = Tmp::new("f");
        let c = cli(&o.0, &o.0.join("none.sock"));
        assert_eq!(c.run_restore(&[]), 1);
        assert_eq!(c.run_backup(&["a".into(), "b".into()]), 1);
    }
}
