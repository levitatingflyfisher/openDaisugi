use super::*;
use crate::gate::pyjson::loads;
use crate::pathways::pmodel::{validate_model, Id, Mode};

fn fixture(name: &str) -> String {
    format!(
        "{}/../../tests/fixtures/mjcf/{name}",
        env!("CARGO_MANIFEST_DIR")
    )
}

/// One step, validated the way ActionPlan.steps does.
fn step(text: &str) -> Object {
    let raw = loads(&format!(
        r#"{{"source": "t", "task": "t", "steps": [{text}]}}"#
    ))
    .unwrap();
    let plan = validate_model(Id::ActionPlan, &raw, Mode::Python).unwrap();
    match plan.as_obj().unwrap().value("steps") {
        Value::List(xs) => xs[0].as_obj().unwrap().clone(),
        _ => unreachable!(),
    }
}

/// numpy's own answers for the damped least-squares update.
#[test]
fn damped_step_matches_numpy() {
    let cases: [(&[f64], [f64; 3], &[f64]); 2] = [
        (
            &[
                -0.35233447033367526,
                -0.6983016521509962,
                0.3018689460797075,
                -0.8551274266649145,
                0.0717640086133784,
                -0.2686221661748289,
            ],
            [
                -0.17680043009011728,
                0.0029742932757680918,
                -0.18500173662320607,
            ],
            &[0.23525914250335522, 0.13681062611887018],
        ),
        (
            &[
                -0.13270863267522826,
                -0.8602891528507621,
                -0.8185739733122699,
                -0.15096162171497207,
                0.6537042493440761,
                -0.7523960777007088,
                -0.5535220707859709,
                0.2548664448111786,
                0.8954178849140113,
            ],
            [
                0.03084117944699946,
                -0.04132781013968795,
                0.19050204223716805,
            ],
            &[
                -0.2599415834835072,
                -0.0525509041310724,
                0.06274993525161676,
            ],
        ),
    ];
    for (jacp, e, dq) in cases {
        assert_eq!(ik::damped_step(jacp, &e, 0.1 * 0.1), dq);
    }
}

#[test]
fn reprs() {
    assert_eq!(tuple_repr(&[0.35, 0.2, 0.0]), "(0.35, 0.2, 0.0)");
    assert_eq!(
        pairs_repr(&[
            ("tip".into(), "wall".into()),
            ("geom#0".into(), "it's".into())
        ]),
        r#"[('tip', 'wall'), ('geom#0', "it's")]"#
    );
    assert_eq!(Num(Value::Float(0.1)).repr(), "0.1");
    assert_eq!(Num(Value::Int("50".into())).repr(), "50");
}

fn arm(opt: Options) -> MuJoCo {
    MuJoCo::new(&fixture("two_joint_arm.xml"), opt).unwrap()
}

#[test]
fn missing_mjcf() {
    let e = MuJoCo::new(&fixture("absent.xml"), Options::default())
        .err()
        .unwrap();
    assert_eq!(e.ty, "ValueError");
    assert!(e.msg.contains("Error opening file"), "{}", e.msg);
}

#[test]
fn joint_move_and_reset() {
    let mut x = arm(Options::default());
    let r = x
        .run_step(&step(
            r#"{"type": "joint_move", "id": "m", "joint_targets": {"j1": 0.5}}"#,
        ))
        .unwrap();
    assert_eq!(r.rc, 0);
    assert!(
        r.stdout
            .starts_with("joint_move complete; positions={'j1': 0.49"),
        "{}",
        r.stdout
    );
    let r = x
        .run_step(&step(r#"{"type": "sim_reset", "id": "r", "seed": 3}"#))
        .unwrap();
    assert_eq!(r.stdout, "sim reset (seed=3)");
    assert_eq!(x.data().qpos()[0], 0.0);
}

#[test]
fn unknown_joint_is_key_error() {
    let mut x = arm(Options::default());
    let e = x
        .run_step(&step(
            r#"{"type": "joint_move", "id": "m", "joint_targets": {"nope": 0.5}}"#,
        ))
        .unwrap_err();
    assert_eq!(
        e,
        PyError::new("KeyError", r#""Joint 'nope' not found in MJCF""#)
    );
}

#[test]
fn gripper_and_torque() {
    let mut x = arm(Options {
        torque_limit: Some(Num::float(0.01)),
        ..Options::default()
    });
    let r = x
        .run_step(&step(
            r#"{"type": "gripper", "id": "g", "action": "close"}"#,
        ))
        .unwrap();
    assert_eq!(r.rc, RC_TORQUE_VIOLATION);
    assert!(r
        .stdout
        .starts_with("torque_limit violated during gripper g: peak |actuator_force|="));
    assert!(r.stdout.ends_with(" > limit=0.01"), "{}", r.stdout);
}

#[test]
fn cartesian_ik_failure() {
    let mut x = arm(Options::default());
    let r = x
        .run_step(&step(
            r#"{"type": "cartesian_move", "id": "c", "target_position": [2, 2, 0]}"#,
        ))
        .unwrap();
    assert_eq!(r.rc, RC_IK_FAILED);
    assert_eq!(
        r.stdout,
        "cartesian_move c IK failed: did not converge in 200 iters (residual=3.0752, tol=0.001)"
    );
}

#[test]
fn step_type_refused() {
    let mut x = arm(Options::default());
    let e = x
        .run_step(&step(r#"{"type": "vla", "id": "v", "task": "wave"}"#))
        .unwrap_err();
    assert_eq!(
        e,
        PyError::new(
            "TypeError",
            "MuJoCoExecutor cannot run step of type VLAStep"
        )
    );
}

#[test]
fn configure() {
    let mut x = arm(Options::default());
    let env =
        loads(r#"{"permissions": {"torque_limit": 2.5, "obstacles": [[[1, 1, 1], [2, 2, 2]]]}}"#)
            .unwrap();
    x.configure_from_envelope(env.as_obj().unwrap());
    assert_eq!(
        x.opt.torque_limit.as_ref().map(|n| n.repr()).as_deref(),
        Some("2.5")
    );
    assert!(x.opt.forbid_contacts);
}

#[test]
fn mock_vla() {
    let mut v = Vla::mock(&fixture("two_joint_arm.xml"), 10).unwrap();
    let r = v.run_step(
        &step(r#"{"type": "vla", "id": "v", "task": "reach", "target_pose": [0.3, -0.2, 0]}"#),
        30,
    );
    assert_eq!(r.rc, 0);
    assert!(
        r.stdout.starts_with(
            r#"{"task": "reach", "actions_requested": 10, "actions_executed": 10, "max_actions": 50, "observation_initial": {"qpos": [0.0, 0.0, 0.0]"#
        ),
        "{}",
        r.stdout
    );
    assert!(r.stdout.ends_with(r#""contact_count": 0}"#), "{}", r.stdout);
    let r = v.run_step(
        &step(r#"{"type": "joint_move", "id": "m", "joint_targets": {"j1": 0.1}}"#),
        30,
    );
    assert_eq!(
        (r.rc, r.stdout.as_str()),
        (1, "VLAExecutorBase: not a VLAStep (JointMoveStep)")
    );
}

#[test]
fn base_vla() {
    let mut v = Vla::new(&fixture("two_joint_arm.xml"), 200, None).unwrap();
    let r = v.run_step(&step(r#"{"type": "vla", "id": "v", "task": "reach"}"#), 30);
    assert_eq!(
        (r.rc, r.stdout.as_str()),
        (1, "VLAExecutorBase._predict_actions not implemented")
    );
    let mut none = Vla::new("", 200, None).unwrap();
    let r = none.run_step(&step(r#"{"type": "vla", "id": "v", "task": "reach"}"#), 30);
    assert_eq!(
        (r.rc, r.stdout.as_str()),
        (1, "VLAExecutorBase: no MJCF loaded; pass mjcf_path")
    );
}

#[test]
fn smolvla_runs_a_chunk_on_the_arm() {
    let Ok(dir) = std::env::var("DAISUGI_SMOLVLA_DIR") else {
        eprintln!("DAISUGI_SMOLVLA_DIR is not set");
        return;
    };
    let (mut v, st) = smolvla_executor(
        &fixture("two_joint_arm.xml"),
        std::path::Path::new(&dir),
        4,
        3,
    )
    .unwrap();
    let r = v.run_step(
        &step(r#"{"type": "vla", "id": "v", "task": "pick up the block", "max_actions": 20, "timeout_s": 120}"#),
        120,
    );
    assert_eq!(r.rc, 0, "{}", r.stdout);
    assert!(
        r.stdout.contains(r#""actions_executed": 20"#),
        "{}",
        r.stdout
    );
    let st = st.borrow();
    assert_eq!(
        (st.chunks, st.last_chunk.len(), st.last_chunk[0].len()),
        (1, 50, 6)
    );
    assert_eq!(st.last_image.len(), 240 * 320 * 3);
    assert!(
        v.sim().unwrap().1.qpos().iter().any(|q| *q != 0.0),
        "the arm did not move"
    );
}

#[test]
fn smolvla_without_the_model_is_an_error() {
    let dir = std::env::temp_dir().join(format!("smolvla-empty-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    assert!(smolvla_executor(&fixture("two_joint_arm.xml"), &dir, 1, 0).is_err());
    std::fs::remove_dir_all(&dir).unwrap();
}
