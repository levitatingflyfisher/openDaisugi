use super::tokenizer::pre_tokenize;
use super::*;

/// The directory of the pinned graphs and assets, from DAISUGI_SMOLVLA_DIR.
/// Tests that need it pass with a note when it is not set.
pub(super) fn model_dir() -> Option<std::path::PathBuf> {
    match std::env::var("DAISUGI_SMOLVLA_DIR") {
        Ok(d) if !d.is_empty() => Some(d.into()),
        _ => {
            eprintln!(
                "DAISUGI_SMOLVLA_DIR is not set (clients/vla_export.py --part assets fills it)"
            );
            None
        }
    }
}

#[test]
fn token_ids_match_the_oracle() {
    let Some(dir) = model_dir() else { return };
    let tok = Tokenizer::load(&dir.join("tokenizer.json")).unwrap();
    let b = std::fs::read(
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/vla/tokens.json"),
    )
    .unwrap();
    let v: serde_json::Value = serde_json::from_slice(&b).unwrap();
    assert_eq!(v["width"], WIDTH);
    let cases = v["cases"].as_array().unwrap();
    assert_eq!(cases.len(), 30);
    let ints = |x: &serde_json::Value| -> Vec<i64> {
        x.as_array()
            .unwrap()
            .iter()
            .map(|n| n.as_i64().unwrap())
            .collect()
    };
    for c in cases {
        let text = c["text"].as_str().unwrap();
        let (ids, mask) = tok.task(text).unwrap();
        assert_eq!(ids.to_vec(), ints(&c["ids"]), "{text:?}");
        assert_eq!(mask.to_vec(), ints(&c["mask"]), "{text:?}");
    }
}

#[test]
fn pre_tokenizer_splits() {
    let cases: &[(&str, &[&str])] = &[
        ("it's 12ab", &["it", "'s", " 12", "ab"]),
        ("a   b", &["a", "  ", " b"]),
        ("end  ", &["end", "  "]),
        ("x\t\ny", &["x", "\t", "\n", "y"]),
        ("!! ok ??", &["!!", " ok", " ??"]),
        ("'re 'x", &["'re", " '", "x"]),
        ("\u{e9}t\u{e9}", &["\u{e9}t\u{e9}"]),
    ];
    for (input, want) in cases {
        assert_eq!(pre_tokenize(input), *want, "{input:?}");
    }
}

#[test]
fn bad_tokenizer_files_are_errors() {
    for s in [
        "",
        "{}",
        r#"{"model": {"type": "WordPiece", "vocab": {}, "merges": []}}"#,
        r#"{"model": {"type": "BPE", "vocab": {"a": 0}, "merges": [["a"]]}}"#,
    ] {
        assert!(Tokenizer::parse(s.as_bytes()).is_err(), "{s:?}");
    }
}

fn process_cases() -> serde_json::Value {
    let b = std::fs::read(
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/vla/process.json"),
    )
    .unwrap();
    serde_json::from_slice(&b).unwrap()
}

/// vla_cases.test_image: a uint8 RGB pattern, top row first.
pub(super) fn test_image(h: usize, w: usize) -> Vec<u8> {
    let mut out = vec![0u8; h * w * 3];
    for y in 0..h {
        for x in 0..w {
            for c in 0..3 {
                out[(y * w + x) * 3 + c] = ((x * 7 + y * 13 + c * 29 + x * y) % 256) as u8;
            }
        }
    }
    out
}

fn floats(v: &serde_json::Value) -> Vec<f64> {
    v.as_array()
        .unwrap()
        .iter()
        .map(|n| n.as_f64().unwrap())
        .collect()
}

#[test]
fn image_matches_the_oracle() {
    let pc = process_cases();
    let samples: Vec<Vec<usize>> = pc["samples"]
        .as_array()
        .unwrap()
        .iter()
        .map(|s| {
            s.as_array()
                .unwrap()
                .iter()
                .map(|n| n.as_u64().unwrap() as usize)
                .collect()
        })
        .collect();
    for im in pc["images"].as_array().unwrap() {
        let (h, w) = (
            im["height"].as_u64().unwrap() as usize,
            im["width"].as_u64().unwrap() as usize,
        );
        let got = image(&test_image(h, w), h, w).unwrap();
        let sum: f64 = got.iter().map(|v| *v as f64).sum();
        assert!(
            (sum - im["sum"].as_f64().unwrap()).abs() < 1e-3,
            "{h}x{w}: sum {sum}"
        );
        for (s, want) in samples.iter().zip(floats(&im["samples"])) {
            let v = got[(s[0] * IMAGE_SIZE + s[1]) * IMAGE_SIZE + s[2]];
            assert!(
                (v as f64 - want).abs() < 1e-6,
                "{h}x{w} at {s:?}: {v}, want {want}"
            );
        }
    }
    assert!(image(&[0; 5], 2, 2).is_err());
}

#[test]
fn time_embedding_matches_the_oracle() {
    let pc = process_cases();
    let times = pc["times"].as_array().unwrap();
    assert_eq!(times.len(), STEPS);
    for (step, tc) in times.iter().enumerate() {
        let t = step_time(step);
        assert_eq!(t as f64, tc["time"].as_f64().unwrap(), "step {step}");
        for (i, (v, want)) in time_embedding(t)
            .iter()
            .zip(floats(&tc["embedding"]))
            .enumerate()
        {
            assert!(
                (*v as f64 - want).abs() < 1e-6,
                "step {step} [{i}]: {v}, want {want}"
            );
        }
    }
}

#[test]
fn noise_matches_the_oracle() {
    for nc in process_cases()["noise"].as_array().unwrap() {
        let n = noise(nc["seed"].as_u64().unwrap());
        let sum: f64 = n.iter().map(|v| *v as f64).sum();
        assert!((sum - nc["sum"].as_f64().unwrap()).abs() < 1e-3);
        for (i, want) in floats(&nc["head"]).iter().enumerate() {
            assert!(
                (n[i] as f64 - want).abs() < 1e-6,
                "[{i}]: {}, want {want}",
                n[i]
            );
        }
    }
}

/// A safetensors file with header h and n zero bytes of data.
fn raw(h: &str, n: usize) -> Vec<u8> {
    let mut out = (h.len() as u64).to_le_bytes().to_vec();
    out.extend_from_slice(h.as_bytes());
    out.extend(std::iter::repeat_n(0u8, n));
    out
}

fn safetensors(tensors: &[(&str, &[f32])]) -> Vec<u8> {
    let mut header = serde_json::Map::new();
    let mut data = Vec::new();
    for (name, v) in tensors {
        let start = data.len();
        for f in *v {
            data.extend_from_slice(&f.to_le_bytes());
        }
        header.insert(
            name.to_string(),
            serde_json::json!({"dtype": "F32", "shape": [v.len()], "data_offsets": [start, data.len()]}),
        );
    }
    let mut out = raw(&serde_json::Value::Object(header).to_string(), 0);
    out.extend(data);
    out
}

#[test]
fn stats_normalize_when_present_and_pass_through_when_missing() {
    let st = Stats::parse(&safetensors(&[
        ("observation.state.mean", &[1.0, 2.0]),
        ("observation.state.std", &[2.0, 4.0]),
        ("action.mean", &[10.0, 20.0]),
        ("action.std", &[0.5, 2.0]),
    ]))
    .unwrap();
    let s = st.state(&[3.0, 2.0]).unwrap();
    assert_eq!((s[0], s[1], s[2]), (2.0 / (2.0f32 + 1e-8), 0.0, 0.0));
    assert!(st.state(&[1.0, 2.0, 3.0]).is_err());
    let mut chunk = vec![0f32; CHUNK * ACTION_DIM];
    chunk[0] = 2.0;
    chunk[1] = 1.0;
    chunk[ACTION_DIM] = -2.0;
    let a = st.actions(&chunk, 2);
    assert_eq!(
        (a.len(), a[0][0], a[0][1], a[1][0]),
        (CHUNK, 11.0, 22.0, 9.0)
    );
    let none = Stats::parse(&safetensors(&[])).unwrap();
    let s = none.state(&[3.0, 2.0, 1.0]).unwrap();
    assert_eq!((s[0], s[2]), (3.0, 1.0));
    let a = none.actions(&chunk, 3);
    assert_eq!((a[0][0], a[0][2]), (2.0, 0.0));
}

#[test]
fn base_checkpoint_stats_are_identity() {
    let Some(dir) = model_dir() else { return };
    let st = Stats::load(&dir.join("stats.safetensors")).unwrap();
    let s = st.state(&[0.5, -0.25]).unwrap();
    assert_eq!((s[0], s[1]), (0.5, -0.25));
}

#[test]
fn bad_stats_files_are_errors() {
    let mut cut = raw("{}", 0);
    cut.truncate(9);
    for b in [
        vec![],
        vec![1, 0, 0, 0, 0, 0, 0, 0, b'{'],
        cut,
        raw(
            r#"{"a":{"dtype":"F64","shape":[1],"data_offsets":[0,8]}}"#,
            8,
        ),
        raw(
            r#"{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,8]}}"#,
            4,
        ),
        raw(
            r#"{"a":{"dtype":"F32","shape":[3],"data_offsets":[0,8]}}"#,
            8,
        ),
        raw(
            r#"{"a":{"dtype":"F32","shape":[2],"data_offsets":[4,0]}}"#,
            8,
        ),
    ] {
        assert!(Stats::parse(&b).is_err(), "{b:?}");
    }
}
