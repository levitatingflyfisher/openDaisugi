//! `daisugi recipe`: list, show, apply (a dry run by default) and revert,
//! with the text, exit codes and files of the Python CLI. The Go client's
//! `recipecmd.go` is the reference.

use super::config::{self, Config};
use super::gardencmd::now_seconds;
use super::recipes::{self, Params, Recipe};
use super::routingcmd::sy_config;
use super::wardenroot::{self, join};
use super::{exit, install, layers, parse_args, Env, Opt, Parsed, Res};
use crate::switchyard as sy;
use crate::tracejournal::iso_seconds;
use crate::warden::pyjson::{dumps, dumps_indent, Object, Value};
use std::collections::HashMap;

const RECIPE_HELP: &str = "Usage: daisugi recipe [OPTIONS] COMMAND [ARGS]...\n\n  Recipes: named setups that run through install. List, show, apply, revert.\n\nCommands:\n  apply   Show what a recipe would write; with --yes, write it.\n  list    List the recipes.\n  revert  Undo a recipe from its receipt.\n  show    Show a recipe: its slots, params and steps.\n";

const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

fn s(o: &Object, k: &str) -> String {
    o.value(k).as_str().unwrap_or("").to_string()
}

fn str_list(v: &Value) -> Vec<String> {
    match v {
        Value::List(l) => l.iter().map(|x| x.as_str().unwrap_or("").to_string()).collect(),
        _ => vec![],
    }
}

fn int(n: usize) -> Value {
    Value::Int(n.to_string())
}

/// One entry of `recipes.plan_apply`.
struct StepPlan {
    index: usize,
    kind: String,
    summary: String,
    writes: Vec<String>,
    state: &'static str,
    notes: Vec<String>,
}

impl StepPlan {
    fn object(&self) -> Object {
        Object::new()
            .with("index", int(self.index))
            .with("kind", self.kind.as_str())
            .with("summary", self.summary.as_str())
            .with("writes", Value::List(self.writes.iter().map(|w| Value::Str(w.clone())).collect()))
            .with("state", self.state)
            .with("notes", Value::List(self.notes.iter().map(|w| Value::Str(w.clone())).collect()))
    }
}

struct CfgChange {
    key: String,
    old: Option<String>,
    new: String,
}

fn shown(v: &Option<String>) -> String {
    v.clone().unwrap_or_else(|| "(unset)".into())
}

/// `getattr(cfg, key)`: None when unset.
fn cfg_field(cfg: &Config, key: &str) -> Option<String> {
    match key {
        "routing_mode" => Some(cfg.routing_mode.clone()),
        "routing_model" => cfg.routing_model.clone(),
        "router_local_model" => cfg.router_local_model.clone(),
        "switchyard_efficient_model" => cfg.switchyard_efficient_model.clone(),
        "switchyard_capable_model" => Some(cfg.switchyard_capable_model.clone()),
        _ => cfg.switchyard_api_key_env.clone(),
    }
}

fn add_unique(list: &mut Vec<String>, v: String) {
    if !list.contains(&v) {
        list.push(v);
    }
}

fn render_plan(total: usize, plan: &[StepPlan], verb: &str) -> Vec<String> {
    let mut lines = vec![];
    for p in plan {
        lines.push(format!("[{}/{total}] {}  {}", p.index, p.kind, p.summary));
        match p.state {
            "skip" => lines.push(format!("  {}", p.notes[0])),
            "kept" => lines.push(format!("  {}", p.notes.first().map(String::as_str).unwrap_or("kept"))),
            _ => {
                for w in &p.writes {
                    lines.push(format!("  {verb} {w}"));
                }
                if p.kind == "config" {
                    for n in &p.notes {
                        lines.push(format!("    {n}"));
                    }
                }
            }
        }
    }
    lines
}

/// `recipes._merge_undo`: what this run added, plus what an earlier run did.
fn merge_undo(new: Option<Value>, old: Option<&Value>) -> Value {
    let (Some(new), old) = (new, old) else { return old.cloned().unwrap_or(Value::Null) };
    let (Value::Obj(n), Some(Value::Obj(o))) = (&new, old) else { return new };
    if let (Some(Value::Obj(nr)), Some(Value::Obj(or))) = (n.get("restore"), o.get("restore")) {
        // The first run saw the value from before the recipe: it wins.
        let mut out = Object::new();
        for k in nr.keys() {
            out.set(k, or.get(k).unwrap_or_else(|| nr.value(k)).clone());
        }
        for k in or.keys() {
            if nr.get(k).is_none() {
                out.set(k, or.value(k).clone());
            }
        }
        return Value::Obj(Object::new().with("restore", Value::Obj(out)));
    }
    let (Some(Value::Obj(nu)), Some(Value::Obj(ou))) = (n.get("uninstall"), o.get("uninstall")) else { return new };
    let mut keys: Vec<String> = ou.keys().to_vec();
    let mut merged: HashMap<String, Vec<String>> = ou.keys().iter().map(|k| (k.clone(), str_list(ou.value(k)))).collect();
    for k in nu.keys() {
        if !merged.contains_key(k) {
            keys.push(k.clone());
        }
        let have = merged.entry(k.clone()).or_default();
        for c in str_list(nu.value(k)) {
            if !have.contains(&c) {
                have.push(c);
            }
        }
    }
    let mut out = Object::new();
    for k in &keys {
        let set = &merged[k];
        let mut list: Vec<Value> = vec![];
        for c in super::components::COMPONENTS {
            if set.iter().any(|x| x == c.name) {
                list.push(Value::Str(c.name.to_string()));
            }
        }
        for c in set {
            if !super::components::COMPONENTS.iter().any(|x| x.name == c) {
                list.push(Value::Str(c.clone()));
            }
        }
        out.set(k, Value::List(list));
    }
    Value::Obj(Object::new().with("uninstall", Value::Obj(out)))
}

fn undo_summary(undo: &Value) -> String {
    let Value::Obj(o) = undo else { return "nothing to undo".into() };
    if let Some(Value::Obj(u)) = o.get("uninstall") {
        let parts: Vec<String> = u.keys().iter().map(|k| format!("{} from {k}", str_list(u.value(k)).join(","))).collect();
        return format!("uninstall {} (added by this recipe)", parts.join("; "));
    }
    if let Some(h) = o.get("harness") {
        return format!("remove {} extension", h.as_str().unwrap_or(""));
    }
    if let Some(Value::Obj(ro)) = o.get("restore") {
        let parts: Vec<String> =
            ro.keys().iter().map(|k| format!("{k}={}", shown(&ro.value(k).as_str().map(String::from)))).collect();
        return format!("restore {}", parts.join(" "));
    }
    format!("remove envelope {}", s(o, "remove_envelope"))
}

impl Env {
    pub(super) fn recipe_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(RECIPE_HELP);
            return Ok(());
        }
        match args[0].as_str() {
            "list" => self.recipe_list(&args[1..]),
            "show" => self.recipe_show(&args[1..]),
            "apply" => self.recipe_apply(&args[1..]),
            "revert" => self.recipe_revert(&args[1..]),
            other => {
                self.errf(&format!(
                    "Usage: daisugi recipe [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi recipe --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `_recipe_or_exit`.
    fn recipe_or_exit(&mut self, name: &str) -> Result<Recipe, super::Stop> {
        let names = recipes::names();
        if names.contains(&name) {
            return recipes::load(name).map_err(|e| self.fail("recipe", &e).unwrap_err());
        }
        self.errf(&format!("unknown recipe '{name}'. Recipes: {}\n", names.join(", ")));
        Err(exit(2).unwrap_err())
    }

    fn recipe_list(&mut self, args: &[String]) -> Res {
        const CMD: &str = "recipe list";
        let opts = [JSON_OPT];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "List the recipes.", &opts);
        }
        let mut rows = vec![];
        for n in recipes::names() {
            let r = recipes::load(n).map_err(|e| self.fail(CMD, &e).unwrap_err())?;
            if p.flag("--json") {
                rows.push(Value::Obj(
                    Object::new().with("name", r.name().as_str()).with("title", r.title().as_str()).with("doc", r.doc().as_str()),
                ));
            } else {
                self.out(&format!("{:<26}{}\n", r.name(), r.title()));
            }
        }
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps(&Value::List(rows), true)));
        }
        Ok(())
    }

    fn recipe_show(&mut self, args: &[String]) -> Res {
        const CMD: &str = "recipe show";
        let opts = [JSON_OPT];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " NAME", "Show a recipe: its slots, params and steps.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "NAME", "NAME");
        }
        let r = self.recipe_or_exit(&p.args[0])?;
        if p.flag("--json") {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(r.obj.clone()), 2, true)));
        } else {
            self.out(&format!("{}\n", r.render_show()));
        }
        Ok(())
    }

    /// `_parse_sets`: the --set keys in order, and their values.
    fn recipe_sets(&mut self, p: &Parsed) -> Result<(Vec<String>, HashMap<String, String>), super::Stop> {
        let mut order = vec![];
        let mut vals = HashMap::new();
        for item in p.list("--set") {
            match item.split_once('=') {
                Some((k, v)) if !k.is_empty() => {
                    if !vals.contains_key(k) {
                        order.push(k.to_string());
                    }
                    vals.insert(k.to_string(), v.to_string());
                }
                _ => {
                    self.errf(&format!("--set needs KEY=VALUE, not '{item}'\n"));
                    return Err(exit(2).unwrap_err());
                }
            }
        }
        Ok((order, vals))
    }

    fn recipe_config_path(&self) -> String {
        join(&self.data_home(), "config.yaml")
    }

    fn recipe_envelope_path(&self) -> String {
        join(&self.data_home(), "warden/envelopes/default.json")
    }

    fn recipe_runtimes(&self, step: &Object) -> Result<Vec<install::Runtime>, String> {
        let names = str_list(step.value("runtime"));
        if names.is_empty() {
            Ok(install::detect(&self.home, &self.env))
        } else {
            install::select(&names)
        }
    }

    fn recipe_opts(&self, step: &Object) -> Result<layers::Opts, String> {
        let only = super::components::component_layers(&str_list(step.value("only")))?;
        let mut url = s(step, "base_url");
        if url.is_empty() {
            url = super::installrouter::DEFAULT_BASE_URL.into();
        }
        Ok(layers::Opts {
            router: only.iter().any(|l| l == "base_url"),
            enforce: step.value("enforce").truthy(),
            ask: false,
            url,
            report: s(step, "report"),
            only: Some(only),
        })
    }

    /// `recipes._config_changes` with `_check_config`.
    fn recipe_config_changes(&self, step: &Object) -> Result<(Vec<CfgChange>, Config), String> {
        let path = self.recipe_config_path();
        let cfg = config::load(&path).map_err(|e| format!("{path} is not a config this binary reads: {e}"))?;
        let Value::Obj(sets) = step.value("set") else { return Err("a config step needs set".into()) };
        let update: HashMap<String, String> = sets.keys().iter().map(|k| (k.clone(), s(sets, k))).collect();
        let mut mode = cfg.routing_mode.clone();
        if let Some(v) = update.get("routing_mode") {
            mode = v.clone();
            if !recipes::ROUTING_MODES.contains(&v.as_str()) {
                return Err(format!("unknown routing_mode '{v}'. Choose one of: {}", recipes::ROUTING_MODES.join(", ")));
            }
        }
        if mode == "switchyard" {
            let mut sc = sy_config(&cfg);
            if let Some(v) = update.get("switchyard_efficient_model") {
                sc.efficient_model = Some(v.clone());
            }
            if let Some(v) = update.get("switchyard_capable_model") {
                sc.capable_model = v.clone();
            }
            if let Some(v) = update.get("switchyard_api_key_env") {
                sc.api_key_env = Some(v.clone());
            }
            if sc.efficient_model.as_deref().unwrap_or("").is_empty() {
                return Err("routing_mode switchyard needs switchyard_efficient_model: a local model id, or a claude-* id".into());
            }
            let targets = sy::targets_from_config(&sc).unwrap_or_default();
            if let Err(why) = sy::render(&targets, &cfg.switchyard_route_id, &|_| true) {
                return Err(format!("cannot build the Switchyard route: {why}"));
            }
        }
        let mut changes = vec![];
        for k in sets.keys() {
            let old = cfg_field(&cfg, k);
            if old.as_deref() != Some(update[k].as_str()) {
                changes.push(CfgChange { key: k.clone(), old, new: update[k].clone() });
            }
        }
        Ok((changes, cfg))
    }

    /// `recipes.plan_apply`.
    fn plan_recipe(&mut self, r: &Recipe, params: &Params) -> Result<Vec<StepPlan>, String> {
        let mut plan = vec![];
        for (i, raw) in r.steps().iter().enumerate() {
            let idx = i + 1;
            let when = s(raw, "when");
            if !when.is_empty() && !params.has(&when) {
                plan.push(StepPlan {
                    index: idx,
                    kind: s(raw, "kind"),
                    summary: recipes::step_summary(raw),
                    writes: vec![],
                    state: "skip",
                    notes: vec![format!("skipped (no --set {when})")],
                });
                continue;
            }
            let Value::Obj(step) = r.fill(&Value::Obj(raw.clone()), params, idx)? else { unreachable!() };
            let kind = s(&step, "kind");
            let mut entry = StepPlan {
                index: idx,
                kind: kind.clone(),
                summary: recipes::step_summary(&step),
                writes: vec![],
                state: "run",
                notes: vec![],
            };
            match kind.as_str() {
                "install" => {
                    let runtimes = self.recipe_runtimes(&step)?;
                    let o = self.recipe_opts(&step)?;
                    if o.enforce {
                        let envs = wardenroot::envelopes(&join(&self.data_home(), "warden")).unwrap_or_default();
                        if envs.is_empty() {
                            return Err(format!("step {idx}: enforce needs a registered envelope"));
                        }
                    }
                    if runtimes.is_empty() {
                        entry.state = "kept";
                        entry.notes = vec!["no supported agent runtime detected".into()];
                    } else {
                        for rt in &runtimes {
                            for st in layers::plan_all_steps(rt, &self.home, &o) {
                                if st.supported && !st.target.is_empty() {
                                    add_unique(&mut entry.writes, st.target.clone());
                                }
                            }
                        }
                    }
                }
                "harness" => entry.writes = vec![install::target(&s(&step, "name"), &self.home, &self.env)],
                "config" => {
                    let (changes, _) = self.recipe_config_changes(&step)?;
                    if changes.is_empty() {
                        entry.state = "kept";
                    } else {
                        entry.writes = vec![self.recipe_config_path()];
                        entry.notes = changes.iter().map(|c| format!("{}: {} -> {}", c.key, shown(&c.old), c.new)).collect();
                    }
                }
                "warden-init" => {
                    if wardenroot::exists(&self.recipe_envelope_path()) {
                        entry.state = "kept";
                    } else {
                        entry.writes = vec![self.recipe_envelope_path()];
                    }
                }
                _ => {}
            }
            plan.push(entry);
        }
        Ok(plan)
    }

    fn recipe_receipt_path(&self, name: &str) -> String {
        join(&self.data_home(), &format!("recipes/{name}.json"))
    }

    fn read_receipt(&self, name: &str) -> Option<Object> {
        let raw = std::fs::read_to_string(self.recipe_receipt_path(name)).ok()?;
        match crate::warden::pyjson::loads(&raw).ok()? {
            Value::Obj(o) => Some(o),
            _ => None,
        }
    }

    /// `recipes._run_step`: the paths written, the extra lines, the undo.
    fn run_recipe_step(&mut self, step: &Object) -> Result<(Vec<String>, Vec<String>, Option<Value>), String> {
        match s(step, "kind").as_str() {
            "note" => Ok((vec![], vec![], None)),
            "install" => {
                let runtimes = self.recipe_runtimes(step)?;
                if runtimes.is_empty() {
                    return Ok((vec![], vec!["no supported agent runtime detected".into()], None));
                }
                let o = self.recipe_opts(step)?;
                let me = self.self_path()?;
                let env = self.env.clone();
                let (me, _) = install::hook_path(&me, &self.env, &|tool: &str| install::mise_which(&env, tool));
                let root = join(&self.data_home(), "warden");
                // What each runtime lacks now: only that is this recipe's to undo.
                let want = super::components::component_layers(&str_list(step.value("only")))?;
                let mut lacking = Object::new();
                for rt in &runtimes {
                    let absent: Vec<String> = want
                        .iter()
                        .filter(|l| layers::wires(rt, l) && !layers::layer_present(rt, &self.home, l))
                        .cloned()
                        .collect();
                    if !absent.is_empty() {
                        let names = super::components::names_of(&absent);
                        lacking.set(rt.key, Value::List(names.into_iter().map(|n| Value::Str(n.to_string())).collect()));
                    }
                }
                let (mut written, mut failures) = (vec![], vec![]);
                for rt in &runtimes {
                    let ch = layers::plan_install(rt, &self.home, &me, &root, &o).map_err(|e| e.to_string())?;
                    let (_, res) = install::apply_change(&ch);
                    res.map_err(|e| e.to_string())?;
                    if ch.failed {
                        failures.push(format!("{}: {}", ch.runtime.name, ch.why));
                    } else {
                        for w in ch.written() {
                            add_unique(&mut written, w);
                        }
                    }
                }
                if !failures.is_empty() {
                    return Err(failures.join("; "));
                }
                let undo = (!written.is_empty() && lacking.len() > 0)
                    .then(|| Value::Obj(Object::new().with("uninstall", Value::Obj(lacking))));
                Ok((written, vec![], undo))
            }
            "harness" => {
                let name = s(step, "name");
                let existed = std::fs::metadata(install::target(&name, &self.home, &self.env)).is_ok();
                let paths = install::install_harness(&name, &self.home, &self.env).map_err(|e| match e {
                    install::HarnessErr::Value(m) => m,
                    install::HarnessErr::Io(e) => e.to_string(),
                })?;
                // A file that was there before is not this recipe's to remove.
                let undo = (!existed).then(|| Value::Obj(Object::new().with("harness", name.as_str())));
                Ok((paths, vec![], undo))
            }
            "config" => {
                let (changes, cfg) = self.recipe_config_changes(step)?;
                if changes.is_empty() {
                    return Ok((vec![], vec![], None));
                }
                let path = self.recipe_config_path();
                let mut updates = vec![];
                let mut restore = Object::new();
                let mut lines = vec![];
                for c in &changes {
                    updates.push((c.key.clone(), Value::Str(c.new.clone())));
                    restore.set(&c.key, match &c.old { Some(o) => Value::Str(o.clone()), None => Value::Null });
                    lines.push(format!("{}: {} -> {}", c.key, shown(&c.old), c.new));
                }
                config::save(&path, &self.home, &updates).map_err(|e| format!("{e:?}"))?;
                let mut written = vec![path.clone()];
                let new = config::load(&path).map_err(|e| e.to_string())?;
                if new.routing_mode == "switchyard" {
                    let targets = sy::targets_from_config(&sy_config(&new)).unwrap_or_default();
                    let text = sy::render(&targets, &cfg.switchyard_route_id, &|_| true)?;
                    let toml = sy::write_config(&self.data_home(), &text, "switchyard.toml").map_err(|e| e.to_string())?;
                    written.push(toml);
                }
                Ok((written, lines, Some(Value::Obj(Object::new().with("restore", Value::Obj(restore))))))
            }
            _ => {
                let target = self.recipe_envelope_path();
                if wardenroot::exists(&target) {
                    return Ok((vec![], vec![], None));
                }
                let cfg = config::load(&self.recipe_config_path()).map_err(|e| e.to_string())?;
                let ws = {
                    let w = s(step, "workspace");
                    let w = if w == "~" {
                        self.home.clone()
                    } else if let Some(rest) = w.strip_prefix("~/") {
                        format!("{}/{rest}", self.home)
                    } else {
                        w
                    };
                    wardenroot::resolve(&w).map_err(|e| e.to_string())?
                };
                let env = super::envelope::starter(&ws, cfg.shell_allow_decomposition).map_err(|e| e.to_string())?;
                let (path, res) = super::envelope::register(&env, "", &join(&self.data_home(), "warden"));
                res.map_err(|e| e.to_string())?;
                Ok((vec![path], vec![], Some(Value::Obj(Object::new().with("remove_envelope", "default")))))
            }
        }
    }

    fn recipe_apply(&mut self, args: &[String]) -> Res {
        const CMD: &str = "recipe apply";
        let opts = [
            Opt::many(&["--set"], "TEXT", "A recipe param, KEY=VALUE. Repeatable."),
            Opt::flag(&["--yes", "-y"], "Write the changes. Without it: a dry run."),
            JSON_OPT,
        ];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " NAME", "Show what a recipe would write; with --yes, write it.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "NAME", "NAME");
        }
        let name = p.args[0].clone();
        let r = self.recipe_or_exit(&name)?;
        let (order, given) = self.recipe_sets(&p)?;
        let params = match r.resolve_params(&order, &given) {
            Ok(x) => x,
            Err(m) => {
                self.errf(&format!("{m}\n"));
                return exit(2);
            }
        };
        let plan = match self.plan_recipe(&r, &params) {
            Ok(x) => x,
            Err(m) => {
                self.errf(&format!("{m}\n"));
                return exit(2);
            }
        };
        let total = r.steps().len();
        let as_json = p.flag("--json");
        if !p.flag("--yes") {
            if as_json {
                let steps = Value::List(plan.iter().map(|s| Value::Obj(s.object())).collect());
                let doc = Object::new().with("recipe", name.as_str()).with("dry_run", true).with("steps", steps);
                self.out(&format!("{}\n", dumps_indent(&Value::Obj(doc), 2, true)));
                return Ok(());
            }
            for l in render_plan(total, &plan, "would write") {
                self.out(&format!("{l}\n"));
            }
            self.out("Dry run. Add --yes to apply.\n");
            return Ok(());
        }
        // The names of the warden and the router changed: move the owner's own
        // state once, before anything new is written (as `daisugi install` does).
        {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs() as i64)
                .unwrap_or(0);
            let mut sink = Vec::new();
            let moved = super::moveold::move_old_state(&self.data_home(), &self.home, &self.env, now, &mut sink, false);
            if as_json {
                self.errf(&String::from_utf8_lossy(&sink));
            } else {
                self.out(&String::from_utf8_lossy(&sink));
            }
            if moved.is_err() {
                return exit(2);
            }
        }
        let mut old_undo: HashMap<String, Value> = HashMap::new();
        let old_receipt = self.read_receipt(&name);
        if let Some(old) = &old_receipt {
            if let Value::List(list) = old.value("steps") {
                for st in list {
                    if let Value::Obj(so) = st {
                        if let Value::Int(ix) = so.value("index") {
                            old_undo.insert(ix.clone(), so.value("undo").clone());
                        }
                    }
                }
            }
        }
        let mut done = vec![];
        let mut completed = 0usize;
        let mut failed: Option<(usize, String)> = None;
        for (i, raw) in r.steps().iter().enumerate() {
            let idx = i + 1;
            let entry = &plan[i];
            if !as_json {
                self.out(&format!("[{idx}/{total}] {}  {}\n", s(raw, "kind"), entry.summary));
            }
            if entry.state == "skip" {
                if !as_json {
                    self.out(&format!("  {}\n", entry.notes[0]));
                }
                completed = idx;
                if let Some(prev) = old_undo.get(&idx.to_string()).filter(|v| !matches!(v, Value::Null)) {
                    // The earlier run's undo stays.
                    done.push(Value::Obj(Object::new().with("index", int(idx)).with("kind", s(raw, "kind").as_str()).with("undo", prev.clone())));
                }
                continue;
            }
            let Ok(Value::Obj(step)) = r.fill(&Value::Obj(raw.clone()), &params, idx) else { unreachable!() };
            let (written, lines, undo) = match self.run_recipe_step(&step) {
                Ok(x) => x,
                Err(why) => {
                    failed = Some((idx, why));
                    break;
                }
            };
            if !as_json {
                for w in &written {
                    self.out(&format!("  wrote {w}\n"));
                }
                for l in &lines {
                    if s(&step, "kind") == "config" {
                        self.out(&format!("    {l}\n"));
                    } else {
                        self.out(&format!("  {l}\n"));
                    }
                }
                if written.is_empty() && lines.is_empty() && s(&step, "kind") != "note" {
                    self.out("  kept\n");
                }
            }
            let undo_v = merge_undo(undo, old_undo.get(&idx.to_string()));
            done.push(Value::Obj(Object::new().with("index", int(idx)).with("kind", s(raw, "kind").as_str()).with("undo", undo_v)));
            completed = idx;
        }
        if failed.is_some() {
            // Steps this run did not reach keep the undo of the run that did them.
            if let Some(old) = &old_receipt {
                if let Value::List(list) = old.value("steps") {
                    for st in list {
                        if let Value::Obj(so) = st {
                            let ixs = match so.value("index") { Value::Int(t) => t.clone(), _ => continue };
                            let have = done.iter().any(|d| matches!(d, Value::Obj(o) if matches!(o.value("index"), Value::Int(t) if *t == ixs)));
                            if !have {
                                done.push(st.clone());
                            }
                        }
                    }
                }
            }
            let key = |v: &Value| match v {
                Value::Obj(o) => match o.value("index") { Value::Int(t) => t.parse::<i64>().unwrap_or(0), _ => 0 },
                _ => 0,
            };
            done.sort_by_key(|v| key(v));
        }
        let receipt = Object::new()
            .with("recipe", name.as_str())
            .with("applied_at", format!("{}Z", iso_seconds(now_seconds() as i64)).as_str())
            .with("params", Value::Obj(params.object()))
            .with("completed", int(completed))
            .with("steps", Value::List(done));
        let path = self.recipe_receipt_path(&name);
        if let Err(e) = std::fs::create_dir_all(wardenroot::parent(&path)) {
            return self.fail(CMD, &e.to_string());
        }
        if let Err(e) = std::fs::write(&path, format!("{}\n", dumps_indent(&Value::Obj(receipt.clone()), 2, true))) {
            return self.fail(CMD, &e.to_string());
        }
        if let Some((idx, why)) = failed {
            self.errf(&format!("step {idx} failed: {why}\n"));
            self.errf(&format!("recipe stopped at step {idx}; nothing after it ran. Run: daisugi recipe revert {name}\n"));
            return exit(1);
        }
        if as_json {
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(receipt), 2, true)));
        }
        Ok(())
    }

    fn recipe_revert(&mut self, args: &[String]) -> Res {
        const CMD: &str = "recipe revert";
        let opts = [Opt::flag(&["--yes", "-y"], "Undo the changes. Without it: a dry run.")];
        let p = parse_args(args, &opts, 1).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, " NAME", "Undo a recipe from its receipt.", &opts);
        }
        if p.args.is_empty() {
            return self.missing_arg(CMD, "NAME", "NAME");
        }
        let name = p.args[0].clone();
        let Some(receipt) = self.read_receipt(&name) else {
            self.errf(&format!(
                "nothing to revert: no receipt for '{name}' under {}/\n",
                join(&self.data_home(), "recipes")
            ));
            return exit(1);
        };
        let r = recipes::load(&name).map_err(|e| self.fail(CMD, &e).unwrap_err())?;
        let total = r.steps().len();
        let steps: Vec<Object> = match receipt.value("steps") {
            Value::List(l) => l.iter().filter_map(|v| if let Value::Obj(o) = v { Some(o.clone()) } else { None }).collect(),
            _ => vec![],
        };
        let ix = |st: &Object| match st.value("index") {
            Value::Int(t) => t.clone(),
            _ => String::new(),
        };
        for st in &steps {
            let known = match st.value("undo") {
                Value::Null => true,
                Value::Obj(o) => ["uninstall", "harness", "restore", "remove_envelope"].iter().any(|k| o.get(k).is_some()),
                _ => false,
            };
            if !known {
                self.errf(&format!(
                    "receipt for '{name}' holds a step this daisugi does not read; nothing was changed. Remove {} by hand.\n",
                    self.recipe_receipt_path(&name)
                ));
                return exit(2);
            }
        }
        if !p.flag("--yes") {
            for st in steps.iter().rev() {
                self.out(&format!("[{}/{total}] {}  {}\n", ix(st), s(st, "kind"), undo_summary(st.value("undo"))));
            }
            self.out("Dry run. Add --yes to revert.\n");
            return Ok(());
        }
        let mut left: Vec<Object> = vec![];
        for st in steps.iter().rev() {
            let undo = st.value("undo").clone();
            self.out(&format!("[{}/{total}] {}  {}\n", ix(st), s(st, "kind"), undo_summary(&undo)));
            let Value::Obj(uo) = &undo else { continue };
            let mut removed: Vec<String> = vec![];
            let mut failures: Vec<String> = vec![];
            if let Some(Value::Obj(un)) = uo.get("uninstall") {
                for key in un.keys() {
                    let only = super::components::component_layers(&str_list(un.value(key)))
                        .map_err(|e| self.fail(CMD, &e).unwrap_err())?;
                    let Ok(runtimes) = install::select(&[key.clone()]) else {
                        failures.push(format!("{key}: not a runtime this CLI knows"));
                        continue;
                    };
                    let mut gone = std::collections::HashSet::new();
                    for rt in &runtimes {
                        let ch = layers::plan_reverse_all(rt, &self.home, &mut gone, Some(&only))
                            .map_err(|e| self.refuse(CMD, &e.to_string()).unwrap_err())?;
                        if ch.failed {
                            failures.push(format!("{}: {}", ch.runtime.name, ch.why));
                            continue;
                        }
                        let (_, res) = install::apply_change(&ch);
                        res.map_err(|e| self.fail(CMD, &e.to_string()).unwrap_err())?;
                        removed.extend(ch.written());
                    }
                }
            } else if let Some(h) = uo.get("harness") {
                removed = install::uninstall_harness(h.as_str().unwrap_or(""), &self.home, &self.env)
                    .map_err(|e| self.fail(CMD, &e.to_string()).unwrap_err())?;
            } else if let Some(Value::Obj(ro)) = uo.get("restore") {
                let updates: Vec<(String, Value)> = ro.keys().iter().map(|k| (k.clone(), ro.value(k).clone())).collect();
                let path = self.recipe_config_path();
                config::save(&path, &self.home, &updates).map_err(|e| self.fail(CMD, &format!("{e:?}")).unwrap_err())?;
                self.out(&format!("  wrote {path}\n"));
                continue;
            } else {
                let target = join(&self.data_home(), &format!("warden/envelopes/{}.json", s(uo, "remove_envelope")));
                if std::fs::remove_file(&target).is_ok() {
                    removed = vec![target];
                }
            }
            for r in &removed {
                self.out(&format!("  removed {r}\n"));
            }
            if !failures.is_empty() {
                for f in &failures {
                    self.out(&format!("  not undone: {f}\n"));
                }
                left.insert(0, st.clone());
            } else if removed.is_empty() {
                self.out("  kept\n");
            }
        }
        if !left.is_empty() {
            let mut kept = receipt.clone();
            kept.set("steps", Value::List(left.into_iter().map(Value::Obj).collect()));
            if let Err(e) = std::fs::write(self.recipe_receipt_path(&name), format!("{}\n", dumps_indent(&Value::Obj(kept), 2, true))) {
                return self.fail(CMD, &e.to_string());
            }
            self.errf(&format!("Some steps were not undone. The receipt keeps them. Fix the cause, then run: daisugi recipe revert {name}\n"));
            return exit(1);
        }
        if let Err(e) = std::fs::remove_file(self.recipe_receipt_path(&name)) {
            return self.fail(CMD, &e.to_string());
        }
        self.out("Reverted. Receipt deleted.\n");
        Ok(())
    }
}
