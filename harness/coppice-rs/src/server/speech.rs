//! speech.status and speech.say: the foreman answers aloud. The page speaks
//! with the browser's own voice unless [voice] speak_url names a speech
//! server with an OpenAI-style POST /v1/audio/speech. speech.say asks that
//! server for one sentence and answers the audio, base64 in the reply. The
//! web server turns that reply into a binary frame on the page's socket.
//! The Go server says the same words; the cases compare them.

use std::io::Read;
use std::net::IpAddr;
use std::os::fd::{AsRawFd, OwnedFd};
use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::{json, Value};

use super::{Client, Server};
use crate::gojson;
use crate::proto::{self, Request, Response};
use crate::web::client;

/// What a pane or a plugin gets for the speech verbs.
pub const SPEECH_REFUSAL: &str = "only the operator reads aloud.";

/// The limits of one sentence. A server that takes longer than the limit,
/// or sends more than MAX_AUDIO bytes of body, has failed: the page then
/// speaks that reply with the browser's voice.
const MAX_AUDIO: usize = 2 << 20;
const MAX_TEXT: usize = 2000;
const LIMIT: Duration = Duration::from_secs(10);
/// The most raw bytes read for a reply: the body plus its framing. A
/// reply that fills this is over MAX_AUDIO in its body too, unless its
/// framing is more than double the body.
const MAX_RAW: u64 = (2 * MAX_AUDIO + 65536) as u64;

const DEFAULT_MODEL: &str = "kokoro";
const DEFAULT_VOICE: &str = "af_heart";

const BAD_URL_LINE: &str = "[voice] speak_url is not an http:// or https:// address with no user name, password or query, so the browser's own voice speaks. Fix speak_url in coppice.toml, then start the coppice server again.";

/// A speak_url, read.
struct Endpoint {
    /// The address of POST /v1/audio/speech.
    url: String,
    /// The host and port, as written.
    host: String,
    /// The host with no brackets or port, in lower case.
    hostname: String,
    https: bool,
    /// The address to show: scheme, host and path.
    shown: String,
}

/// Reads a speak_url. None for an address with a user name, a password, a
/// query or a fragment, a port that is not digits, or one that is not http
/// or https with a host.
fn endpoint(raw: &str) -> Option<Endpoint> {
    let raw = raw.trim();
    if raw.contains(['?', '#', '@']) {
        return None;
    }
    let lower = raw.get(..8).unwrap_or(raw).to_ascii_lowercase();
    let (scheme, rest) = if lower.starts_with("http://") {
        ("http", &raw[7..])
    } else if lower.starts_with("https://") {
        ("https", &raw[8..])
    } else {
        return None;
    };
    let (host, path) = match rest.find('/') {
        Some(i) => (&rest[..i], &rest[i..]),
        None => (rest, ""),
    };
    let (name, port) = match host.strip_prefix('[') {
        Some(r) => {
            let (n, after) = r.split_once(']')?;
            (n, after.strip_prefix(':').unwrap_or(""))
        }
        None => {
            if host.matches(':').count() > 1 {
                return None;
            }
            host.rsplit_once(':').unwrap_or((host, ""))
        }
    };
    if name.is_empty() || !port.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    // A host is letters, digits, dots and hyphens, or an IP address, as in
    // Go: no zone, no underscore, nothing a name lookup should not see.
    let plain = !host.starts_with('[')
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'.' || b == b'-');
    if !plain && name.parse::<IpAddr>().is_err() {
        return None;
    }
    let mut p = path.trim_end_matches('/').to_string();
    if p.ends_with("/audio/speech") {
    } else if p.ends_with("/v1") {
        p.push_str("/audio/speech");
    } else {
        p.push_str("/v1/audio/speech");
    }
    Some(Endpoint {
        url: format!("{scheme}://{host}{p}"),
        host: host.to_string(),
        hostname: name.to_ascii_lowercase(),
        https: scheme == "https",
        shown: format!("{scheme}://{host}{}", p.trim_end_matches('/')),
    })
}

/// Whether a key may be sent to e: over https, or over plain http to this
/// box or to a tailnet address or name. The operator sets speak_url, so a
/// name that ends in .ts.net is trusted by that suffix: no lookup checks it
/// is on the tailnet.
fn key_may_go(e: &Endpoint) -> bool {
    if e.https || e.hostname == "localhost" || e.hostname.ends_with(".ts.net") {
        return true;
    }
    match e.hostname.parse::<IpAddr>() {
        Ok(ip) => tailnet_or_here(ip),
        Err(_) => false,
    }
}

fn tailnet_or_here(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v) => v.is_loopback() || (v.octets()[0] == 100 && v.octets()[1] & 0xC0 == 0x40),
        IpAddr::V6(v) => match v.to_ipv4_mapped() {
            Some(m) => tailnet_or_here(IpAddr::V4(m)),
            None => {
                let s = v.segments();
                v.is_loopback() || (s[0] == 0xfd7a && s[1] == 0x115c && s[2] == 0xa1e0)
            }
        },
    }
}

fn or_default(a: &str, b: &str) -> String {
    if a.trim().is_empty() {
        b.to_string()
    } else {
        a.trim().to_string()
    }
}

fn slow_line(host: &str) -> String {
    format!(
        "The speech server at {host} took longer than {} seconds. Check that it runs and is not busy.",
        LIMIT.as_secs()
    )
}

/// The media type the speech server named, with no parameters, or
/// audio/mpeg when it named none that looks like audio.
fn speech_type(ct: &str) -> String {
    let ct = ct.split(';').next().unwrap_or("").trim().to_lowercase();
    if ct.starts_with("audio/") && ct.len() <= 40 && !ct.contains([' ', '"', '\'', '<', '>']) {
        ct
    } else {
        "audio/mpeg".to_string()
    }
}

fn base64(b: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(b.len().div_ceil(3) * 4);
    for c in b.chunks(3) {
        let n = (c[0] as u32) << 16
            | (*c.get(1).unwrap_or(&0) as u32) << 8
            | *c.get(2).unwrap_or(&0) as u32;
        out.push(T[(n >> 18) as usize & 63] as char);
        out.push(T[(n >> 12) as usize & 63] as char);
        out.push(if c.len() > 1 {
            T[(n >> 6) as usize & 63] as char
        } else {
            '='
        });
        out.push(if c.len() > 2 {
            T[n as usize & 63] as char
        } else {
            '='
        });
    }
    out
}

/// The pipe that carries the profile bundle. Both ends are close-on-exec,
/// so no pane or command started while it is open inherits the read end,
/// which holds every profile's key. Only the daisugi that writes the
/// bundle gets the write end, as descriptor 3.
fn bundle_pipe() -> std::io::Result<(OwnedFd, OwnedFd)> {
    crate::adapters::pipe()
}

/// The last line of a command's complaint, at most 200 bytes, as the Go
/// server reads it: the line that names the fault. With no text, the
/// error says it.
fn last_line(text: &str, err: &str) -> String {
    let text = text.trim();
    let line = match text.rfind('\n') {
        Some(i) => &text[i + 1..],
        None => text,
    };
    if line.is_empty() && !err.is_empty() {
        return err.to_string();
    }
    let mut end = line.len().min(200);
    while !line.is_char_boundary(end) {
        end -= 1;
    }
    line[..end].to_string()
}

/// The key of the operator profile `name`, from
/// `daisugi agent-profiles bundle --fd 3`. The bytes live in this call
/// only. Err is the line to say, with no key in it.
fn bundle_key(name: &str, host: &str) -> Result<Option<String>, String> {
    let bin = std::env::var("COPPICE_DAISUGI_BIN")
        .ok()
        .filter(|b| !b.is_empty())
        .unwrap_or_else(|| "daisugi".to_string());
    let dir = format!("{}/compactor", Server::foreman_dir());
    {
        use std::os::unix::fs::DirBuilderExt;
        let _ = std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&dir);
    }
    let unknown = |why: String| {
        format!(
            "The agent profiles could not be read, so the speech server at {host} gets no key: {}.",
            why.strip_suffix('.').unwrap_or(&why)
        )
    };
    let (r, w) = bundle_pipe().map_err(|_| unknown("no pipe".into()))?;
    let wfd = w.as_raw_fd();
    let mut cmd = Command::new(&bin);
    cmd.args(["agent-profiles", "bundle", "--fd", "3"])
        .current_dir(&dir)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped());
    // SAFETY: setpgid, dup2 and fcntl are async-signal-safe. The child gets
    // the write end as descriptor 3: dup2 clears close-on-exec on the new
    // descriptor, and a write end that already is 3 is cleared by hand.
    // The read end is close-on-exec and closes at exec.
    unsafe {
        cmd.pre_exec(move || {
            libc::setpgid(0, 0);
            if wfd == 3 {
                libc::fcntl(3, libc::F_SETFD, 0);
            } else if libc::dup2(wfd, 3) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            super::foreman_turn::close_from_4();
            Ok(())
        });
    }
    let child = cmd.spawn();
    drop(w);
    let mut child = match child {
        Ok(c) => c,
        Err(e) => {
            return Err(unknown(format!(
                "cannot start daisugi to read the agent profiles: {}",
                crate::adapters::exec_error(&bin, &e)
            )))
        }
    };
    let (tx, rx) = mpsc::channel::<Vec<u8>>();
    let mut source = std::fs::File::from(r);
    thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = (&mut source).take(1 << 20).read_to_end(&mut buf);
        let _ = tx.send(buf);
    });
    let mut err_pipe = child.stderr.take().expect("stderr is piped");
    let (etx, erx) = mpsc::channel::<Vec<u8>>();
    thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = (&mut err_pipe).take(4096).read_to_end(&mut buf);
        let _ = etx.send(buf);
    });
    let start = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(st)) => break Some(st),
            Ok(None) if start.elapsed() < Duration::from_secs(5) => {
                thread::sleep(Duration::from_millis(5))
            }
            _ => {
                // SAFETY: killing the group we made.
                unsafe { libc::kill(-(child.id() as i32), libc::SIGKILL) };
                let _ = child.kill();
                let _ = child.wait();
                break None;
            }
        }
    };
    let mut bytes = rx
        .recv_timeout(Duration::from_millis(200))
        .unwrap_or_default();
    let stderr = erx
        .recv_timeout(Duration::from_millis(200))
        .unwrap_or_default();
    let ok = status.map(|s| s.success()).unwrap_or(false);
    if !ok {
        bytes.fill(0);
        let why = match status {
            Some(s) => match s.code() {
                Some(c) => format!("exit status {c}"),
                None => "signal: killed".to_string(),
            },
            None => "signal: killed".to_string(),
        };
        return Err(unknown(format!(
            "daisugi agent-profiles bundle failed: {}",
            last_line(&String::from_utf8_lossy(&stderr), &why)
        )));
    }
    let table: Result<Value, _> = serde_json::from_slice(&bytes);
    bytes.fill(0);
    let Ok(Value::Object(table)) = table else {
        return Err(unknown(
            "daisugi agent-profiles bundle wrote something that is not a profile table".into(),
        ));
    };
    let Some(entry) = table.get(name) else {
        return Err(format!("The speak profile \"{name}\" is not in the operator's agent profiles (a key variable it names may be unset). Fix [voice] speak_profile in coppice.toml."));
    };
    Ok(entry
        .get("key")
        .and_then(|k| k.as_str())
        .map(|k| k.to_string()))
}

pub fn register(s: &Arc<Server>) {
    let _ = s.handle(
        "speech.status",
        Arc::new(|s, c, r| s.handle_speech_status(c, r)),
    );
    let _ = s.handle("speech.say", Arc::new(|s, c, r| s.handle_speech_say(c, r)));
}

impl Server {
    fn handle_speech_status(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let v = &self.cfg.voice;
        let mut res = vec![("mode", json!("browser"))];
        if v.speak_url.trim().is_empty() {
            return proto::ok_resp(&r.id, gojson::map(res));
        }
        let Some(e) = endpoint(&v.speak_url) else {
            res.push(("line", json!(BAD_URL_LINE)));
            return proto::ok_resp(&r.id, gojson::map(res));
        };
        res = vec![
            ("mode", json!("server")),
            ("url", json!(e.shown)),
            ("model", json!(or_default(&v.speak_model, DEFAULT_MODEL))),
            ("voice", json!(or_default(&v.speak_voice, DEFAULT_VOICE))),
        ];
        if !v.speak_profile.is_empty() {
            res.push(("profile", json!(v.speak_profile)));
        }
        proto::ok_resp(&r.id, gojson::map(res))
    }

    fn handle_speech_say(self: &Arc<Server>, _c: &Arc<Client>, r: &Request) -> Response {
        let bad = |m: &str| proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, m);
        let adapter = |m: &str| proto::err_resp(&r.id, proto::ERR_ADAPTER, m);
        let text = r.str("text").unwrap_or_default();
        let text = text.trim();
        if text.is_empty() {
            return bad("speech.say needs the text to speak.");
        }
        if text.len() > MAX_TEXT {
            return bad(&format!(
                "That text is over {MAX_TEXT} bytes. Send one sentence at a time."
            ));
        }
        let v = &self.cfg.voice;
        let Some(ep) = endpoint(&v.speak_url) else {
            return bad("No speech server is set. Set [voice] speak_url in coppice.toml, then start the coppice server again.");
        };
        let host = ep.host.clone();
        let mut key = String::new();
        if !v.speak_profile.is_empty() {
            match bundle_key(&v.speak_profile, &host) {
                Err(line) => return adapter(&line),
                Ok(k) => key = k.unwrap_or_default(),
            }
            if !key.is_empty() && !key_may_go(&ep) {
                return adapter(&format!("The speech server at {host} is reached over plain http, so the key of the profile \"{}\" is not sent. Use https, or an address on this box or the tailnet (127.0.0.1, 100.64.0.0/10, fd7a:115c:a1e0::/48, *.ts.net), in speak_url in coppice.toml.", v.speak_profile));
            }
        }
        let body = serde_json::to_vec(&json!({
            "model": or_default(&v.speak_model, DEFAULT_MODEL),
            "voice": or_default(&v.speak_voice, DEFAULT_VOICE),
            "input": text,
            "response_format": "mp3",
        }))
        .unwrap_or_default();
        let mut headers: Vec<(String, String)> =
            vec![("Content-Type".into(), "application/json".into())];
        if !key.is_empty() {
            headers.push(("Authorization".into(), format!("Bearer {key}")));
        }
        // The client holds the whole exchange to LIMIT, the connection and
        // every read included. The thread is for the name lookup, which it
        // does not bound.
        let (tx, rx) = mpsc::channel();
        let url = ep.url.clone();
        thread::spawn(move || {
            let h: Vec<(&str, &str)> = headers
                .iter()
                .map(|(k, v)| (k.as_str(), v.as_str()))
                .collect();
            let _ = tx.send(client::send_capped("POST", &url, &h, &body, LIMIT, MAX_RAW));
        });
        let reply = match rx.recv_timeout(LIMIT + Duration::from_millis(500)) {
            Err(_) => return proto::err_resp(&r.id, proto::ERR_TIMEOUT, &slow_line(&host)),
            Ok(Err(client::Fail::Reach(e))) if e == "timed out" => {
                return proto::err_resp(&r.id, proto::ERR_TIMEOUT, &slow_line(&host))
            }
            Ok(Err(_)) => {
                return adapter(&format!("The speech server at {host} did not answer. Check that it runs and that speak_url in coppice.toml names it."))
            }
            Ok(Ok(rep)) => rep,
        };
        if reply.status / 100 != 2 {
            return adapter(&format!("The speech server at {host} answered {}. Check [voice] speak_model, speak_voice and speak_profile in coppice.toml.", reply.status));
        }
        let audio = reply.body;
        if audio.len() > MAX_AUDIO {
            return adapter(&format!("The speech server at {host} sent more than {} MiB of audio for one sentence. Use a shorter sentence or another server.", MAX_AUDIO >> 20));
        }
        if audio.is_empty() {
            return adapter(&format!("The speech server at {host} sent no audio. Check [voice] speak_model and speak_voice in coppice.toml."));
        }
        proto::ok_resp(
            &r.id,
            gojson::map(vec![
                ("type", json!(speech_type(&reply.content_type))),
                ("bytes", json!(audio.len())),
                ("audio", json!(base64(&audio))),
            ]),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn endpoints_and_refusals() {
        let e = |s: &str| endpoint(s).map(|x| (x.url, x.shown));
        assert_eq!(
            e("http://gpu:8880").unwrap().0,
            "http://gpu:8880/v1/audio/speech"
        );
        assert_eq!(
            e("http://gpu:8880/v1/").unwrap().1,
            "http://gpu:8880/v1/audio/speech"
        );
        assert_eq!(
            e("https://x.test/audio/speech").unwrap().0,
            "https://x.test/audio/speech"
        );
        assert!(e("HTTP://GPU:8880").is_some());
        for bad in [
            "gpu:8880",
            "http://u:pw@gpu",
            "http://gpu/?key=S",
            "ftp://gpu",
            "http://",
            "http://:80",
            "http://gpu:abc",
            "http://::1/",
        ] {
            assert!(e(bad).is_none(), "{bad}");
        }
        assert!(e("http://gpu:").is_some());
        for bad in [
            "http://gpu_box:1",
            "http://gp u",
            "http://[::1%lo]:1",
            "http://gpu!:1",
            "http://[gpu]:1",
        ] {
            assert!(e(bad).is_none(), "{bad}");
        }
        assert!(e("http://gpu-box.local:1").is_some());
        assert!(e("http://[::1]:8880").is_some());
    }

    #[test]
    fn a_key_goes_only_where_it_is_safe() {
        for (raw, want) in [
            ("https://api.example.com", true),
            ("http://127.0.0.1:8880", true),
            ("http://localhost:8880", true),
            ("http://[::1]:8880", true),
            ("http://100.64.0.1:8880", true),
            ("http://100.127.255.254", true),
            ("http://[fd7a:115c:a1e0::1]:8880", true),
            ("http://gpu.tail1.ts.net:8880", true),
            ("http://100.128.0.1", false),
            ("http://100.63.255.255", false),
            ("http://10.0.0.5:8880", false),
            ("http://gpu:8880", false),
            ("http://example.com", false),
            ("http://[fd7a:115c:a1e1::1]", false),
            ("http://notts.net.example.com", false),
        ] {
            assert_eq!(key_may_go(&endpoint(raw).unwrap()), want, "{raw}");
        }
    }

    #[test]
    fn base64_matches() {
        assert_eq!(base64(b"ID3 fake audio"), "SUQzIGZha2UgYXVkaW8=");
        assert_eq!(base64(b"ab"), "YWI=");
        assert_eq!(base64(b"abc"), "YWJj");
    }

    fn fd_flags(fd: &OwnedFd) -> i32 {
        // SAFETY: reads the flags of a descriptor this test owns.
        unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) }
    }

    /// The pipe that carries every profile's key must not leak into a pane
    /// or any other child started while it is open.
    #[test]
    fn the_bundle_pipe_closes_on_exec() {
        let (r, w) = bundle_pipe().unwrap();
        assert!(fd_flags(&r) & libc::FD_CLOEXEC != 0, "read end leaks");
        assert!(fd_flags(&w) & libc::FD_CLOEXEC != 0, "write end leaks");
        // A child started now holds neither end.
        let out = Command::new("/bin/sh")
            .args(["-c", "ls /proc/self/fd"])
            .output()
            .unwrap();
        let listed = String::from_utf8_lossy(&out.stdout).to_string();
        for fd in [r.as_raw_fd(), w.as_raw_fd()] {
            assert!(
                !listed.lines().any(|l| l.trim() == fd.to_string()),
                "a child holds fd {fd}: {listed}"
            );
        }
    }

    #[test]
    fn a_complaint_is_its_last_line_cut_at_200_bytes() {
        assert_eq!(last_line("a\nb fault\n", "x"), "b fault");
        assert_eq!(last_line("  \n", "exit status 2"), "exit status 2");
        assert_eq!(last_line(&"é".repeat(300), "").len(), 200);
    }
}
