use super::*;

const TINY: &str = r#"<mujoco model="tiny">
  <compiler angle="radian"/>
  <option timestep="0.01" gravity="0 0 0"/>
  <visual><global offwidth="64" offheight="48"/></visual>
  <worldbody>
    <light pos="0 0 2"/>
    <camera name="cam" pos="0.25 -1 0.6" xyaxes="1 0 0 0 0.5 1"/>
    <geom name="floor" type="plane" size="1 1 0.01" pos="0 0 -0.1" rgba="0.3 0.6 0.3 1"/>
    <body name="arm">
      <joint name="hinge" type="hinge" axis="0 0 1" range="-1 1"/>
      <geom name="rod" type="capsule" fromto="0 0 0 0.5 0 0" size="0.03" rgba="0.9 0.2 0.2 1"/>
      <site name="tip" pos="0.5 0 0"/>
      <body name="tipbody" pos="0.5 0 0"/>
    </body>
  </worldbody>
  <actuator><position name="act" joint="hinge" kp="10" ctrlrange="-0.5 0.5"/></actuator>
</mujoco>"#;

const TOUCHING: &str = r#"<mujoco>
  <option gravity="0 0 0"/>
  <worldbody>
    <geom name="a" type="box" size="0.1 0.1 0.1"/>
    <body pos="0.1 0 0"><freejoint/><geom type="box" size="0.1 0.1 0.1"/></body>
  </worldbody>
</mujoco>"#;

fn tiny() -> Model {
    Model::from_xml_string(TINY).expect("tiny loads")
}

#[test]
fn version() {
    assert_eq!(super::version(), 3012000);
}

#[test]
fn model_counts() {
    let m = tiny();
    assert_eq!(
        [
            m.nq(),
            m.nv(),
            m.nu(),
            m.njnt(),
            m.nbody(),
            m.nsite(),
            m.ngeom(),
            m.ncam()
        ],
        [1, 1, 1, 1, 3, 1, 2, 1]
    );
    assert_eq!(m.timestep(), 0.01);
}

#[test]
fn load_errors() {
    assert!(Model::from_xml_string("<mujoco><worldbody><body></mujoco>").is_err());
    let dir = std::env::temp_dir().join(format!("dmj-test-{}", std::process::id()));
    let missing = dir.join("absent.xml");
    let err = Model::from_xml_path(missing.to_str().unwrap())
        .err()
        .expect("missing file fails");
    assert!(err.contains("Error opening file"), "{err}");
}

#[test]
fn load_file() {
    let dir = std::env::temp_dir().join(format!("dmj-load-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join("tiny.xml");
    std::fs::write(&path, TINY).unwrap();
    let m = Model::from_xml_path(path.to_str().unwrap()).expect("loads");
    std::fs::remove_dir_all(&dir).unwrap();
    assert_eq!(m.nu(), 1);
}

#[test]
fn names() {
    let m = tiny();
    let cases = [
        (Obj::Body, "world", 0),
        (Obj::Body, "arm", 1),
        (Obj::Body, "tipbody", 2),
        (Obj::Joint, "hinge", 0),
        (Obj::Geom, "rod", 1),
        (Obj::Site, "tip", 0),
        (Obj::Camera, "cam", 0),
        (Obj::Actuator, "act", 0),
        (Obj::Joint, "nope", -1),
    ];
    for (kind, name, id) in cases {
        assert_eq!(m.name2id(kind, name), id, "{name}");
        if id >= 0 {
            assert_eq!(m.id2name(kind, id).as_deref(), Some(name));
        }
    }
    assert_eq!(m.id2name(Obj::Geom, 99), None);
}

#[test]
fn model_fields() {
    let m = tiny();
    assert_eq!(
        (m.jnt_type(0), m.jnt_qposadr(0), m.jnt_limited(0)),
        (JNT_HINGE, 0, true)
    );
    assert_eq!(m.jnt_range(0), (-1.0, 1.0));
    assert_eq!((m.actuator_trnid(0), m.actuator_ctrllimited(0)), (0, true));
    assert_eq!(m.actuator_ctrlrange(0), (-0.5, 0.5));
}

#[test]
fn data_forward_step_reset() {
    let m = tiny();
    let mut d = Data::new(&m);
    assert_eq!(d.xpos(2), [0.0; 3]);
    d.forward();
    assert_eq!(d.xpos(2), [0.5, 0.0, 0.0]);
    assert_eq!(d.site_xpos(0), [0.5, 0.0, 0.0]);
    d.ctrl_mut()[0] = 0.3;
    for _ in 0..200 {
        d.step();
    }
    assert!(d.qpos()[0] > 0.1, "{:?}", d.qpos());
    assert_eq!(d.actuator_force().len(), 1);
    d.reset();
    assert_eq!((d.qpos()[0], d.ctrl()[0], d.qvel()[0]), (0.0, 0.0, 0.0));
}

#[test]
fn jac_body() {
    let m = tiny();
    let mut d = Data::new(&m);
    d.forward();
    assert_eq!(d.jac_body(2), vec![0.0, 0.5, 0.0]);
}

#[test]
fn contacts() {
    let m = Model::from_xml_string(TOUCHING).unwrap();
    let mut d = Data::new(&m);
    d.step();
    assert!(d.ncon() > 0);
    assert_eq!(d.contact_geoms(0), (0, 1));
}

#[test]
fn render() {
    let m = tiny();
    let mut d = Data::new(&m);
    d.forward();
    let mut r = Renderer::new(&m, 64, 48).expect("renderer");
    let a = r.render(&mut d, 0).expect("renders");
    assert_eq!(a.len(), 64 * 48 * 3);
    assert!(a.chunks(3).any(|px| px != &a[..3]), "one color");
    assert_eq!(r.render(&mut d, 0).unwrap(), a);
    assert_ne!(r.render(&mut d, -1).unwrap(), a);
    assert!(r.render(&mut d, 1).is_err());
    assert!(Renderer::new(&m, 65, 48).is_err());
}
