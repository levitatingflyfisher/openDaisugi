//! `daisugi tiers setup` (the hardware probe, the model sizing and the
//! Tier-1 qualification) and the `daisugi setup` stub that names it. The
//! Go client's `cli/setupcmd.go` is the reference; ruling K3-9 says where
//! it differs from Python.

use super::config;
use super::gateroot::{join, path_str};
use super::statuscmd::{py_fixed, py_round1, ram_gb};
use super::{exit, parse_args, Env, Opt, Res};
use crate::envgen::tier1::{Tier1, CONFIG_FILE};
use crate::gate::py::text::repr;
use crate::gate::pyjson::{dumps_indent, float_repr, round, Object, Value};

/// `cli._MATCHERS`: the matcher_model values `tiers setup` sets.
const MATCHERS: [&str; 4] = ["lexical", "potion", "all-MiniLM-L6-v2", "int8"];

const TIERS_HELP: &str = "Usage: daisugi tiers [OPTIONS] COMMAND [ARGS]...

  Tier-0/1/2 routing stats derived from the journal.

Options:
  --help  Show this message and exit.

Commands:
  setup  Detect hardware, recommend a local model, and optionally qualify and wire it.
  stats  Not yet in this binary.
";

/// `hardware.HardwareProfile` on Linux.
struct Hardware {
    system: String,
    arch: String,
    cpus: usize,
    ram: Option<f64>,
    vram: f64,
    gpu: Option<String>,
}

impl Hardware {
    fn discrete(&self) -> bool {
        self.vram > 0.0
    }

    fn budget(&self) -> f64 {
        if self.discrete() {
            return py_round1(self.vram * 0.8);
        }
        match self.ram {
            Some(r) if r != 0.0 => py_round1(r * 0.6),
            _ => 0.0,
        }
    }
}

/// `os.cpu_count()`: the CPUs online.
fn cpu_count() -> usize {
    let Ok(raw) = std::fs::read_to_string("/sys/devices/system/cpu/online") else {
        return 1;
    };
    let mut n: i64 = 0;
    for part in raw.trim().split(',') {
        let (lo, hi) = match part.split_once('-') {
            Some((a, b)) => (a, Some(b)),
            None => (part, None),
        };
        let Ok(a) = lo.parse::<i64>() else { return 1 };
        let b = match hi {
            Some(h) => match h.parse::<i64>() {
                Ok(b) => b,
                Err(_) => return 1,
            },
            None => a,
        };
        n += b - a + 1;
    }
    if n < 1 {
        1
    } else {
        n as usize
    }
}

fn uname() -> (String, String) {
    // SAFETY: uname() fills the struct it is given and nothing else.
    let mut u: libc::utsname = unsafe { std::mem::zeroed() };
    if unsafe { libc::uname(&mut u) } != 0 {
        return ("Linux".into(), String::new());
    }
    let s = |f: &[libc::c_char]| {
        let b: Vec<u8> = f
            .iter()
            .take_while(|c| **c != 0)
            .map(|c| *c as u8)
            .collect();
        String::from_utf8_lossy(&b).into_owned()
    };
    (s(&u.sysname), s(&u.machine))
}

/// `recommend_model`.
struct Recommendation {
    size: &'static str,
    params: i64,
    download: f64,
    families: Vec<&'static str>,
    rationale: String,
}

/// `format(f, "g")`.
fn py_g(f: f64) -> String {
    if f == 0.0 {
        return if f.is_sign_negative() {
            "-0".into()
        } else {
            "0".into()
        };
    }
    if !f.is_finite() {
        return py_fixed(f, 0);
    }
    let sci = format!("{f:.5e}");
    let (mant, exp) = sci.split_once('e').unwrap_or((&sci, "0"));
    let exp: i32 = exp.parse().unwrap_or(0);
    let trim = |s: &str| -> String {
        if s.contains('.') {
            s.trim_end_matches('0').trim_end_matches('.').to_string()
        } else {
            s.to_string()
        }
    };
    if (-4..6).contains(&exp) {
        let decimals = (5 - exp).max(0) as usize;
        trim(&format!("{f:.decimals$}"))
    } else {
        let sign = if exp < 0 { '-' } else { '+' };
        format!("{}e{sign}{:02}", trim(mant), exp.abs())
    }
}

fn recommend(h: &Hardware) -> Recommendation {
    let b = h.budget();
    let tiers: [(f64, &str, i64, f64); 5] = [
        (3.0, "≤1B", 1, 0.8),
        (6.0, "~3B", 3, 2.2),
        (12.0, "~8B", 8, 5.0),
        (24.0, "~14B", 14, 9.0),
        (f64::INFINITY, "~32B", 32, 20.0),
    ];
    let (mut size, mut params, mut download) = ("", 0, 0.0);
    for (below, label, p, d) in tiers {
        if b < below {
            (size, params, download) = (label, p, d);
            break;
        }
    }
    let where_ = if h.discrete() {
        format!(
            "{}GB VRAM ({})",
            py_g(h.vram),
            h.gpu.clone().unwrap_or_else(|| "None".into())
        )
    } else {
        match h.ram {
            Some(r) if r != 0.0 => format!(
                "{}GB RAM (CPU inference — expect slower generation; favor the smaller end and a low context size)",
                py_g(r)
            ),
            _ => "undetected memory (treating conservatively)".into(),
        }
    };
    let rationale = format!(
        "budget ~{}GB from {where_}. Recommending a {size}-class instruct model at Q4_K_M. This is provisional — \
         qualify it on YOUR box (run the candidate against the real envelope schema and check the pass rate) before \
         trusting it as Tier-1; the model family is your pick, not a verified default.",
        py_fixed(b, 0)
    );
    let families = if params >= 3 {
        vec!["Qwen2.5", "Gemma", "Llama", "Phi"]
    } else {
        vec!["Qwen2.5", "Gemma"]
    };
    Recommendation {
        size,
        params,
        download,
        families,
        rationale,
    }
}

/// `local_setup.DEFAULT_PROBE_TASKS`.
const PROBE_TASKS: [&str; 3] = [
    "Delete .tmp files older than 7 days in /var/log",
    "Read /data/sales.csv and print the row count",
    "List the running processes and save them to processes.txt",
];

/// `format(f, ".0%")`.
fn py_percent0(f: f64) -> String {
    format!("{}%", py_fixed(f * 100.0, 0))
}

struct Qualification {
    attempts: i64,
    valid: i64,
    rate: f64,
    passed: bool,
}

impl Env {
    fn detect_hardware(&self) -> Hardware {
        let (system, arch) = uname();
        let (mut vram, gpu) = self.gpu_probe();
        if vram.is_nan() {
            vram = 0.0;
        }
        Hardware {
            system,
            arch,
            cpus: cpu_count(),
            ram: ram_gb(),
            vram,
            gpu,
        }
    }

    pub(super) fn tiers(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(TIERS_HELP);
            return Ok(());
        }
        match args[0].as_str() {
            "setup" => return self.tiers_setup(&args[1..]),
            "stats" => return self.not_yet("daisugi tiers stats"),
            _ => {}
        }
        self.errf(&format!(
            "Usage: daisugi tiers [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi tiers --help' for help.\n\nError: No such command '{}'.\n",
            args[0]
        ));
        exit(2)
    }

    /// `daisugi setup`: a stub that names its replacement.
    pub(super) fn setup_moved(&mut self, args: &[String]) -> Res {
        if args.first().is_some_and(|a| a == "--help") {
            self.out(
                "Usage: daisugi setup [OPTIONS]\n\n  Moved: `daisugi setup` is now `daisugi tiers setup` (same flags).\n\n\
                 Options:\n  --help  Show this message and exit.\n",
            );
            return Ok(());
        }
        if let Some(a) = args.first() {
            if a.starts_with('-') {
                return self.usage("setup", format!("No such option: {a}"));
            }
            return self.usage("setup", format!("Got unexpected extra argument ({a})"));
        }
        Err(self.fail3(
            "`daisugi setup` moved.",
            "hardware detection and local-model qualification now live under `tiers`.",
            "run: daisugi tiers setup",
            2,
        ))
    }

    /// `cli._set_matcher`: `tiers setup --matcher` records the pathway
    /// matcher in config.yaml.
    fn set_matcher(&mut self, cmd: &str, path: &str, matcher: &str) -> Res {
        if !MATCHERS.contains(&matcher) {
            return Err(self.fail3(
                &format!("{} is not a pathway matcher.", repr(matcher)),
                &format!("the built matchers are {}.", MATCHERS.join(", ")),
                "run: daisugi tiers setup --matcher potion",
                2,
            ));
        }
        let was = match config::load(path) {
            Ok(c) => c.matcher_model,
            Err(e) => return self.refuse(cmd, &format!("{path} is not one this binary rewrites: {e}")),
        };
        let update = vec![("matcher_model".to_string(), Value::Str(matcher.to_string()))];
        match config::save(path, &self.home, &update) {
            Ok(()) => {}
            Err(config::SaveErr::Io(e)) => return self.fail(cmd, &e.to_string()),
            Err(config::SaveErr::Config(e)) => {
                return self.refuse(cmd, &format!("{path} is not one this binary rewrites: {e}"))
            }
        }
        self.out(&format!("Pathway matcher set to {matcher} (was {was}) in {path}.\n"));
        if matcher != was {
            self.out("A new matcher embeds text differently. Run `daisugi tend` to re-embed pathways.\n");
        }
        Ok(())
    }

    fn tiers_setup(&mut self, args: &[String]) -> Res {
        const CMD: &str = "tiers setup";
        let opts = [
            Opt::val(&["--data-dir"], "PATH", "Daisugi data directory."),
            Opt::val(
                &["--endpoint"],
                "TEXT",
                "OpenAI-compatible local /v1 URL to qualify (e.g. http://localhost:8080/v1).",
            ),
            Opt::val(
                &["--remote"],
                "TEXT",
                "host[:port] of a self-hosted model server to probe and record.",
            ),
            Opt::val(
                &["--kind"],
                "TEXT",
                "auto | ollama | openai | anthropic. The wire --remote speaks.",
            ),
            Opt::val(
                &["--context"],
                "INTEGER",
                "Override the probed context window in tokens.",
            ),
            Opt::val(
                &["--model"],
                "TEXT",
                "Model name served by --endpoint or --remote.",
            ),
            Opt::val(
                &["--threshold"],
                "FLOAT",
                "Min valid-envelope pass rate to promote.",
            ),
            Opt::val(&["--repeats"], "INTEGER", "Sample each probe task N times."),
            Opt::flag(&["--wire"], "Persist the model as Tier-1 if it qualifies."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
            Opt::val(
                &["--matcher"],
                "TEXT",
                "Set the pathway matcher and stop: lexical (the default, no model), potion (a one-time download \
                 of about 30 MB), all-MiniLM-L6-v2 (needs torch) or int8.",
            ),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Detect hardware, recommend a local model, and optionally qualify and wire it.",
                &opts,
            );
        }
        let threshold = self.click_float(CMD, &p, "--threshold", 0.8)?;
        let repeats = self.click_int(CMD, &p, "--repeats", 1)?;
        if p.has("--context") {
            self.click_int(CMD, &p, "--context", 0)?;
        }
        let data_dir = path_str(&p.str("--data-dir", &join(&self.home, ".opendaisugi")));
        if p.has("--matcher") {
            return self.set_matcher(CMD, &join(&data_dir, "config.yaml"), &p.str("--matcher", ""));
        }
        let (endpoint, remote) = (p.str("--endpoint", ""), p.str("--remote", ""));
        if !endpoint.is_empty() && !remote.is_empty() {
            return Err(self.fail3(
                "--endpoint and --remote are mutually exclusive.",
                "--endpoint qualifies a local /v1 server against the envelope-generation gate. --remote probes \
                 and records a model host for your harness to point at.",
                "run one at a time: --endpoint URL --model NAME, or --remote HOST[:PORT].",
                1,
            ));
        }
        if !remote.is_empty() {
            return self.not_yet("daisugi tiers setup --remote");
        }
        let model = p.str("--model", "");
        if !endpoint.is_empty() && model.is_empty() {
            self.errf(
                "--model is required with --endpoint (the model name the local server serves).\n",
            );
            return exit(2);
        }
        let h = self.detect_hardware();
        let rec = recommend(&h);
        let mut qual: Option<Qualification> = None;
        let mut wired = false;
        if !endpoint.is_empty() {
            let mut c = self.llm_client();
            let t = Tier1::new(&model, Some(endpoint.clone()), None, "");
            if let Err(why) = c.check(&t.model) {
                return self.refuse(CMD, &why);
            }
            let n = repeats.max(1);
            let mut valid = 0i64;
            for _ in 0..n {
                for task in PROBE_TASKS {
                    if t.generate(&mut c, task, None).is_some() {
                        valid += 1;
                    }
                }
            }
            let attempts = PROBE_TASKS.len() as i64 * n as i64;
            let rate = round(valid as f64 / attempts as f64, 2);
            let q = Qualification {
                attempts,
                valid,
                rate,
                passed: attempts > 0 && rate >= threshold,
            };
            if q.passed && p.flag("--wire") {
                if let Err(e) = std::fs::create_dir_all(&data_dir) {
                    return Err(self.pw_err(CMD, crate::pathways::PwErr::Io(e)));
                }
                let body = format!(
                    "{}\n",
                    dumps_indent(
                        &Value::Obj(
                            Object::new()
                                .with("model", model.as_str())
                                .with("base_url", endpoint.as_str())
                        ),
                        2,
                        true
                    )
                );
                if let Err(e) = std::fs::write(format!("{data_dir}/{CONFIG_FILE}"), body) {
                    return Err(self.pw_err(CMD, crate::pathways::PwErr::Io(e)));
                }
                wired = true;
            }
            qual = Some(q);
        }
        let budget = h.budget();
        if p.flag("--json") {
            let q = match &qual {
                Some(q) => Value::Obj(
                    Object::new()
                        .with("attempts", q.attempts)
                        .with("valid", q.valid)
                        .with("pass_rate", q.rate)
                        .with("passed", q.passed)
                        .with("threshold", threshold)
                        .with("wired", wired),
                ),
                None => Value::Null,
            };
            let fams: Vec<Value> = rec
                .families
                .iter()
                .map(|f| Value::Str(f.to_string()))
                .collect();
            let payload = Object::new()
                .with(
                    "hardware",
                    Object::new()
                        .with("system", h.system.as_str())
                        .with("arch", h.arch.as_str())
                        .with("cpu_count", h.cpus as i64)
                        .with("ram_gb", h.ram)
                        .with("vram_gb", h.vram)
                        .with("gpu_name", h.gpu.clone())
                        .with("unified_memory", false)
                        .with("model_budget_gb", budget),
                )
                .with(
                    "recommendation",
                    Object::new()
                        .with("size_class", rec.size)
                        .with("params_b_max", rec.params)
                        .with("quant", "Q4_K_M")
                        .with("runtime", "llamafile")
                        .with("est_download_gb", rec.download)
                        .with("candidate_families", Value::List(fams))
                        .with("provisional", true)
                        .with("rationale", rec.rationale.as_str()),
                )
                .with("qualification", q);
            self.out(&format!(
                "{}\n",
                dumps_indent(&Value::Obj(payload), 2, true)
            ));
            return Ok(());
        }
        let mut line = format!("Hardware: {}/{}, {} CPU", h.system, h.arch, h.cpus);
        match h.ram {
            Some(r) if r != 0.0 => line.push_str(&format!(", {}GB RAM", float_repr(r))),
            _ => line.push_str(", RAM undetected"),
        }
        if h.discrete() {
            line.push_str(&format!(
                ", {}GB VRAM ({})",
                float_repr(h.vram),
                h.gpu.clone().unwrap_or_else(|| "None".into())
            ));
        } else {
            line.push_str(", no discrete GPU");
        }
        let mut b = format!("{line}\n");
        b.push_str(&format!("Model budget: ~{}GB\n\n", float_repr(budget)));
        b.push_str(&format!(
            "Recommended: a {}-class instruct model at Q4_K_M via llamafile (~{}GB).\n",
            rec.size,
            float_repr(rec.download)
        ));
        b.push_str(&format!(
            "  candidate families (your pick, none verified-best): {}\n",
            rec.families.join(", ")
        ));
        b.push_str(&format!("  {}\n\n", rec.rationale));
        let Some(q) = qual else {
            b.push_str(
                "Get a local server running (one file, no install), then qualify + wire it:\n",
            );
            b.push_str("  1. Find a trusted, commit-pinned model llamafile:  daisugi models\n");
            b.push_str(
                "     (canonical engine repo: github.com/mozilla-ai/llamafile; model org: huggingface.co/mozilla-ai)\n",
            );
            b.push_str("  2. Serve it:  ./<model>.llamafile --server --port 8080 --nobrowser\n");
            b.push_str(
                "  3. Qualify:   daisugi tiers setup --endpoint http://localhost:8080/v1 --model <name> --wire\n",
            );
            b.push_str("\nPathway matcher: lexical by default (no model, no download). For better recall:\n");
            b.push_str("  daisugi tiers setup --matcher potion   (a one-time download of about 30 MB)\n");
            self.out(&b);
            return Ok(());
        };
        let verdict = if q.passed { "PASSED" } else { "FAILED" };
        b.push_str(&format!(
            "Qualification: {verdict} — {}/{} valid envelopes (pass rate {}, threshold {}).\n",
            q.valid,
            q.attempts,
            py_percent0(q.rate),
            py_percent0(threshold)
        ));
        if wired {
            b.push_str(&format!("  → Wired as Tier-1 in {data_dir}; `daisugi onboard`/`tend` will now defer to it.\n"));
        } else if q.passed {
            b.push_str("  → Passed. Re-run with --wire to persist it as Tier-1.\n");
        } else {
            // The provider declines rather than raises, so no attempt is an
            // error and the all-errored hint is never reached (K3-9).
            b.push_str("  → Not promoted. Try a larger model, a higher quant, or lower --threshold deliberately.\n");
        }
        self.out(&b);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hw(ram: Option<f64>, vram: f64) -> Hardware {
        Hardware {
            system: "Linux".into(),
            arch: "x86_64".into(),
            cpus: 4,
            ram,
            vram,
            gpu: None,
        }
    }

    /// The CPU-only recommendation depends on the box's memory, so no
    /// case holds it; these are the oracle's answers for fixed profiles.
    #[test]
    fn cpu_only_recommendations_match_the_oracle() {
        let tail = ". This is provisional — qualify it on YOUR box (run the candidate against the real envelope \
                    schema and check the pass rate) before trusting it as Tier-1; the model family is your pick, not a \
                    verified default.";
        let cpu = " (CPU inference — expect slower generation; favor the smaller end and a low context size)";
        for (ram, budget, size, head) in [
            (
                Some(16.7),
                10.0,
                "~8B",
                format!("budget ~10GB from 16.7GB RAM{cpu}. Recommending a ~8B-class instruct model at Q4_K_M"),
            ),
            (Some(4.0), 2.4, "≤1B", format!("budget ~2GB from 4GB RAM{cpu}. Recommending a ≤1B-class instruct model at Q4_K_M")),
            (
                None,
                0.0,
                "≤1B",
                "budget ~0GB from undetected memory (treating conservatively). Recommending a ≤1B-class instruct model at Q4_K_M"
                    .to_string(),
            ),
        ] {
            let h = hw(ram, 0.0);
            let r = recommend(&h);
            assert_eq!(h.budget(), budget, "{ram:?}");
            assert_eq!(r.size, size, "{ram:?}");
            assert_eq!(r.rationale, format!("{head}{tail}"), "{ram:?}");
        }
    }

    #[test]
    fn g_format_is_pythons() {
        for (f, want) in [
            (15.6, "15.6"),
            (8.0, "8"),
            (1234567.0, "1.23457e+06"),
            (0.0001, "0.0001"),
            (0.00001, "1e-05"),
            (123456.0, "123456"),
        ] {
            assert_eq!(py_g(f), want, "{f}");
        }
    }
}
