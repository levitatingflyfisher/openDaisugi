use super::tests::model_dir;
use super::*;

#[test]
fn chunk_matches_the_oracle() {
    let Some(dir) = model_dir() else { return };
    let b = std::fs::read(
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/vla/chunk.json"),
    )
    .unwrap();
    let c: serde_json::Value = serde_json::from_slice(&b).unwrap();
    let (h, w) = (
        c["image"][0].as_u64().unwrap() as usize,
        c["image"][1].as_u64().unwrap() as usize,
    );
    let state: Vec<f64> = c["state"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_f64().unwrap())
        .collect();
    let mut p = Policy::open(&dir, 4).unwrap();
    let got = p
        .chunk(
            &super::tests::test_image(h, w),
            h,
            w,
            c["task"].as_str().unwrap(),
            &state,
            &noise(c["seed"].as_u64().unwrap()),
            6,
        )
        .unwrap();
    let mut worst = 0f64;
    for (r, row) in c["actions"].as_array().unwrap().iter().enumerate() {
        for (i, want) in row.as_array().unwrap().iter().enumerate() {
            worst = worst.max((got[r][i] - want.as_f64().unwrap()).abs());
        }
    }
    eprintln!("largest difference from the oracle: {worst:.3e}");
    assert!(worst <= CHUNK_TOLERANCE, "largest difference {worst:e}");
}

#[test]
fn open_checks_the_pins() {
    let dir = std::env::temp_dir().join(format!("smolvla-pins-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    assert!(Policy::open(&dir, 1).is_err());
    for (name, _) in PINS {
        std::fs::write(dir.join(name), b"x").unwrap();
    }
    assert!(Policy::open(&dir, 1).is_err());
    std::fs::remove_dir_all(&dir).unwrap();
}
