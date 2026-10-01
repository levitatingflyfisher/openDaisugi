//! `daisugi backup [FILE]` and `daisugi restore FILE`. The archive code is
//! in `crate::backup`; this file holds the allow-list and the words.

use super::statuscmd::DATA_DIR_OPT;
use super::{parse_args, Env, Opt, Res, VERSION};
use crate::backup::{self, Spec};
use std::path::{Path, PathBuf};

/// What `daisugi backup` holds: history and settings, never keys, tokens,
/// sockets or locks. It is an allow-list. A new file in the data home stays
/// out until it is added here.
fn spec(data_dir: &str) -> Spec {
    let units = [
        "config.yaml",
        "pathways.db",
        "journal",
        "gate/envelopes",
        "gate/audit",
    ];
    Spec {
        tool: "daisugi".into(),
        version: VERSION.into(),
        roots: vec![("data".into(), PathBuf::from(data_dir))],
        units: units.iter().map(|u| ("data".to_string(), u.to_string())).collect(),
        // The two SQLite files are copied with VACUUM INTO, so a live
        // session cannot tear them and no -wal file is needed.
        snapshots: vec!["data/pathways.db".into(), "data/journal/index.db".into()],
        snap: Some(vacuum_into),
    }
}

/// Writes a consistent copy of the SQLite file `src` at `dst`, while
/// another process may write `src`.
fn vacuum_into(src: &Path, dst: &Path) -> Result<(), String> {
    let con = rusqlite::Connection::open(src).map_err(|e| e.to_string())?;
    con.busy_timeout(std::time::Duration::from_secs(5)).map_err(|e| e.to_string())?;
    con.execute("VACUUM INTO ?1", [dst.to_string_lossy().as_ref()])
        .map(|_| ())
        .map_err(|e| format!("it is not a SQLite file that reads: {e}"))
}

fn now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

impl Env {
    pub(super) fn backup_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "backup";
        let opts = [DATA_DIR_OPT];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                " [FILE]",
                "Write your journal, pathways, envelopes and config to one .tgz (default ./daisugi-backup-DATE.tgz). No key, token or credential files go in. Your chat and logs do, so keep the file as private as the data.",
                &opts,
            );
        }
        let t = now();
        let out = p
            .args
            .first()
            .cloned()
            .unwrap_or_else(|| format!("daisugi-backup-{}.tgz", backup::date(t)));
        let dir = self.data_dir_of(&p);
        match backup::create(&spec(&dir), Path::new(&out), t) {
            Ok(r) => {
                if r.left > 0 {
                    self.out(&format!("Left out {} files whose names look like secrets or locks.\n", r.left));
                }
                let abs = std::path::absolute(&out).map(|a| a.display().to_string()).unwrap_or(out);
                self.out(&format!("Backed up {} files ({} bytes) from {dir} to {abs}.\n", r.files, r.bytes));
                Ok(())
            }
            Err(e) => self.fail(CMD, &e),
        }
    }

    pub(super) fn restore_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "restore";
        let opts = [
            DATA_DIR_OPT,
            Opt::flag(&["--force"], "Move the data now there aside (.broken-TIME) and restore."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                " FILE",
                "Put a daisugi backup back. It refuses to overwrite data unless --force; then the old data is moved aside, never deleted.",
                &opts,
            );
        }
        let Some(file) = p.args.first() else {
            return Err(self.usage_stop(CMD, "Missing argument 'FILE'.".into()));
        };
        let dir = self.data_dir_of(&p);
        // A running gate writes the journal and the audit on every call. Do
        // not move those files from under it.
        for sock in [format!("{dir}/gate/gate.sock"), format!("{dir}/gate.sock")] {
            if Path::new(&sock).exists() && crate::fence::gate_live(&sock) {
                return self.fail(CMD, &format!("a gate answers on {sock}. Stop agent sessions or run `daisugi gate disarm` first"));
            }
        }
        match backup::restore(&spec(&dir), Path::new(file), p.flag("--force"), now()) {
            Ok(r) => {
                for a in &r.aside {
                    self.out(&format!("Moved aside: {a}\n"));
                }
                self.out(&format!("Restored {} files ({} bytes) into {dir}.\n", r.files, r.bytes));
                Ok(())
            }
            Err(f) => {
                for a in &f.aside {
                    self.out(&format!("Moved aside: {a}\n"));
                }
                self.fail(CMD, &f.msg)
            }
        }
    }
}
