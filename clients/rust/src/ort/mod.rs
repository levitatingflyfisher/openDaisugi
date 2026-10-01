//! A thin wrapper over ONNX Runtime's C API: open a session on a model
//! file and run it on float32 and int64 inputs into float32 outputs. That
//! is all the VLA executors need.
//!
//! It links the libonnxruntime.so that `clients/go/scripts/native.sh
//! --onnxruntime` installs and the small C layer in `clients/native/ort`,
//! which the Go wrapper shares, through hand-written FFI. It builds only
//! with the mujoco feature, the robotics build option, so the shipped
//! binaries never link ONNX Runtime. At run time the dynamic loader must
//! find libonnxruntime.so.1 (LD_LIBRARY_PATH).

use std::ffi::{c_char, c_int, c_void, CStr, CString};

const ERR_LEN: usize = 2000;
const DORT_FLOAT: c_int = 1;
const DORT_INT64: c_int = 7;

#[repr(C)]
struct DortSession {
    _private: [u8; 0],
}

#[repr(C)]
struct DortInput {
    name: *const c_char,
    kind: c_int,
    data: *const c_void,
    shape: *const i64,
    rank: c_int,
}

#[repr(C)]
struct DortOutput {
    name: *const c_char,
    data: *mut f32,
    count: i64,
}

extern "C" {
    fn dort_version() -> *const c_char;
    fn dort_open(
        path: *const c_char,
        threads: c_int,
        err: *mut c_char,
        errlen: c_int,
    ) -> *mut DortSession;
    fn dort_close(s: *mut DortSession);
    fn dort_run(
        s: *mut DortSession,
        input: *const DortInput,
        nin: c_int,
        output: *const DortOutput,
        nout: c_int,
        err: *mut c_char,
        errlen: c_int,
    ) -> c_int;
}

fn message(buf: &[c_char]) -> String {
    // SAFETY: the C layer always terminates the message within the buffer.
    unsafe { CStr::from_ptr(buf.as_ptr()) }
        .to_string_lossy()
        .into_owned()
}

/// The ONNX Runtime version the loaded library reports.
pub fn version() -> String {
    // SAFETY: ONNX Runtime returns a static, terminated string.
    unsafe { CStr::from_ptr(dort_version()) }
        .to_string_lossy()
        .into_owned()
}

/// The data of one input tensor.
pub enum Data<'a> {
    F32(&'a [f32]),
    I64(&'a [i64]),
}

/// One input tensor: its name, data and shape.
pub struct Input<'a> {
    pub name: &'a str,
    pub data: Data<'a>,
    pub shape: &'a [i64],
}

/// One float32 output, written into data, which must hold exactly the
/// output's element count.
pub struct Output<'a> {
    pub name: &'a str,
    pub data: &'a mut [f32],
}

/// One model, loaded. Not shared between threads.
pub struct Session {
    s: *mut DortSession,
}

impl Session {
    /// Load the model at path, to run with threads intra-op threads.
    pub fn open(path: &std::path::Path, threads: usize) -> Result<Session, String> {
        let cpath = CString::new(path.to_string_lossy().as_bytes())
            .map_err(|_| "ort: the model path holds a NUL byte".to_string())?;
        let mut buf = [0 as c_char; ERR_LEN];
        // SAFETY: the path and the buffer outlive the call.
        let s = unsafe {
            dort_open(
                cpath.as_ptr(),
                threads as c_int,
                buf.as_mut_ptr(),
                ERR_LEN as c_int,
            )
        };
        if s.is_null() {
            return Err(message(&buf));
        }
        Ok(Session { s })
    }

    /// Run the session.
    pub fn run(&mut self, input: &[Input], output: &mut [Output]) -> Result<(), String> {
        let names =
            |n: &str| CString::new(n).map_err(|_| format!("ort: the name {n:?} holds a NUL byte"));
        let in_names = input
            .iter()
            .map(|x| names(x.name))
            .collect::<Result<Vec<_>, _>>()?;
        let out_names = output
            .iter()
            .map(|y| names(y.name))
            .collect::<Result<Vec<_>, _>>()?;
        let mut cin = Vec::with_capacity(input.len());
        for (x, name) in input.iter().zip(&in_names) {
            let n: i64 = x.shape.iter().product();
            let (kind, data, len) = match x.data {
                Data::F32(v) => (DORT_FLOAT, v.as_ptr() as *const c_void, v.len()),
                Data::I64(v) => (DORT_INT64, v.as_ptr() as *const c_void, v.len()),
            };
            if len as i64 != n || n <= 0 {
                return Err(format!("ort: input {}: {len} values, not {n}", x.name));
            }
            cin.push(DortInput {
                name: name.as_ptr(),
                kind,
                data,
                shape: x.shape.as_ptr(),
                rank: x.shape.len() as c_int,
            });
        }
        let mut cout = Vec::with_capacity(output.len());
        for (y, name) in output.iter_mut().zip(&out_names) {
            if y.data.is_empty() {
                return Err(format!("ort: output {} has no buffer", y.name));
            }
            cout.push(DortOutput {
                name: name.as_ptr(),
                data: y.data.as_mut_ptr(),
                count: y.data.len() as i64,
            });
        }
        let mut buf = [0 as c_char; ERR_LEN];
        // SAFETY: every pointer refers to memory borrowed for this call; the
        // C layer reads the inputs, writes at most count values into each
        // output and keeps nothing after it returns.
        let rc = unsafe {
            dort_run(
                self.s,
                cin.as_ptr(),
                cin.len() as c_int,
                cout.as_ptr(),
                cout.len() as c_int,
                buf.as_mut_ptr(),
                ERR_LEN as c_int,
            )
        };
        if rc != 0 {
            return Err(format!("ort: {}", message(&buf)));
        }
        Ok(())
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        // SAFETY: s came from dort_open and is freed once.
        unsafe { dort_close(self.s) };
    }
}

#[cfg(test)]
mod tests;
