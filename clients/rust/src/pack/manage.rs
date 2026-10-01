//! `opendaisugi.pack.manage`: list, status, remove, install, bundle, run.
//! The lines printed are Python's, byte for byte.

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

use crate::gate::pyjson::{dumps, dumps_indent, loads, Object, Value};
use crate::gate::sha256::hexdigest;

use super::catalog::{parse_lock, wheel_key, Catalog, Pack, Req};
use super::client::run_job;
use super::{ustar, LORA_TRAIN_PY, PROTOCOL, VLA_ORACLE_PY, WORKER_FILE, WORKER_PY};

/// Where the lines go.
pub trait Io {
    fn out(&mut self, line: &str);
    fn err(&mut self, line: &str);
}

/// `manage.Ctx`. `env` is the environment of every child process and of
/// the proxy choice; None is this process's.
pub struct Ctx<'a> {
    pub data_dir: String,
    pub cat: Catalog,
    pub env: Option<Vec<(String, String)>>,
    pub io: &'a mut dyn Io,
    /// Where the system packs are; None is none.
    pub system_dir: Option<String>,
}

/// `manage.SYSTEM_PACKS`; `SYSTEM_ENV` names another.
pub const SYSTEM_PACKS: &str = "/usr/lib/opendaisugi/packs";
pub const SYSTEM_ENV: &str = "OPENDAISUGI_SYSTEM_PACKS";

const PIP_FLAGS: [&str; 7] = [
    "--isolated",
    "--disable-pip-version-check",
    "--no-input",
    "--no-cache-dir",
    "--quiet",
    "--require-hashes",
    "--only-binary=:all:",
];

/// Stop: lines to print, then exit 1.
type Step<T> = Result<T, Vec<String>>;

fn stop<T>(line: String) -> Step<T> {
    Err(vec![line])
}

fn io_err<E: std::fmt::Display>(e: E) -> Vec<String> {
    vec![e.to_string()]
}

/// `manage.pack_dir`.
pub fn pack_dir(ctx: &Ctx, name: &str) -> PathBuf {
    Path::new(&ctx.data_dir).join("packs").join(name)
}

fn disp(p: &Path) -> String {
    p.to_string_lossy().into_owned()
}

fn sha_file(p: &Path) -> Option<String> {
    std::fs::read(p).ok().map(|b| hexdigest(&b))
}

fn this_platform() -> String {
    let arch = match std::env::consts::ARCH {
        "x86_64" => "x86_64",
        "aarch64" => "aarch64",
        other => other,
    };
    format!("{arch}-{}", std::env::consts::OS)
}

fn lexists(p: &Path) -> bool {
    std::fs::symlink_metadata(p).is_ok()
}

/// `manage.read_manifest`.
pub fn read_manifest(d: &Path) -> Option<Object> {
    let text = std::fs::read_to_string(d.join("manifest.json")).ok()?;
    match loads(&text) {
        Ok(Value::Obj(o)) => Some(o),
        _ => None,
    }
}

/// `manage.find_pack`: (directory, from the system) of the data dir's
/// pack, else the system's.
pub fn find_pack(ctx: &Ctx, name: &str) -> Option<(PathBuf, bool)> {
    let d = pack_dir(ctx, name);
    if read_manifest(&d).is_some() {
        return Some((d, false));
    }
    let sd = Path::new(ctx.system_dir.as_deref()?).join(name);
    read_manifest(&sd).is_some().then_some((sd, true))
}

fn lookup<'c>(ctx: &mut Ctx<'c>, name: &str, build: bool) -> Option<Pack> {
    let Some(p) = ctx.cat.find(name).cloned() else {
        ctx.io.err(&format!("No pack named {name}."));
        ctx.io.err("See: daisugi pack list");
        return None;
    };
    if build && p.gpu {
        ctx.io.err(&format!(
            "The pack {name} needs a GPU build, which this release does not carry."
        ));
        return None;
    }
    if build && this_platform() != ctx.cat.platform {
        let plat = ctx.cat.platform.clone();
        ctx.io
            .err(&format!("The packs of this release are for {plat} only."));
        return None;
    }
    Some(p)
}

fn unknown_code(ctx: &Ctx, name: &str) -> i32 {
    if ctx.cat.find(name).is_none() {
        2
    } else {
        1
    }
}

fn obj_str(o: Option<&Object>, k: &str) -> String {
    o.and_then(|o| o.get(k))
        .and_then(|v| v.as_str())
        .unwrap_or_default()
        .to_string()
}

fn obj_obj<'o>(o: Option<&'o Object>, k: &str) -> Option<&'o Object> {
    o.and_then(|o| o.get(k)).and_then(|v| v.as_obj())
}

/// Python's str() of a JSON value in an f-string.
fn py_str(v: Option<&Value>) -> String {
    match v {
        None | Some(Value::Null) => "None".into(),
        Some(Value::Str(s)) => s.clone(),
        Some(other) => dumps(other, true),
    }
}

// ---------------------------------------------------------------------------
// list, status, remove
// ---------------------------------------------------------------------------

/// `manage.list_packs`.
pub fn list(ctx: &mut Ctx) -> i32 {
    for p in ctx.cat.packs.clone() {
        let state = if p.gpu {
            "needs a GPU"
        } else {
            match find_pack(ctx, &p.name) {
                Some((_, true)) => "system",
                Some(_) => "installed",
                None => "not installed",
            }
        };
        ctx.io
            .out(&format!("{:<14}{:<15}{}", p.name, state, p.summary));
    }
    ctx.io.out("");
    ctx.io.out("Install one: daisugi pack install NAME");
    0
}

fn problems(ctx: &Ctx, p: &Pack, d: &Path, m: &Object, system: bool) -> Vec<String> {
    let mut found = vec![];
    if std::fs::metadata(d.join("venv/bin/python")).is_err() {
        found.push("the virtual environment has no python".to_string());
    }
    if let Some(w) = obj_obj(Some(m), "worker") {
        for (fname, want) in w.iter() {
            let f = d.join("worker").join(fname);
            let ok = f.is_file() && sha_file(&f).as_deref() == want.as_str();
            if !ok {
                found.push(format!("the worker file {fname} was changed"));
            }
        }
    }
    if m.get("protocol").and_then(|v| v.as_str()) != Some(PROTOCOL) {
        found.push(format!(
            "the worker speaks {}, not {PROTOCOL}",
            py_str(m.get("protocol"))
        ));
    }
    if system {
        return found;
    }
    let lock = ctx.cat.lock_text(p).unwrap_or_default();
    if obj_str(Some(m), "lock_sha256") != hexdigest(lock.as_bytes()) {
        found.push("this binary pins another lock".to_string());
    }
    if obj_str(obj_obj(Some(m), "python"), "sha256") != ctx.cat.python.sha256 {
        found.push("this binary pins another Python".to_string());
    }
    found
}

/// `manage.status`; None is every installed pack.
pub fn status(ctx: &mut Ctx, name: Option<&str>) -> i32 {
    let names: Vec<String> = match name {
        Some(n) => {
            if lookup(ctx, n, false).is_none() {
                return 2;
            }
            vec![n.to_string()]
        }
        None => {
            let names: Vec<String> = ctx
                .cat
                .packs
                .iter()
                .filter(|p| !p.gpu && find_pack(ctx, &p.name).is_some())
                .map(|p| p.name.clone())
                .collect();
            if names.is_empty() {
                ctx.io.out("No pack is installed. See: daisugi pack list");
                return 0;
            }
            names
        }
    };
    let mut bad = false;
    for n in names {
        let p = ctx.cat.find(&n).cloned().unwrap_or_default();
        let found = if p.gpu { None } else { find_pack(ctx, &n) };
        let Some((d, system)) = found else {
            ctx.io.out(&format!("{n}: not installed"));
            bad = true;
            continue;
        };
        let m = read_manifest(&d).unwrap_or_default();
        if system {
            ctx.io
                .out(&format!("{n}: provided by the system in {}", disp(&d)));
            let found = problems(ctx, &p, &d, &m, true);
            for pr in &found {
                ctx.io.out(&format!("  problem: {pr}"));
            }
            if found.is_empty() {
                ctx.io.out("  ok");
            } else {
                ctx.io
                    .out("  fix: reinstall the system package that provides it");
                bad = true;
            }
            continue;
        }
        let py = obj_obj(Some(&m), "python");
        let pkgs = match m.get("packages") {
            Some(Value::List(l)) => l.len(),
            _ => 0,
        };
        let sha: String = obj_str(Some(&m), "lock_sha256").chars().take(12).collect();
        ctx.io.out(&format!("{n}: installed in {}", disp(&d)));
        ctx.io.out(&format!(
            "  python {} ({})",
            obj_str(py, "version"),
            obj_str(py, "file")
        ));
        ctx.io.out(&format!(
            "  {pkgs} packages from {} (sha256 {sha})",
            obj_str(Some(&m), "lock")
        ));
        let found = problems(ctx, &p, &d, &m, false);
        for pr in &found {
            ctx.io.out(&format!("  problem: {pr}"));
        }
        if found.is_empty() {
            ctx.io.out("  ok");
        } else {
            ctx.io
                .out(&format!("  fix: daisugi pack install {n} --force"));
            bad = true;
        }
    }
    i32::from(bad)
}

/// `manage.remove`.
pub fn remove(ctx: &mut Ctx, name: &str) -> i32 {
    if ctx.cat.find(name).is_none() {
        lookup(ctx, name, false);
        return 2;
    }
    let d = pack_dir(ctx, name);
    if !lexists(&d) {
        ctx.io.out(&format!(
            "Nothing to remove: the pack {name} is not installed."
        ));
        return 0;
    }
    if std::fs::remove_dir_all(&d).is_err() {
        ctx.io.err(&format!("Could not remove {}.", disp(&d)));
        return 1;
    }
    ctx.io
        .out(&format!("Removed the pack {name} ({}).", disp(&d)));
    0
}

// ---------------------------------------------------------------------------
// The steps install and bundle share
// ---------------------------------------------------------------------------

fn env_vars(ctx: &Ctx) -> Vec<(String, String)> {
    match &ctx.env {
        Some(v) => v.clone(),
        None => std::env::vars().collect(),
    }
}

fn fetch(ctx: &Ctx, url: &str, size: u64) -> Step<Vec<u8>> {
    let proxies = crate::netproxy::Urllib::from_vars(&env_vars(ctx), true);
    match crate::pathways::potion::fetch::get_reply(url, size, &proxies) {
        Ok((_, status, body)) if (200..300).contains(&status) => Ok(body),
        Ok((_, status, _)) => stop(format!("Could not fetch {url}: HTTP {status}.")),
        Err(e) if e.contains("larger than the pinned") => stop(format!(
            "Could not fetch {url}: larger than its pinned size of {size} bytes."
        )),
        Err(_) => stop(format!("Could not fetch {url}: no answer.")),
    }
}

fn check_hash(label: &str, got: &str, want: &str) -> Step<()> {
    if got != want {
        return Err(vec![
            format!("Hash mismatch: {label} has sha256 {got}, the pin is {want}."),
            "Nothing was installed.".into(),
        ]);
    }
    Ok(())
}

fn python_tarball(ctx: &mut Ctx, src: Option<&Path>, label: &str) -> Step<Vec<u8>> {
    let py = ctx.cat.python.clone();
    let data = match src {
        Some(s) => {
            let f = s.join(&py.file);
            if !f.is_file() {
                return stop(format!("The bundle {label} has no {}.", py.file));
            }
            std::fs::read(&f).map_err(io_err)?
        }
        None => {
            ctx.io.out(&format!("Fetching {} ...", py.file));
            fetch(ctx, &py.url, py.size)?
        }
    };
    check_hash(&py.file, &hexdigest(&data), &py.sha256)?;
    ctx.io.out(&format!(
        "Checked {} (sha256 {}).",
        py.file,
        &py.sha256[..12]
    ));
    Ok(data)
}

fn unpack_python(ctx: &mut Ctx, data: &[u8], dest: &Path) -> Step<()> {
    let py = ctx.cat.python.clone();
    ustar::unpack_tar_gz(data, dest, &py.file).map_err(|e| vec![e])?;
    if !dest.join("python/bin/python3").is_file() {
        return stop(format!("{} has no python/bin/python3.", py.file));
    }
    ctx.io.out(&format!("Unpacked Python {}.", py.version));
    Ok(())
}

fn check_wheels(reqs: &[Req], wheels: &Path) -> Step<()> {
    let mut files: Vec<String> = match std::fs::read_dir(wheels) {
        Ok(rd) => rd
            .filter_map(|e| e.ok())
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .collect(),
        Err(_) => vec![],
    };
    files.sort();
    for r in reqs {
        let found: Vec<&String> = files
            .iter()
            .filter(|f| wheel_key(f).as_ref() == Some(&(r.name.clone(), r.version.clone())))
            .collect();
        if found.is_empty() {
            return stop(format!(
                "The bundle has no wheel for {}=={}.",
                r.name, r.version
            ));
        }
        for f in found {
            let got = sha_file(&wheels.join(f)).unwrap_or_default();
            if !r.hashes.contains(&got) {
                return Err(vec![
                    format!(
                        "Hash mismatch: wheels/{f} has sha256 {got}, which the lock does not pin."
                    ),
                    "Nothing was installed.".into(),
                ]);
            }
        }
    }
    Ok(())
}

fn index_flags(p: &Pack) -> Vec<String> {
    let mut flags = vec!["--index-url".to_string(), p.index_url.clone()];
    for e in &p.extra_index_urls {
        flags.push("--extra-index-url".into());
        flags.push(e.clone());
    }
    flags
}

fn run_quiet(ctx: &Ctx, argv: &[String], what: &str) -> Step<()> {
    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..]).stdin(Stdio::null());
    if let Some(vars) = &ctx.env {
        cmd.env_clear().envs(vars.iter().map(|(k, v)| (k, v)));
    }
    let out = match cmd.output() {
        Ok(o) => o,
        Err(_) => return stop(format!("{what}: cannot start {}.", argv[0])),
    };
    if out.status.success() {
        return Ok(());
    }
    let mut all = out.stderr.clone();
    all.extend(&out.stdout);
    let text = String::from_utf8_lossy(&all);
    let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
    let tail = &lines[lines.len().saturating_sub(5)..];
    let rc = out.status.code().unwrap_or(-1);
    let mut msg = vec![format!("{what} (exit {rc}):")];
    msg.extend(tail.iter().map(|l| format!("  {l}")));
    Err(msg)
}

fn open_bundle(offline: &str, scratch: &Path) -> Step<PathBuf> {
    let src = Path::new(offline);
    if src.is_dir() {
        return Ok(src.to_path_buf());
    }
    if src.is_file() {
        ustar::extract(src, scratch)
            .map_err(|e| vec![format!("The bundle {offline} is not readable: {e}.")])?;
        return Ok(scratch.to_path_buf());
    }
    stop(format!(
        "The bundle {offline} is not a directory or a .tar file."
    ))
}

fn print_stop(ctx: &mut Ctx, lines: Vec<String>) {
    for ln in lines {
        ctx.io.err(&ln);
    }
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

/// `manage.install`; offline None fetches from the network.
pub fn install(ctx: &mut Ctx, name: &str, offline: Option<&str>, force: bool) -> i32 {
    let Some(p) = lookup(ctx, name, true) else {
        return unknown_code(ctx, name);
    };
    let lock = match ctx.cat.lock_text(&p) {
        Ok(t) => t,
        Err(e) => {
            ctx.io
                .err(&format!("The lock {} is not usable: {e}.", p.lock));
            return 1;
        }
    };
    let lock_sha = hexdigest(lock.as_bytes());
    let d = pack_dir(ctx, name);
    if let Some(m) = read_manifest(&d) {
        if !force {
            if obj_str(Some(&m), "lock_sha256") == lock_sha
                && problems(ctx, &p, &d, &m, false).is_empty()
            {
                ctx.io.out(&format!(
                    "The pack {name} is already installed in {}.",
                    disp(&d)
                ));
                return 0;
            }
            ctx.io.out(&format!(
                "The pack {name} is installed from another pin; installing it again."
            ));
        }
    }
    let reqs = match parse_lock(&lock) {
        Ok(r) => r,
        Err(e) => {
            ctx.io
                .err(&format!("The lock {} is not usable: {e}.", p.lock));
            return 1;
        }
    };
    if lexists(&d) {
        let _ = std::fs::remove_dir_all(&d);
    }
    if let Err(e) = std::fs::create_dir_all(&d) {
        ctx.io.err(&e.to_string());
        return 1;
    }
    if let Err(lines) = install_steps(ctx, &p, &d, &lock, &lock_sha, &reqs, offline) {
        print_stop(ctx, lines);
        let _ = std::fs::remove_dir_all(&d);
        return 1;
    }
    ctx.io.out(&format!(
        "Installed the pack {name}. Run a job: daisugi pack run {name} JOB"
    ));
    0
}

fn install_steps(
    ctx: &mut Ctx,
    p: &Pack,
    d: &Path,
    lock: &str,
    lock_sha: &str,
    reqs: &[Req],
    offline: Option<&str>,
) -> Step<()> {
    let name = p.name.as_str();
    let py = ctx.cat.python.clone();
    ctx.io
        .out(&format!("Installing the pack {name} in {}.", disp(d)));
    let bundle_dir = d.join("bundle");
    let src = match offline {
        Some(o) => Some(open_bundle(o, &bundle_dir)?),
        None => None,
    };
    let data = python_tarball(ctx, src.as_deref(), offline.unwrap_or(""))?;
    unpack_python(ctx, &data, d)?;
    drop(data);
    let s = |p: PathBuf| p.to_string_lossy().into_owned();
    run_quiet(
        ctx,
        &[
            s(d.join("python/bin/python3")),
            "-m".into(),
            "venv".into(),
            s(d.join("venv")),
        ],
        "Could not make the virtual environment",
    )?;
    ctx.io.out("Made the virtual environment.");
    std::fs::write(d.join("lock.txt"), lock).map_err(io_err)?;
    let mut pip: Vec<String> = vec![
        s(d.join("venv/bin/python")),
        "-m".into(),
        "pip".into(),
        "install".into(),
    ];
    pip.extend(PIP_FLAGS.iter().map(|f| f.to_string()));
    pip.extend(["-r".to_string(), s(d.join("lock.txt"))]);
    match &src {
        Some(srcd) => {
            check_wheels(reqs, &srcd.join("wheels"))?;
            pip.extend([
                "--no-index".to_string(),
                "--find-links".into(),
                s(srcd.join("wheels")),
            ]);
        }
        None => pip.extend(index_flags(p)),
    }
    ctx.io.out(&format!(
        "Installing {} packages from {} ...",
        reqs.len(),
        p.lock
    ));
    run_quiet(ctx, &pip, "pip could not install the lock")?;
    ctx.io.out(&format!("Installed {} packages.", reqs.len()));
    if src.as_deref() == Some(bundle_dir.as_path()) {
        let _ = std::fs::remove_dir_all(&bundle_dir);
    }
    let w = d.join("worker");
    std::fs::create_dir(&w).map_err(io_err)?;
    let mut shas = Object::new();
    for (fname, body) in [
        (WORKER_FILE, WORKER_PY),
        ("lora_train.py", LORA_TRAIN_PY),
        ("vla_oracle.py", VLA_ORACLE_PY),
    ] {
        std::fs::write(w.join(fname), body).map_err(io_err)?;
        shas.set(fname, hexdigest(body.as_bytes()).as_str());
    }
    let check = Value::List(p.check.iter().map(|c| Value::Str(c.clone())).collect());
    let info = format!(
        "{}\n",
        dumps_indent(
            &Value::Obj(Object::new().with("name", name).with("check", check)),
            2,
            true
        )
    );
    std::fs::write(w.join("pack.json"), &info).map_err(io_err)?;
    shas.set("pack.json", hexdigest(info.as_bytes()).as_str());
    let env = ctx.env.clone();
    let got = run_job(d, name, "selftest", &[], &mut |_: &str| {}, &env);
    if got.code != 0 {
        return stop(format!(
            "The self-test failed: {}",
            got.error.unwrap_or_default()
        ));
    }
    let mut parts = vec![];
    if let Some(imports) = obj_obj(got.result.as_ref(), "imports") {
        for (k, v) in imports.iter() {
            parts.push(format!("{k} {}", py_str(Some(v))).trim().to_string());
        }
    }
    ctx.io.out(&format!("Self-test: {}.", parts.join(", ")));
    let pkgs = Value::List(
        reqs.iter()
            .map(|r| Value::Str(format!("{}=={}", r.name, r.version)))
            .collect(),
    );
    let manifest = Object::new()
        .with("pack", name)
        .with("protocol", PROTOCOL)
        .with(
            "python",
            Value::Obj(
                Object::new()
                    .with("version", py.version.as_str())
                    .with("file", py.file.as_str())
                    .with("sha256", py.sha256.as_str()),
            ),
        )
        .with("lock", p.lock.as_str())
        .with("lock_sha256", lock_sha)
        .with("packages", pkgs)
        .with("source", if src.is_some() { "bundle" } else { "index" })
        .with("worker", Value::Obj(shas));
    std::fs::write(
        d.join("manifest.json"),
        format!("{}\n", dumps_indent(&Value::Obj(manifest), 2, true)),
    )
    .map_err(io_err)
}

// ---------------------------------------------------------------------------
// bundle
// ---------------------------------------------------------------------------

/// `manage.bundle`.
pub fn bundle(ctx: &mut Ctx, name: &str, out: &str) -> i32 {
    let Some(p) = lookup(ctx, name, true) else {
        return unknown_code(ctx, name);
    };
    let reqs = match ctx.cat.lock_text(&p).and_then(|t| parse_lock(&t)) {
        Ok(r) => r,
        Err(e) => {
            ctx.io
                .err(&format!("The lock {} is not usable: {e}.", p.lock));
            return 1;
        }
    };
    let target = Path::new(out);
    let base = target
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    let as_tar = base.ends_with(".tar");
    let work = if as_tar {
        target.with_file_name(format!("{base}.partial"))
    } else {
        target.to_path_buf()
    };
    if lexists(target) || (as_tar && lexists(&work)) {
        ctx.io
            .err(&format!("{out} already exists. Give a new path."));
        return 1;
    }
    if let Err(e) = std::fs::create_dir_all(&work) {
        ctx.io.err(&e.to_string());
        return 1;
    }
    let mut res = bundle_steps(ctx, &p, &reqs, &work);
    if res.is_ok() && as_tar {
        let py_file = ctx.cat.python.file.clone();
        let mut files = vec![(py_file.clone(), work.join(&py_file))];
        if let Ok(rd) = std::fs::read_dir(work.join("wheels")) {
            for e in rd.filter_map(|e| e.ok()) {
                let n = e.file_name().to_string_lossy().into_owned();
                files.push((format!("wheels/{n}"), e.path()));
            }
        }
        match ustar::write_file(target, &files) {
            Ok(()) => {
                let _ = std::fs::remove_dir_all(&work);
            }
            Err(e) => res = stop(format!("Could not write {out}: {e}.")),
        }
    }
    if let Err(lines) = res {
        print_stop(ctx, lines);
        let _ = std::fs::remove_dir_all(&work);
        if as_tar && lexists(target) {
            let _ = std::fs::remove_file(target);
        }
        return 1;
    }
    let ver = ctx.cat.python.version.clone();
    ctx.io.out(&format!(
        "Bundled the pack {name} in {out}: Python {ver} and {} wheels.",
        reqs.len()
    ));
    ctx.io.out(&format!(
        "Install it with no network: daisugi pack install {name} --offline {out}"
    ));
    0
}

fn bundle_steps(ctx: &mut Ctx, p: &Pack, reqs: &[Req], work: &Path) -> Step<()> {
    let py = ctx.cat.python.clone();
    let data = python_tarball(ctx, None, "")?;
    std::fs::write(work.join(&py.file), &data).map_err(io_err)?;
    let tmp = work.join(".python");
    std::fs::create_dir(&tmp).map_err(io_err)?;
    unpack_python(ctx, &data, &tmp)?;
    drop(data);
    let lock = ctx.cat.lock_text(p).map_err(|e| vec![e])?;
    std::fs::write(tmp.join("lock.txt"), lock).map_err(io_err)?;
    let s = |p: PathBuf| p.to_string_lossy().into_owned();
    let mut pip: Vec<String> = vec![
        s(tmp.join("python/bin/python3")),
        "-m".into(),
        "pip".into(),
        "download".into(),
    ];
    pip.extend(PIP_FLAGS.iter().map(|f| f.to_string()));
    pip.extend([
        "--no-deps".to_string(),
        "-d".into(),
        s(work.join("wheels")),
        "-r".into(),
        s(tmp.join("lock.txt")),
    ]);
    pip.extend(index_flags(p));
    ctx.io.out(&format!(
        "Fetching {} wheels from {} ...",
        reqs.len(),
        p.lock
    ));
    run_quiet(ctx, &pip, "pip could not fetch the lock's wheels")?;
    let _ = std::fs::remove_dir_all(&tmp);
    check_wheels(reqs, &work.join("wheels"))?;
    ctx.io.out(&format!(
        "Checked {} wheels against {}.",
        reqs.len(),
        p.lock
    ));
    Ok(())
}

// ---------------------------------------------------------------------------
// run, and the trainer through the train pack
// ---------------------------------------------------------------------------

fn installed_or_say(ctx: &mut Ctx, name: &str) -> Option<PathBuf> {
    let Some((d, _)) = find_pack(ctx, name) else {
        ctx.io.err(&format!("The pack {name} is not installed."));
        ctx.io
            .err(&format!("Install it: daisugi pack install {name}"));
        return None;
    };
    Some(d)
}

/// `manage.run`.
pub fn run(ctx: &mut Ctx, name: &str, job: &str, args: &[String]) -> i32 {
    if lookup(ctx, name, false).is_none() {
        return 2;
    }
    let Some(d) = installed_or_say(ctx, name) else {
        return 1;
    };
    let env = ctx.env.clone();
    let io = &mut *ctx.io;
    let got = run_job(&d, name, job, args, &mut |s: &str| io.err(s), &env);
    if got.code != 0 {
        ctx.io.err(&got.error.unwrap_or_default());
        return got.code;
    }
    let res = Value::Obj(got.result.unwrap_or_default());
    ctx.io.out(&dumps_indent(&res, 2, true));
    0
}

/// `manage.TRAIN_PACK`.
pub const TRAIN_PACK: &str = "train";

/// `manage.lora_train`.
pub fn lora_train(ctx: &mut Ctx, args: &[String]) -> i32 {
    let Some(d) = installed_or_say(ctx, TRAIN_PACK) else {
        return 1;
    };
    let env = ctx.env.clone();
    let io = &mut *ctx.io;
    let got = run_job(
        &d,
        TRAIN_PACK,
        "train",
        args,
        &mut |s: &str| io.err(s),
        &env,
    );
    if got.code != 0 {
        ctx.io.err(&got.error.unwrap_or_default());
        return got.code;
    }
    let adapter = py_str(got.result.as_ref().and_then(|r| r.get("adapter")));
    ctx.io.out(&format!("The adapter is in {adapter}."));
    0
}
