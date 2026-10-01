//! The model and router swaps of the stack bar (F12a), the Rust twin of
//! swap.go. A model swap on claude lands at the next idle prompt, on
//! headless sprig with the next turn; a router swap restarts the agent and
//! resumes its conversation with a new base URL in its env, since a running
//! process cannot change its own env.

use std::sync::Arc;
use std::time::Duration;

use serde_json::json;

use super::ready::QUEUED_NOTE;
use super::{role, Client, Server};
use crate::gojson;
use crate::layout;
use crate::proto::{self, Request, Response};
use crate::sys;

/// What a pane, a plugin and an unplaced peer get for agent.set_model and
/// for a model on a prompt.
pub const MODEL_REFUSAL: &str = "only the operator sets an agent's model.";

/// The same for agent.set_router and for a router on a resume.
pub const ROUTER_REFUSAL: &str = "only the operator sets an agent's router.";

/// What a model name that is not plain gets. The name is typed into
/// claude's input box, so it carries no space and no control character.
pub const MODEL_WORDS_REFUSAL: &str =
    "model must be a plain name: letters, digits and . _ : / - [ ], up to 64 characters.";

/// What a router word other than direct or gateway gets. Switchyard is a
/// mode of the gateway itself, so a pane picks the gateway and the
/// gateway's own setting names what routes behind it.
pub const ROUTER_WORDS_REFUSAL: &str = "router must be direct or gateway.";

/// The longest a router swap waits for the old process to go.
const STOP_WAIT: Duration = Duration::from_secs(10);

/// `[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,63}`
pub fn valid_model_name(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 64 || !b[0].is_ascii_alphanumeric() {
        return false;
    }
    b.iter()
        .all(|c| c.is_ascii_alphanumeric() || b"._:/[]-".contains(c))
}

pub fn valid_router_word(w: &str) -> bool {
    w == "direct" || w == "gateway"
}

/// The words for a swap the pane's harness has no way to do.
pub fn swap_refusal(what: &str, label: &str, harness: &str) -> String {
    format!("Swapping the {what} of {label} is not built for {harness} yet.")
}

/// A harness name for a sentence.
pub fn harness_word(h: &str) -> String {
    if h.is_empty() {
        "a pane with no harness".into()
    } else {
        h.into()
    }
}

/// For each harness a router swap is built for: the env key that holds its
/// base URL and the vendor address it takes for direct.
fn router_key(harness: &str) -> Option<(&'static str, &'static str)> {
    match harness {
        "claude" => Some(("ANTHROPIC_BASE_URL", "https://api.anthropic.com")),
        _ => None,
    }
}

impl Server {
    /// The env entry a router swap sets for a harness. Direct is the
    /// vendor's own address, never a missing key: a pane's process also
    /// inherits the server's env, so a missing key could still route
    /// through the gateway.
    pub(super) fn router_env(
        &self,
        harness: &str,
        router: &str,
    ) -> Result<(String, String), String> {
        let Some((key, vendor)) = router_key(harness) else {
            return Err(format!(
                "Swapping the router is not built for {} yet.",
                harness_word(harness)
            ));
        };
        if router == "direct" {
            return Ok((key.into(), vendor.into()));
        }
        let (gateway, _) = self.gateway_facts();
        if gateway.is_empty() {
            return Err(
                "no gateway is set. Add gateway = \"http://127.0.0.1:8787\" to coppice.toml."
                    .into(),
            );
        }
        Ok((key.into(), gateway))
    }

    /// True when pane.resume would resume rec's conversation rather than
    /// start fresh: the record has a harness session id, and a pty pane's
    /// harness has resume_args.
    fn can_resume(&self, rec: &layout::Pane) -> bool {
        if rec.harness_session_id.is_empty() {
            return false;
        }
        match rec.kind.as_str() {
            layout::KIND_HEADLESS => true,
            layout::KIND_PTY => super::ended::resume_argv_for(
                &rec.harness,
                &rec.harness_session_id,
                &rec.extra_args,
            )
            .is_some(),
            _ => false,
        }
    }

    /// agent.set_model {pane, model}. On claude it queues /model NAME
    /// through the ready queue, so it lands only at an idle prompt. On a
    /// headless sprig pane the turns after it run on NAME. Any other
    /// harness is refused with words.
    pub(super) fn handle_set_model(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let model = r.s("model");
        if !valid_model_name(&model) {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, MODEL_WORDS_REFUSAL);
        }
        let lp = match self.live_pane(r) {
            Ok(lp) => lp,
            Err(resp) => return resp,
        };
        let rec = self.tree.pane(&id).unwrap_or_default();
        let label = self.label_of(&id);
        if rec.harness == "claude" && lp.pty.is_some() {
            let text = format!("/model {model}");
            // With no verdict from the screen reader, nothing can say the
            // prompt is idle, so the line waits in the queue as it does for
            // a busy screen. It is never typed at once (F12-R-9).
            let from = if c.role().pane {
                c.role().pane_id
            } else {
                String::new()
            };
            return match self.submit_prompt(&id, &text, &from, Some(c)) {
                Err(e) => self.prompt_err(r, e),
                Ok(out) if out.sent => proto::ok_resp(
                    &r.id,
                    gojson::map(vec![
                        ("pane", json!(id)),
                        ("model", json!(model)),
                        ("when", json!("now")),
                    ]),
                ),
                Ok(out) => proto::ok_resp(
                    &r.id,
                    gojson::map(vec![
                        ("pane", json!(id)),
                        ("model", json!(model)),
                        ("when", json!("queued")),
                        ("queued", json!(out.queued)),
                        ("note", json!(QUEUED_NOTE.replacen("{}", &label, 1))),
                    ]),
                ),
            };
        }
        if rec.harness == "sprig" && lp.adapter.as_ref().is_some_and(|a| a.set_model(&model)) {
            // The record keeps the choice, so a resume or a restart of this
            // pane runs its turns on the same model.
            let _ = self.update_pane(&id, |p| p.model = model.clone());
            return proto::ok_resp(
                &r.id,
                gojson::map(vec![
                    ("pane", json!(id)),
                    ("model", json!(model)),
                    ("when", json!("next turn")),
                ]),
            );
        }
        proto::err_resp(
            &r.id,
            proto::ERR_BAD_REQUEST,
            &swap_refusal("model", &label, &harness_word(&rec.harness)),
        )
    }

    /// agent.set_router {pane, router}: stops the agent, keeps its record
    /// as ended, and resumes the conversation in a new pane whose env names
    /// the new base URL. It refuses an agent that has no conversation to
    /// resume, because a restart would lose it. The agent comes back with a
    /// new pane id and enforcing: a watching marker goes with the old pane.
    pub(super) fn handle_set_router(self: &Arc<Server>, c: &Arc<Client>, r: &Request) -> Response {
        let id = r.s("pane");
        if c.view_only_on(&id) {
            return role::refuse_view_only(r, &id);
        }
        let router = r.s("router");
        if !valid_router_word(&router) {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, ROUTER_WORDS_REFUSAL);
        }
        if self.is_closed() {
            return proto::err_resp(
                &r.id,
                proto::ERR_SERVER_CLOSED,
                "this server has closed. It cannot start a new pane.",
            );
        }
        let Some(rec) = self.tree.pane(&id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_NO_SUCH_PANE,
                &format!("no pane {}. Run: coppice agent list", sys::go_quote(&id)),
            );
        };
        let label = self.label_of(&id);
        if rec.closed {
            return proto::err_resp(
                &r.id,
                proto::ERR_PANE_CLOSED,
                &format!("{label} has ended."),
            );
        }
        if router_key(&rec.harness).is_none() {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &swap_refusal("router", &label, &harness_word(&rec.harness)),
            );
        }
        if let Err(e) = self.router_env(&rec.harness, &router) {
            return proto::err_resp(&r.id, proto::ERR_BAD_REQUEST, &e);
        }
        // The agent would come back with a base URL that fails every call,
        // so a gateway that does not answer is refused before anything
        // stops. A gateway off this computer is not dialed, so it is not
        // judged here.
        if router == "gateway" {
            let (gateway, _) = self.gateway_facts();
            if self.gateway_answers(&gateway) == Some(false) {
                return proto::err_resp(
                    &r.id,
                    proto::ERR_BAD_REQUEST,
                    &format!("the gateway at {gateway} does not answer. Start it, then swap."),
                );
            }
        }
        if !self.can_resume(&rec) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("{label} has no conversation to resume yet, so a restart would lose it. Wait for its first reply, then swap."),
            );
        }
        if !self.claim_resume(&id) {
            return proto::err_resp(
                &r.id,
                proto::ERR_BAD_REQUEST,
                &format!("pane {id} is already resuming."),
            );
        }
        let resp = self.swap_router_claimed(r, &id, &label, &router);
        self.release_resume(&id);
        resp
    }
    fn swap_router_claimed(
        self: &Arc<Server>,
        r: &Request,
        id: &str,
        label: &str,
        router: &str,
    ) -> Response {
        let Some(lp) = self.live(id) else {
            return proto::err_resp(
                &r.id,
                proto::ERR_PANE_CLOSED,
                &format!("{label} has ended."),
            );
        };
        if let Some(p) = &lp.pty {
            p.close();
        }
        if let Some(a) = &lp.adapter {
            a.stop();
        }
        if !lp.wait_gone(STOP_WAIT) {
            return proto::err_resp(
                &r.id,
                proto::ERR_INTERNAL,
                &format!("{label} did not stop in time. Try again."),
            );
        }
        // Keep the record, as a process that ends on its own does, so the
        // resume reads it.
        let _ = self.close_pane(id, None);
        let first = self.resume_claimed(r, id, None, None, router, false);
        if first.ok {
            return first;
        }
        // The old process is gone, so the swap must not leave the agent
        // dead without a word: try one plain resume with the old env.
        let back = self.resume_claimed(r, id, None, None, "", false);
        router_swap_failed(&r.id, label, id, &first, &back)
    }
}

/// The reply for a router swap whose new start failed. `back` is the plain
/// resume that followed: when it worked the agent runs again on its old
/// router, and when it failed too the agent is in the ended list.
fn router_swap_failed(
    req_id: &str,
    label: &str,
    id: &str,
    first: &Response,
    back: &Response,
) -> Response {
    if back.ok {
        let new_id = back
            .result
            .as_ref()
            .and_then(|v| v.get("pane"))
            .and_then(|v| v.as_str())
            .unwrap_or("");
        let why = first.error.as_ref().map(|e| e.1.trim()).unwrap_or("");
        let why = if why.is_empty() || why.ends_with('.') {
            why.to_string()
        } else {
            format!("{why}.")
        };
        return proto::err_resp(
            req_id,
            proto::ERR_SPAWN_FAILED,
            &format!(
                "The router of {label} did not change: {why} The agent runs again on its old router, as pane {new_id}."
            ),
        );
    }
    proto::err_resp(
        req_id,
        proto::ERR_SPAWN_FAILED,
        &format!(
            "{label} was stopped and its router did not change. It is in the ended list. To start it again, run: coppice pane resume {id}."
        ),
    )
}

/// A pane stamp at the start of a URL path: /pane/<16 hex>/w<n>:p<n>.
fn stamp_len(path: &str) -> usize {
    let Some(rest) = path.strip_prefix("/pane/") else {
        return 0;
    };
    let Some((floor, tail)) = rest.split_once('/') else {
        return 0;
    };
    if floor.len() != 16
        || !floor
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
    {
        return 0;
    }
    let end = tail.find('/').unwrap_or(tail.len());
    if !super::mode::marker_pane_id(&tail[..end]) {
        return 0;
    }
    "/pane/".len() + floor.len() + 1 + end
}

/// Puts the pane prefix /pane/<floor>/<pane> at the start of a base URL's
/// path, ahead of any path it has (a /v1), and takes off a stamp it already
/// carries. The gateway strips the prefix and stamps its turn record with
/// the pane and the floor, so the floor can say which pane a saving
/// belongs to. A URL it cannot read comes back as it was.
pub fn stamp_url(raw: &str, floor: &str, id: &str) -> String {
    let Some(i) = raw.find("://") else {
        return raw.into();
    };
    let (scheme, after) = raw.split_at(i + 3);
    let (auth, path) = match after.find('/') {
        Some(j) => after.split_at(j),
        None => (after, ""),
    };
    if auth.is_empty() || auth.contains(['?', '#']) || path.contains(['?', '#']) {
        return raw.into();
    }
    let rest = &path[stamp_len(path)..];
    format!(
        "{scheme}{auth}/pane/{floor}/{id}{}",
        rest.trim_end_matches('/')
    )
}

impl Server {
    /// The env a pane's process starts with: the record's own, with each
    /// base URL that the record's own env sets to the gateway carrying the
    /// pane prefix. A base URL the process only inherits from the server is
    /// left alone, and the record keeps the plain URL, so a resume or a
    /// restart stamps the new pane id.
    pub(super) fn stamped_env(
        &self,
        rec: &layout::Pane,
    ) -> std::collections::BTreeMap<String, String> {
        let mut out = rec.env.clone();
        if out.is_empty() {
            return out;
        }
        let (gateway, _) = self.gateway_facts();
        if gateway.is_empty() {
            return out;
        }
        for k in ["ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"] {
            let v = rec.env.get(k).map(|v| v.trim()).unwrap_or("");
            if v.is_empty()
                || !super::facts::same_endpoint(v, &gateway)
                || !self.gateway_stamps(&gateway)
            {
                continue;
            }
            out.insert(k.into(), stamp_url(v, &self.floor_key(), &rec.id));
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::{router_swap_failed, stamp_url, valid_model_name, valid_router_word};
    use crate::proto;
    use serde_json::json;

    #[test]
    fn a_failed_router_swap_says_the_router_did_not_change() {
        let first = proto::err_resp(
            "1",
            proto::ERR_SPAWN_FAILED,
            "the new router would not start",
        );
        // The retry on the old env worked: an error that names the cause.
        let back = proto::ok_resp("1", json!({"pane": "w1:p3"}));
        let r = router_swap_failed("1", "auth", "w1:p1", &first, &back);
        let (code, msg) = r.error.clone().unwrap();
        assert!(!r.ok && code == proto::ERR_SPAWN_FAILED);
        assert_eq!(
            msg,
            "The router of auth did not change: the new router would not start. \
             The agent runs again on its old router, as pane w1:p3."
        );
        // The retry failed too: the agent is in the ended list.
        let back = proto::err_resp("1", proto::ERR_SPAWN_FAILED, "no start");
        let r = router_swap_failed("1", "auth", "w1:p1", &first, &back);
        assert_eq!(
            r.error.unwrap().1,
            "auth was stopped and its router did not change. It is in the ended list. \
             To start it again, run: coppice pane resume w1:p1."
        );
    }

    /// A loopback server that answers every request with `reply` and counts
    /// the asks. It answers its base URL.
    fn caps_gateway(
        reply: &'static str,
    ) -> (String, std::sync::Arc<std::sync::atomic::AtomicUsize>) {
        use std::io::{Read, Write};
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}", l.local_addr().unwrap());
        let asks = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let n = asks.clone();
        std::thread::spawn(move || {
            for c in l.incoming() {
                let Ok(mut c) = c else { return };
                let mut b = [0u8; 1024];
                let got = c.read(&mut b).unwrap_or(0);
                if String::from_utf8_lossy(&b[..got]).starts_with("GET /_caps ") {
                    n.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                }
                let _ = c.write_all(reply.as_bytes());
            }
        });
        (url, asks)
    }

    fn test_server(name: &str) -> std::sync::Arc<crate::server::Server> {
        let dir =
            std::env::temp_dir().join(format!("coppice-rs-caps-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        crate::server::Server::new(crate::server::Config {
            socket_path: dir.join("s.sock"),
            data_dir: dir.clone(),
            start_dir: dir,
            voice: Default::default(),
            foreman: Default::default(),
        })
    }

    #[test]
    fn a_pane_is_stamped_only_when_the_gateway_says_it_takes_the_prefix() {
        const YES: &str = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 21\r\nConnection: close\r\n\r\n{\"pane_prefix\": true}";
        const CHUNKED: &str = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n15\r\n{\"pane_prefix\": true}\r\n0\r\n\r\n";
        let s = test_server("yes");
        for reply in [YES, CHUNKED] {
            let (gw, asks) = caps_gateway(reply);
            // A yes is asked once per address, however many panes start.
            assert!(s.gateway_stamps(&gw) && s.gateway_stamps(&gw) && s.gateway_stamps(&gw));
            assert_eq!(asks.load(std::sync::atomic::Ordering::SeqCst), 1);
        }
        for (name, reply) in [
            (
                "old gateway",
                "HTTP/1.1 404 Not Found\r\nContent-Length: 2\r\n\r\n{}",
            ),
            (
                "says no",
                "HTTP/1.1 200 OK\r\nContent-Length: 21\r\n\r\n{\"pane_prefix\":false}",
            ),
            (
                "says nothing",
                "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}",
            ),
            ("not JSON", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"),
            (
                "refusal",
                "HTTP/1.1 403 Forbidden\r\nContent-Length: 20\r\n\r\n{\"error\":\"loopback\"}",
            ),
            ("no HTTP", "garbage"),
        ] {
            let (gw, asks) = caps_gateway(reply);
            assert!(!s.gateway_stamps(&gw), "{name}");
            // The no is kept for a while, so the next start does not ask.
            assert!(!s.gateway_stamps(&gw), "{name}");
            assert_eq!(asks.load(std::sync::atomic::Ordering::SeqCst), 1, "{name}");
        }
        // A gateway that does not answer, and one that is not loopback.
        assert!(!s.gateway_stamps("http://127.0.0.1:1"));
        assert!(!s.gateway_stamps("http://192.0.2.1:8787"));
    }

    #[test]
    fn model_names_are_plain() {
        for ok in ["opus", "claude-opus-4-1", "o3", "sonnet[1m]", "a.b_c:d/e-f"] {
            assert!(valid_model_name(ok), "{ok}");
        }
        let long = "a".repeat(65);
        for bad in [
            "",
            "two words",
            "a\nb",
            "-rf",
            "a;b",
            "ñ",
            "[x]",
            long.as_str(),
        ] {
            assert!(!valid_model_name(bad), "{bad:?}");
        }
        assert!(valid_model_name(&"a".repeat(64)));
    }

    #[test]
    fn router_words_are_direct_and_gateway() {
        assert!(valid_router_word("direct") && valid_router_word("gateway"));
        assert!(
            !valid_router_word("switchyard")
                && !valid_router_word("")
                && !valid_router_word("Direct")
        );
    }

    #[test]
    fn stamp_url_puts_the_prefix_ahead_of_the_path_and_replaces_old_stamps() {
        let floor = "0123456789abcdef";
        let at = |tail: &str| format!("http://127.0.0.1:8787/pane/{floor}/w1:p2{tail}");
        for (input, want) in [
            ("http://127.0.0.1:8787", at("")),
            ("http://127.0.0.1:8787/", at("")),
            ("http://127.0.0.1:8787/v1", at("/v1")),
            ("http://127.0.0.1:8787/pane/fedcba9876543210/w9:p9", at("")),
            (
                "http://127.0.0.1:8787/pane/fedcba9876543210/w9:p9/v1",
                at("/v1"),
            ),
            ("not a url", "not a url".to_string()),
        ] {
            assert_eq!(stamp_url(input, floor, "w1:p2"), want, "{input}");
        }
    }
}
