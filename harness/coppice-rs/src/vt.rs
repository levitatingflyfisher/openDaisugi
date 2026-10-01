//! The only module that calls libghostty-vt, through the small C layer in
//! csrc/vt_shim.c. Everything above it sees Term and Cell. Title, progress
//! and size serve screen detection, a later slice.
#![allow(dead_code)]

use std::os::raw::{c_int, c_void};
use std::sync::Mutex;

#[repr(C)]
struct CopVt {
    _private: [u8; 0],
}

extern "C" {
    fn cop_vt_new(cols: u16, rows: u16, scrollback: usize, step: *mut c_int) -> *mut CopVt;
    fn cop_vt_free(v: *mut CopVt);
    fn cop_vt_write(v: *mut CopVt, data: *const u8, len: usize);
    fn cop_vt_resize(v: *mut CopVt, cols: u16, rows: u16) -> c_int;
    fn cop_vt_cols(v: *mut CopVt, out: *mut u16) -> c_int;
    fn cop_vt_rows(v: *mut CopVt, out: *mut u16) -> c_int;
    fn cop_vt_cursor_x(v: *mut CopVt, out: *mut u16) -> c_int;
    fn cop_vt_cursor_y(v: *mut CopVt, out: *mut u16) -> c_int;
    fn cop_vt_cursor_visible(v: *mut CopVt, out: *mut bool) -> c_int;
    fn cop_vt_title(v: *mut CopVt, ptr: *mut *const u8, len: *mut usize) -> c_int;
    fn cop_vt_progress(v: *mut CopVt, state: *mut c_int, progress: *mut c_int) -> c_int;
    fn cop_vt_plain(
        v: *mut CopVt,
        all: c_int,
        unwrap: bool,
        out: *mut *mut u8,
        out_len: *mut usize,
    ) -> c_int;
    fn cop_vt_free_buf(ptr: *mut u8, len: usize);
    fn cop_vt_rows_begin(v: *mut CopVt) -> c_int;
    fn cop_vt_row_next(v: *mut CopVt) -> bool;
    fn cop_vt_row_cells(v: *mut CopVt) -> c_int;
    fn cop_vt_cell_next(v: *mut CopVt) -> bool;
    fn cop_vt_cell_text(v: *mut CopVt, buf: *mut u8, cap: usize, len: *mut usize) -> c_int;
    fn cop_vt_cell_style(v: *mut CopVt, attrs: *mut u16) -> c_int;
    fn cop_vt_cell_color(v: *mut CopVt, bg: c_int, rgb: *mut u32) -> c_int;
}

const SUCCESS: c_int = 0;
const OUT_OF_SPACE: c_int = -3;

/// The names of GhosttyResult values, for error text.
fn result_name(r: c_int) -> String {
    match r {
        -1 => "out of memory".into(),
        -2 => "invalid value".into(),
        -3 => "out of space".into(),
        -4 => "no value".into(),
        -5 => "io error".into(),
        -6 => "limit exceeded".into(),
        -7 => "rejected".into(),
        100 => "terminal has zero size".into(),
        n => format!("ghostty result {n}"),
    }
}

/// One resolved colour. None means the terminal default.
pub type Rgb = Option<(u8, u8, u8)>;

/// One grid cell. text is the whole grapheme cluster; an empty text is a
/// blank cell, not a typed space.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Cell {
    pub text: String,
    pub fg: Rgb,
    pub bg: Rgb,
    pub attrs: u16,
}

struct Inner {
    v: *mut CopVt,
    closed: bool,
}

// SAFETY: the C handle is only ever touched under the Mutex in Term.
unsafe impl Send for Inner {}

/// One libghostty terminal. Every method holds the lock: libghostty
/// terminals are not safe for concurrent use.
pub struct Term {
    inner: Mutex<Inner>,
}

impl Term {
    /// A terminal of the given size, keeping scrollback_lines of history.
    pub fn new(cols: u16, rows: u16, scrollback_lines: usize) -> Result<Term, String> {
        let mut step: c_int = 0;
        // SAFETY: step is a live int; the call allocates and returns a handle.
        let v = unsafe { cop_vt_new(cols, rows, scrollback_lines, &mut step) };
        if v.is_null() {
            let what = match step {
                1 => "create terminal",
                2 => "scrollback",
                3 => "progress report",
                4 => "render state",
                5 => "row iterator",
                _ => "row cells",
            };
            return Err(format!("vt: {what}: failed"));
        }
        Ok(Term {
            inner: Mutex::new(Inner { v, closed: false }),
        })
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn feed(&self, p: &[u8]) -> Result<(), String> {
        let g = self.lock();
        if g.closed {
            return Err("vt: terminal is closed".into());
        }
        // SAFETY: the handle is open and p is a live slice.
        unsafe { cop_vt_write(g.v, p.as_ptr(), p.len()) };
        Ok(())
    }

    pub fn resize(&self, cols: u16, rows: u16) -> Result<(), String> {
        let g = self.lock();
        if g.closed {
            return Err("vt: terminal is closed".into());
        }
        // SAFETY: the handle is open.
        let r = unsafe { cop_vt_resize(g.v, cols, rows) };
        if r != SUCCESS {
            return Err(format!("ghostty: {}", result_name(r)));
        }
        Ok(())
    }

    fn size_locked(g: &Inner) -> (u16, u16) {
        let (mut c, mut r) = (0u16, 0u16);
        // SAFETY: the handle is open; c and r are live.
        unsafe {
            if cop_vt_cols(g.v, &mut c) != SUCCESS || cop_vt_rows(g.v, &mut r) != SUCCESS {
                return (0, 0);
            }
        }
        (c, r)
    }

    pub fn size(&self) -> (u16, u16) {
        let g = self.lock();
        if g.closed {
            return (0, 0);
        }
        Term::size_locked(&g)
    }

    /// The cursor column, row and whether it shows.
    pub fn cursor(&self) -> (u16, u16, bool) {
        let g = self.lock();
        if g.closed {
            return (0, 0, false);
        }
        let (mut x, mut y, mut vis) = (0u16, 0u16, false);
        // SAFETY: the handle is open; the outs are live.
        unsafe {
            if cop_vt_cursor_x(g.v, &mut x) != SUCCESS {
                return (0, 0, false);
            }
            if cop_vt_cursor_y(g.v, &mut y) != SUCCESS {
                return (0, 0, false);
            }
            if cop_vt_cursor_visible(g.v, &mut vis) != SUCCESS {
                return (x, y, false);
            }
        }
        (x, y, vis)
    }

    pub fn title(&self) -> String {
        let g = self.lock();
        if g.closed {
            return String::new();
        }
        let mut ptr: *const u8 = std::ptr::null();
        let mut len = 0usize;
        // SAFETY: the handle is open; the title bytes stay valid until the
        // next change, which cannot happen while the lock is held.
        unsafe {
            if cop_vt_title(g.v, &mut ptr, &mut len) != SUCCESS || ptr.is_null() {
                return String::new();
            }
            String::from_utf8_lossy(std::slice::from_raw_parts(ptr, len)).into_owned()
        }
    }

    /// The last OSC 9;4 payload, "4;state" or "4;state;progress", or "".
    pub fn progress(&self) -> String {
        let g = self.lock();
        if g.closed {
            return String::new();
        }
        let (mut st, mut pr) = (0, 0);
        // SAFETY: the handle is open; the outs are live.
        let has = unsafe { cop_vt_progress(g.v, &mut st, &mut pr) };
        if has == 0 {
            return String::new();
        }
        let pr = pr as i8;
        if pr < 0 {
            format!("4;{st}")
        } else {
            format!("4;{st};{pr}")
        }
    }

    fn plain(&self, all: bool, unwrap: bool) -> Result<String, String> {
        let g = self.lock();
        if g.closed {
            return Err("vt: terminal is closed".into());
        }
        let mut out: *mut u8 = std::ptr::null_mut();
        let mut len = 0usize;
        // SAFETY: the handle is open; on success out holds len bytes that
        // libghostty allocated, freed below.
        unsafe {
            let r = cop_vt_plain(g.v, all as c_int, unwrap, &mut out, &mut len);
            if r != SUCCESS {
                if !out.is_null() {
                    cop_vt_free_buf(out, len);
                }
                return Err(format!("vt: format: {}", result_name(r)));
            }
            let s = if out.is_null() {
                String::new()
            } else {
                String::from_utf8_lossy(std::slice::from_raw_parts(out, len)).into_owned()
            };
            cop_vt_free_buf(out, len);
            Ok(s)
        }
    }

    /// The visible screen only, as plain text.
    pub fn plain_screen(&self) -> Result<String, String> {
        self.plain(false, false)
    }

    /// The visible screen with soft-wrapped rows joined.
    pub fn plain_screen_unwrapped(&self) -> Result<String, String> {
        self.plain(false, true)
    }

    /// The scrollback and the viewport, as plain text, trailing newlines
    /// trimmed.
    pub fn plain_all(&self) -> Result<String, String> {
        let s = self.plain(true, false)?;
        Ok(s.trim_end_matches('\n').to_string())
    }

    /// The visible grid, exactly rows by cols cells.
    pub fn viewport(&self) -> Result<Vec<Vec<Cell>>, String> {
        let g = self.lock();
        if g.closed {
            return Err("vt: terminal is closed".into());
        }
        // SAFETY: every call below runs on the open handle under the lock;
        // the buffers handed in are live and sized as stated.
        unsafe {
            let r = cop_vt_rows_begin(g.v);
            if r != SUCCESS {
                return Err(format!("vt: render state update: {}", result_name(r)));
            }
            let (c, rr) = Term::size_locked(&g);
            let (cols, nrows) = (c as usize, rr as usize);
            let mut out: Vec<Vec<Cell>> = Vec::with_capacity(nrows);
            let mut buf: Vec<u8> = vec![0; 8];
            while cop_vt_row_next(g.v) {
                let mut row = vec![Cell::default(); cols];
                let r = cop_vt_row_cells(g.v);
                if r != SUCCESS {
                    return Err(format!("vt: row cells: {}", result_name(r)));
                }
                let mut x = 0;
                while x < cols && cop_vt_cell_next(g.v) {
                    let mut len = 0usize;
                    let mut r = cop_vt_cell_text(g.v, buf.as_mut_ptr(), buf.len(), &mut len);
                    if r == OUT_OF_SPACE && len > buf.len() {
                        buf.resize(len.max(buf.len() * 2).max(64), 0);
                        r = cop_vt_cell_text(g.v, buf.as_mut_ptr(), buf.len(), &mut len);
                    }
                    if r != SUCCESS {
                        return Err(format!("vt: graphemes: {}", result_name(r)));
                    }
                    let mut cell = Cell {
                        text: String::from_utf8_lossy(&buf[..len]).into_owned(),
                        ..Default::default()
                    };
                    let mut attrs = 0u16;
                    if cop_vt_cell_style(g.v, &mut attrs) == SUCCESS {
                        cell.attrs = attrs;
                    }
                    let mut rgb = 0u32;
                    if cop_vt_cell_color(g.v, 0, &mut rgb) == SUCCESS {
                        cell.fg = Some(((rgb >> 16) as u8, (rgb >> 8) as u8, rgb as u8));
                    }
                    if cop_vt_cell_color(g.v, 1, &mut rgb) == SUCCESS {
                        cell.bg = Some(((rgb >> 16) as u8, (rgb >> 8) as u8, rgb as u8));
                    }
                    row[x] = cell;
                    x += 1;
                }
                out.push(row);
                if out.len() == nrows {
                    break;
                }
            }
            while out.len() < nrows {
                out.push(vec![Cell::default(); cols]);
            }
            Ok(out)
        }
    }

    /// Releases the C handles. A second call does nothing.
    pub fn close(&self) {
        let mut g = self.lock();
        if g.closed {
            return;
        }
        g.closed = true;
        // SAFETY: the handle was open and is never used again.
        unsafe { cop_vt_free(g.v) };
        g.v = std::ptr::null_mut::<c_void>() as *mut CopVt;
    }
}

impl Drop for Term {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn text_lands_on_the_screen() {
        let t = Term::new(20, 4, 100).unwrap();
        t.feed(b"hello\r\nworld").unwrap();
        assert_eq!(t.plain_screen().unwrap(), "hello\nworld");
        let (x, y, _) = t.cursor();
        assert_eq!((x, y), (5, 1));
        let vp = t.viewport().unwrap();
        assert_eq!(vp.len(), 4);
        assert_eq!(vp[0].len(), 20);
        assert_eq!(vp[0][0].text, "h");
        assert_eq!(vp[3][0].text, "");
    }

    #[test]
    fn progress_and_title_read_back() {
        let t = Term::new(20, 4, 0).unwrap();
        t.feed(b"\x1b]0;my title\x07\x1b]9;4;1;40\x07").unwrap();
        assert_eq!(t.title(), "my title");
        assert_eq!(t.progress(), "4;1;40");
        t.close();
        assert!(t.feed(b"x").is_err());
    }
}
