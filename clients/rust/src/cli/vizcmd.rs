//! `daisugi viz [PATHWAY_ID] [--data-dir D] [-o FILE]`: a distilled
//! pathway's plan as a standalone execution-monitor page (`opendaisugi.viz`),
//! or the list of pathways to pick from. The page is the oracle's own
//! template with the view model in its data block. The Go client's
//! `internal/viz` is the twin.

use std::collections::HashMap;

use super::gateroot::{join, path_str};
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{dumps, round, Object, Value};
use crate::orchestrate::sizing::{build_ladder, size_plan};
use crate::tracejournal::dag::levels;

const TEMPLATE: &str = include_str!("../../../../src/opendaisugi/viz_dag_template.html");

/// `viz._KIND_LABEL`.
fn kind_label(t: &str) -> &str {
    match t {
        "shell" => "Shell",
        "file_read" => "Read",
        "file_write" => "Write",
        "network" => "Network",
        "task" => "Sub-agent",
        "skill" => "Skill",
        "mcp" => "MCP",
        other => other,
    }
}

fn s<'a>(o: &'a Object, k: &str) -> &'a str {
    o.value(k).as_str().unwrap_or("")
}

/// `viz._step_label`.
fn step_label(st: &Object) -> String {
    let t = s(st, "type");
    match t {
        "shell" => s(st, "command").into(),
        "file_read" | "file_write" => s(st, "path").into(),
        "network" => s(st, "url").into(),
        "task" => s(st, "prompt").into(),
        "skill" => s(st, "skill_id").into(),
        "mcp" => format!("{}/{}", s(st, "server"), s(st, "tool")),
        _ => t.into(),
    }
}

fn list_of(v: &Value) -> Value {
    match v {
        Value::List(l) => Value::List(l.clone()),
        _ => Value::List(vec![]),
    }
}

/// `viz.plan_to_viz_data` with the default ladder. Err is a plan or
/// envelope not read here, or the exception `dependency_levels` raises.
fn viz_data(plan: &Object, env: &Object) -> Result<Object, String> {
    let r = crate::pathways::verify::verify(&Value::Obj(plan.clone()), &Value::Obj(env.clone()), None, 500)
        .map_err(|e| e.to_string())?;
    let steps: Vec<Object> = crate::tracejournal::dag::steps(plan).into_iter().cloned().collect();
    let sizings: HashMap<String, crate::orchestrate::sizing::Sizing> =
        size_plan(&steps, &build_ladder("")).into_iter().map(|z| (z.step_id.clone(), z)).collect();
    let lv = levels(plan).map_err(|e| format!("{}: {}", e.typ, e.msg))?;
    let mut level_of: HashMap<String, i64> = HashMap::new();
    let mut level_ids = vec![];
    for (i, l) in lv.iter().enumerate() {
        let mut ids = vec![];
        for st in l {
            let id = s(st, "id").to_string();
            level_of.insert(id.clone(), i as i64);
            ids.push(Value::Str(id));
        }
        level_ids.push(Value::List(ids));
    }
    let re = regex::Regex::new(r"[Ss]tep '([^']+)'").expect("a fixed pattern");
    let mut viol_by_step: HashMap<String, Vec<Value>> = HashMap::new();
    for v in &r.violations {
        let sid = match re.captures(&v.message) {
            Some(m) => Some(m[1].to_string()),
            None => v.step.clone(),
        };
        // A violation keyed by None matches no step.
        if let Some(sid) = sid {
            viol_by_step.entry(sid).or_default().push(Value::Obj(
                Object::new()
                    .with("stage", Value::Str(v.stage.to_string()))
                    .with("message", Value::Str(v.message.clone())),
            ));
        }
    }
    let mut out = vec![];
    for st in &steps {
        let id = s(st, "id").to_string();
        let t = s(st, "type").to_string();
        let vs = viol_by_step.get(&id).cloned().unwrap_or_default();
        let mut o = Object::new()
            .with("id", Value::Str(id.clone()))
            .with("type", Value::Str(t.clone()))
            .with("kind", Value::Str(kind_label(&t).to_string()))
            .with("label", Value::Str(step_label(st)))
            .with("depends_on", list_of(st.value("depends_on")))
            .with("level", Value::Int(level_of.get(&id).copied().unwrap_or(0).to_string()))
            .with("blocked", Value::Bool(!vs.is_empty()))
            .with("violations", Value::List(vs));
        match sizings.get(&id) {
            Some(z) => {
                o.set("model", Value::Str(z.model.clone()));
                o.set("tier", Value::Str(z.tier.clone()));
                o.set("difficulty", Value::Float(round(z.difficulty, 2)));
                o.set("est_tokens", Value::Int(z.est_tokens.to_string()));
            }
            None => {
                for k in ["model", "tier", "difficulty", "est_tokens"] {
                    o.set(k, Value::Null);
                }
            }
        }
        o.set("runs_llm", Value::Bool(t == "task" || t == "agentic"));
        out.push(Value::Obj(o));
    }
    let perms = env.value("permissions").as_obj().cloned().unwrap_or_default();
    let task = match s(env, "task") {
        "" => s(plan, "task").to_string(),
        t => t.to_string(),
    };
    Ok(Object::new()
        .with("task", Value::Str(task))
        .with(
            "envelope",
            Object::new()
                .with("shell_allowlist", list_of(perms.value("shell_allowlist")))
                .with("file_read", list_of(perms.value("file_read")))
                .with("network", perms.value("network").clone())
                .with("mcp_allowlist", list_of(perms.value("mcp_allowlist"))),
        )
        .with("ok", Value::Bool(r.violations.is_empty()))
        .with("n_violations", Value::Int(r.violations.len().to_string()))
        .with("levels", Value::List(level_ids))
        .with("steps", Value::List(out)))
}

/// `viz.render_dag_html`: `</` escaped so a label cannot close the script.
fn render(plan: &Object, env: &Object) -> Result<String, String> {
    let d = viz_data(plan, env)?;
    let payload = dumps(&Value::Obj(d), true).replace("</", "<\\/");
    Ok(TEMPLATE.replace("/*__DATA__*/ null", &payload))
}

impl Env {
    pub(super) fn viz_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "viz";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory (pathway store)."),
            Opt::val(&["--output", "-o"], "PATH", "Write the HTML here (default: <pathway_id>.html)."),
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                " [PATHWAY_ID]",
                "Render a distilled pathway's plan as a standalone execution-monitor page.",
                &opts,
            );
        }
        let data_dir = self.data_dir_of(&p);
        let db = join(&data_dir, "pathways.db");
        if std::fs::metadata(&db).is_err() {
            self.errf(&format!("No pathway store at {db}. Run `daisugi onboard` or `daisugi tend` first.\n"));
            return exit(1);
        }
        let st = self.open_store(CMD, &p)?;
        let Some(id) = p.args.first().cloned().filter(|x| !x.is_empty()) else {
            let all = st.read_all(&|_| false).map_err(|e| self.pw_err(CMD, e))?;
            if all.is_empty() {
                self.out("No pathways distilled yet. Run `daisugi onboard` or `daisugi tend`.\n");
                return Ok(());
            }
            let mut b = format!("{} pathway(s) — pass an id to `daisugi viz`:\n", all.len());
            for pw in &all {
                let task: String = pw.task().chars().take(64).collect();
                b.push_str(&format!("  {}  {task}\n", pw.id()));
            }
            self.out(&b);
            return Ok(());
        };
        let Some(pw) = self.find_by_id(CMD, &st, &id)? else {
            self.errf(&format!("No pathway {} in {db}. Run `daisugi viz` to list.\n", repr(&id)));
            return exit(1);
        };
        let (Some(env), Some(plan)) = (pw.obj.value("envelope").as_obj(), pw.obj.value("plan_template").as_obj())
        else {
            return self.refuse(CMD, "the pathway's envelope or plan is not one this binary reads");
        };
        // Title the page with the distilled task description.
        let mut env2 = env.clone();
        env2.set("task", pw.obj.value("task_description").clone());
        let html = match render(plan, &env2) {
            Ok(h) => h,
            Err(w) => return self.refuse(CMD, &w),
        };
        let out = path_str(&p.str("--output", &format!("{id}.html")));
        if let Err(e) = std::fs::write(&out, html.as_bytes()) {
            let name = match e.kind() {
                std::io::ErrorKind::NotFound => "FileNotFoundError",
                std::io::ErrorKind::PermissionDenied => "PermissionError",
                std::io::ErrorKind::IsADirectory => "IsADirectoryError",
                std::io::ErrorKind::NotADirectory => "NotADirectoryError",
                _ => "OSError",
            };
            let text = match e.raw_os_error() {
                Some(n) => format!("[Errno {n}] {}: {}", crate::gate::paths::strerror(n), repr(&out)),
                None => e.to_string(),
            };
            self.errf(&format!("Traceback (most recent call last): ...\n{name}: {text}\n"));
            return exit(1);
        }
        self.out(&format!("Wrote {out} ({} bytes) — open it in a browser.\n", html.chars().count()));
        Ok(())
    }
}
