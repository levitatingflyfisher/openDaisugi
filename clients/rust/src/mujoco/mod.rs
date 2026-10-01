//! A thin wrapper over MuJoCo's C API: what the robotics executors need
//! and nothing more. Load MJCF from a file or a string, make data, reset,
//! forward, step, read and write qpos, qvel and ctrl, read body and site
//! positions, look names up, the body Jacobian, the contacts, and
//! offscreen rendering of a camera to RGB.
//!
//! The twin of the Go package internal/mujoco. Both call MuJoCo's own
//! functions and the small C layer in clients/native/mujoco (dmj.c), which
//! build.rs compiles. The declarations below are written by hand from
//! MuJoCo 3.12.0's headers and dmj.h; only the mujoco feature builds them.

use std::ffi::{c_char, c_int, c_uchar, CStr, CString};
use std::rc::Rc;

#[repr(C)]
struct MjModel {
    _p: [u8; 0],
}

#[repr(C)]
struct MjData {
    _p: [u8; 0],
}

#[repr(C)]
struct DmjRenderer {
    _p: [u8; 0],
}

/// dmj_model_view.
#[repr(C)]
struct ModelView {
    nq: c_int,
    nv: c_int,
    nu: c_int,
    njnt: c_int,
    nbody: c_int,
    nsite: c_int,
    ngeom: c_int,
    ncam: c_int,
    timestep: f64,
    actuator_trnid: *const c_int,
    jnt_type: *const c_int,
    jnt_qposadr: *const c_int,
    jnt_limited: *const c_uchar,
    jnt_range: *const f64,
    actuator_ctrllimited: *const c_uchar,
    actuator_ctrlrange: *const f64,
}

/// dmj_data_view.
#[repr(C)]
struct DataView {
    qpos: *mut f64,
    qvel: *mut f64,
    ctrl: *mut f64,
    xpos: *mut f64,
    site_xpos: *mut f64,
    actuator_force: *mut f64,
}

extern "C" {
    fn dmj_version() -> c_int;
    fn dmj_load_xml(path: *const c_char, err: *mut c_char, errlen: c_int) -> *mut MjModel;
    fn dmj_load_xml_string(xml: *const c_char, err: *mut c_char, errlen: c_int) -> *mut MjModel;
    fn dmj_model_view_get(m: *const MjModel, out: *mut ModelView);
    fn dmj_data_view_get(m: *const MjModel, d: *mut MjData, out: *mut DataView);
    fn dmj_ncon(d: *const MjData) -> c_int;
    fn dmj_contact_geoms(d: *const MjData, i: c_int, geom1: *mut c_int, geom2: *mut c_int);
    fn dmj_renderer_new(
        m: *const MjModel,
        width: c_int,
        height: c_int,
        err: *mut c_char,
        errlen: c_int,
    ) -> *mut DmjRenderer;
    fn dmj_renderer_render(
        r: *mut DmjRenderer,
        m: *const MjModel,
        d: *mut MjData,
        camid: c_int,
        rgb: *mut c_uchar,
        err: *mut c_char,
        errlen: c_int,
    ) -> c_int;
    fn dmj_renderer_free(r: *mut DmjRenderer);

    fn mj_deleteModel(m: *mut MjModel);
    fn mj_makeData(m: *const MjModel) -> *mut MjData;
    fn mj_deleteData(d: *mut MjData);
    fn mj_resetData(m: *const MjModel, d: *mut MjData);
    fn mj_forward(m: *const MjModel, d: *mut MjData);
    fn mj_step(m: *const MjModel, d: *mut MjData);
    fn mj_name2id(m: *const MjModel, kind: c_int, name: *const c_char) -> c_int;
    fn mj_id2name(m: *const MjModel, kind: c_int, id: c_int) -> *const c_char;
    fn mj_jacBody(m: *const MjModel, d: *const MjData, jacp: *mut f64, jacr: *mut f64, body: c_int);
}

const ERR_LEN: usize = 1000;

/// mjtObj: the object types the executors look up.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Obj {
    Body = 1,
    Joint = 3,
    Geom = 5,
    Site = 6,
    Camera = 7,
    Actuator = 19,
}

/// mjtJoint.
pub const JNT_FREE: i32 = 0;
pub const JNT_BALL: i32 = 1;
pub const JNT_SLIDE: i32 = 2;
pub const JNT_HINGE: i32 = 3;

/// mj_version(): 3012000 for MuJoCo 3.12.0.
pub fn version() -> i32 {
    unsafe { dmj_version() }
}

fn err_text(buf: &[u8]) -> String {
    let end = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    String::from_utf8_lossy(&buf[..end]).into_owned()
}

fn cstring(s: &str) -> Result<CString, String> {
    CString::new(s).map_err(|_| "the text holds a NUL byte".to_string())
}

/// The compiled model and its field views, freed when the last Model,
/// Data or Renderer that holds it goes.
struct ModelPtr {
    m: *mut MjModel,
    v: ModelView,
}

impl Drop for ModelPtr {
    fn drop(&mut self) {
        unsafe { mj_deleteModel(self.m) }
    }
}

/// An mjModel. Clones share one compiled model.
#[derive(Clone)]
pub struct Model {
    p: Rc<ModelPtr>,
}

impl Model {
    fn wrap(m: *mut MjModel, err: &[u8]) -> Result<Model, String> {
        if m.is_null() {
            return Err(err_text(err));
        }
        let mut v = std::mem::MaybeUninit::<ModelView>::uninit();
        unsafe { dmj_model_view_get(m, v.as_mut_ptr()) };
        Ok(Model {
            p: Rc::new(ModelPtr {
                m,
                v: unsafe { v.assume_init() },
            }),
        })
    }

    /// Compile the MJCF file at path. The error is MuJoCo's message.
    pub fn from_xml_path(path: &str) -> Result<Model, String> {
        let p = cstring(path)?;
        let mut err = vec![0u8; ERR_LEN];
        let m = unsafe {
            dmj_load_xml(
                p.as_ptr(),
                err.as_mut_ptr() as *mut c_char,
                ERR_LEN as c_int,
            )
        };
        Model::wrap(m, &err)
    }

    /// Compile MJCF held in a string.
    pub fn from_xml_string(xml: &str) -> Result<Model, String> {
        let x = cstring(xml)?;
        let mut err = vec![0u8; ERR_LEN];
        let m = unsafe {
            dmj_load_xml_string(
                x.as_ptr(),
                err.as_mut_ptr() as *mut c_char,
                ERR_LEN as c_int,
            )
        };
        Model::wrap(m, &err)
    }

    pub fn nq(&self) -> usize {
        self.p.v.nq as usize
    }
    pub fn nv(&self) -> usize {
        self.p.v.nv as usize
    }
    pub fn nu(&self) -> usize {
        self.p.v.nu as usize
    }
    pub fn njnt(&self) -> usize {
        self.p.v.njnt as usize
    }
    pub fn nbody(&self) -> usize {
        self.p.v.nbody as usize
    }
    pub fn nsite(&self) -> usize {
        self.p.v.nsite as usize
    }
    pub fn ngeom(&self) -> usize {
        self.p.v.ngeom as usize
    }
    pub fn ncam(&self) -> usize {
        self.p.v.ncam as usize
    }
    pub fn timestep(&self) -> f64 {
        self.p.v.timestep
    }

    fn ints(&self, p: *const c_int, n: usize) -> &[c_int] {
        if p.is_null() || n == 0 {
            return &[];
        }
        unsafe { std::slice::from_raw_parts(p, n) }
    }

    fn f64s(&self, p: *const f64, n: usize) -> &[f64] {
        if p.is_null() || n == 0 {
            return &[];
        }
        unsafe { std::slice::from_raw_parts(p, n) }
    }

    fn bytes(&self, p: *const c_uchar, n: usize) -> &[c_uchar] {
        if p.is_null() || n == 0 {
            return &[];
        }
        unsafe { std::slice::from_raw_parts(p, n) }
    }

    /// actuator_trnid[a, 0]: the joint actuator a drives.
    pub fn actuator_trnid(&self, a: usize) -> i32 {
        self.ints(self.p.v.actuator_trnid, 2 * self.nu())[2 * a]
    }
    /// jnt_type[j].
    pub fn jnt_type(&self, j: usize) -> i32 {
        self.ints(self.p.v.jnt_type, self.njnt())[j]
    }
    /// jnt_qposadr[j].
    pub fn jnt_qposadr(&self, j: usize) -> usize {
        self.ints(self.p.v.jnt_qposadr, self.njnt())[j] as usize
    }
    /// jnt_limited[j].
    pub fn jnt_limited(&self, j: usize) -> bool {
        self.bytes(self.p.v.jnt_limited, self.njnt())[j] != 0
    }
    /// jnt_range[j].
    pub fn jnt_range(&self, j: usize) -> (f64, f64) {
        let r = self.f64s(self.p.v.jnt_range, 2 * self.njnt());
        (r[2 * j], r[2 * j + 1])
    }
    /// actuator_ctrllimited[a].
    pub fn actuator_ctrllimited(&self, a: usize) -> bool {
        self.bytes(self.p.v.actuator_ctrllimited, self.nu())[a] != 0
    }
    /// actuator_ctrlrange[a].
    pub fn actuator_ctrlrange(&self, a: usize) -> (f64, f64) {
        let r = self.f64s(self.p.v.actuator_ctrlrange, 2 * self.nu());
        (r[2 * a], r[2 * a + 1])
    }

    /// mj_name2id: -1 when no object of that type has the name.
    pub fn name2id(&self, kind: Obj, name: &str) -> i32 {
        let Ok(n) = CString::new(name) else { return -1 };
        unsafe { mj_name2id(self.p.m, kind as c_int, n.as_ptr()) }
    }

    /// mj_id2name: None when the object has no name or the id is out of
    /// range.
    pub fn id2name(&self, kind: Obj, id: i32) -> Option<String> {
        let p = unsafe { mj_id2name(self.p.m, kind as c_int, id) };
        if p.is_null() {
            return None;
        }
        let s = unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned();
        (!s.is_empty()).then_some(s)
    }
}

/// An mjData of one model. It keeps the model alive.
pub struct Data {
    d: *mut MjData,
    m: Model,
    v: DataView,
}

impl Data {
    /// mj_makeData. It runs no computation: positions read zero until
    /// forward or step.
    pub fn new(m: &Model) -> Data {
        let d = unsafe { mj_makeData(m.p.m) };
        assert!(!d.is_null(), "mj_makeData failed");
        let mut v = std::mem::MaybeUninit::<DataView>::uninit();
        unsafe { dmj_data_view_get(m.p.m, d, v.as_mut_ptr()) };
        Data {
            d,
            m: m.clone(),
            v: unsafe { v.assume_init() },
        }
    }

    pub fn model(&self) -> &Model {
        &self.m
    }

    fn view(&self, p: *mut f64, n: usize) -> &[f64] {
        if p.is_null() || n == 0 {
            return &[];
        }
        unsafe { std::slice::from_raw_parts(p, n) }
    }

    fn view_mut(&mut self, p: *mut f64, n: usize) -> &mut [f64] {
        if p.is_null() || n == 0 {
            return &mut [];
        }
        unsafe { std::slice::from_raw_parts_mut(p, n) }
    }

    pub fn qpos(&self) -> &[f64] {
        self.view(self.v.qpos, self.m.nq())
    }
    pub fn qpos_mut(&mut self) -> &mut [f64] {
        self.view_mut(self.v.qpos, self.m.nq())
    }
    pub fn qvel(&self) -> &[f64] {
        self.view(self.v.qvel, self.m.nv())
    }
    pub fn ctrl(&self) -> &[f64] {
        self.view(self.v.ctrl, self.m.nu())
    }
    pub fn ctrl_mut(&mut self) -> &mut [f64] {
        self.view_mut(self.v.ctrl, self.m.nu())
    }
    pub fn actuator_force(&self) -> &[f64] {
        self.view(self.v.actuator_force, self.m.nu())
    }

    /// The world position of body b.
    pub fn xpos(&self, b: usize) -> [f64; 3] {
        let x = self.view(self.v.xpos, 3 * self.m.nbody());
        [x[3 * b], x[3 * b + 1], x[3 * b + 2]]
    }

    /// The world position of site s.
    pub fn site_xpos(&self, s: usize) -> [f64; 3] {
        let x = self.view(self.v.site_xpos, 3 * self.m.nsite());
        [x[3 * s], x[3 * s + 1], x[3 * s + 2]]
    }

    /// The number of contacts.
    pub fn ncon(&self) -> usize {
        unsafe { dmj_ncon(self.d) as usize }
    }

    /// Contact i's geom1 and geom2.
    pub fn contact_geoms(&self, i: usize) -> (i32, i32) {
        let (mut g1, mut g2) = (0, 0);
        unsafe { dmj_contact_geoms(self.d, i as c_int, &mut g1, &mut g2) };
        (g1, g2)
    }

    /// mj_resetData.
    pub fn reset(&mut self) {
        unsafe { mj_resetData(self.m.p.m, self.d) }
    }

    /// mj_forward.
    pub fn forward(&mut self) {
        unsafe { mj_forward(self.m.p.m, self.d) }
    }

    /// mj_step.
    pub fn step(&mut self) {
        unsafe { mj_step(self.m.p.m, self.d) }
    }

    /// The translational Jacobian of body b's frame (mj_jacBody's jacp):
    /// 3 x nv, row major.
    pub fn jac_body(&self, b: usize) -> Vec<f64> {
        let mut jacp = vec![0.0; 3 * self.m.nv()];
        if !jacp.is_empty() {
            unsafe {
                mj_jacBody(
                    self.m.p.m,
                    self.d,
                    jacp.as_mut_ptr(),
                    std::ptr::null_mut(),
                    b as c_int,
                )
            };
        }
        jacp
    }
}

impl Drop for Data {
    fn drop(&mut self) {
        unsafe { mj_deleteData(self.d) }
    }
}

/// Draws a camera offscreen through EGL, with no display. Each call makes
/// its context current and releases it again, so it may move between
/// threads, one call at a time.
pub struct Renderer {
    r: *mut DmjRenderer,
    width: usize,
    height: usize,
    _m: Model,
}

impl Renderer {
    /// A width x height renderer for m. The size must fit the model's
    /// offscreen buffer (visual/global offwidth and offheight).
    pub fn new(m: &Model, width: usize, height: usize) -> Result<Renderer, String> {
        let mut err = vec![0u8; ERR_LEN];
        let r = unsafe {
            dmj_renderer_new(
                m.p.m,
                width as c_int,
                height as c_int,
                err.as_mut_ptr() as *mut c_char,
                ERR_LEN as c_int,
            )
        };
        if r.is_null() {
            return Err(err_text(&err));
        }
        Ok(Renderer {
            r,
            width,
            height,
            _m: m.clone(),
        })
    }

    /// Draw camera cam (-1 for the default free camera) of the state in d:
    /// width x height x 3 bytes of RGB, top row first.
    pub fn render(&mut self, d: &mut Data, cam: i32) -> Result<Vec<u8>, String> {
        let mut rgb = vec![0u8; self.width * self.height * 3];
        let mut err = vec![0u8; ERR_LEN];
        let rc = unsafe {
            dmj_renderer_render(
                self.r,
                d.m.p.m,
                d.d,
                cam,
                rgb.as_mut_ptr(),
                err.as_mut_ptr() as *mut c_char,
                ERR_LEN as c_int,
            )
        };
        if rc != 0 {
            return Err(err_text(&err));
        }
        Ok(rgb)
    }
}

impl Drop for Renderer {
    fn drop(&mut self) {
        unsafe { dmj_renderer_free(self.r) }
    }
}

#[cfg(test)]
mod tests;
