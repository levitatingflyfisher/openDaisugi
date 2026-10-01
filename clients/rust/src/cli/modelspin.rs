//! `daisugi models pin REPO`: `model_registry.resolve_pinned` through the
//! Hub API, as huggingface_hub's `list_repo_files` and `model_info` ask
//! it, following the tree's pages with huggingface_hub's backoff, and
//! `--pull` writes the Hugging Face cache as `hf_hub_download` does
//! (RF-7). The Go client's `cli/modelspin.go` and `cli/hubclient.go` are
//! the twin.

use std::sync::OnceLock;

use regex::Regex;

use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text::{repr, upper};
use crate::gate::pyjson::{dumps_indent, loads_strict, Object, Value};
use crate::llm::http;
use crate::netproxy;

/// The Hub's default endpoint, as huggingface_hub's constants name it.
const HUB_DEFAULT_ENDPOINT: &str = "https://huggingface.co";

fn re(cell: &'static OnceLock<Regex>, pat: &str) -> &'static Regex {
    cell.get_or_init(|| Regex::new(pat).unwrap())
}

/// `validate_repo_id`: None when the id passes, else the
/// HFValidationError's text. Python's `\w` is a letter or number of any
/// script (`str.isalnum`) or "_".
fn hub_repo_invalid(repo: &str) -> Option<String> {
    if repo.matches('/').count() > 1 {
        return Some(format!(
            "Repo id must be in the form 'repo_name' or 'namespace/repo_name': '{repo}'. Use `repo_type` argument if needed."
        ));
    }
    let word = |c: char| c == '_' || crate::gate::py::text::is_alnum(c);
    let part = |s: &str, max: usize| {
        let cs: Vec<char> = s.chars().collect();
        !cs.is_empty()
            && (max == 0 || cs.len() <= max)
            && word(cs[0])
            && word(cs[cs.len() - 1])
            && cs.iter().all(|&c| word(c) || c == '-' || c == '.')
    };
    let ok = match repo.split_once('/') {
        Some((ns, name)) => part(ns, 0) && part(name, 96),
        None => part(repo, 96),
    };
    if !ok {
        return Some(format!(
            "Repo id must use alphanumeric chars, '-', '_' or '.'. The name cannot start or end with '-' or '.' and the maximum length is 96: '{repo}'."
        ));
    }
    if repo.contains("--") || repo.contains("..") {
        return Some(format!("Cannot have -- or .. in repo_id: '{repo}'."));
    }
    if repo.ends_with(".git") {
        return Some(format!("Repo_id cannot end by '.git': '{repo}'."));
    }
    None
}

/// `parse_datetime` reading `d` without raising, for the one form the Hub
/// writes.
fn hub_date_ok(d: &Value) -> bool {
    static DATE: OnceLock<Regex> = OnceLock::new();
    let Value::Str(s) = d else { return false };
    let Some(m) = re(&DATE, r"^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?Z$").captures(s)
    else {
        return false;
    };
    let n = |i: usize| m[i].parse::<u32>().unwrap_or(99);
    let (y, mo, d, h, mi, se) = (n(1), n(2), n(3), n(4), n(5), n(6));
    let leap = (y % 4 == 0 && y % 100 != 0) || y % 400 == 0;
    let days = match mo {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 if leap => 29,
        2 => 28,
        _ => return false,
    };
    y >= 1 && d >= 1 && d <= days && h < 24 && mi < 60 && se < 60
}

/// Why `models pin` stops: an exception Python does not catch (its
/// traceback names the class first; exit 1), or a refusal.
enum HubErr {
    Raise(&'static str, String),
    Refuse(String),
    /// A refusal after `--pull` wrote to the cache: the words do not say
    /// that nothing changed.
    Partly(String, String),
}

impl From<String> for HubErr {
    fn from(s: String) -> HubErr {
        HubErr::Refuse(s)
    }
}

impl From<&str> for HubErr {
    fn from(s: &str) -> HubErr {
        HubErr::Refuse(s.to_string())
    }
}

fn raise(class: &'static str, msg: impl Into<String>) -> HubErr {
    HubErr::Raise(class, msg.into())
}

/// A refused connection: httpcore's ConnectError, the first exception of
/// Python's traceback.
fn refused() -> HubErr {
    raise("httpcore.ConnectError", "[Errno 111] Connection refused")
}

/// Any other network failure (a name that does not resolve, TLS, a
/// connection that breaks or times out): huggingface_hub names it in
/// httpx's words, which this binary does not carry, so it refuses.
const UNREACHED: &str = "the Hub could not be reached, a failure huggingface_hub names in httpx's words, which this binary does not carry";

/// One page of `list_repo_tree`, read as `RepoFile(**entry)` or
/// `RepoFolder(**entry)`: iterating the JSON (a dict iterates its keys, a
/// str its characters), `entry["type"]`, then the class's own fields.
fn tree_page(v: &Value) -> Result<Vec<String>, HubErr> {
    let items: Vec<Value> = match v {
        Value::List(l) => l.clone(),
        Value::Obj(o) => o.keys().iter().map(|k| Value::Str(k.clone())).collect(),
        Value::Str(s) => s.chars().map(|c| Value::Str(c.to_string())).collect(),
        _ => return Err(raise("TypeError", "the tree is not iterable")),
    };
    let mut out = vec![];
    for it in &items {
        let Value::Obj(o) = it else { return Err(raise("TypeError", "a tree entry is not subscriptable by 'type'")) };
        let Some(kind) = o.get("type") else { return Err(raise("KeyError", "'type'")) };
        if o.get("self").is_some() {
            return Err(raise("TypeError", "__init__() got multiple values for argument 'self'"));
        }
        let file = matches!(kind, Value::Str(k) if k == "file");
        let need: &[&str] = if file { &["path", "size", "oid"] } else { &["path", "oid"] };
        for k in need {
            if o.get(k).is_none() {
                return Err(raise("KeyError", format!("'{k}'")));
            }
        }
        let lc = if o.value("lastCommit").truthy() { o.value("lastCommit") } else { o.value("last_commit") };
        if lc.truthy() {
            return Err("a tree entry carries its last commit, which this binary does not read".into());
        }
        if !file {
            continue;
        }
        match o.value("lfs") {
            Value::Null => {}
            Value::Obj(l) => {
                for k in ["size", "oid", "pointerSize"] {
                    if l.get(k).is_none() {
                        return Err(raise("KeyError", format!("'{k}'")));
                    }
                }
            }
            _ => return Err("a tree entry's lfs is not an object".into()),
        }
        if !matches!(o.value("securityFileStatus"), Value::Null) {
            return Err("a tree entry carries a security status, which this binary does not read".into());
        }
        let Value::Str(path) = o.value("path") else { return Err("a tree entry's path is not text".into()) };
        out.push(path.clone());
    }
    Ok(out)
}

/// `ModelInfo(**data).sha`, for a body `ModelInfo` is sure to read: a
/// string or None.
fn model_info_sha(v: &Value) -> Result<Option<String>, HubErr> {
    let Value::Obj(o) = v else { return Err("the model info is not an object".into()) };
    if o.get("id").is_none() {
        return Err(raise("KeyError", "'id'"));
    }
    if o.get("self").is_some() {
        return Err(raise("TypeError", "ModelInfo.__init__() got multiple values for argument 'self'"));
    }
    let either = |a: &str, b: &str| if o.value(a).truthy() { o.value(a).clone() } else { o.value(b).clone() };
    for d in [either("lastModified", "last_modified"), either("createdAt", "created_at")] {
        if d.truthy() && !hub_date_ok(&d) {
            return Err("a model info date is not in the form this binary reads".into());
        }
    }
    match o.value("inferenceProviderMapping") {
        Value::Null => {}
        Value::List(l) if l.is_empty() => {}
        Value::Obj(m) if m.len() == 0 => {}
        Value::List(_) | Value::Obj(_) => {
            return Err("the model info maps inference providers, which this binary does not read".into())
        }
        _ => return Err("huggingface_hub raises on this inferenceProviderMapping".into()),
    }
    if let Value::Obj(card) = either("cardData", "card_data") {
        for k in ["model-index", "eval_results"] {
            if card.value(k).truthy() {
                return Err("the model card holds eval results, which this binary does not read".into());
            }
        }
        for k in ["self", "ignore_metadata_errors"] {
            if card.get(k).is_some() {
                return Err(format!("the model card holds the key {k}").into());
            }
        }
        if !matches!(card.value("tags"), Value::Null | Value::Str(_) | Value::List(_)) {
            return Err("the model card's tags are not a list".into());
        }
    }
    let ti = either("transformersInfo", "transformers_info");
    if ti.truthy() {
        let Value::Obj(t) = &ti else { return Err("transformersInfo is not an object".into()) };
        if t.get("auto_model").is_none() {
            return Err("transformersInfo has no auto_model".into());
        }
        for k in t.keys() {
            if !matches!(k.as_str(), "auto_model" | "custom_class" | "pipeline_tag" | "processor") {
                return Err(format!("transformersInfo holds the key {k}").into());
            }
        }
    }
    match o.value("siblings") {
        Value::Null => {}
        Value::List(l) => {
            for s in l {
                let Value::Obj(so) = s else { return Err("a sibling is not an object".into()) };
                if so.get("rfilename").is_none() {
                    return Err("a sibling has no rfilename".into());
                }
                let lfs = so.value("lfs");
                if lfs.truthy() {
                    let Value::Obj(l) = lfs else { return Err("a sibling's lfs is not an object".into()) };
                    for k in ["size", "sha256", "pointerSize"] {
                        if l.get(k).is_none() {
                            return Err(format!("a sibling's lfs has no {k}").into());
                        }
                    }
                }
            }
        }
        _ => return Err("siblings is not a list".into()),
    }
    let st = o.value("safetensors");
    if st.truthy() {
        let Value::Obj(s) = st else { return Err("safetensors is not an object".into()) };
        for k in ["parameters", "total"] {
            if s.get(k).is_none() {
                return Err(format!("safetensors has no {k}").into());
            }
        }
    }
    if o.value("evalResults").truthy() {
        return Err("the model info holds eval results, which this binary does not read".into());
    }
    match o.value("sha") {
        Value::Null => Ok(None),
        Value::Str(s) => Ok(Some(s.clone())),
        _ => Err("the model info's sha is not text".into()),
    }
}

/// Requests as huggingface_hub's httpx session sends them, through the
/// proxies httpx would use, with the token it would send.
struct HubClient {
    proxies: netproxy::Httpx,
    token: String,
}

/// One answer: its status, headers, body and the URL asked.
struct HubResp {
    status: u16,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
    url: String,
}

impl HubResp {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers.iter().find(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_str())
    }
}

/// A URL as httpx sends it: a non-ASCII byte percent-encoded.
fn http_url(url: &str) -> String {
    let mut out = String::new();
    for b in url.bytes() {
        if b >= 0x80 {
            out.push_str(&format!("%{b:02X}"));
        } else {
            out.push(b as char);
        }
    }
    out
}

impl HubClient {
    /// One request. A connection that fails is httpcore's ConnectError,
    /// the first exception of Python's traceback.
    fn request(&self, method: &str, url: &str, extra: &[(&str, &str)]) -> Result<HubResp, HubErr> {
        let mut headers = vec![("accept", "*/*".to_string()), ("user-agent", "opendaisugi".to_string())];
        if extra.iter().all(|(k, _)| !k.eq_ignore_ascii_case("accept-encoding")) {
            headers.push(("accept-encoding", "identity".to_string()));
        }
        for (k, v) in extra {
            headers.push((k, v.to_string()));
        }
        if !self.token.is_empty() {
            headers.push(("authorization", format!("Bearer {}", self.token)));
        }
        match http::request(method, &http_url(url), &headers, 6000.0, &self.proxies) {
            Ok((status, headers, body)) => {
                if headers.iter().any(|(k, v)| k.eq_ignore_ascii_case("content-encoding") && !v.eq_ignore_ascii_case("identity")) {
                    return Err("the Hub's answer is compressed".into());
                }
                Ok(HubResp { status, headers, body, url: url.to_string() })
            }
            Err(http::Fail::Refused) => Err(refused()),
            Err(http::Fail::Other(_)) => Err(UNREACHED.into()),
            Err(_) => Err("the Hub could not be reached the way huggingface_hub reaches it".into()),
        }
    }

    /// The download's GET, its 200 body written to `out` as it arrives:
    /// the status, the headers and the bytes written.
    fn download(&self, url: &str, out: &mut dyn std::io::Write) -> Result<(u16, Vec<(String, String)>, u64), HubErr> {
        let mut headers = vec![
            ("accept", "*/*".to_string()),
            ("user-agent", "opendaisugi".to_string()),
            ("accept-encoding", "identity".to_string()),
        ];
        if !self.token.is_empty() {
            headers.push(("authorization", format!("Bearer {}", self.token)));
        }
        match http::request_to("GET", &http_url(url), &headers, &self.proxies, out) {
            Ok(r) => Ok(r),
            Err(http::Fail::Refused) => Err(refused()),
            Err(http::Fail::Write(e)) => Err(HubErr::Refuse(e)),
            Err(http::Fail::Other(_) | http::Fail::Timeout) => {
                Err("the download broke off, where huggingface_hub resumes it".into())
            }
            Err(_) => Err("the Hub could not be reached the way huggingface_hub reaches it".into()),
        }
    }

    /// `session.get`, `hf_raise_for_status` and `response.json()`.
    fn get_json(&self, url: &str) -> Result<Value, HubErr> {
        let r = self.request("GET", url, &[])?;
        raise_for_status(&r)?;
        json_of(&r)
    }

    /// `http_backoff("GET", url)`: a 429 or 5xx answer is asked again up to
    /// five times, waiting 1, 2, 4, 8 and 8 seconds (or the Retry-After
    /// seconds plus one), and each wait is logged on stderr by
    /// huggingface_hub's logger, as Python prints it.
    fn backoff_get(&self, url: &str, log: &mut String) -> Result<HubResp, HubErr> {
        const MAX: u32 = 5;
        let mut sleep = 1u64;
        let mut tries = 0;
        loop {
            tries += 1;
            let r = match self.request("GET", url, &[]) {
                Ok(r) => r,
                Err(_) => {
                    return Err("the Hub could not be reached for a next page, which huggingface_hub retries".into())
                }
            };
            if !matches!(r.status, 429 | 500 | 502 | 503 | 504) {
                return Ok(r);
            }
            log.push_str(&format!("HTTP Error {} thrown while requesting GET {url}\n", r.status));
            if tries > MAX {
                return Ok(r);
            }
            if r.status == 429 && r.header("ratelimit").is_some() {
                return Err("the Hub answered with ratelimit headers, which this binary does not read".into());
            }
            let after = r.header("Retry-After").map(str::trim).unwrap_or("");
            if !after.is_ascii() || (after.bytes().all(|b| b.is_ascii_digit()) && after.len() > 9) {
                // str.isdigit() reads digits of every script, and a wait
                // this long is not one this binary sleeps.
                return Err("the Hub answered with a Retry-After this binary does not read".into());
            }
            let reset = Some(after)
                .filter(|v| !v.is_empty() && v.bytes().all(|b| b.is_ascii_digit()))
                .and_then(|v| v.parse::<u64>().ok());
            let wait = match reset {
                Some(n) => {
                    log.push_str(&format!("Rate limited. Waiting {}.0s before retry [Retry {tries}/{MAX}].\n", n + 1));
                    n + 1
                }
                None => {
                    log.push_str(&format!("Retrying in {sleep}s [Retry {tries}/{MAX}].\n"));
                    sleep
                }
            };
            std::thread::sleep(std::time::Duration::from_secs(wait));
            sleep = (sleep * 2).min(8);
        }
    }

    /// `list_repo_files`: the first page of the tree with `session.get`,
    /// then each next page the Link header names with `http_backoff`, and
    /// the path of every file entry. `log` gathers the backoff's stderr.
    fn tree_files(&self, url: &str, log: &mut String) -> Result<Vec<String>, HubErr> {
        let mut r = self.request("GET", url, &[])?;
        let mut out = vec![];
        loop {
            raise_for_status(&r)?;
            out.extend(tree_page(&json_of(&r)?)?);
            let Some(next) = next_page(&r) else { return Ok(out) };
            if !next.starts_with("http://") && !next.starts_with("https://") {
                return Err("the Hub's next page is not an absolute URL".into());
            }
            r = self.backoff_get(&next, log)?;
        }
    }
}

/// `hf_raise_for_status`: a status of 400 or more raises HfHubHTTPError
/// from httpx's HTTPStatusError, which the traceback names first. A
/// redirect is not one this binary follows the oracle's way.
fn raise_for_status(r: &HubResp) -> Result<(), HubErr> {
    if r.status >= 400 {
        return Err(raise("httpx.HTTPStatusError", format!("HTTP {} for url '{}'", r.status, r.url)));
    }
    if r.status >= 300 {
        return Err(format!("the Hub answered HTTP {}, a redirect this binary does not follow", r.status).into());
    }
    Ok(())
}

/// `response.json()`.
fn json_of(r: &HubResp) -> Result<Value, HubErr> {
    let text = std::str::from_utf8(&r.body).map_err(|_| HubErr::Refuse("the Hub's answer is not UTF-8".into()))?;
    loads_strict(text).map_err(|e| raise("json.decoder.JSONDecodeError", format!("{e:?}")))
}

/// `response.links["next"]["url"]`.
fn next_page(r: &HubResp) -> Option<String> {
    static LINK: OnceLock<Regex> = OnceLock::new();
    let re = re(&LINK, r"<([^>]*)>([^,]*)");
    for (k, v) in &r.headers {
        if !k.eq_ignore_ascii_case("link") {
            continue;
        }
        for m in re.captures_iter(v) {
            for param in m[2].split(';') {
                if let Some((k, val)) = param.trim().split_once('=') {
                    if k.trim() == "rel" && val.trim().trim_matches(|c| c == '"' || c == '\'' || c == ' ') == "next" {
                        return Some(m[1].trim().to_string());
                    }
                }
            }
        }
    }
    None
}

/// `urllib.parse.quote(s)`: unreserved characters and "/" kept, the rest
/// %XX of its UTF-8.
fn py_quote(s: &str) -> String {
    let mut out = String::new();
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || b"_.-~/".contains(&b) {
            out.push(b as char);
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

const CACHEDIR_TAG: &str = "Signature: 8a477f597d28d172789f06886806bc55\n\
# This file is a cache directory tag created by huggingface_hub.\n\
# For information about cache directory tags, see:\n\
#\thttps://bford.info/cachedir/\n";

fn rand_hex(n: usize) -> String {
    let mut b = vec![0u8; n];
    let _ = getrandom_fill(&mut b);
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn getrandom_fill(b: &mut [u8]) -> std::io::Result<()> {
    use std::io::Read;
    std::fs::File::open("/dev/urandom")?.read_exact(b)
}

/// `os.path.relpath(target, start)` for two absolute paths.
fn relpath(target: &str, start: &str) -> String {
    let t: Vec<&str> = target.split('/').filter(|s| !s.is_empty()).collect();
    let s: Vec<&str> = start.split('/').filter(|s| !s.is_empty()).collect();
    let common = t.iter().zip(&s).take_while(|(a, b)| a == b).count();
    let mut parts: Vec<&str> = vec![".."; s.len() - common];
    parts.extend(&t[common..]);
    if parts.is_empty() {
        ".".into()
    } else {
        parts.join("/")
    }
}

fn exists(p: &str) -> bool {
    std::fs::metadata(p).is_ok()
}

impl Env {
    /// `constants.HF_HUB_CACHE`: HF_HUB_CACHE, else HUGGINGFACE_HUB_CACHE,
    /// else <HF_HOME>/hub, HF_HOME defaulting to <XDG_CACHE_HOME or
    /// ~/.cache>/huggingface; "~" expanded. A "$" (which expandvars reads)
    /// is not modelled.
    fn hub_cache_dir(&self) -> Result<String, HubErr> {
        let expand = |p: &str| -> Result<String, HubErr> {
            if p.contains('$') {
                return Err("a Hugging Face cache path with $, which expandvars reads".into());
            }
            if p == "~" || p.starts_with("~/") {
                return Ok(format!("{}{}", self.home, &p[1..]));
            }
            if p.starts_with('~') {
                return Err("a Hugging Face cache path with ~user".into());
            }
            Ok(p.to_string())
        };
        if let Some(v) = self.env.get("HF_HUB_CACHE") {
            return expand(v);
        }
        if let Some(v) = self.env.get("HUGGINGFACE_HUB_CACHE") {
            return expand(v);
        }
        let home = match self.env.get("HF_HOME") {
            Some(h) => h.clone(),
            None => {
                let cache = self.env.get("XDG_CACHE_HOME").cloned().unwrap_or_else(|| format!("{}/.cache", self.home));
                format!("{cache}/huggingface")
            }
        };
        Ok(format!("{}/hub", expand(&home)?))
    }

    /// `hf_hub_download(repo_id, filename, revision)` into the cache: the
    /// HEAD of the file's resolve URL for its commit, etag and size, then
    /// the blob downloaded to a temporary file and moved into
    /// blobs/<etag>, the snapshot's relative symlink to it,
    /// refs/<revision> when the revision is not the commit, the lock file
    /// and CACHEDIR.TAG, as huggingface_hub lays them out. What it does not
    /// model (a redirect, Xet storage, a Hub answer it would retry or fall
    /// back on) is refused.
    fn hub_pull(&mut self, hub: &HubClient, endpoint: &str, repo: &str, filename: &str, revision: &str) -> Result<String, HubErr> {
        let mut cache = self.hub_cache_dir()?;
        if !cache.starts_with('/') {
            cache = format!("{}/{cache}", self.cwd());
        }
        let cache = cache.trim_end_matches('/').to_string();
        if filename.split('/').any(|p| p.is_empty() || p == "..") {
            return Err("a file name huggingface_hub rejects".into());
        }
        let folder = format!("models--{}", repo.replace('/', "--"));
        let storage = format!("{cache}/{folder}");
        let pointer = format!("{storage}/snapshots/{revision}/{filename}");
        let is_commit = revision.len() == 40 && revision.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
        if is_commit && exists(&pointer) {
            return Ok(pointer);
        }
        let url = format!("{endpoint}/{repo}/resolve/{}/{}", py_quote(revision), py_quote(filename));
        let r = match hub.request("HEAD", &url, &[("accept-encoding", "identity")]) {
            Ok(r) => r,
            Err(_) => {
                return Err("the Hub could not be reached for the file, where huggingface_hub falls back to the cache".into())
            }
        };
        if r.status == 404 && r.header("X-Error-Code") == Some("EntryNotFound") && r.header("X-Repo-Commit").is_none() {
            raise_for_status(&r)?;
        }
        if r.status >= 300 {
            return Err(format!("the Hub answered the file's HEAD with HTTP {}, which this binary does not follow", r.status).into());
        }
        if r.headers.iter().any(|(k, _)| k.to_ascii_lowercase().starts_with("x-xet-")) {
            return Err("the file is in Xet storage, which this binary does not read".into());
        }
        let commit = r.header("X-Repo-Commit").unwrap_or("").to_string();
        let etag = r.header("X-Linked-Etag").filter(|v| !v.is_empty()).or_else(|| r.header("ETag")).unwrap_or("");
        let etag = etag.trim_start_matches(['W', '/']).trim_matches('"').to_string();
        let size = r.header("X-Linked-Size").filter(|v| !v.is_empty()).or_else(|| r.header("Content-Length")).and_then(|v| v.parse::<u64>().ok());
        let Some(size) = size else {
            return Err("the file's HEAD lacks a size, where huggingface_hub falls back to the cache".into());
        };
        let dots = |v: &str| v == "." || v == "..";
        if commit.is_empty() || etag.is_empty() || etag.contains('/') || commit.contains('/') || dots(&etag) || dots(&commit) {
            return Err("the file's HEAD lacks a commit or an etag, where huggingface_hub falls back to the cache".into());
        }
        let blob = format!("{storage}/blobs/{etag}");
        let pointer = format!("{storage}/snapshots/{commit}/{filename}");
        let write_ref = || -> std::io::Result<()> {
            if revision == commit {
                return Ok(());
            }
            let rf = format!("{storage}/refs/{revision}");
            if let Some(parent) = std::path::Path::new(&rf).parent() {
                std::fs::create_dir_all(parent)?;
            }
            if std::fs::read_to_string(&rf).is_ok_and(|old| old == commit) {
                return Ok(());
            }
            let tmp = format!("{rf}.{}.tmp", rand_hex(4));
            std::fs::write(&tmp, &commit)?;
            std::fs::rename(&tmp, &rf)
        };
        if exists(&pointer) {
            let _ = write_ref();
            return Ok(pointer);
        }
        let io = |e: std::io::Error| HubErr::Partly(e.to_string(), cache.clone());
        let parent = |p: &str| p.rsplit_once('/').map(|(a, _)| a.to_string()).unwrap_or_default();
        std::fs::create_dir_all(parent(&blob)).map_err(io)?;
        std::fs::create_dir_all(parent(&pointer)).map_err(io)?;
        let tag = format!("{cache}/CACHEDIR.TAG");
        if !exists(&tag) {
            let _ = std::fs::write(&tag, CACHEDIR_TAG);
        }
        write_ref().map_err(io)?;
        let lock = format!("{cache}/.locks/{folder}/{etag}.lock");
        std::fs::create_dir_all(parent(&lock)).map_err(io)?;
        if std::fs::OpenOptions::new().read(true).write(true).create(true).truncate(false).open(&lock).is_ok() {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(&lock, std::fs::Permissions::from_mode(0o664));
        }
        if !exists(&blob) {
            let partly = |why: String| HubErr::Partly(why, cache.clone());
            self.hub_download(hub, &url, &blob, size, filename).map_err(|e| match e {
                HubErr::Refuse(why) => partly(why),
                e => e,
            })?;
        }
        if !exists(&pointer) {
            let _ = std::fs::remove_file(&pointer);
            std::os::unix::fs::symlink(relpath(&blob, &parent(&pointer)), &pointer).map_err(io)?;
        }
        Ok(pointer)
    }

    /// `http_get` into a process-unique `<blob>.<hex>.incomplete` file,
    /// written as the body arrives, checked against the HEAD's size, then
    /// moved to the blob; the temporary file is removed on any failure, as
    /// `_download_to_tmp_and_move` does. Progress goes to stderr only on a
    /// terminal.
    fn hub_download(&mut self, hub: &HubClient, url: &str, blob: &str, size: u64, filename: &str) -> Result<(), HubErr> {
        let tmp = format!("{blob}.{}.incomplete", rand_hex(4));
        let mut f = std::fs::File::create(&tmp).map_err(|e| HubErr::Refuse(e.to_string()))?;
        let r = hub.download(url, &mut f);
        drop(f);
        let done = r.and_then(|(status, headers, n)| {
            let header = |name: &str| headers.iter().find(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_str());
            if status >= 400 {
                return Err(raise("httpx.HTTPStatusError", format!("HTTP {status} for url '{url}'")));
            }
            if status != 200 {
                return Err(format!("the download answered HTTP {status}, which this binary does not follow").into());
            }
            if header("content-encoding").is_some_and(|v| !v.eq_ignore_ascii_case("identity")) {
                return Err("the download came compressed".into());
            }
            if n != size {
                let mut shown = header("content-disposition")
                    .and_then(|v| v.split_once("filename=\"").and_then(|(_, rest)| rest.split_once("\";")).map(|(name, _)| name.to_string()))
                    .unwrap_or_else(|| url.to_string());
                let chars: Vec<char> = shown.chars().collect();
                if chars.len() > 40 {
                    shown = format!("(\u{2026}){}", chars[chars.len() - 40..].iter().collect::<String>());
                }
                return Err(raise(
                    "OSError",
                    format!(
                        "Consistency check failed: file should be of size {size} but has size {n} ({shown}).\nThis is usually due to network issues while downloading the file. Please retry with `force_download=True`."
                    ),
                ));
            }
            use std::io::IsTerminal;
            if std::io::stderr().is_terminal() {
                let name = filename.rsplit('/').next().unwrap_or(filename);
                self.errf(&format!("{name}: {n}/{size} bytes\n"));
            }
            std::fs::rename(&tmp, blob).map_err(|e| HubErr::Refuse(e.to_string()))
        });
        let _ = std::fs::remove_file(&tmp);
        done
    }

    /// What Python prints for `err`: the traceback of an exception it does
    /// not catch, else this binary's refusal.
    fn hub_err(&mut self, cmd: &str, err: HubErr) -> Res {
        match err {
            HubErr::Raise(class, msg) => {
                self.errf(&format!("Traceback (most recent call last): ...\n{class}: {msg}\n"));
                exit(1)
            }
            HubErr::Refuse(why) => self.refuse(cmd, &why),
            HubErr::Partly(why, cache) => {
                self.errf(&format!("daisugi {cmd}: {why}. The Hugging Face cache {cache} may hold folders and a lock file from this pull.\n"));
                exit(2)
            }
        }
    }
}

impl Env {
    /// `get_token`: HF_TOKEN, else the token file.
    fn hub_token(&self) -> String {
        for k in ["HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"] {
            if let Some(t) = self.env.get(k) {
                if !t.trim().is_empty() {
                    return t.trim().to_string();
                }
            }
        }
        let path = match self.env.get("HF_TOKEN_PATH").filter(|p| !p.is_empty()) {
            Some(p) => p.clone(),
            None => {
                let home = match self.env.get("HF_HOME").filter(|p| !p.is_empty()) {
                    Some(h) => h.clone(),
                    None => {
                        let cache = match self.env.get("XDG_CACHE_HOME").filter(|p| !p.is_empty()) {
                            Some(c) => c.clone(),
                            None => format!("{}/.cache", self.home),
                        };
                        format!("{cache}/huggingface")
                    }
                };
                format!("{home}/token")
            }
        };
        std::fs::read_to_string(path).map(|t| t.trim().to_string()).unwrap_or_default()
    }

    pub(super) fn models_pin(&mut self, args: &[String]) -> Res {
        const CMD: &str = "models pin";
        let opts = [
            Opt::val(&["--suffix"], "TEXT", "File suffix to resolve (.gguf or .llamafile)."),
            Opt::flag(&["--pull"], "Download the resolved file (pinned to its commit)."),
            Opt::flag(&["--json"], "Machine-readable JSON output."),
        ];
        let p = match parse_args(args, &opts, 1) {
            Ok(p) => p,
            Err(m) => return self.usage_args(CMD, "REPO", &m),
        };
        if p.help {
            return self.cmd_help(CMD, " REPO", "Resolve a repo to a file pinned to its commit; --pull downloads it.", &opts);
        }
        if p.args.is_empty() {
            return self.usage_args(CMD, "REPO", "Missing argument 'repo'.");
        }
        let repo = p.args[0].clone();
        let suffix = p.str("--suffix", ".gguf");
        // validate_repo_id runs before any request, then the offline check.
        if let Some(why) = hub_repo_invalid(&repo) {
            return self.hub_err(CMD, raise("huggingface_hub.errors.HFValidationError", why));
        }
        let endpoint = match self.env.get("HF_ENDPOINT") {
            Some(v) => v.trim_end_matches('/').to_string(),
            None => HUB_DEFAULT_ENDPOINT.to_string(),
        };
        if !endpoint.starts_with("http://") && !endpoint.starts_with("https://") {
            return self.refuse(CMD, "HF_ENDPOINT is not an http or https URL");
        }
        let tree_url = format!("{endpoint}/api/models/{repo}/tree/main?recursive=true&expand=false");
        if matches!(self.env.get("HF_HUB_OFFLINE").map(|v| upper(v)).as_deref(), Some("1" | "ON" | "YES" | "TRUE")) {
            return self.hub_err(
                CMD,
                raise(
                    "huggingface_hub.errors.OfflineModeIsEnabled",
                    format!(
                        "Cannot reach {tree_url}: offline mode is enabled. To disable it, please unset the `HF_HUB_OFFLINE` environment variable."
                    ),
                ),
            );
        }
        let hub = HubClient { proxies: netproxy::httpx_for(&self.env), token: self.hub_token() };
        let mut log = String::new();
        let files = hub.tree_files(&tree_url, &mut log);
        self.errf(&log);
        let files = match files {
            Ok(f) => f,
            Err(e) => return self.hub_err(CMD, e),
        };
        let mut matches: Vec<&String> = files.iter().filter(|f| f.ends_with(suffix.as_str())).collect();
        if matches.is_empty() {
            self.errf(&format!("no {} file in {} (saw {} files)\n", repr(&suffix), repr(&repo), files.len()));
            return exit(2);
        }
        matches.sort();
        let file = matches[0].clone();
        let sha = match hub.get_json(&format!("{endpoint}/api/models/{repo}")).and_then(|v| model_info_sha(&v)) {
            Ok(s) => s,
            Err(e) => return self.hub_err(CMD, e),
        };
        let mut pulled = None;
        if p.flag("--pull") {
            let Some(rev) = sha.clone() else {
                return self.refuse(CMD, "--pull with a model info that has no sha");
            };
            match self.hub_pull(&hub, &endpoint, &repo, &file, &rev) {
                Ok(path) => pulled = Some(path),
                Err(e) => return self.hub_err(CMD, e),
            }
        }
        if p.flag("--json") {
            let o = Object::new()
                .with("repo_id", repo.as_str())
                .with("filename", file.as_str())
                .with("revision", sha.map(Value::Str).unwrap_or(Value::Null))
                .with("downloaded_path", pulled.map(Value::Str).unwrap_or(Value::Null));
            self.out(&format!("{}\n", dumps_indent(&Value::Obj(o), 2, true)));
            return Ok(());
        }
        let rev = sha.unwrap_or_else(|| "None".to_string());
        self.out(&format!("repo:     {repo}\nfile:     {file}\nrevision: {rev}   (immutable commit — reproducible)\n"));
        match pulled {
            Some(path) => self.out(&format!("pulled:   {path}\n")),
            None => self.out(&format!("\nDownload it (pinned):  daisugi models pin {repo} --suffix {suffix} --pull\n")),
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_repo_id_rule_is_validate_repo_ids() {
        for ok in ["a", "foo/bar", "123", "Foo-BAR_foo.bar123", "ibm-granite/granite-4.1-3b-GGUF", "é/x", "\u{661}\u{662}/x"] {
            assert!(hub_repo_invalid(ok).is_none(), "{ok}");
        }
        for bad in ["datasets/foo/bar", ".repo_id", "foo--bar", "foo.git", "a..b", "-a/b", "a/b-", "", "a/", "a/b\u{301}"] {
            assert!(hub_repo_invalid(bad).is_some(), "{bad}");
        }
    }

    #[test]
    fn a_hub_date_is_read_only_in_the_hubs_form() {
        for ok in ["2024-11-16T00:27:02Z", "2022-08-19T07:19:38.123456789Z", "2024-02-29T00:00:00.1Z"] {
            assert!(hub_date_ok(&Value::Str(ok.into())), "{ok}");
        }
        for bad in ["2023-02-29T00:00:00Z", "2024-11-16 00:27:02Z", "2024-11-16T00:27:02", "x"] {
            assert!(!hub_date_ok(&Value::Str(bad.into())), "{bad}");
        }
    }
}
