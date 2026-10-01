use super::*;

fn tiny() -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures/vla/tiny.onnx")
}

#[test]
fn version_is_the_pin() {
    assert_eq!(version(), "1.23.2");
}

#[test]
fn run_tiny() {
    let mut s = Session::open(&tiny(), 1).unwrap();
    let mut y = [0f32; 6];
    s.run(
        &[
            Input {
                name: "x",
                data: Data::F32(&[0.0, 1.0, 2.0, 3.0, 4.0, 5.0]),
                shape: &[2, 3],
            },
            Input {
                name: "k",
                data: Data::I64(&[10, 20, 30, 40, 50, 60]),
                shape: &[2, 3],
            },
        ],
        &mut [Output {
            name: "y",
            data: &mut y,
        }],
    )
    .unwrap();
    assert_eq!(y, [10.0, 22.0, 34.0, 46.0, 58.0, 70.0]);
}

#[test]
fn errors_are_errors() {
    let err = Session::open(std::path::Path::new("no/such/model.onnx"), 1)
        .err()
        .unwrap();
    assert!(err.contains("could not load"), "{err}");
    let mut s = Session::open(&tiny(), 1).unwrap();
    let x = [0f32; 6];
    let k = [0i64; 6];
    let xi = || Input {
        name: "x",
        data: Data::F32(&x),
        shape: &[2, 3],
    };
    let ki = || Input {
        name: "k",
        data: Data::I64(&k),
        shape: &[2, 3],
    };
    let mut y6 = [0f32; 6];
    let mut y5 = [0f32; 5];
    assert!(s
        .run(
            &[xi(), ki()],
            &mut [Output {
                name: "y",
                data: &mut y5
            }]
        )
        .is_err());
    assert!(s
        .run(
            &[xi()],
            &mut [Output {
                name: "y",
                data: &mut y6
            }]
        )
        .is_err());
    assert!(s
        .run(
            &[xi(), ki()],
            &mut [Output {
                name: "z",
                data: &mut y6
            }]
        )
        .is_err());
    let wrong = Input {
        name: "x",
        data: Data::F32(&x),
        shape: &[3, 2],
    };
    assert!(s
        .run(
            &[wrong, ki()],
            &mut [Output {
                name: "y",
                data: &mut y6
            }]
        )
        .is_err());
    let short = Input {
        name: "x",
        data: Data::F32(&x[..5]),
        shape: &[2, 3],
    };
    assert!(s
        .run(
            &[short, ki()],
            &mut [Output {
                name: "y",
                data: &mut y6
            }]
        )
        .is_err());
}
