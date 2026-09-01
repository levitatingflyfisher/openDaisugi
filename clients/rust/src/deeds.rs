//! The oracle's deed ledger (`opendaisugi/deeds.py`): a run's reversible
//! file writes undone from the journal alone, with no model, no executor
//! and no re-run. The Go client's `internal/deeds` is the reference.

use crate::gate::pyjson::{Object, Value};
use crate::lerr::{LErr, PyError, LR};
use crate::pathways::pmodel::{validate_model, Id, Mode, ValidationError};
use crate::tracejournal::Journal;

/// A validated ReversalHandle.
#[derive(Debug, Clone, PartialEq)]
pub struct Handle {
    pub kind: String,
    pub path: String,
    pub prior_existed: bool,
    pub prior_content: Option<String>,
    pub created_dirs: Vec<String>,
}

/// `ReversalHandle.model_validate(v)`.
pub fn parse_handle(v: &Value) -> Result<Handle, ValidationError> {
    let o = match validate_model(Id::ReversalHandle, v, Mode::Python)? {
        Value::Obj(o) => o,
        _ => unreachable!("a model validates to an object"),
    };
    let s = |k: &str| o.value(k).as_str().unwrap_or("").to_string();
    Ok(Handle {
        kind: s("kind"),
        path: s("path"),
        prior_existed: matches!(o.value("prior_existed"), Value::Bool(true)),
        prior_content: o.value("prior_content").as_str().map(str::to_string),
        created_dirs: match o.value("created_dirs") {
            Value::List(l) => l
                .iter()
                .filter_map(|d| d.as_str().map(str::to_string))
                .collect(),
            _ => vec![],
        },
    })
}

fn dirname(p: &str) -> String {
    let d = crate::supervise::executors::py_dirname(p);
    if d.is_empty() {
        ".".into()
    } else {
        d
    }
}

fn basename(p: &str) -> &str {
    p.rsplit('/').next().unwrap_or(p)
}

/// `_atomic_write`: the parent made, the text written to a temporary name
/// beside the target, then renamed over it.
fn atomic_write(path: &str, content: &str) -> LR<()> {
    let parent = dirname(path);
    std::fs::create_dir_all(&parent).map_err(|e| LErr::io(e, &parent))?;
    let token = crate::gate::random_hex(6).map_err(|_| LErr::Unread("no random name".into()))?;
    let tmp = format!(
        "{}/.daisugi-undo-{token}-{}",
        parent.trim_end_matches('/'),
        basename(path)
    );
    std::fs::write(&tmp, content).map_err(|e| LErr::io(e, &tmp))?;
    std::fs::rename(&tmp, path).map_err(|e| LErr::io(e, &tmp))
}

/// `apply_reversal`: the prior content written back when the target
/// existed; otherwise the file removed (absent is fine) and each directory
/// the write made removed, deepest first, when it is empty.
pub fn apply(h: &Handle) -> LR<()> {
    if h.kind != "file_write" {
        return Err(PyError::new(
            "ValueError",
            format!("cannot reverse deed of kind '{}'", h.kind),
        )
        .into());
    }
    if h.prior_existed {
        return atomic_write(&h.path, h.prior_content.as_deref().unwrap_or(""));
    }
    // os.unlink: an absent file is fine; anything else (a directory, a
    // parent that is a file) raises, as in the oracle.
    if let Err(e) = std::fs::remove_file(&h.path) {
        if e.kind() != std::io::ErrorKind::NotFound {
            return Err(LErr::io(e, &h.path));
        }
    }
    for d in &h.created_dirs {
        let _ = std::fs::remove_dir(d);
    }
    Ok(())
}

/// `RollbackReport`.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Report {
    pub undone: Vec<String>,
    pub skipped: Vec<Object>,
}

impl Report {
    /// `dataclasses.asdict(report)`.
    pub fn dump(&self) -> Object {
        Object::new()
            .with(
                "undone",
                Value::List(self.undone.iter().map(Value::from).collect()),
            )
            .with(
                "skipped",
                Value::List(self.skipped.iter().cloned().map(Value::Obj).collect()),
            )
    }
}

/// `rollback_run`: the run's reversible deeds undone newest first;
/// irreversible deeds reported as skipped; read-only deeds left.
pub fn rollback(j: &Journal, run_id: &str) -> LR<Report> {
    let rows = j.receipts(run_id)?;
    let mut rep = Report::default();
    for r in rows.iter().rev() {
        match (r.reversibility.as_deref(), &r.reversal) {
            (Some("reversible"), Some(h)) => {
                let h = parse_handle(&Value::Obj(h.clone()))?;
                apply(&h)?;
                rep.undone.push(h.path);
            }
            (Some("irreversible"), _) => rep.skipped.push(
                Object::new()
                    .with("step_id", r.step_id.as_str())
                    .with("effect_class", r.effect_class.clone())
                    .with("reason", "irreversible"),
            ),
            _ => {}
        }
    }
    Ok(rep)
}

/// `PathState`: what was at a path before the run first wrote it.
#[derive(Debug, Clone, PartialEq)]
pub struct PathState {
    pub path: String,
    pub pre_existed: bool,
    pub pre_content: Option<String>,
}

/// `touched_files`: each file the run wrote with a handle, in the order
/// first written, with the state before its first write.
pub fn touched(j: &Journal, run_id: &str) -> LR<Vec<PathState>> {
    let mut out: Vec<PathState> = vec![];
    for r in j.receipts(run_id)? {
        let (Some("file_write"), Some(h)) = (r.effect_class.as_deref(), &r.reversal) else {
            continue;
        };
        let h = parse_handle(&Value::Obj(h.clone()))?;
        if out.iter().any(|p| p.path == h.path) {
            continue;
        }
        out.push(PathState {
            path: h.path,
            pre_existed: h.prior_existed,
            pre_content: h.prior_content,
        });
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> String {
        let d = std::env::temp_dir().join(format!("deeds-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d.to_string_lossy().into_owned()
    }

    fn handle(path: &str, existed: bool, prior: Option<&str>, dirs: &[&str]) -> Handle {
        let v = Value::Obj(
            Object::new()
                .with("kind", "file_write")
                .with("path", path)
                .with("prior_existed", existed)
                .with("prior_content", prior.map(Value::from))
                .with(
                    "created_dirs",
                    Value::List(dirs.iter().map(|d| Value::from(*d)).collect()),
                ),
        );
        parse_handle(&v).unwrap()
    }

    #[test]
    fn a_write_over_a_file_is_undone_to_its_prior_text() {
        let d = scratch("prior");
        let p = format!("{d}/a.txt");
        std::fs::write(&p, "new").unwrap();
        apply(&handle(&p, true, Some("old ☃"), &[])).unwrap();
        assert_eq!(std::fs::read_to_string(&p).unwrap(), "old ☃");
        assert_eq!(
            std::fs::read_dir(&d).unwrap().count(),
            1,
            "no temporary file is left"
        );
        std::fs::remove_dir_all(&d).unwrap();
    }

    #[test]
    fn a_new_file_and_the_directories_it_made_are_removed() {
        let d = scratch("new");
        let (sub, deep) = (format!("{d}/s"), format!("{d}/s/t"));
        std::fs::create_dir_all(&deep).unwrap();
        let p = format!("{deep}/x.txt");
        std::fs::write(&p, "x").unwrap();
        apply(&handle(&p, false, None, &[&deep, &sub])).unwrap();
        assert!(std::fs::metadata(&sub).is_err());
        // Undoing it again finds nothing and is fine.
        apply(&handle(&p, false, None, &[])).unwrap();
        std::fs::remove_dir_all(&d).unwrap();
    }

    #[test]
    fn a_directory_at_the_target_is_the_oracles_os_error() {
        let d = scratch("dir");
        let e = apply(&handle(&d, false, None, &[])).unwrap_err();
        assert_eq!(e.py().0, "IsADirectoryError");
        std::fs::remove_dir_all(&d).unwrap();
    }

    #[test]
    fn a_handle_of_another_kind_does_not_validate() {
        let v = Value::Obj(
            Object::new()
                .with("kind", "shell")
                .with("path", "p")
                .with("prior_existed", false),
        );
        assert!(parse_handle(&v).is_err());
    }
}
