//! Tests of the classifier and the chain. The twin of Go's failover_test.go.

use super::failover::*;

/// 2026-10-21T12:00:00Z
const T0: i64 = 1_792_584_000;

fn chain3() -> Chain {
    Chain::new(&["".into(), "desk".into(), "grok".into()])
}

#[test]
fn classify_codes() {
    let cases = [
        ("sprig ended with an error: anthropic api 429: slow down", Outcome::Out),
        ("sprig ended with an error: anthropic api 529: overloaded", Outcome::Out),
        ("sprig ended with an error: openai api 503: unavailable", Outcome::Out),
        ("sprig ended with an error: openai api 400: bad request", Outcome::NotOut),
        ("sprig ended with an error: anthropic api 401: invalid x-api-key", Outcome::KeyProblem),
        ("sprig ended with an error: openai api 403: forbidden", Outcome::KeyProblem),
        ("sprig ended with an error: openai api 500: boom", Outcome::NotOut),
        ("the model said no", Outcome::NotOut),
        ("a tool failed: exit 2", Outcome::NotOut),
        ("claude gave an answer that is not a result", Outcome::NotOut),
        ("my id is 14290 and 5030 items", Outcome::NotOut),
    ];
    for (text, want) in cases {
        assert_eq!(classify(text, T0).outcome, want, "{text}");
    }
}

#[test]
fn classify_each_quota_text() {
    for text in OUT_TEXTS {
        let input = format!("sprig ended with an error: openai api 400: {text}");
        assert_eq!(classify(&input, T0).outcome, Outcome::Out, "text {text}");
        assert_eq!(
            classify(&input.to_uppercase(), T0).outcome,
            Outcome::Out,
            "text {text} upper"
        );
    }
    assert!(OUT_TEXTS.len() >= 8);
}

#[test]
fn classify_connection_faults() {
    for text in [
        "sprig ended with an error: Post \"http://100.64.0.2:8080/v1/chat\": dial tcp 100.64.0.2:8080: connect: connection refused",
        "sprig ended with an error: dial tcp: lookup desk.tail.ts.net: no such host",
        "sprig ended with an error: dial tcp 100.64.0.2:8080: i/o timeout",
        "sprig ended with an error: dial tcp 100.64.0.2:8080: connect: no route to host",
        "sprig ended with an error: dial tcp 100.64.0.2:8080: connect: network is unreachable",
        "openai api: error sending request for url (http://desk/v1): tcp connect error: Connection refused (os error 111)",
    ] {
        assert_eq!(classify(text, T0).outcome, Outcome::Out, "{text}");
    }
    assert_eq!(
        classify("the foreman's turn timed out", T0).outcome,
        Outcome::NotOut
    );
}

#[test]
fn a_key_problem_is_not_hidden_behind_a_quota_text() {
    let v = classify("anthropic api 401: invalid key. usage limit reached", T0);
    assert_eq!(v.outcome, Outcome::KeyProblem);
    assert!(v.reason.contains("401"));
}

#[test]
fn classify_reads_the_reset_time() {
    let cases: [(&str, &str, Option<i64>); 11] = [
        ("retry-after seconds", "anthropic api 429: x\nretry-after: 90", Some(T0 + 90)),
        ("retry-after date", "api 429: x\nRetry-After: Wed, 21 Oct 2026 12:30:00 GMT", Some(T0 + 1800)),
        ("anthropic reset", "api 429: x\nanthropic-ratelimit-requests-reset: 2026-10-21T14:05:00Z", Some(T0 + 2 * 3600 + 5 * 60)),
        ("openai reset duration", "api 429: x\nx-ratelimit-reset-requests: 6m0s", Some(T0 + 360)),
        ("openai reset seconds", "api 429: x\nx-ratelimit-reset-tokens: 20", Some(T0 + 20)),
        ("claude epoch", "Claude AI usage limit reached|1792590000", Some(1_792_590_000)),
        ("retry-after wins", "api 429\nretry-after: 10\nanthropic-ratelimit-tokens-reset: 2026-10-21T20:00:00Z", Some(T0 + 10)),
        ("earliest of the resets", "api 429\nanthropic-ratelimit-tokens-reset: 2026-10-21T20:00:00Z\nx-ratelimit-reset-requests: 1m", Some(T0 + 60)),
        ("none", "api 429: slow down", None),
        ("past reset is ignored", "api 429\nanthropic-ratelimit-tokens-reset: 2026-10-21T11:00:00Z", None),
        ("capped at 7 days", "api 429\nretry-after: 99999999", Some(T0 + MAX_NAMED_RESET)),
    ];
    for (name, text, want) in cases {
        let v = classify(text, T0);
        assert_eq!(v.outcome, Outcome::Out, "{name}");
        assert_eq!(v.reset, want, "{name}");
    }
}

fn out_v() -> Verdict {
    Verdict {
        outcome: Outcome::Out,
        reason: "HTTP 429".into(),
        reset: None,
    }
}

#[test]
fn cool_down_defaults_to_fifteen_minutes_and_doubles() {
    let c = chain3();
    let want = [900, 1800, 3600, 7200, 7200];
    let mut now = T0;
    for (i, d) in want.iter().enumerate() {
        let until = c.mark_out(0, &out_v(), now);
        assert_eq!(until - now, *d, "repeat {}", i + 1);
        now = until;
    }
}

#[test]
fn cool_down_ends_at_the_named_reset_and_success_resets_the_doubling() {
    let c = chain3();
    let reset = T0 + 420;
    let v = Verdict {
        reset: Some(reset),
        ..out_v()
    };
    assert_eq!(c.mark_out(0, &v, T0), reset);
    c.mark_out(0, &out_v(), reset);
    c.mark_ok(0);
    assert_eq!(c.mark_out(0, &out_v(), reset) - reset, 900);
}

#[test]
fn chain_order_skips_cooling_profiles_and_returns_to_the_first() {
    let c = chain3();
    assert_eq!(c.first(T0), Some(0));
    let until = c.mark_out(0, &out_v(), T0);
    assert_eq!(c.first(T0 + 60), Some(1));
    c.mark_out(1, &out_v(), T0);
    assert_eq!(c.first(T0 + 60), Some(2));
    assert_eq!(c.first(until), Some(0));
}

#[test]
fn when_every_profile_cools_the_earliest_end_is_known() {
    let c = chain3();
    for (i, d) in [(0, 2400), (1, 600), (2, 1200)] {
        let v = Verdict {
            reset: Some(T0 + d),
            ..out_v()
        };
        c.mark_out(i, &v, T0);
    }
    assert_eq!(c.first(T0), None);
    assert_eq!(c.earliest(T0), Some(T0 + 600));
}

#[test]
fn status_states_and_the_file() {
    let dir = std::env::temp_dir().join(format!("fo-status-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let c = chain3();
    c.mark_out(0, &out_v(), T0);
    c.mark_failing(1, "HTTP 401: a key problem");
    c.mark_ran(2);
    c.save(&dir, T0).unwrap();
    let mode = {
        use std::os::unix::fs::PermissionsExt;
        std::fs::metadata(dir.join(FAILOVER_FILE))
            .unwrap()
            .permissions()
            .mode()
            & 0o777
    };
    assert_eq!(mode, 0o600);
    let got = Chain::load(&dir, &["".into(), "desk".into(), "grok".into()], T0);
    let rows = got.status(T0 + 60);
    assert_eq!(rows[0].state, "cooling");
    assert_eq!(rows[0].until, "2026-10-21T12:15:00Z");
    assert_eq!(rows[0].name, "Claude");
    assert_eq!(rows[0].reason, "HTTP 429");
    assert_eq!(rows[1].state, "failing");
    assert_eq!(rows[1].reason, "HTTP 401");
    assert_eq!(rows[2].state, "ready");
    assert_eq!(got.last_ran(), Some(2));
    assert_eq!(got.status(T0 + 960)[0].state, "ready");
    // A chain that changed in the config starts clean.
    let other = Chain::load(&dir, &["".into(), "other".into()], T0);
    assert_eq!(other.status(T0)[0].state, "ready");
    assert_eq!(other.last_ran(), None);
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn status_json_has_a_fixed_shape() {
    let c = Chain::new(&["".into(), "desk".into()]);
    c.mark_out(0, &out_v(), T0);
    c.mark_ran(1);
    assert_eq!(
        c.status_json(T0),
        "{\"chain\":[{\"profile\":\"\",\"name\":\"Claude\",\"state\":\"cooling\",\"until\":\"2026-10-21T12:15:00Z\",\"reason\":\"HTTP 429\"},{\"profile\":\"desk\",\"name\":\"desk\",\"state\":\"ready\"}],\"last_ran\":\"desk\"}\n"
    );
}

fn write_state(dir: &std::path::Path, until0: &str, until1: &str) {
    let doc = format!(
        "{{\"chain\":[{{\"profile\":\"\",\"name\":\"Claude\",\"state\":\"cooling\",\"until\":\"{until0}\",\"reason\":\"HTTP 429\",\"repeats\":9}},{{\"profile\":\"desk\",\"name\":\"desk\",\"state\":\"cooling\",\"until\":\"{until1}\",\"reason\":\"HTTP 429\"}}]}}\n"
    );
    std::fs::write(dir.join(FAILOVER_FILE), doc).unwrap();
}

fn tmp_dir(name: &str) -> std::path::PathBuf {
    let d = std::env::temp_dir().join(format!("fo-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

#[test]
fn load_drops_an_end_time_more_than_seven_days_ahead_and_says_so_once() {
    let dir = tmp_dir("far");
    write_state(&dir, "2031-01-01T00:00:00Z", "2026-10-21T12:20:00Z");
    let c = Chain::load(&dir, &["".into(), "desk".into()], T0);
    let rows = c.status(T0);
    assert_eq!(rows[0].state, "ready");
    assert_eq!(rows[1].state, "cooling");
    let note = c.load_note();
    assert!(note.contains("Claude") && !note.contains("desk") && note.contains("7 days"), "{note}");
    let until = c.mark_out(0, &Verdict { outcome: Outcome::Out, reason: "x".into(), reset: None }, T0);
    assert_eq!(until - T0, COOL_START);
}

#[test]
fn load_drops_an_unparsable_end_time_and_keeps_a_past_one_as_ready() {
    let dir = tmp_dir("bad");
    write_state(&dir, "next tuesday", "2020-01-01T00:00:00Z");
    let c = Chain::load(&dir, &["".into(), "desk".into()], T0);
    assert!(c.status(T0).iter().all(|r| r.state == "ready"));
    let n = c.load_note();
    assert!(n.contains("Claude") && !n.contains("desk"), "{n}");
}

#[test]
fn load_keeps_an_end_time_inside_seven_days() {
    let dir = tmp_dir("near");
    write_state(&dir, &super::failover::tests_date(T0 + MAX_NAMED_RESET), "2026-10-21T12:20:00Z");
    let c = Chain::load(&dir, &["".into(), "desk".into()], T0);
    assert_eq!(c.load_note(), "");
    assert_eq!(c.status(T0)[0].state, "cooling");
}

#[test]
fn safe_reason_keeps_only_fixed_words_whatever_the_key_looks_like() {
    let hostile = [
        "openai api 400: bad key sk-ant-api03-AbCdEf0123456789xyzXYZ",
        "b64 QUJD/REVG+RzEy/MzQ1+Njc4/OWFi+Y2Rl/ZmdoaWprbG1ub3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OQ==",
        r#"openai api 400: {"key":"sk-ab\/cd\u0045fGh1234567890"}"#,
        "openai api 400: bad key sk-abcdefgh\nijklmnop12345678",
        r#"openai api 400: {"api_key": "shortkey1"} x-api-key: abc123 authorization: Basic Zm9v token=tok123"#,
        "Bearer abcDEF123.tok-en_value",
    ];
    for input in hostile {
        let got = safe_reason(input);
        assert!(got == "HTTP 400" || got == "the turn failed", "{input} -> {got}");
        assert!(!got.contains(['/', '+', '\\']) && got.len() <= 20, "{input} -> {got}");
    }
    for input in ["HTTP 429", "cannot connect", "usage limit reached", "HTTP 401: the key was refused. That is a key problem, not a quota"] {
        assert_eq!(safe_reason(input), input);
    }
}

#[test]
fn a_failing_reason_is_masked_and_short_in_state_and_status() {
    let c = Chain::new(&["".into(), "desk".into()]);
    c.mark_failing(0, &format!("openai api 400: bad key sk-proj-0123456789abcdefABCDEF {}", "x ".repeat(200)));
    let row = &c.status(T0)[0];
    assert!(!row.reason.contains("sk-proj") && row.reason.len() <= 130 && row.state == "failing", "{row:?}");
    let dir = tmp_dir("mask");
    c.save(&dir, T0).unwrap();
    let data = std::fs::read_to_string(dir.join(FAILOVER_FILE)).unwrap();
    assert!(!data.contains("sk-proj"), "{data}");
}

#[test]
fn classify_reads_the_whole_text_not_the_last_line() {
    let body = "openai api 429: {\n  \"error\": {\n    \"type\": \"insufficient_quota\"\n  }\n}";
    assert_eq!(classify(body, T0).outcome, Outcome::Out);
    let long = format!("anthropic api 429: x\n{}", "filler line\n".repeat(600));
    assert_eq!(classify(&long, T0).outcome, Outcome::NotOut);
    let v = classify("anthropic api 429: {\n}\nretry-after: 1200\n}", T0);
    assert_eq!(v.reset, Some(T0 + 1200));
}

#[test]
fn two_saves_use_two_temp_names() {
    let dir = tmp_dir("seq");
    let c = Chain::new(&["".into()]);
    c.save(&dir, T0).unwrap();
    c.save(&dir, T0).unwrap();
    let left: Vec<_> = std::fs::read_dir(&dir).unwrap().filter_map(|e| e.ok()).collect();
    assert_eq!(left.len(), 1);
}
