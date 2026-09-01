//! `daisugi pathways list|show|stats|delete|export|import`: the pathway
//! store, with the flags, output, exit codes and files of the Python CLI.

use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{Object, Value};
use crate::pathways::export::{export, FORMATS};
use crate::pathways::importer::{is_skill, parse_bundle, parse_skill, storable, verify_pathway};
use crate::pathways::pathway::{dump_json_indent, json_mode, py_dumps, Pathway};
use crate::pathways::store::{Col, Store};
use crate::pathways::PwErr;

const PATHWAYS_HELP: &str = "Usage: daisugi pathways [OPTIONS] COMMAND [ARGS]...

  Manage compiled pathways.

Commands:
  list     List all compiled pathways.
  show     Show a compiled pathway in detail.
  stats    Summarize stored pathways (count, total hits).
  delete   Delete a compiled pathway.
  export   Export a compiled pathway for sharing or inspection.
  import   Import a pathway bundle, re-verify, and admit to the PathwayStore.
";

pub(super) const DATA_DIR: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");
const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

impl Env {
    pub(super) fn pathways(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(PATHWAYS_HELP);
            return Ok(());
        }
        let (sub, rest) = (args[0].as_str(), &args[1..]);
        match sub {
            "list" => self.pathways_list(rest),
            "show" => self.pathways_show(rest),
            "stats" => self.pathways_stats(rest),
            "delete" => self.pathways_delete(rest),
            "export" => self.pathways_export(rest),
            "import" => self.pathways_import(rest),
            _ => {
                self.errf(&format!(
                    "Usage: daisugi pathways [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi pathways --help' for help.\n\n\
                     Error: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// click's error for an absent required argument.
    fn missing_arg(&mut self, cmd: &str, usage_args: &str, name: &str) -> Res {
        self.errf(&format!(
            "Usage: daisugi {cmd} [OPTIONS] {usage_args}\nTry 'daisugi {cmd} --help' for help.\n\n\
             Error: Missing argument '{}'.\n",
            name.to_lowercase()
        ));
        exit(2)
    }

    /// `PathwayStore(data_dir / "pathways.db")`.
    pub(super) fn open_store(&mut self, cmd: &str, p: &super::Parsed) -> Result<Store, Stop> {
        let dir = p.str("--data-dir", &format!("{}/.opendaisugi", self.home));
        if std::fs::metadata(&dir).map(|m| !m.is_dir()).unwrap_or(false) {
            // Path.mkdir(parents=True, exist_ok=True) on a file.
            self.errf(&format!("daisugi {cmd}: FileExistsError: {dir} exists and is not a directory\n"));
            return Err(Stop::Exit(1));
        }
        let path = path_str(&format!("{dir}/pathways.db"));
        Store::open(&path).map_err(|e| self.pw_err(cmd, e))
    }

    /// Ends a command on an error from the store, the model or the file
    /// system: a refusal (exit 2, nothing changed), Python's exception
    /// (exit 1), or an import error (exit 1, its code and message).
    pub(super) fn pw_err(&mut self, cmd: &str, e: PwErr) -> Stop {
        match e {
            PwErr::Unreadable(why) => {
                self.errf(&format!("daisugi {cmd}: {why}. Nothing was changed.\n"));
                Stop::Exit(2)
            }
            PwErr::NotYet(_) => {
                self.errf("daisugi pathways import of this skill file's frontmatter is not in this binary yet.\n");
                Stop::Exit(2)
            }
            PwErr::Invalid(why) => {
                if self.tend_failed {
                    self.errf(&format!("{}\n", super::tendcmd::tend_failure(&why)));
                } else {
                    self.errf(&format!("daisugi {cmd}: {why}\n"));
                }
                Stop::Exit(1)
            }
            PwErr::Import(code, msg) => {
                self.errf(&format!("[{code}] {msg}\n"));
                Stop::Exit(1)
            }
            PwErr::Io(err) => {
                if self.tend_failed {
                    self.errf(&format!("tend failed: {}: {err}\n", os_name(&err)));
                } else {
                    self.errf(&format!("daisugi {cmd}: {}: {err}\n", os_name(&err)));
                }
                Stop::Exit(1)
            }
        }
    }

    fn pathways_list(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways list";
        let opts = [DATA_DIR, JSON_OPT];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "List all compiled pathways.", &opts);
        }
        let s = self.open_store(CMD, &p)?;
        let all = s.read_all(&|_| false).map_err(|e| self.pw_err(CMD, e))?;
        if p.flag("--json") {
            let items: Vec<Value> = all
                .iter()
                .map(|pw| {
                    let o = &pw.obj;
                    let mut x = Object::new();
                    for k in ["id", "task_description", "hit_count", "version", "distilled_at"] {
                        x.push_new(k.to_string(), json_mode(o.value(k)));
                    }
                    Value::Obj(x)
                })
                .collect();
            self.out(&format!("{}\n", py_dumps(&Value::List(items))));
            return Ok(());
        }
        if all.is_empty() {
            self.out("No compiled pathways.\n");
            return Ok(());
        }
        let mut b = String::new();
        for pw in &all {
            let o = &pw.obj;
            b.push_str(&format!(
                "{}  hits={}  v{}  {}\n",
                pw.id(),
                int_text(o.value("hit_count")),
                int_text(o.value("version")),
                pw.task()
            ));
        }
        self.out(&b);
        Ok(())
    }

    /// The pathway with this id, reading every row as `list_all()` does.
    fn find_by_id(&mut self, cmd: &str, s: &Store, id: &str) -> Result<Option<Pathway>, Stop> {
        let all = s.read_all(&|x| x == id).map_err(|e| self.pw_err(cmd, e))?;
        Ok(all.into_iter().find(|pw| pw.id() == id))
    }

    fn pathways_show(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways show";
        let opts = [DATA_DIR, JSON_OPT];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " PATHWAY_ID", "Show a compiled pathway in detail.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "PATHWAY_ID", "PATHWAY_ID");
        }
        let id = p.args[0].clone();
        let s = self.open_store(CMD, &p)?;
        match self.find_by_id(CMD, &s, &id)? {
            Some(pw) => {
                if p.flag("--json") {
                    let mut o = Object::new();
                    for k in pw.obj.keys() {
                        if k != "task_embedding" {
                            o.set(k, pw.obj.value(k).clone());
                        }
                    }
                    self.out(&format!("{}\n", py_dumps(&json_mode(&Value::Obj(o)))));
                    return Ok(());
                }
                let full = pw.full();
                self.out(&format!("{}\n", dump_json_indent(&Value::Obj(full.obj), 2)));
                Ok(())
            }
            None => {
                self.errf(&format!("Pathway {} not found.\n", repr(&id)));
                exit(1)
            }
        }
    }

    fn pathways_stats(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways stats";
        let opts = [DATA_DIR, Opt::flag(&["--json"], "Emit stats as JSON.")];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Summarize stored pathways (count, total hits).", &opts);
        }
        let s = self.open_store(CMD, &p)?;
        let (count, hits) = s.stats().map_err(|e| self.pw_err(CMD, e))?;
        let hv = match hits {
            Col::Int(n) => Value::Int(n.to_string()),
            Col::Real(f) => Value::Float(f),
            _ => {
                let e = PwErr::Unreadable("SUM(hit_count) is neither an int nor a float".into());
                return Err(self.pw_err(CMD, e));
            }
        };
        if p.flag("--json") {
            let mut o = Object::new();
            o.set("count", Value::Int(count.to_string()));
            o.set("total_hits", hv);
            self.out(&format!("{}\n", crate::pathways::pathway::py_dumps_indent(&Value::Obj(o), 2)));
            return Ok(());
        }
        self.out(&format!("count: {count}\ntotal_hits: {}\n", py_dumps(&hv)));
        Ok(())
    }

    fn pathways_delete(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways delete";
        let opts = [DATA_DIR];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " PATHWAY_ID", "Delete a compiled pathway.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "PATHWAY_ID", "PATHWAY_ID");
        }
        let id = p.args[0].clone();
        let s = self.open_store(CMD, &p)?;
        if !s.delete(&id).map_err(|e| self.pw_err(CMD, e))? {
            self.errf(&format!("Pathway {} not found.\n", repr(&id)));
            return exit(1);
        }
        self.out(&format!("Deleted {id}.\n"));
        Ok(())
    }

    fn pathways_export(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways export";
        let opts = [Opt::val(&["--format"], "TEXT", "Export format: json, skill, mermaid, md, smtlib."), DATA_DIR];
        let p = parse_args(args, &opts, 2).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " PATHWAY_ID OUTPUT", "Export a compiled pathway for sharing or inspection.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "PATHWAY_ID OUTPUT", "PATHWAY_ID");
        }
        if p.args.len() < 2 {
            return self.missing_arg(CMD, "PATHWAY_ID OUTPUT", "OUTPUT");
        }
        let (id, output) = (p.args[0].clone(), p.args[1].clone());
        let format = p.str("--format", "skill");
        if !FORMATS.contains(&format.as_str()) {
            self.errf(&format!("Unknown format {}. Supported: {}.\n", repr(&format), FORMATS.join(", ")));
            return exit(2);
        }
        let s = self.open_store(CMD, &p)?;
        let pw = match self.find_by_id(CMD, &s, &id)? {
            Some(pw) => pw.full(),
            None => {
                self.errf(&format!("Pathway {} not found.\n", repr(&id)));
                return exit(1);
            }
        };
        let text = export(&pw, &format, super::VERSION).map_err(|e| self.pw_err(CMD, e))?;
        let out = path_str(&output);
        if let Some(parent) = std::path::Path::new(&out).parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent).map_err(|e| self.pw_err(CMD, PwErr::Io(e)))?;
            }
        }
        std::fs::write(&out, text.as_bytes()).map_err(|e| self.pw_err(CMD, PwErr::Io(e)))?;
        self.out(&format!("Exported {id} \u{2192} {out} ({format})\n"));
        Ok(())
    }

    fn pathways_import(&mut self, args: &[String]) -> Res {
        const CMD: &str = "pathways import";
        let opts = [
            DATA_DIR,
            Opt::flag(&["--overwrite"], "Replace an existing pathway with the same ID."),
            Opt::val(&["--z3-timeout-ms"], "INTEGER", "Z3 timeout for the re-verification."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                " SOURCE",
                "Import a pathway bundle, re-verify, and admit to the PathwayStore.",
                &opts,
            );
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "SOURCE", "SOURCE");
        }
        let mut timeout: u32 = 500;
        if p.has("--z3-timeout-ms") {
            let raw = p.str("--z3-timeout-ms", "");
            let n = match py_int(&raw) {
                Some(n) => n,
                None => {
                    return Err(self.usage_stop(
                        CMD,
                        format!("Invalid value for '--z3-timeout-ms': '{raw}' is not a valid int range."),
                    ))
                }
            };
            // Z3 takes a timeout from 1 to 2**32 - 1 ms; the Python CLI
            // refuses any other at the flag, and so does this one.
            if !(1..=4_294_967_295i128).contains(&n) {
                return Err(self.usage_stop(
                    CMD,
                    format!("Invalid value for '--z3-timeout-ms': {n} is not in the range 1<=x<=4294967295."),
                ));
            }
            timeout = n as u32;
        }
        let source = p.args[0].clone();
        // Everything that needs no store is done first, so a bundle this
        // binary cannot read or verify the Python way is refused with
        // nothing written; the errors are reported after the store is
        // opened, in Python's order.
        let read = std::fs::read(&source).map_err(PwErr::Io).and_then(|b| {
            crate::gate::pymodel::decode_utf8_strict(&b)
                .map_err(|m| PwErr::Invalid(format!("UnicodeDecodeError: {m}")))
                .map(|t| crate::gate::pymodel::universal_newlines(&t))
        });
        let src_name = path_str(&source);
        let mut staged: Result<Pathway, PwErr> = Err(PwErr::Invalid(String::new()));
        let mut refusal: Option<PwErr> = None;
        let mut read_err: Option<PwErr> = None;
        match read {
            Err(e) => read_err = Some(e),
            Ok(text) => {
                let parsed = if is_skill(&text) { parse_skill(&text, &src_name) } else { parse_bundle(&text, &src_name) };
                match parsed {
                    Err(e @ (PwErr::NotYet(_) | PwErr::Unreadable(_))) => return Err(self.pw_err(CMD, e)),
                    Err(e) => staged = Err(e),
                    Ok(pw) => {
                        match verify_pathway(&pw, timeout) {
                            Err(e) => return Err(self.pw_err(CMD, e)),
                            Ok(r) => refusal = r,
                        }
                        if refusal.is_none() {
                            if let Err(e) = storable(&pw) {
                                refusal = Some(e);
                            }
                        }
                        staged = Ok(pw);
                    }
                }
            }
        }
        let mut s = self.open_store(CMD, &p)?;
        if let Some(e) = read_err {
            return Err(self.pw_err(CMD, e));
        }
        let pw = staged.map_err(|e| self.pw_err(CMD, e))?;
        if let Some(e) = refusal {
            return Err(self.pw_err(CMD, e));
        }
        let overwrite = p.flag("--overwrite");
        if !overwrite {
            let all = s.read_all(&|_| false).map_err(|e| self.pw_err(CMD, e))?;
            if all.iter().any(|x| x.id() == pw.id()) {
                self.errf(&format!(
                    "[DUPLICATE_ID] pathway {} already exists; pass allow_overwrite=True to replace\n",
                    repr(pw.id())
                ));
                return exit(1);
            }
        }
        let existed = s.import_write(&pw, overwrite).map_err(|e| self.pw_err(CMD, e))?;
        let action = if existed { "Replaced" } else { "Imported" };
        let head: String = pw.task().chars().take(60).collect();
        self.out(&format!("{action} pathway {} ({head})\n", pw.id()));
        Ok(())
    }

    /// click's usage error, as a Stop.
    pub(super) fn usage_stop(&mut self, cmd: &str, msg: String) -> Stop {
        match self.usage(cmd, msg) {
            Err(s) => s,
            Ok(()) => Stop::Exit(2),
        }
    }
}

/// An int field's text.
fn int_text(v: &Value) -> String {
    match v {
        Value::Int(t) => t.clone(),
        other => py_dumps(other),
    }
}

/// `int(s)` for a flag value: white space around it, a sign, and
/// underscores between digits.
pub(super) fn py_int(s: &str) -> Option<i128> {
    let t = crate::gate::py::text::strip(s);
    let (neg, t) = match t.as_bytes().first() {
        Some(b'-') => (true, &t[1..]),
        Some(b'+') => (false, &t[1..]),
        _ => (false, t),
    };
    if t.is_empty() || t.starts_with('_') || t.ends_with('_') || t.contains("__") {
        return None;
    }
    let digits: String = t.chars().filter(|c| *c != '_').collect();
    if !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    let digits = digits.trim_start_matches('0');
    let n: i128 = if digits.len() > 30 { i128::MAX / 2 } else if digits.is_empty() { 0 } else { digits.parse().ok()? };
    Some(if neg { -n } else { n })
}

/// The name of the exception Python raises for an OS error.
fn os_name(e: &std::io::Error) -> &'static str {
    match e.raw_os_error() {
        Some(21) => "IsADirectoryError",
        Some(20) => "NotADirectoryError",
        Some(2) => "FileNotFoundError",
        Some(17) => "FileExistsError",
        Some(13) | Some(1) => "PermissionError",
        _ => "OSError",
    }
}
