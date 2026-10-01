//! `daisugi models list|search|use`: the model catalog for the garden
//! (`opendaisugi.model_catalog`). The catalog is the oracle's own file;
//! the Go client's `internal/catalog` and `cli/modelscmd.go` are the
//! reference.

use super::gateroot::{join, path_str};
use super::pathwayscmd::py_int;
use super::setupcmd::py_g;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::argparse::py_float;
use crate::gate::py::text;
use crate::gate::pyjson::{dumps_indent, loads_py, Object, Value};
use crate::netproxy;

/// `src/opendaisugi/model_catalog.json`, the file Python ships.
const RAW: &str = include_str!("../../../../src/opendaisugi/model_catalog.json");
const CHOICE_FILE: &str = "garden_model.json";
const DEFAULT_ENDPOINT: &str = "https://huggingface.co";
const SEARCH_LIMIT: usize = 100;
const EXPAND: [&str; 5] = ["cardData", "gguf", "pipeline_tag", "safetensors", "tags"];
/// The most bytes a model list may hold.
const LISTING_CAP: u64 = 64 << 20;

const MODELS_HELP: &str = "Usage: daisugi models [OPTIONS] COMMAND [ARGS]...

  The models the garden can use: a curated list, a search, and your choice.

Options:
  --help  Show this message and exit.

Commands:
  list    List the curated models, the default for this box, and the one in use.
  search  Search the Hugging Face API for models, filtered by size and license.
  use     Record the model the garden uses. Any id, path or GGUF file is accepted.
  pin     Resolve a repo to a file pinned to its commit; --pull downloads it.
";

/// One model in the catalog's fields. `license`, `gguf` and `suits` are
/// None or a string; `params` and `context` None or an integer's text.
#[derive(Clone, Debug)]
pub struct Row {
    pub id: String,
    pub params: Option<String>,
    pub license: Option<String>,
    pub context: Option<String>,
    pub gguf: Option<String>,
    pub suits: Option<String>,
    pub note: String,
}

impl Row {
    /// The row as the oracle dumps it.
    pub fn object(&self) -> Object {
        let int = |v: &Option<String>| v.clone().map(Value::Int).unwrap_or(Value::Null);
        Object::new()
            .with("id", self.id.as_str())
            .with("params", int(&self.params))
            .with("license", self.license.clone())
            .with("context_length", int(&self.context))
            .with("gguf", self.gguf.clone())
            .with("suits", self.suits.clone())
            .with("note", self.note.as_str())
    }
}

fn num(v: &Value) -> f64 {
    match v {
        Value::Float(f) => *f,
        Value::Int(t) => t.parse().unwrap_or(0.0),
        _ => 0.0,
    }
}

fn int_of(v: &Value) -> Option<String> {
    match v {
        Value::Int(t) => Some(t.clone()),
        _ => None,
    }
}

fn str_of(v: &Value) -> Option<String> {
    v.as_str().map(str::to_string)
}

/// The loaded catalog.
pub struct Catalog {
    pub models: Vec<Row>,
    capable_min_ram: f64,
    capable_min_vram: f64,
    weak_max_params: f64,
    defaults: Object,
    search_max: Object,
}

impl Catalog {
    pub fn load() -> Result<Catalog, String> {
        let doc = loads_py(RAW, 100)?;
        let doc = doc.as_obj().ok_or("the model catalog is not an object")?;
        let models = match doc.value("models") {
            Value::List(l) => l
                .iter()
                .filter_map(|m| m.as_obj())
                .map(|o| Row {
                    id: str_of(o.value("id")).unwrap_or_default(),
                    params: int_of(o.value("params")),
                    license: str_of(o.value("license")),
                    context: int_of(o.value("context_length")),
                    gguf: str_of(o.value("gguf")),
                    suits: str_of(o.value("suits")),
                    note: str_of(o.value("note")).unwrap_or_default(),
                })
                .collect(),
            _ => vec![],
        };
        Ok(Catalog {
            models,
            capable_min_ram: num(doc.value("capable_min_ram_gb")),
            capable_min_vram: num(doc.value("capable_min_vram_gb")),
            weak_max_params: num(doc.value("weak_max_params")),
            defaults: doc.value("defaults").as_obj().cloned().unwrap_or_default(),
            search_max: doc
                .value("search_max_params_b")
                .as_obj()
                .cloned()
                .unwrap_or_default(),
        })
    }

    /// `hardware_class`: capable with enough GPU memory or RAM.
    pub fn class(&self, ram: Option<f64>, vram: f64) -> &'static str {
        if vram >= self.capable_min_vram || ram.is_some_and(|r| r >= self.capable_min_ram) {
            "capable"
        } else {
            "weak"
        }
    }

    pub fn default_model(&self, ram: Option<f64>, vram: f64) -> String {
        self.defaults
            .value(self.class(ram, vram))
            .as_str()
            .unwrap_or("")
            .to_string()
    }

    pub fn search_max(&self, ram: Option<f64>, vram: f64) -> f64 {
        num(self.search_max.value(self.class(ram, vram)))
    }

    /// `hardware_line`.
    pub fn hardware_line(&self, ram: Option<f64>, vram: f64) -> String {
        let r = match ram {
            Some(r) => format!("{} GB of RAM", py_g(r)),
            None => "an unknown amount of RAM".into(),
        };
        let gpu = if vram > 0.0 {
            format!(" and a GPU with {} GB", py_g(vram))
        } else {
            String::new()
        };
        format!(
            "This box has {r}{gpu}, so the default is {}.",
            self.default_model(ram, vram)
        )
    }

    /// `parse_listing`; Err is BadAnswer.
    pub fn parse_listing(&self, raw: &str) -> Result<Vec<Row>, ()> {
        let Ok(Value::List(list)) = loads_py(raw, 900) else {
            return Err(());
        };
        let empty = Object::new();
        let mut rows = vec![];
        for item in &list {
            let Some(row) = item.as_obj() else { continue };
            let Some(id) = row.value("id").as_str() else {
                continue;
            };
            let st = row.value("safetensors").as_obj().unwrap_or(&empty);
            let gg = row.value("gguf").as_obj().unwrap_or(&empty);
            let params = int_of(st.value("total")).or_else(|| int_of(gg.value("total")));
            let mut has_gguf = row.value("gguf").as_obj().is_some();
            if let Value::List(tags) = row.value("tags") {
                has_gguf |= tags.iter().any(|t| t.as_str() == Some("gguf"));
            }
            let suits = params.as_ref().map(|p| {
                if less_eq(p, self.weak_max_params) {
                    "weak"
                } else {
                    "capable"
                }
                .to_string()
            });
            rows.push(Row {
                id: id.to_string(),
                params,
                license: license(row),
                context: int_of(gg.value("context_length")),
                gguf: has_gguf.then(|| id.to_string()),
                suits,
                note: str_of(row.value("pipeline_tag")).unwrap_or_default(),
            });
        }
        Ok(rows)
    }
}

fn license(row: &Object) -> Option<String> {
    let card = row.value("cardData").as_obj();
    let lic = card.map(|c| c.value("license")).unwrap_or(&Value::Null);
    if lic.as_str() == Some("other") {
        if let Some(name) = card.and_then(|c| c.value("license_name").as_str()) {
            return Some(name.to_string());
        }
    }
    if let Some(s) = lic.as_str() {
        return Some(s.to_string());
    }
    if let Value::List(tags) = row.value("tags") {
        for t in tags {
            if let Some(s) = t.as_str().and_then(|s| s.strip_prefix("license:")) {
                return Some(s.to_string());
            }
        }
    }
    None
}

/// `int(text) <= f`, exactly for the sizes a model has.
fn less_eq(text: &str, f: f64) -> bool {
    match text.parse::<i64>() {
        Ok(n) if n.abs() < 1 << 53 => (n as f64) <= f,
        _ => text.parse::<f64>().unwrap_or(f64::INFINITY) <= f,
    }
}

/// `fmt_params`.
pub fn fmt_params(n: &Option<String>) -> String {
    match n {
        None => "?".into(),
        Some(t) => format!("{:.1}B", t.parse::<f64>().unwrap_or(0.0) / 1e9),
    }
}

/// `filter_rows`.
pub fn filter_rows(rows: &[Row], max_b: f64, licenses: &[String], limit: usize) -> Vec<Row> {
    rows.iter()
        .filter(|r| !(max_b > 0.0 && !r.params.as_ref().is_some_and(|p| less_eq(p, max_b * 1e9))))
        .filter(|r| licenses.is_empty() || r.license.as_ref().is_some_and(|l| licenses.contains(l)))
        .take(limit)
        .cloned()
        .collect()
}

/// `urllib.parse.quote(s, safe="")`.
fn quote(s: &str) -> String {
    let mut out = String::new();
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || b"_.-~".contains(&b) {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// `search_target`: the request target in one fixed order.
pub fn search_target(query: &str) -> String {
    let mut parts = vec![
        format!("search={}", quote(query)),
        format!("limit={SEARCH_LIMIT}"),
        "sort=downloads".into(),
        "direction=-1".into(),
    ];
    parts.extend(EXPAND.iter().map(|e| format!("expand%5B%5D={e}")));
    format!("/api/models?{}", parts.join("&"))
}

fn or_q(v: &Option<String>) -> String {
    match v {
        Some(s) if !s.is_empty() => s.clone(),
        _ => "?".into(),
    }
}

/// `table_lines`: a header and a line per row, every column padded but
/// the last, each line without trailing white space.
pub fn table_lines(rows: &[Row]) -> Vec<String> {
    let mut grid: Vec<Vec<String>> = vec![[
        "ID", "SIZE", "LICENSE", "CONTEXT", "GGUF", "SUITS", "GOOD AT",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect()];
    for r in rows {
        grid.push(vec![
            r.id.clone(),
            fmt_params(&r.params),
            or_q(&r.license),
            r.context.clone().unwrap_or_else(|| "?".into()),
            if r.gguf.as_ref().is_some_and(|g| !g.is_empty()) {
                "yes"
            } else {
                "no"
            }
            .into(),
            or_q(&r.suits),
            r.note.clone(),
        ]);
    }
    let n = grid[0].len();
    let widths: Vec<usize> = (0..n - 1)
        .map(|i| grid.iter().map(|row| text::len(&row[i])).max().unwrap_or(0))
        .collect();
    grid.iter()
        .map(|row| {
            let mut parts: Vec<String> = (0..n - 1)
                .map(|i| format!("{}{}", row[i], " ".repeat(widths[i] - text::len(&row[i]))))
                .collect();
            parts.push(row[n - 1].clone());
            text::rstrip(&format!("  {}", parts.join("  "))).to_string()
        })
        .collect()
}

/// Why a search found nothing to show.
enum SearchErr {
    Offline,
    Status(u16),
    BadAnswer,
}

fn offline_env(env: &std::collections::HashMap<String, String>) -> bool {
    matches!(
        env.get("HF_HUB_OFFLINE").map(|v| text::upper(v)).as_deref(),
        Some("1" | "ON" | "YES" | "TRUE")
    )
}

impl Env {
    pub(super) fn models(&mut self, args: &[String]) -> Res {
        let Some(first) = args.first() else {
            self.out(MODELS_HELP);
            return exit(2);
        };
        match first.as_str() {
            "--help" => {
                self.out(MODELS_HELP);
                Ok(())
            }
            "list" => self.models_list(&args[1..]),
            "search" => self.models_search(&args[1..]),
            "use" => self.models_use(&args[1..]),
            "pin" => self.models_pin(&args[1..]),
            sub => {
                self.errf(&format!(
                    "Usage: daisugi models [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi models --help' for help.\n\nError: No such command '{sub}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// `detect_voice_hardware`: OPENDAISUGI_VOICE_HARDWARE when it is
    /// set, else the probe tiers setup uses.
    pub(super) fn models_hardware(&self) -> crate::voice::hardware::VoiceHardware {
        let raw = self
            .env
            .get(crate::voice::hardware::HARDWARE_ENV)
            .cloned()
            .unwrap_or_default();
        if !raw.is_empty() {
            if let Some(hw) = crate::voice::hardware::parse_hardware_env(&raw) {
                return hw;
            }
        }
        self.voice_hardware()
    }

    fn data_dir(&self, p: &super::Parsed) -> String {
        path_str(&p.str("--data-dir", &self.data_home()))
    }

    pub(super) fn models_catalog(&mut self, cmd: &str) -> Result<Catalog, super::Stop> {
        self.catalog(cmd)
    }

    fn catalog(&mut self, cmd: &str) -> Result<Catalog, super::Stop> {
        Catalog::load().map_err(|e| match self.fail(cmd, &e) {
            Err(s) => s,
            Ok(()) => super::Stop::Exit(1),
        })
    }

    fn models_list(&mut self, args: &[String]) -> Res {
        const CMD: &str = "models list";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = match parse_args(args, &opts, 0) {
            Ok(p) => p,
            Err(m) => return self.usage(CMD, m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "List the curated models, the default for this box, and the one in use.",
                &opts,
            );
        }
        let cat = self.catalog(CMD)?;
        let data_dir = self.data_dir(&p);
        let hw = self.models_hardware();
        let def = cat.default_model(hw.ram_gb, hw.vram_gb);
        let chosen = in_use(&data_dir);
        if p.flag("--json") {
            let models: Vec<Value> = cat.models.iter().map(|m| Value::Obj(m.object())).collect();
            let payload = Object::new()
                .with(
                    "hardware",
                    Object::new()
                        .with("ram_gb", hw.ram_gb)
                        .with("vram_gb", hw.vram_gb)
                        .with("class", cat.class(hw.ram_gb, hw.vram_gb)),
                )
                .with("default", def)
                .with("in_use", chosen)
                .with("models", models);
            self.out(&format!(
                "{}\n",
                dumps_indent(&Value::Obj(payload), 2, true)
            ));
            return Ok(());
        }
        self.out(&format!("{}\n", cat.hardware_line(hw.ram_gb, hw.vram_gb)));
        match chosen {
            Some(c) => self.out(&format!("In use: {c} (recorded by daisugi models use).\n")),
            None => self.out("In use: the default.\n"),
        }
        self.out("\n");
        for ln in table_lines(&cat.models) {
            self.out(&format!("{ln}\n"));
        }
        self.out("\nAny other model works too: daisugi models search QUERY, then daisugi models use ID.\n");
        Ok(())
    }

    fn models_use(&mut self, args: &[String]) -> Res {
        const CMD: &str = "models use";
        let opts = [Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.")];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "ID", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " ID",
                "Record the model the garden uses. Any id, path or GGUF file is accepted.",
                &opts,
            );
        }
        let Some(id) = p.args.first().cloned() else {
            return self.usage_args(CMD, "ID", "Missing argument 'ID'.");
        };
        if text::strip(&id).is_empty() {
            return Err(self.fail3(
                "A model id cannot be blank.",
                "models use records the id it is given, as given.",
                "run: daisugi models list",
                2,
            ));
        }
        let data_dir = self.data_dir(&p);
        let path = join(&data_dir, CHOICE_FILE);
        let body = format!(
            "{}\n",
            dumps_indent(
                &Value::Obj(Object::new().with("model", id.as_str())),
                2,
                true
            )
        );
        let wrote = std::fs::create_dir_all(&data_dir).and_then(|_| std::fs::write(&path, body));
        if let Err(e) = wrote {
            return self.fail(CMD, &e.to_string());
        }
        self.out(&format!("The garden now uses {id} (recorded in {path}).\n"));
        Ok(())
    }

    /// `model_catalog.fetch` then `parse_listing`.
    fn hf_listing(&self, cat: &Catalog, url: &str) -> Result<Vec<Row>, SearchErr> {
        let proxies = netproxy::urllib_for(&self.env);
        let (_, status, body) =
            crate::pathways::potion::fetch::get_reply(url, LISTING_CAP, &proxies)
                .map_err(|_| SearchErr::Offline)?;
        if !(200..300).contains(&status) {
            return Err(SearchErr::Status(status));
        }
        let text = String::from_utf8(body).map_err(|_| SearchErr::BadAnswer)?;
        cat.parse_listing(&text).map_err(|_| SearchErr::BadAnswer)
    }

    fn models_search(&mut self, args: &[String]) -> Res {
        const CMD: &str = "models search";
        let opts = [
            Opt::val(
                &["--max-params"],
                "FLOAT",
                "Largest size to show, in billions of parameters; 0 shows any size. Default: 8 on a capable box, 2 on a small one.",
            ),
            Opt::many(&["--license"], "TEXT", "Show only this license (repeatable), such as apache-2.0."),
            Opt::val(&["--limit"], "INTEGER RANGE", "Most results to show."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "QUERY", &m),
        };
        if p.help {
            return self.cmd_help(
                CMD,
                " QUERY",
                "Search the Hugging Face API for models, filtered by size and license.",
                &opts,
            );
        }
        let Some(query) = p.args.first().cloned() else {
            return self.usage_args(CMD, "QUERY", "Missing argument 'QUERY'.");
        };
        let mut max_b = 0.0;
        if p.has("--max-params") {
            let raw = p.str("--max-params", "");
            match py_float(&raw) {
                Some(f) => max_b = f,
                None => {
                    return self.usage_args(
                        CMD,
                        "QUERY",
                        &format!(
                            "Invalid value for '--max-params': {} is not a valid float.",
                            text::repr(&raw)
                        ),
                    )
                }
            }
        }
        let mut limit = 20usize;
        if p.has("--limit") {
            let raw = p.str("--limit", "");
            let Some(n) = py_int(&raw) else {
                return self.usage_args(
                    CMD,
                    "QUERY",
                    &format!(
                        "Invalid value for '--limit': {} is not a valid int range.",
                        text::repr(&raw)
                    ),
                );
            };
            if !(1..=100).contains(&n) {
                return self.usage_args(
                    CMD,
                    "QUERY",
                    &format!("Invalid value for '--limit': {n} is not in the range 1<=x<=100."),
                );
            }
            limit = n as usize;
        }
        let cat = self.catalog(CMD)?;
        if !p.has("--max-params") {
            let hw = self.models_hardware();
            max_b = cat.search_max(hw.ram_gb, hw.vram_gb);
        }
        let lics = p.list("--license");
        let endpoint = match self.env.get("HF_ENDPOINT").filter(|e| !e.is_empty()) {
            Some(e) => e.trim_end_matches('/').to_string(),
            None => DEFAULT_ENDPOINT.to_string(),
        };
        let rows = if offline_env(&self.env) {
            Err(SearchErr::Offline)
        } else {
            self.hf_listing(&cat, &format!("{endpoint}{}", search_target(&query)))
        };
        let rows = match rows {
            Ok(r) => r,
            Err(SearchErr::Offline) => {
                if offline_env(&self.env) {
                    self.errf(
                        "Offline: HF_HUB_OFFLINE is set, so the Hugging Face API was not asked.\n",
                    );
                } else {
                    self.errf(&format!(
                        "Offline: the Hugging Face API at {endpoint} could not be reached.\n"
                    ));
                }
                return exit(1);
            }
            Err(SearchErr::Status(code)) => {
                self.errf(&format!(
                    "The Hugging Face API at {endpoint} answered HTTP {code}.\n"
                ));
                return exit(1);
            }
            Err(SearchErr::BadAnswer) => {
                self.errf(&format!(
                    "The Hugging Face API at {endpoint} did not answer with a model list.\n"
                ));
                return exit(1);
            }
        };
        let shown = filter_rows(&rows, max_b, &lics, limit);
        if p.flag("--json") {
            let results: Vec<Value> = shown.iter().map(|r| Value::Obj(r.object())).collect();
            let ls: Vec<Value> = lics.iter().map(|l| Value::Str(l.clone())).collect();
            let payload = Object::new()
                .with("query", query.as_str())
                .with("endpoint", endpoint.as_str())
                .with("max_params_b", max_b)
                .with("licenses", ls)
                .with("results", results);
            self.out(&format!(
                "{}\n",
                dumps_indent(&Value::Obj(payload), 2, true)
            ));
            return Ok(());
        }
        let size = if max_b <= 0.0 {
            "any size".to_string()
        } else {
            format!("up to {}B parameters", py_g(max_b))
        };
        let lic = if lics.is_empty() {
            "any license".to_string()
        } else {
            format!("license {}", lics.join(" or "))
        };
        self.out(&format!(
            "Search: \"{query}\" on {endpoint}, {size}, {lic}.\n\n"
        ));
        if shown.is_empty() {
            self.out("No model matches.\n");
            return Ok(());
        }
        for ln in table_lines(&shown) {
            self.out(&format!("{ln}\n"));
        }
        self.out("\nRecord one for the garden: daisugi models use ID\n");
        Ok(())
    }
}

/// `model_catalog.in_use`.
pub(super) fn in_use(data_dir: &str) -> Option<String> {
    let raw = std::fs::read(join(data_dir, CHOICE_FILE)).ok()?;
    let text = String::from_utf8(raw).ok()?;
    let v = loads_py(&text, 900).ok()?;
    let s = v.as_obj()?.value("model").as_str()?.to_string();
    (!text::strip(&s).is_empty()).then_some(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    const ENTRY_KEYS: [&str; 7] = [
        "id",
        "params",
        "license",
        "context_length",
        "gguf",
        "suits",
        "note",
    ];

    #[test]
    fn the_default_follows_the_hardware() {
        let c = Catalog::load().unwrap();
        assert_eq!(c.class(Some(16.0), 0.0), "capable");
        assert_eq!(c.class(Some(4.0), 0.0), "weak");
        assert_eq!(c.class(Some(4.0), 8.0), "capable");
        assert_eq!(c.class(None, 0.0), "weak");
        assert_eq!(
            c.default_model(Some(16.0), 0.0),
            "ibm-granite/granite-4.1-3b"
        );
        assert_eq!(c.default_model(None, 0.0), "ibm-granite/granite-4.0-1b");
        assert_eq!(
            c.hardware_line(Some(4.0), 8.0),
            "This box has 4 GB of RAM and a GPU with 8 GB, so the default is ibm-granite/granite-4.1-3b."
        );
    }

    #[test]
    fn each_default_carries_a_permissive_licence() {
        let c = Catalog::load().unwrap();
        for (ram, vram) in [(Some(16.0), 0.0), (None, 0.0)] {
            let id = c.default_model(ram, vram);
            let m = c.models.iter().find(|m| m.id == id).expect("the default is in the list");
            let lic = m.license.as_deref().unwrap_or("");
            assert!(["apache-2.0", "mit", "bsd-3-clause"].contains(&lic), "{id}: {lic}");
        }
    }

    #[test]
    fn every_entry_has_the_neutral_fields() {
        let c = Catalog::load().unwrap();
        assert!(!c.models.is_empty());
        for m in &c.models {
            assert_eq!(m.object().keys(), ENTRY_KEYS);
        }
    }

    #[test]
    fn the_search_target_is_built_in_one_fixed_order() {
        assert_eq!(
            search_target("granite é/4 & x+y~_.-"),
            "/api/models?search=granite%20%C3%A9%2F4%20%26%20x%2By~_.-&limit=100&sort=downloads&direction=-1\
             &expand%5B%5D=cardData&expand%5B%5D=gguf&expand%5B%5D=pipeline_tag&expand%5B%5D=safetensors&expand%5B%5D=tags"
        );
    }

    #[test]
    fn a_listing_reads_into_the_neutral_fields_and_filters() {
        let c = Catalog::load().unwrap();
        let raw = r#"[{"id":"a/st","cardData":{"license":"apache-2.0"},"safetensors":{"total":3000000000},"pipeline_tag":"text-generation"},
            {"id":"b/gguf","gguf":{"total":1000000000,"context_length":32768},"tags":["gguf","license:mit"]},
            {"id":"c/other","cardData":{"license":"other","license_name":"custom-x"}},
            {"id":"d/float","safetensors":{"total":3e9}},
            {"no":"id"},5,{"id":true}]"#;
        let rows = c.parse_listing(raw).unwrap();
        let ids = |rs: &[Row]| {
            rs.iter()
                .map(|r| r.id.clone())
                .collect::<Vec<_>>()
                .join(",")
        };
        assert_eq!(ids(&rows), "a/st,b/gguf,c/other,d/float");
        assert_eq!(rows[1].license.as_deref(), Some("mit"));
        assert_eq!(rows[1].gguf.as_deref(), Some("b/gguf"));
        assert_eq!(rows[1].suits.as_deref(), Some("weak"));
        assert_eq!(rows[2].license.as_deref(), Some("custom-x"));
        assert!(rows[3].params.is_none());
        assert_eq!(ids(&filter_rows(&rows, 8.0, &[], 20)), "a/st,b/gguf");
        assert_eq!(
            ids(&filter_rows(
                &rows,
                0.0,
                &["mit".into(), "custom-x".into()],
                20
            )),
            "b/gguf,c/other"
        );
        assert_eq!(ids(&filter_rows(&rows, 0.0, &[], 1)), "a/st");
        for bad in ["{}", "not json", "\"x\"", ""] {
            assert!(c.parse_listing(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn the_table_pads_every_column_but_the_last() {
        let c = Catalog::load().unwrap();
        let lines = table_lines(&c.models[..2]);
        assert_eq!(
            lines[1],
            "  ibm-granite/granite-4.1-3b  3.4B  apache-2.0  131072   yes   capable  Tool calling and following instructions."
        );
        assert!(lines.iter().all(|l| !l.ends_with(' ')));
    }
}
