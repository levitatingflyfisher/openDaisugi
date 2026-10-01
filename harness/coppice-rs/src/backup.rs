//! One tar.gz of a tool's own history and settings. The same format and
//! rules as clients/go/internal/backup, harness/coppice/internal/backup and
//! clients/rust/src/backup.rs: a backup from one port restores in another.
//!
//! The archive holds only paths on an allow-list (`Spec::units`). Keys,
//! tokens, credentials, sockets and locks are never on it, and a restore
//! refuses any member that is not on it, whatever the archive says.
//!
//! Layout: MANIFEST.json first, then one member per file, named
//! "<area>/<path under that area's root>".
//!
//! Both directions stream. Memory use does not grow with the archive: a
//! backup moves 64 KiB blocks from a file through the deflater to disk, and
//! a restore inflates in blocks and writes each member as it arrives.

use std::fs;
use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::path::{Path, PathBuf};

use miniz_oxide::deflate::core::{compress, create_comp_flags_from_zip_params, CompressorOxide, TDEFLFlush, TDEFLStatus};
use miniz_oxide::inflate::stream::{inflate, InflateState};
use miniz_oxide::{DataFormat, MZError, MZFlush, MZStatus};
use serde_json::{json, Value};

pub const FORMAT: i64 = 1;
pub const MANIFEST: &str = "MANIFEST.json";
const BLOCK: usize = 64 << 10;

/// A tool, its roots (area name, directory) and its allow-list (area, path
/// under the root: a file or a whole tree).
#[derive(Default)]
pub struct Spec {
    pub tool: String,
    pub version: String,
    pub roots: Vec<(String, PathBuf)>,
    pub units: Vec<(String, String)>,
    /// Members ("area/rel") that `create` copies through `snap`: a SQLite
    /// file that a live tool writes. The -wal, -shm and -journal files
    /// beside it are left out.
    pub snapshots: Vec<String>,
    /// Writes a consistent copy of the file at `src` to `dst`.
    pub snap: Option<fn(src: &Path, dst: &Path) -> Result<(), String>>,
}

#[derive(Default, Debug)]
pub struct Done {
    pub files: usize,
    pub bytes: u64,
    /// Paths a forced restore moved aside and could not put back. After a
    /// clean rollback it is empty.
    pub aside: Vec<String>,
    /// Files a backup left out because their names look like secrets or
    /// locks.
    pub left: usize,
}

/// A restore that failed, and the paths it could not move back.
#[derive(Debug)]
pub struct Failure {
    pub msg: String,
    pub aside: Vec<String>,
}

impl From<String> for Failure {
    fn from(msg: String) -> Failure {
        Failure { msg, aside: Vec::new() }
    }
}

impl std::fmt::Display for Failure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.msg)
    }
}

fn hex(d: &[u8]) -> String {
    d.iter().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
fn sha256_hex(data: &[u8]) -> String {
    hex(ring::digest::digest(&ring::digest::SHA256, data).as_ref())
}

/// A name that looks like a lock, a socket, a pid file or a secret is
/// skipped even inside an allow-listed tree.
fn unsafe_name(base: &str) -> bool {
    let b = base.to_lowercase();
    [".lock", ".sock", ".pid", ".pem", ".key"]
        .iter()
        .any(|s| b.ends_with(s))
        || ["token", "secret", "credential", "password", "id_rsa", "id_ed25519"]
            .iter()
            .any(|s| b.contains(s))
        || b == "lock"
}

#[cfg(test)]
thread_local! {
    /// A test sets this to make chosen renames fail.
    static FAIL_RENAME: std::cell::RefCell<Option<fn(&Path, &Path) -> bool>> = const { std::cell::RefCell::new(None) };
}

/// fs::rename. A test can make one step fail.
fn rename(from: &Path, to: &Path) -> io::Result<()> {
    #[cfg(test)]
    if FAIL_RENAME.with(|f| f.borrow().map(|f| f(from, to)).unwrap_or(false)) {
        return Err(io::Error::other("injected"));
    }
    fs::rename(from, to)
}

// A test sets this to make a file appear just before an install rename.
#[cfg(test)]
thread_local! {
    static BEFORE_INSTALL: std::cell::RefCell<Option<fn(&Path)>> = const { std::cell::RefCell::new(None) };
}

/// Moves `p` to `p.broken-<time>` when something is at `p`, and records the
/// move for the undo.
fn aside_if_there(p: &Path, now: i64, done: &mut Vec<(PathBuf, PathBuf)>, aside: &mut Vec<PathBuf>) -> Result<(), String> {
    if fs::symlink_metadata(p).is_err() {
        return Ok(());
    }
    let dst = free_name(&format!("{}.broken-{}", p.display(), stamp(now)));
    rename(p, &dst).map_err(|e| format!("cannot move {} aside: {e}", p.display()))?;
    done.push((p.to_path_buf(), dst.clone()));
    aside.push(dst);
    Ok(())
}

/// Makes the entries of a directory durable.
fn sync_dir(p: &Path) {
    if let Ok(d) = fs::File::open(p) {
        let _ = d.sync_all();
    }
}

impl Spec {
    fn root(&self, area: &str) -> Option<&Path> {
        self.roots.iter().find(|(a, _)| a == area).map(|(_, p)| p.as_path())
    }

    /// Whether "<area>/<rel>" is on the allow-list. It never accepts "..",
    /// an absolute path or an empty part.
    fn allowed(&self, name: &str) -> bool {
        let Some((area, rel)) = name.split_once('/') else {
            return false;
        };
        if rel.is_empty() || self.root(area).is_none() {
            return false;
        }
        let parts: Vec<&str> = rel.split('/').collect();
        if parts.iter().any(|p| p.is_empty() || *p == "." || *p == ".." || p.contains('\0')) {
            return false;
        }
        if unsafe_name(parts[parts.len() - 1]) {
            return false;
        }
        self.units.iter().any(|(a, u)| {
            a == area && (rel == u || (rel.starts_with(u.as_str()) && rel.as_bytes().get(u.len()) == Some(&b'/')))
        })
    }

    fn path(&self, name: &str) -> PathBuf {
        let (area, rel) = name.split_once('/').unwrap_or(("", name));
        self.root(area).map(Path::to_path_buf).unwrap_or_default().join(rel)
    }

    fn snapshot(&self, name: &str) -> bool {
        self.snap.is_some() && self.snapshots.iter().any(|n| n == name)
    }

    /// A -wal, -shm or -journal file of a member that `create` snapshots.
    /// The snapshot holds what those files hold.
    fn sidecar(&self, name: &str) -> bool {
        ["-wal", "-shm", "-journal"]
            .iter()
            .any(|suf| name.strip_suffix(suf).is_some_and(|base| self.snapshot(base)))
    }

    fn walk(&self, area: &str, dir: &Path, rel: &str, skip: &[PathBuf], out: &mut Vec<String>, left: &mut usize) -> Result<(), String> {
        let md = match fs::symlink_metadata(dir) {
            Ok(m) => m,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
            Err(e) => return Err(format!("{}: {e}", dir.display())),
        };
        if md.is_file() {
            let name = format!("{area}/{rel}");
            if skip.iter().any(|s| s == dir) || self.sidecar(&name) {
            } else if self.allowed(&name) {
                out.push(name);
            } else {
                *left += 1;
            }
        } else if md.is_dir() {
            let rd = fs::read_dir(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
            for ent in rd {
                let ent = ent.map_err(|e| e.to_string())?;
                let n = ent.file_name().to_string_lossy().into_owned();
                self.walk(area, &ent.path(), &format!("{rel}/{n}"), skip, out, left)?;
            }
        }
        Ok(())
    }

    /// The member names in sorted order, and how many files were left out
    /// for their names. A link at the root of a unit is an error: a backup
    /// never follows it.
    fn collect(&self, skip: &[PathBuf]) -> Result<(Vec<String>, usize), String> {
        let mut names = Vec::new();
        let mut left = 0;
        for (area, rel) in &self.units {
            if let Some(root) = self.root(area) {
                let base = root.join(rel);
                if fs::symlink_metadata(&base).is_ok_and(|m| m.file_type().is_symlink()) {
                    return Err(format!(
                        "{} is a symlink. A backup does not follow links. Back up the real directory, or move it into place",
                        base.display()
                    ));
                }
                self.walk(area, &base, rel, skip, &mut names, &mut left)?;
            }
        }
        names.sort();
        names.dedup();
        Ok((names, left))
    }

    /// Allow-listed paths already on disk and not empty.
    fn existing(&self) -> Vec<PathBuf> {
        let mut out = Vec::new();
        for (area, rel) in &self.units {
            let Some(root) = self.root(area) else { continue };
            let p = root.join(rel);
            if let Ok(md) = fs::symlink_metadata(&p) {
                if md.is_dir() && fs::read_dir(&p).map(|mut d| d.next().is_none()).unwrap_or(true) {
                    continue;
                }
                out.push(p);
            }
        }
        out
    }
}

// ---- time ----

/// (year, month, day, hour, minute, second) of a Unix time in UTC.
fn civil(t: i64) -> (i64, u32, u32, u32, u32, u32) {
    let days = t.div_euclid(86400);
    let s = t.rem_euclid(86400);
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let y = yoe + era * 400 + i64::from(m <= 2);
    (y, m, d, (s / 3600) as u32, (s % 3600 / 60) as u32, (s % 60) as u32)
}

/// "2026-10-24", for a default file name.
pub fn date(t: i64) -> String {
    let (y, m, d, ..) = civil(t);
    format!("{y:04}-{m:02}-{d:02}")
}

fn rfc3339(t: i64) -> String {
    let (y, mo, d, h, mi, s) = civil(t);
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}Z")
}

fn stamp(t: i64) -> String {
    let (y, mo, d, h, mi, s) = civil(t);
    format!("{y:04}{mo:02}{d:02}T{h:02}{mi:02}{s:02}Z")
}

// ---- gzip, streaming ----

const fn crc_table() -> [u32; 256] {
    let mut table = [0u32; 256];
    let mut i = 0;
    while i < 256 {
        let mut c = i as u32;
        let mut k = 0;
        while k < 8 {
            c = if c & 1 != 0 { 0xEDB88320 ^ (c >> 1) } else { c >> 1 };
            k += 1;
        }
        table[i] = c;
        i += 1;
    }
    table
}

static CRC_TABLE: [u32; 256] = crc_table();

/// Adds data to a running crc32. Start with `!0`; the crc is `!state`.
fn crc_update(mut c: u32, data: &[u8]) -> u32 {
    for b in data {
        c = CRC_TABLE[((c ^ u32::from(*b)) & 0xff) as usize] ^ (c >> 8);
    }
    c
}

/// A gzip writer: it deflates what it is given in blocks and writes the
/// header, the body and the trailer to `inner`.
struct GzWriter<W: Write> {
    inner: W,
    comp: Box<CompressorOxide>,
    crc: u32,
    size: u32,
    buf: Vec<u8>,
}

impl<W: Write> GzWriter<W> {
    fn new(mut inner: W, mtime: u32) -> io::Result<Self> {
        let mut head = vec![0x1f, 0x8b, 8, 0];
        head.extend_from_slice(&mtime.to_le_bytes());
        head.extend_from_slice(&[0, 0xff]);
        inner.write_all(&head)?;
        // Level 6, a raw deflate stream (no zlib header): window_bits < 0.
        let flags = create_comp_flags_from_zip_params(6, -15, 0);
        Ok(GzWriter { inner, comp: Box::new(CompressorOxide::new(flags)), crc: !0, size: 0, buf: vec![0; BLOCK] })
    }

    fn feed(&mut self, mut input: &[u8], flush: TDEFLFlush) -> io::Result<()> {
        loop {
            let (st, used, out) = compress(&mut self.comp, input, &mut self.buf, flush);
            self.inner.write_all(&self.buf[..out])?;
            input = &input[used..];
            match st {
                TDEFLStatus::Done => return Ok(()),
                // All input is taken and the output buffer was not full:
                // there is nothing more to drain until the next call.
                TDEFLStatus::Okay if flush == TDEFLFlush::None && input.is_empty() && out < self.buf.len() => return Ok(()),
                TDEFLStatus::Okay if used == 0 && out == 0 => return Err(io::Error::other("the deflater made no progress")),
                TDEFLStatus::Okay => {}
                _ => return Err(io::Error::other("the deflater failed")),
            }
        }
    }

    /// Ends the deflate stream, writes the trailer and returns `inner`.
    fn finish(mut self) -> io::Result<W> {
        self.feed(&[], TDEFLFlush::Finish)?;
        self.inner.write_all(&(!self.crc).to_le_bytes())?;
        self.inner.write_all(&self.size.to_le_bytes())?;
        Ok(self.inner)
    }
}

impl<W: Write> Write for GzWriter<W> {
    fn write(&mut self, data: &[u8]) -> io::Result<usize> {
        self.crc = crc_update(self.crc, data);
        self.size = self.size.wrapping_add(data.len() as u32);
        self.feed(data, TDEFLFlush::None)?;
        Ok(data.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

/// A gzip reader: it inflates in blocks and checks the crc and the size
/// when the stream ends.
struct GzReader<R: Read> {
    r: R,
    state: Box<InflateState>,
    inbuf: Vec<u8>,
    pos: usize,
    end: usize,
    eof: bool,
    done: bool,
    crc: u32,
    size: u32,
}

fn bad_gzip(file: &str, why: &str) -> String {
    format!("{file} is not a gzip file: {why}")
}

impl<R: Read> GzReader<R> {
    fn open(file: &str, mut r: R) -> Result<Self, String> {
        let mut h = [0u8; 10];
        r.read_exact(&mut h).map_err(|_| bad_gzip(file, "bad header"))?;
        if h[0] != 0x1f || h[1] != 0x8b || h[2] != 8 {
            return Err(bad_gzip(file, "bad header"));
        }
        let short = |_| bad_gzip(file, "short");
        let flags = h[3];
        let mut byte = [0u8; 1];
        if flags & 4 != 0 {
            let mut n = [0u8; 2];
            r.read_exact(&mut n).map_err(short)?;
            let mut skip = vec![0u8; usize::from(u16::from_le_bytes(n))];
            r.read_exact(&mut skip).map_err(short)?;
        }
        for bit in [8u8, 16] {
            if flags & bit != 0 {
                loop {
                    r.read_exact(&mut byte).map_err(short)?;
                    if byte[0] == 0 {
                        break;
                    }
                }
            }
        }
        if flags & 2 != 0 {
            r.read_exact(&mut [0u8; 2]).map_err(short)?;
        }
        Ok(GzReader {
            r,
            state: InflateState::new_boxed(DataFormat::Raw),
            inbuf: vec![0; BLOCK],
            pos: 0,
            end: 0,
            eof: false,
            done: false,
            crc: !0,
            size: 0,
        })
    }

    fn trailer(&mut self) -> io::Result<()> {
        let mut t = [0u8; 8];
        let have = self.end - self.pos;
        let take = have.min(8);
        t[..take].copy_from_slice(&self.inbuf[self.pos..self.pos + take]);
        self.pos += take;
        self.r.read_exact(&mut t[take..])?;
        let crc = u32::from_le_bytes(t[..4].try_into().unwrap());
        let size = u32::from_le_bytes(t[4..].try_into().unwrap());
        if crc != !self.crc || size != self.size {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "gzip checksum"));
        }
        Ok(())
    }
}

impl<R: Read> Read for GzReader<R> {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        if self.done || out.is_empty() {
            return Ok(0);
        }
        loop {
            if self.pos == self.end && !self.eof {
                self.end = self.r.read(&mut self.inbuf)?;
                self.pos = 0;
                self.eof = self.end == 0;
            }
            let flush = if self.eof { MZFlush::Finish } else { MZFlush::None };
            let res = inflate(&mut self.state, &self.inbuf[self.pos..self.end], out, flush);
            self.pos += res.bytes_consumed;
            self.crc = crc_update(self.crc, &out[..res.bytes_written]);
            self.size = self.size.wrapping_add(res.bytes_written as u32);
            match res.status {
                Ok(MZStatus::StreamEnd) => {
                    self.done = true;
                    self.trailer()?;
                    return Ok(res.bytes_written);
                }
                Ok(_) | Err(MZError::Buf) => {
                    if res.bytes_written > 0 {
                        return Ok(res.bytes_written);
                    }
                    let stuck = res.bytes_consumed == 0 && (self.eof || self.pos < self.end);
                    if stuck {
                        return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "the deflate stream ends early"));
                    }
                }
                Err(e) => return Err(io::Error::new(io::ErrorKind::InvalidData, format!("{e:?}"))),
            }
        }
    }
}

// ---- tar (ustar, regular files only), streaming ----

fn octal(buf: &mut [u8], v: u64) {
    let w = buf.len() - 1;
    let s = format!("{v:0w$o}");
    buf[..w].copy_from_slice(s.as_bytes());
    buf[w] = 0;
}

/// The 512-byte header of a regular member.
fn tar_header(name: &str, size: u64, mtime: u64) -> Result<[u8; 512], String> {
    let raw = name.as_bytes();
    let (prefix, short): (&[u8], &[u8]) = if raw.len() <= 100 {
        (&[], raw)
    } else {
        let i = (1..raw.len())
            .rev()
            .find(|&i| raw[i] == b'/' && i <= 155 && raw.len() - i - 1 <= 100)
            .ok_or_else(|| format!("a name too long for a ustar header: {name}"))?;
        (&raw[..i], &raw[i + 1..])
    };
    let mut h = [0u8; 512];
    h[..short.len()].copy_from_slice(short);
    octal(&mut h[100..108], 0o600);
    octal(&mut h[108..116], 0);
    octal(&mut h[116..124], 0);
    octal(&mut h[124..136], size);
    octal(&mut h[136..148], mtime);
    h[148..156].copy_from_slice(b"        ");
    h[156] = b'0';
    h[257..263].copy_from_slice(b"ustar\0");
    h[263..265].copy_from_slice(b"00");
    h[345..345 + prefix.len()].copy_from_slice(prefix);
    let sum: u32 = h.iter().map(|b| u32::from(*b)).sum();
    h[148..155].copy_from_slice(format!("{sum:06o}\0").as_bytes());
    h[155] = b' ';
    Ok(h)
}

fn pad_len(size: u64) -> usize {
    ((512 - size % 512) % 512) as usize
}

fn cstr(b: &[u8]) -> String {
    let end = b.iter().position(|c| *c == 0).unwrap_or(b.len());
    String::from_utf8_lossy(&b[..end]).into_owned()
}

struct TarHeader {
    name: String,
    size: u64,
}

/// A tar reader. `next` moves to the next member. Reading it gives the
/// bytes of the current member, and 0 at its end.
struct TarReader<R: Read> {
    r: R,
    file: String,
    left: u64,
    pad: u64,
}

impl<R: Read> TarReader<R> {
    fn new(file: &str, r: R) -> Self {
        TarReader { r, file: file.to_string(), left: 0, pad: 0 }
    }

    fn damaged(&self, why: &str) -> String {
        format!("{} is damaged: {why}", self.file)
    }

    fn skip(&mut self, mut n: u64) -> Result<(), String> {
        let mut sink = [0u8; 8192];
        while n > 0 {
            let want = n.min(sink.len() as u64) as usize;
            let got = self.r.read(&mut sink[..want]).map_err(|e| self.damaged(&e.to_string()))?;
            if got == 0 {
                return Err(self.damaged("short member"));
            }
            n -= got as u64;
        }
        Ok(())
    }

    /// Reads the rest of the stream, so the gzip trailer is checked too.
    fn drain(&mut self) -> Result<(), String> {
        io::copy(&mut self.r, &mut io::sink()).map(|_| ()).map_err(|e| self.damaged(&e.to_string()))
    }

    fn next(&mut self) -> Result<Option<TarHeader>, String> {
        self.skip(self.left + self.pad)?;
        self.left = 0;
        self.pad = 0;
        let mut h = [0u8; 512];
        let mut got = 0;
        while got < 512 {
            let n = self.r.read(&mut h[got..]).map_err(|e| self.damaged(&e.to_string()))?;
            if n == 0 {
                break;
            }
            got += n;
        }
        if got == 0 || h.iter().all(|b| *b == 0) {
            return Ok(None);
        }
        if got < 512 {
            return Err(self.damaged("short tar header"));
        }
        let mut sum = 0u32;
        for (k, b) in h.iter().enumerate() {
            sum += if (148..156).contains(&k) { 32 } else { u32::from(*b) };
        }
        let want = u32::from_str_radix(cstr(&h[148..156]).trim(), 8).map_err(|_| self.damaged("tar header"))?;
        if sum != want {
            return Err(self.damaged("tar header checksum"));
        }
        let size = u64::from_str_radix(cstr(&h[124..136]).trim(), 8).map_err(|_| self.damaged("tar size"))?;
        let kind = h[156];
        let mut name = cstr(&h[..100]);
        let prefix = cstr(&h[345..500]);
        if !prefix.is_empty() {
            name = format!("{prefix}/{name}");
        }
        if kind != b'0' && kind != 0 {
            // Only regular files are ever written. Anything else is refused
            // here, before it can reach the disk.
            return Err(format!("{} holds {name:?}, which is not a regular file. Nothing was changed", self.file));
        }
        self.left = size;
        self.pad = pad_len(size) as u64;
        Ok(Some(TarHeader { name, size }))
    }
}

impl<R: Read> Read for TarReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if self.left == 0 || buf.is_empty() {
            return Ok(0);
        }
        let n = (buf.len() as u64).min(self.left) as usize;
        let got = self.r.read(&mut buf[..n])?;
        if got == 0 {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "short member"));
        }
        self.left -= got as u64;
        Ok(got)
    }
}

// ---- create ----

fn absolute(p: &Path) -> PathBuf {
    if p.is_absolute() {
        p.to_path_buf()
    } else {
        std::env::current_dir().map(|d| d.join(p)).unwrap_or_else(|_| p.to_path_buf())
    }
}

fn changed(name: &str) -> String {
    format!("{name} changed while it was copied. Stop the tool and try again")
}

// A test sets this to change a file just after `member` reads it.
#[cfg(test)]
thread_local! {
    static AFTER_READ: std::cell::RefCell<Option<fn(&Path)>> = const { std::cell::RefCell::new(None) };
}

/// Reads `src` once, writes it to `w` as a tar member called `name`, and
/// hashes it on the way. It fails if the size or the modification time of
/// the file differs before and after the read.
fn member(w: &mut impl Write, name: &str, src: &Path, mtime: u64) -> Result<(u64, String), String> {
    let e = |e: io::Error| format!("{name}: {e}");
    let mut f = fs::File::open(src).map_err(e)?;
    let before = f.metadata().map_err(e)?;
    if !before.is_file() {
        return Err(changed(name));
    }
    let size = before.len();
    w.write_all(&tar_header(name, size, mtime)?).map_err(e)?;
    let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
    let mut buf = vec![0u8; BLOCK];
    let mut n = 0u64;
    while n < size {
        let want = (size - n).min(BLOCK as u64) as usize;
        let got = f.read(&mut buf[..want]).map_err(e)?;
        if got == 0 {
            return Err(changed(name));
        }
        ctx.update(&buf[..got]);
        w.write_all(&buf[..got]).map_err(e)?;
        n += got as u64;
    }
    w.write_all(&vec![0u8; pad_len(size)]).map_err(e)?;
    #[cfg(test)]
    if let Some(hook) = AFTER_READ.with(|h| *h.borrow()) {
        hook(src);
    }
    let after = f.metadata().map_err(e)?;
    if after.len() != before.len() || after.modified().ok() != before.modified().ok() {
        return Err(changed(name));
    }
    Ok((size, hex(ctx.finish().as_ref())))
}

/// Writes the archive to `out`. It refuses to replace a file already there.
/// It reads each file once, hashing as it writes, and stops if a file
/// changed under the read. A SQLite file named in `snapshots` is copied
/// through `snap` first, so a live tool cannot tear it. `now` is Unix
/// seconds.
pub fn create(spec: &Spec, out: &Path, now: i64) -> Result<Done, String> {
    if fs::symlink_metadata(out).is_ok() {
        return Err(format!("{} already exists. Name another file", out.display()));
    }
    let abs = absolute(out);
    let tmp = PathBuf::from(format!("{}.partial", abs.display()));
    let body = PathBuf::from(format!("{}.body", abs.display()));
    let snap_dir = PathBuf::from(format!("{}.snap", abs.display()));
    let (names, left) = spec.collect(&[abs.clone(), tmp.clone(), body.clone()])?;
    if names.is_empty() {
        return Err("nothing to back up: none of the files a backup holds exist yet".into());
    }
    let r = build(spec, out, now, &names, &tmp, &body, &snap_dir);
    let _ = fs::remove_file(&body);
    let _ = fs::remove_dir_all(&snap_dir);
    match r {
        Ok(mut done) => {
            done.left = left;
            Ok(done)
        }
        Err(e) => {
            let _ = fs::remove_file(&tmp);
            Err(e)
        }
    }
}

fn build(spec: &Spec, out: &Path, now: i64, names: &[String], tmp: &Path, body: &Path, snap_dir: &Path) -> Result<Done, String> {
    let mtime = now.max(0) as u64;
    let io_err = |p: &Path| {
        let p = p.display().to_string();
        move |e: io::Error| format!("{p}: {e}")
    };
    // The members go into a body file first, because the manifest leads the
    // archive and holds their hashes.
    let bf = fs::OpenOptions::new().read(true).write(true).create_new(true).mode(0o600).open(body).map_err(io_err(body))?;
    let mut bw = io::BufWriter::with_capacity(BLOCK, bf);
    let mut done = Done::default();
    let mut entries = Vec::new();
    for (i, n) in names.iter().enumerate() {
        let mut src = spec.path(n);
        if spec.snapshot(n) {
            fs::create_dir_all(snap_dir).map_err(io_err(snap_dir))?;
            let dst = snap_dir.join(i.to_string());
            (spec.snap.unwrap())(&src, &dst).map_err(|e| format!("cannot copy {}: {e}", src.display()))?;
            src = dst;
        }
        let (size, sha) = member(&mut bw, n, &src, mtime)?;
        entries.push(json!({"path": n, "size": size, "sha256": sha}));
        done.files += 1;
        done.bytes += size;
    }
    let mut bf = bw.into_inner().map_err(|e| e.to_string())?;
    io::Seek::seek(&mut bf, io::SeekFrom::Start(0)).map_err(io_err(body))?;

    let roots: serde_json::Map<String, Value> = spec
        .roots
        .iter()
        .map(|(a, p)| (a.clone(), json!(p.to_string_lossy())))
        .collect();
    let m = json!({"format": FORMAT, "tool": spec.tool, "version": spec.version,
        "created": rfc3339(now), "roots": roots, "files": entries});
    let mut mb = serde_json::to_vec_pretty(&m).map_err(|e| e.to_string())?;
    mb.push(b'\n');

    let af = fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(tmp).map_err(io_err(tmp))?;
    let w = || -> io::Result<()> {
        let mut gz = GzWriter::new(io::BufWriter::with_capacity(BLOCK, af), mtime as u32)?;
        gz.write_all(&tar_header(MANIFEST, mb.len() as u64, mtime).map_err(io::Error::other)?)?;
        gz.write_all(&mb)?;
        gz.write_all(&vec![0u8; pad_len(mb.len() as u64)])?;
        // The body already holds whole tar blocks. Copy them as they are,
        // then write the two empty blocks that end the archive.
        let mut buf = vec![0u8; BLOCK];
        loop {
            let n = bf.read(&mut buf)?;
            if n == 0 {
                break;
            }
            gz.write_all(&buf[..n])?;
        }
        gz.write_all(&[0u8; 1024])?;
        let f = gz.finish()?.into_inner().map_err(|e| e.into_error())?;
        f.sync_all()?;
        drop(f);
        rename(tmp, out)
    };
    w().map_err(io_err(out))?;
    sync_dir(absolute(out).parent().unwrap_or(Path::new(".")));
    Ok(done)
}

// ---- restore ----

fn damaged(file: &str, e: impl std::fmt::Display) -> String {
    format!("{file} is damaged: {e}")
}

/// Reads the manifest, the first member, and checks that it is the
/// manifest of the right tool and that every file it lists is on the
/// allow-list. Returns the listed files: path, size, sha256.
fn read_manifest<R: Read>(spec: &Spec, f: &str, tar: &mut TarReader<R>) -> Result<Vec<(String, u64, String)>, String> {
    let Some(h) = tar.next()? else {
        return Err(format!("{f} is empty. It is not a {} backup", spec.tool));
    };
    if h.name != MANIFEST {
        return Err(format!("{f} has no manifest first. It is not a {} backup", spec.tool));
    }
    let mut mb = Vec::new();
    tar.by_ref().take(64 << 20).read_to_end(&mut mb).map_err(|e| damaged(f, e))?;
    let m: Value = serde_json::from_slice(&mb).map_err(|e| format!("{f} has a manifest that does not read: {e}"))?;
    let tool = m["tool"].as_str().unwrap_or("");
    if tool != spec.tool {
        return Err(format!("{f} is a {tool} backup, not a {} backup", spec.tool));
    }
    if m["format"].as_i64() != Some(FORMAT) {
        return Err(format!("{f} has backup format {}. This build reads format {FORMAT}", m["format"]));
    }
    let listed = m["files"].as_array().cloned().unwrap_or_default();
    if listed.is_empty() {
        return Err(format!("{f} lists no files. There is nothing to restore. Nothing was changed"));
    }
    let mut out = Vec::new();
    for e in &listed {
        let p = e["path"].as_str().unwrap_or("").to_string();
        if !spec.allowed(&p) {
            return Err(format!("{f} lists {p:?}, which a {} backup never holds. Nothing was changed", spec.tool));
        }
        out.push((p, e["size"].as_u64().unwrap_or(u64::MAX), e["sha256"].as_str().unwrap_or("").to_string()));
    }
    Ok(out)
}

/// `base`, or `base-2`, `base-3` and so on: the first that is not on disk.
fn free_name(base: &str) -> PathBuf {
    let mut dst = PathBuf::from(base);
    let mut i = 2;
    while fs::symlink_metadata(&dst).is_ok() {
        dst = PathBuf::from(format!("{base}-{i}"));
        i += 1;
    }
    dst
}

/// The staging directories, one beside each root: <root>/.restore-<time>.
/// A rename from one to the other stays on one file system.
struct Stager {
    stamp: String,
    dirs: Vec<(PathBuf, PathBuf)>, // root, staging dir
    made: Vec<PathBuf>,            // roots this restore made
}

impl Stager {
    fn dir(&mut self, root: &Path) -> Result<PathBuf, String> {
        if let Some((_, d)) = self.dirs.iter().find(|(r, _)| r == root) {
            return Ok(d.clone());
        }
        if !root.exists() {
            self.made.push(root.to_path_buf());
        }
        let mk = |p: &Path| fs::DirBuilder::new().mode(0o700).create(p).map_err(|e| format!("{}: {e}", p.display()));
        fs::DirBuilder::new().recursive(true).mode(0o700).create(root).map_err(|e| format!("{}: {e}", root.display()))?;
        let d = free_name(&format!("{}/.restore-{}", root.display(), self.stamp));
        mk(&d)?;
        self.dirs.push((root.to_path_buf(), d.clone()));
        Ok(d)
    }

    fn for_root(&self, root: &Path) -> Option<&Path> {
        self.dirs.iter().find(|(r, _)| r == root).map(|(_, d)| d.as_path())
    }

    /// Removes the staging directories. They hold only copies of archive
    /// members.
    fn discard(&self) {
        for (_, d) in &self.dirs {
            let _ = fs::remove_dir_all(d);
        }
        // A root this restore made stays only if something is in it now.
        for r in &self.made {
            let _ = fs::remove_dir(r);
        }
    }

    fn list(&self) -> String {
        let mut v: Vec<String> = self.dirs.iter().map(|(_, d)| d.display().to_string()).collect();
        v.sort();
        v.join(", ")
    }
}

/// Reads every member once. It checks the name, the type, the manifest and
/// the hash while it writes the member into a staging directory. It changes
/// nothing else.
fn stage<R: Read>(spec: &Spec, f: &str, tar: &mut TarReader<R>, listed: &[(String, u64, String)], st: &mut Stager) -> Result<(usize, u64), String> {
    let want: std::collections::HashMap<&str, (u64, &str)> = listed.iter().map(|(p, s, h)| (p.as_str(), (*s, h.as_str()))).collect();
    let mut seen = std::collections::HashSet::new();
    let (mut files, mut bytes) = (0usize, 0u64);
    while let Some(h) = tar.next()? {
        let Some((size, sha)) = want.get(h.name.as_str()) else {
            return Err(format!("{f} holds {:?}, which the manifest does not list. Nothing was changed", h.name));
        };
        if !spec.allowed(&h.name) || !seen.insert(h.name.clone()) {
            return Err(format!("{f} holds {:?}, which the manifest does not list. Nothing was changed", h.name));
        }
        let (area, rel) = h.name.split_once('/').unwrap_or(("", &h.name));
        let root = spec.root(area).unwrap_or(Path::new("")).to_path_buf();
        let dst = st.dir(&root)?.join(rel);
        if let Some(dir) = dst.parent() {
            fs::DirBuilder::new().recursive(true).mode(0o700).create(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
        }
        let mut out = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&dst)
            .map_err(|e| format!("{}: {e}", dst.display()))?;
        let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
        let mut buf = vec![0u8; BLOCK];
        let mut n = 0u64;
        loop {
            let got = tar.read(&mut buf).map_err(|e| damaged(f, e))?;
            if got == 0 {
                break;
            }
            ctx.update(&buf[..got]);
            out.write_all(&buf[..got]).map_err(|e| format!("{}: {e}", dst.display()))?;
            n += got as u64;
        }
        out.sync_all().map_err(|e| format!("{}: {e}", dst.display()))?;
        if n != *size || hex(ctx.finish().as_ref()) != *sha || n != h.size {
            return Err(format!("{f} fails its checksum in {}. Nothing was changed", h.name));
        }
        files += 1;
        bytes += n;
    }
    if seen.len() != want.len() {
        return Err(format!("{f} lacks files its manifest lists. Nothing was changed"));
    }
    tar.drain().map_err(|e| format!("{e}. Nothing was changed"))?;
    Ok((files, bytes))
}

/// Puts the files of an archive back. It reads the archive once and writes
/// every member into a staging directory next to its root, checking name,
/// type and hash as it goes. Nothing live changes until all members are
/// staged and good. A target that already holds any allow-listed file is
/// refused unless `force`. Then each old path moves aside to
/// <name>.broken-<time> (never deleted) and the staged path takes its place,
/// by rename. If a rename fails, the renames done so far are undone in
/// reverse order and the staging directory stays. On failure, `aside` holds
/// the paths that could not be moved back.
pub fn restore(spec: &Spec, file: &Path, force: bool, now: i64) -> Result<Done, Failure> {
    let f = file.display().to_string();
    let fh = fs::File::open(file).map_err(|e| format!("{f}: {e}"))?;
    let gz = GzReader::open(&f, io::BufReader::with_capacity(BLOCK, fh))?;
    let mut tar = TarReader::new(&f, gz);
    let want = read_manifest(spec, &f, &mut tar)?;
    let have = spec.existing();
    if !have.is_empty() && !force {
        let more = if have.len() > 1 { format!(" and {} more", have.len() - 1) } else { String::new() };
        return Err(format!("{}{more} already hold data. Restore with --force to move it aside and continue", have[0].display()).into());
    }
    let mut st = Stager { stamp: stamp(now), dirs: Vec::new(), made: Vec::new() };
    let (files, bytes) = match stage(spec, &f, &mut tar, &want, &mut st) {
        Ok(x) => x,
        Err(e) => {
            st.discard();
            return Err(e.into());
        }
    };
    let mut done: Vec<(PathBuf, PathBuf)> = Vec::new();
    let mut aside: Vec<PathBuf> = Vec::new();
    let mut installed: Vec<PathBuf> = Vec::new();
    let mut run = || -> Result<(), String> {
        for (area, rel) in &spec.units {
            let Some(root) = spec.root(area) else { continue };
            let fin = root.join(rel);
            if have.contains(&fin) {
                let dst = free_name(&format!("{}.broken-{}", fin.display(), stamp(now)));
                rename(&fin, &dst).map_err(|e| format!("cannot move {} aside: {e}", fin.display()))?;
                done.push((fin.clone(), dst.clone()));
                aside.push(dst);
            }
            let Some(sd) = st.for_root(root) else { continue };
            let staged = sd.join(rel);
            if fs::symlink_metadata(&staged).is_err() {
                continue;
            }
            if let Some(dir) = fin.parent() {
                fs::DirBuilder::new().recursive(true).mode(0o700).create(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
            }
            // A SQLite file must not meet an old -wal, -shm or -journal file:
            // SQLite could replay it onto the restored file. Move those aside too.
            if spec.snapshots.iter().any(|n| *n == format!("{area}/{rel}")) {
                for suf in ["-wal", "-shm", "-journal"] {
                    aside_if_there(Path::new(&format!("{}{suf}", fin.display())), now, &mut done, &mut aside)?;
                }
            }
            // Something may have appeared at the target since the first look.
            // Move it aside too. An empty directory holds nothing and is
            // replaced by the rename.
            #[cfg(test)]
            if let Some(hook) = BEFORE_INSTALL.with(|h| *h.borrow()) {
                hook(&fin);
            }
            if fs::symlink_metadata(&fin).is_ok_and(|m| !(m.is_dir() && fs::read_dir(&fin).is_ok_and(|mut d| d.next().is_none()))) {
                aside_if_there(&fin, now, &mut done, &mut aside)?;
            }
            rename(&staged, &fin).map_err(|e| format!("cannot put {} in place: {e}", fin.display()))?;
            done.push((staged, fin.clone()));
            installed.push(fin);
        }
        Ok(())
    };
    if let Err(e) = run() {
        let mut stuck: Vec<PathBuf> = Vec::new();
        for (from, to) in done.iter().rev() {
            if rename(to, from).is_err() {
                stuck.push(to.clone());
            }
        }
        let left: Vec<String> = aside.iter().filter(|a| stuck.contains(a)).map(|a| a.display().to_string()).collect();
        let mut msg = format!("{e}. The restored files stay in {}", st.list());
        if stuck.is_empty() {
            msg.push_str(". The old data is back in place");
        } else {
            let s: Vec<String> = stuck.iter().map(|p| p.display().to_string()).collect();
            msg.push_str(&format!(". These could not be moved back: {}", s.join(", ")));
        }
        return Err(Failure { msg, aside: left });
    }
    // Everything is in place. Make it durable, then drop the staging.
    for p in &installed {
        if let Some(d) = p.parent() {
            sync_dir(d);
        }
    }
    for (root, _) in &st.dirs {
        sync_dir(root);
    }
    st.discard();
    Ok(Done { files, bytes, aside: aside.iter().map(|a| a.display().to_string()).collect(), left: 0 })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A temp dir that goes away on drop, on failure too.
    struct Tmp(PathBuf);
    impl Tmp {
        fn new(tag: &str) -> Tmp {
            let p = std::env::temp_dir().join(format!("coppice-backup-{tag}-{}-{:?}", std::process::id(), std::thread::current().id()).replace(['(', ')', ' '], ""));
            let _ = fs::remove_dir_all(&p);
            fs::create_dir_all(&p).unwrap();
            Tmp(p)
        }
        fn p(&self, s: &str) -> PathBuf {
            self.0.join(s)
        }
    }
    impl Drop for Tmp {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn put(t: &Tmp, rel: &str, body: &str) {
        let p = t.p(rel);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::write(p, body).unwrap();
    }

    fn spec(root: &Path) -> Spec {
        Spec {
            tool: "daisugi".into(),
            version: "9.9".into(),
            roots: vec![("data".into(), root.to_path_buf())],
            units: ["config.yaml", "journal", "gate/envelopes"].iter().map(|u| ("data".to_string(), u.to_string())).collect(),
            ..Spec::default()
        }
    }

    const T0: i64 = 1_792_843_200; // 2026-10-24T12:00:00Z

    fn err(r: Result<Done, Failure>) -> String {
        r.unwrap_err().msg
    }

    fn gz_bytes(tar: &[u8]) -> Vec<u8> {
        let mut gz = GzWriter::new(Vec::new(), 0).unwrap();
        gz.write_all(tar).unwrap();
        gz.finish().unwrap()
    }

    fn tar_put(out: &mut Vec<u8>, name: &str, data: &[u8], mtime: u64) -> Result<(), String> {
        out.extend_from_slice(&tar_header(name, data.len() as u64, mtime)?);
        out.extend_from_slice(data);
        out.resize(out.len() + pad_len(data.len() as u64), 0);
        Ok(())
    }

    /// A member of another kind: the header of a regular file with its type
    /// byte changed and the checksum made again.
    fn tar_kind(out: &mut Vec<u8>, name: &str, kind: u8, data: &[u8]) {
        let mut h = tar_header(name, data.len() as u64, 0).unwrap();
        h[156] = kind;
        h[148..156].copy_from_slice(b"        ");
        let sum: u32 = h.iter().map(|b| u32::from(*b)).sum();
        h[148..155].copy_from_slice(format!("{sum:06o}\0").as_bytes());
        out.extend_from_slice(&h);
        out.extend_from_slice(data);
        out.resize(out.len() + pad_len(data.len() as u64), 0);
    }

    fn manifest(files: &[(&str, &str)]) -> String {
        let list: Vec<String> = files
            .iter()
            .map(|(n, body)| format!(r#"{{"path":"{n}","size":{},"sha256":"{}"}}"#, body.len(), sha256_hex(body.as_bytes())))
            .collect();
        format!(r#"{{"format":1,"tool":"daisugi","version":"1","created":"x","roots":{{}},"files":[{}]}}"#, list.join(","))
    }

    fn write_archive(out: &Tmp, tar: &mut Vec<u8>) -> PathBuf {
        tar.resize(tar.len() + 1024, 0);
        let p = out.p("e.tgz");
        fs::write(&p, gz_bytes(tar)).unwrap();
        p
    }

    #[test]
    fn civil_dates() {
        assert_eq!(rfc3339(T0), "2026-10-24T12:00:00Z");
        assert_eq!(stamp(0), "19700101T000000Z");
        assert_eq!(date(T0), "2026-10-24");
    }

    #[test]
    fn round_trip_keeps_allowed_and_drops_secrets() {
        let (src, dst, out) = (Tmp::new("a1"), Tmp::new("a2"), Tmp::new("a3"));
        put(&src, "config.yaml", "a: 1\n");
        put(&src, "journal/index.db", "db");
        put(&src, "journal/traces/x.json", "{}");
        put(&src, "gate/envelopes/s.json", "{}");
        put(&src, "gate/token", "SECRET");
        put(&src, "journal/api-token.txt", "SECRET");
        put(&src, "journal/gate.lock", "");
        put(&src, "keys/id_ed25519", "SECRET");
        let file = out.p("b.tgz");
        let r = create(&spec(&src.0), &file, T0).unwrap();
        assert_eq!(r.files, 4);
        assert_eq!(r.left, 2);
        restore(&spec(&dst.0), &file, false, T0).unwrap();
        for p in ["config.yaml", "journal/index.db", "journal/traces/x.json", "gate/envelopes/s.json"] {
            assert!(dst.p(p).exists(), "{p}");
        }
        for p in ["gate/token", "journal/api-token.txt", "journal/gate.lock", "keys"] {
            assert!(!dst.p(p).exists(), "{p}");
        }
        use std::os::unix::fs::MetadataExt;
        assert_eq!(fs::metadata(&file).unwrap().mode() & 0o777, 0o600);
        assert_eq!(fs::read_dir(out.p("")).unwrap().count(), 1, "temp files were left");
    }

    #[test]
    fn restore_refuses_full_target_then_force_moves_aside() {
        let (src, dst, out) = (Tmp::new("b1"), Tmp::new("b2"), Tmp::new("b3"));
        put(&src, "config.yaml", "new\n");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        put(&dst, "config.yaml", "old\n");
        put(&dst, "keep-me.key", "K");
        let e = err(restore(&spec(&dst.0), &file, false, T0));
        assert!(e.contains("--force"), "{e}");
        assert_eq!(fs::read_to_string(dst.p("config.yaml")).unwrap(), "old\n");
        let r = restore(&spec(&dst.0), &file, true, T0).unwrap();
        assert_eq!(r.aside.len(), 1);
        assert_eq!(fs::read_to_string(dst.p("config.yaml")).unwrap(), "new\n");
        assert_eq!(fs::read_to_string(dst.p("config.yaml.broken-20261024T120000Z")).unwrap(), "old\n");
        assert!(dst.p("keep-me.key").exists());
        assert!(!dst.p(".restore-20261024T120000Z").exists(), "staging dir left after success");
    }

    #[test]
    fn an_empty_target_dir_counts_as_empty() {
        let (src, dst, out) = (Tmp::new("g1"), Tmp::new("g2"), Tmp::new("g3"));
        put(&src, "journal/a", "1");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        fs::create_dir_all(dst.p("journal")).unwrap();
        restore(&spec(&dst.0), &file, false, T0).unwrap();
        assert_eq!(fs::read_to_string(dst.p("journal/a")).unwrap(), "1");
    }

    #[test]
    fn create_refuses_existing_file_and_empty_data() {
        let (src, out) = (Tmp::new("c1"), Tmp::new("c2"));
        let file = out.p("b.tgz");
        assert!(create(&spec(&src.0), &file, T0).unwrap_err().contains("nothing to back up"));
        put(&src, "config.yaml", "x");
        put(&out, "b.tgz", "precious");
        assert!(create(&spec(&src.0), &file, T0).unwrap_err().contains("already exists"));
        assert_eq!(fs::read_to_string(&file).unwrap(), "precious");
    }

    #[test]
    fn restore_refuses_foreign_tool_and_garbage() {
        let (src, dst, out) = (Tmp::new("d1"), Tmp::new("d2"), Tmp::new("d3"));
        put(&src, "config.yaml", "x");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        let mut other = spec(&dst.0);
        other.tool = "coppice".into();
        assert!(err(restore(&other, &file, false, T0)).contains("daisugi backup, not a coppice"));
        put(&out, "j.tgz", "not gzip");
        assert!(restore(&spec(&dst.0), &out.p("j.tgz"), false, T0).is_err());
    }

    fn evil(out: &Tmp, name: &str, sha: &str) -> PathBuf {
        let man = format!(r#"{{"format":1,"tool":"daisugi","version":"1","created":"x","roots":{{}},"files":[{{"path":"{name}","size":1,"sha256":"{sha}"}}]}}"#);
        let mut tar = Vec::new();
        tar_put(&mut tar, MANIFEST, man.as_bytes(), 0).unwrap();
        tar_put(&mut tar, name, b"x", 0).unwrap();
        write_archive(out, &mut tar)
    }

    #[test]
    fn restore_refuses_paths_off_the_allow_list() {
        for name in ["data/../../etc/x", "data/keys/id_ed25519", "data/journal/../config.yaml", "/abs", "other/config.yaml", "data/journal/api-token"] {
            let (dst, out) = (Tmp::new("e1"), Tmp::new("e2"));
            let file = evil(&out, name, "00");
            assert!(restore(&spec(&dst.0), &file, false, T0).is_err(), "{name}");
            assert_eq!(fs::read_dir(&dst.0).unwrap().count(), 0, "{name}");
        }
    }

    #[test]
    fn checksum_failure_changes_nothing() {
        let (dst, out) = (Tmp::new("f1"), Tmp::new("f2"));
        let file = evil(&out, "data/config.yaml", "00");
        assert!(err(restore(&spec(&dst.0), &file, false, T0)).contains("checksum"));
        assert_eq!(fs::read_dir(&dst.0).unwrap().count(), 0);
    }

    #[test]
    fn long_names_use_the_ustar_prefix() {
        let name = format!("data/journal/{}/{}", "d".repeat(60), "f".repeat(60));
        let mut tar = Vec::new();
        tar_put(&mut tar, &name, b"hi", 0).unwrap();
        let mut r = TarReader::new("t", io::Cursor::new(tar));
        assert_eq!(r.next().unwrap().unwrap().name, name);
    }

    #[test]
    fn restore_refuses_links_pax_duplicates_and_empty_lists() {
        let m = manifest(&[("data/config.yaml", "x")]);
        let kinds: [(&str, Vec<(u8, &str)>); 6] = [
            ("symlink", vec![(b'2', "")]),
            ("hardlink", vec![(b'1', "")]),
            ("pax", vec![(b'x', "30 comment=hi\n")]),
            ("gnu long name", vec![(b'L', "data/config.yaml\0")]),
            ("duplicate", vec![(b'0', "x"), (b'0', "x")]),
            ("directory", vec![(b'5', "")]),
        ];
        for (what, members) in kinds {
            let (dst, out) = (Tmp::new("h1"), Tmp::new("h2"));
            let mut tar = Vec::new();
            tar_put(&mut tar, MANIFEST, m.as_bytes(), 0).unwrap();
            for (k, body) in members {
                tar_kind(&mut tar, "data/config.yaml", k, body.as_bytes());
            }
            let file = write_archive(&out, &mut tar);
            assert!(restore(&spec(&dst.0), &file, false, T0).is_err(), "{what}");
            assert_eq!(fs::read_dir(&dst.0).unwrap().count(), 0, "{what} left something");
        }
        let (dst, out) = (Tmp::new("h3"), Tmp::new("h4"));
        let mut tar = Vec::new();
        tar_put(&mut tar, MANIFEST, manifest(&[]).as_bytes(), 0).unwrap();
        let file = write_archive(&out, &mut tar);
        assert!(err(restore(&spec(&dst.0), &file, false, T0)).contains("lists no files"));
        assert_eq!(fs::read_dir(&dst.0).unwrap().count(), 0);
    }

    #[test]
    fn a_failed_swap_puts_the_old_files_back() {
        let (src, dst, out) = (Tmp::new("i1"), Tmp::new("i2"), Tmp::new("i3"));
        put(&src, "config.yaml", "new\n");
        put(&src, "journal/a", "new");
        put(&src, "gate/envelopes/s.json", "{}");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        put(&dst, "config.yaml", "old\n");
        put(&dst, "journal/old", "old");
        // "gate" is a file, so gate/envelopes cannot be put in place.
        put(&dst, "gate", "a file");
        let f = restore(&spec(&dst.0), &file, true, T0).unwrap_err();
        assert!(f.msg.contains("back in place") && f.msg.contains(".restore-"), "{}", f.msg);
        assert!(f.aside.is_empty(), "{:?}", f.aside);
        assert_eq!(fs::read_to_string(dst.p("config.yaml")).unwrap(), "old\n");
        assert_eq!(fs::read_to_string(dst.p("journal/old")).unwrap(), "old");
        assert_eq!(fs::read_to_string(dst.p("gate")).unwrap(), "a file");
        assert!(!dst.p("journal/a").exists());
        let names: Vec<String> = fs::read_dir(&dst.0).unwrap().map(|e| e.unwrap().file_name().to_string_lossy().into_owned()).collect();
        assert!(names.iter().all(|n| !n.contains(".broken-")), "{names:?}");
        assert!(dst.p(".restore-20261024T120000Z/config.yaml").exists(), "staging dir was not kept: {names:?}");
    }

    #[test]
    fn a_failed_undo_returns_what_stays_aside() {
        let (src, dst, out) = (Tmp::new("j1"), Tmp::new("j2"), Tmp::new("j3"));
        put(&src, "config.yaml", "new\n");
        put(&src, "gate/envelopes/s.json", "{}");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        put(&dst, "config.yaml", "old\n");
        put(&dst, "gate", "a file");
        FAIL_RENAME.with(|f| *f.borrow_mut() = Some(|from, _| from.to_string_lossy().contains(".broken-")));
        let r = restore(&spec(&dst.0), &file, true, T0);
        FAIL_RENAME.with(|f| *f.borrow_mut() = None);
        let f = r.unwrap_err();
        assert!(f.msg.contains("could not be moved back"), "{}", f.msg);
        assert_eq!(f.aside.len(), 1);
        assert!(f.aside[0].ends_with("config.yaml.broken-20261024T120000Z"), "{:?}", f.aside);
    }

    #[test]
    fn backup_refuses_a_symlinked_unit_root() {
        let (src, real, out) = (Tmp::new("k1"), Tmp::new("k2"), Tmp::new("k3"));
        put(&src, "config.yaml", "x");
        put(&real, "a", "1");
        std::os::unix::fs::symlink(&real.0, src.p("journal")).unwrap();
        let e = create(&spec(&src.0), &out.p("b.tgz"), T0).unwrap_err();
        assert!(e.contains(&format!("{} is a symlink", src.p("journal").display())), "{e}");
        assert!(!out.p("b.tgz").exists());
    }

    #[test]
    fn backup_stops_when_a_file_changes_under_the_copy() {
        let (src, out) = (Tmp::new("l1"), Tmp::new("l2"));
        put(&src, "config.yaml", "x");
        AFTER_READ.with(|h| {
            *h.borrow_mut() = Some(|p| {
                let f = fs::OpenOptions::new().write(true).open(p).unwrap();
                f.set_modified(std::time::SystemTime::now() + std::time::Duration::from_secs(3600)).unwrap();
            })
        });
        let r = create(&spec(&src.0), &out.p("b.tgz"), T0);
        AFTER_READ.with(|h| *h.borrow_mut() = None);
        assert!(r.unwrap_err().contains("changed while it was copied"));
        assert_eq!(fs::read_dir(&out.0).unwrap().count(), 0, "temp files were left");
    }

    #[test]
    fn snapshots_replace_the_live_file_and_drop_their_sidecars() {
        let (src, dst, out) = (Tmp::new("m1"), Tmp::new("m2"), Tmp::new("m3"));
        put(&src, "journal/index.db", "live");
        put(&src, "journal/index.db-wal", "wal");
        put(&src, "journal/index.db-shm", "shm");
        let mut s = spec(&src.0);
        s.snapshots = vec!["data/journal/index.db".into()];
        s.snap = Some(|from, to| fs::write(to, format!("snap of {}", from.file_name().unwrap().to_string_lossy())).map_err(|e| e.to_string()));
        let file = out.p("b.tgz");
        let r = create(&s, &file, T0).unwrap();
        assert_eq!((r.files, r.left), (1, 0));
        restore(&spec(&dst.0), &file, false, T0).unwrap();
        assert_eq!(fs::read_to_string(dst.p("journal/index.db")).unwrap(), "snap of index.db");
        assert!(!dst.p("journal/index.db-wal").exists());
        assert!(!out.p("b.tgz.snap").exists());
    }

    /// Bytes that do not compress, from a small generator.
    fn noise(n: usize) -> String {
        let mut x = 0x2545F4914F6CDD1Du64;
        (0..n)
            .map(|_| {
                x ^= x << 13;
                x ^= x >> 7;
                x ^= x << 17;
                (b'a' + (x % 26) as u8) as char
            })
            .collect()
    }

    #[test]
    fn many_blocks_and_many_files_round_trip_through_the_streams() {
        let (src, dst, out) = (Tmp::new("n1"), Tmp::new("n2"), Tmp::new("n3"));
        let big = noise(3 << 20);
        put(&src, "journal/big.bin", &big);
        for i in 0..200 {
            put(&src, &format!("journal/traces/t{i}.json"), &format!("{{\"i\":{i}}}\n").repeat(i + 1));
        }
        let file = out.p("b.tgz");
        let r = create(&spec(&src.0), &file, T0).unwrap();
        assert_eq!(r.files, 201);
        restore(&spec(&dst.0), &file, false, T0).unwrap();
        assert_eq!(fs::read_to_string(dst.p("journal/big.bin")).unwrap(), big);
        assert_eq!(fs::read_to_string(dst.p("journal/traces/t199.json")).unwrap(), "{\"i\":199}\n".repeat(200));
    }

    #[test]
    fn a_cut_or_flipped_archive_changes_nothing() {
        let (src, dst, out) = (Tmp::new("o1"), Tmp::new("o2"), Tmp::new("o3"));
        put(&src, "journal/big.bin", &noise(1 << 20));
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        let good = fs::read(&file).unwrap();
        let cut = out.p("cut.tgz");
        fs::write(&cut, &good[..good.len() / 2]).unwrap();
        assert!(restore(&spec(&dst.0), &cut, false, T0).is_err());
        let flip = out.p("flip.tgz");
        let mut bad = good.clone();
        let n = bad.len();
        bad[n - 6] ^= 0xff; // the crc32 in the gzip trailer
        fs::write(&flip, &bad).unwrap();
        assert!(restore(&spec(&dst.0), &flip, false, T0).is_err());
        assert_eq!(fs::read_dir(&dst.0).unwrap().count(), 0, "a bad archive left files");
    }

    fn snap_spec(root: &Path) -> Spec {
        let mut s = spec(root);
        s.units.push(("data".into(), "pathways.db".into()));
        s.snapshots = vec!["data/pathways.db".into()];
        s.snap = Some(|_, to| fs::write(to, "snap").map_err(|e| e.to_string()));
        s
    }

    #[test]
    fn a_stale_wal_goes_aside_with_the_database() {
        let (src, dst, out) = (Tmp::new("r1"), Tmp::new("r2"), Tmp::new("r3"));
        put(&src, "pathways.db", "new db");
        let file = out.p("b.tgz");
        create(&snap_spec(&src.0), &file, T0).unwrap();
        put(&dst, "pathways.db", "old db");
        put(&dst, "pathways.db-wal", "stale wal");
        put(&dst, "pathways.db-shm", "stale shm");
        let r = restore(&snap_spec(&dst.0), &file, true, T0).unwrap();
        assert_eq!(r.aside.len(), 3, "{:?}", r.aside);
        assert_eq!(fs::read_to_string(dst.p("pathways.db")).unwrap(), "snap");
        for n in ["-wal", "-shm"] {
            assert!(!dst.p(&format!("pathways.db{n}")).exists(), "{n} stayed");
            assert!(dst.p(&format!("pathways.db{n}.broken-20261024T120000Z")).exists(), "{n} not kept");
        }
        // With no old database and no --force, a stale wal still goes aside.
        let dst2 = Tmp::new("r4");
        put(&dst2, "pathways.db-wal", "stale wal");
        restore(&snap_spec(&dst2.0), &file, false, T0).unwrap();
        assert!(!dst2.p("pathways.db-wal").exists());
    }

    #[test]
    fn a_file_that_appears_before_the_install_goes_aside() {
        let (src, dst, out) = (Tmp::new("s1"), Tmp::new("s2"), Tmp::new("s3"));
        put(&src, "config.yaml", "new\n");
        let file = out.p("b.tgz");
        create(&spec(&src.0), &file, T0).unwrap();
        BEFORE_INSTALL.with(|h| *h.borrow_mut() = Some(|p| if p.ends_with("config.yaml") { fs::write(p, "late\n").unwrap() }));
        let r = restore(&spec(&dst.0), &file, false, T0);
        BEFORE_INSTALL.with(|h| *h.borrow_mut() = None);
        let r = r.unwrap();
        assert_eq!(r.aside.len(), 1);
        assert_eq!(fs::read_to_string(dst.p("config.yaml")).unwrap(), "new\n");
        assert_eq!(fs::read_to_string(&r.aside[0]).unwrap(), "late\n");
    }

    #[test]
    fn a_failed_stage_removes_the_empty_root_it_made() {
        let (base, out) = (Tmp::new("t1"), Tmp::new("t2"));
        let root = base.p("sub");
        let file = evil(&out, "data/config.yaml", "00");
        assert!(restore(&spec(&root), &file, false, T0).is_err());
        assert!(!root.exists(), "the empty root stayed");
        let there = Tmp::new("t3");
        let _ = restore(&spec(&there.0), &file, false, T0);
        assert!(there.0.exists());
    }

    /// The shape `coppice backup` has: three areas and its allow-list.
    fn coppice_spec(data: &Path, cfg: &Path) -> Spec {
        let units = [
            ("data", "layout.json"),
            ("data", "recent-dirs.json"),
            ("foreman", "main"),
            ("foreman", "tree"),
            ("foreman", "view.json"),
            ("config", "coppice.toml"),
            ("config", "pools.toml"),
        ];
        Spec {
            tool: "coppice".into(),
            version: "9.9".into(),
            roots: vec![("data".into(), data.to_path_buf()), ("foreman".into(), data.join("foreman")), ("config".into(), cfg.to_path_buf())],
            units: units.iter().map(|(a, u)| (a.to_string(), u.to_string())).collect(),
            ..Spec::default()
        }
    }

    fn coppice_files(t: &Tmp, who: &str) {
        put(t, "d/layout.json", &format!("{{\"from\":\"{who}\"}}\n"));
        put(t, "d/foreman/main/2026-10-24.jsonl", "{}\n");
        put(t, "d/foreman/tree/a.jsonl", "{}\n");
        put(t, "d/foreman/view.json", "{}");
        put(t, "c/coppice.toml", "fence = \"auto\"\n");
    }

    const GO_FIXTURE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/testdata/backup/go-made.tgz");

    /// The archive the Go coppice made reads here. The Go coppice reads
    /// testdata/rust-made.tgz in the same way. MAKE_FIXTURE=<dir> writes a
    /// new rust-made.tgz there.
    #[test]
    fn reads_the_archive_the_go_port_made() {
        let dst = Tmp::new("p1");
        let r = restore(&coppice_spec(&dst.p("d"), &dst.p("c")), Path::new(GO_FIXTURE), false, T0).unwrap();
        assert_eq!(r.files, 5);
        assert_eq!(fs::read_to_string(dst.p("d/layout.json")).unwrap(), "{\"from\":\"go\"}\n");
        assert_eq!(fs::read_to_string(dst.p("d/foreman/tree/a.jsonl")).unwrap(), "{}\n");
        assert_eq!(fs::read_to_string(dst.p("c/coppice.toml")).unwrap(), "fence = \"auto\"\n");
    }

    #[test]
    fn make_fixture() {
        let Ok(dir) = std::env::var("MAKE_FIXTURE") else { return };
        let src = Tmp::new("q1");
        coppice_files(&src, "rust");
        create(&coppice_spec(&src.p("d"), &src.p("c")), &Path::new(&dir).join("rust-made.tgz"), T0).unwrap();
    }
}
