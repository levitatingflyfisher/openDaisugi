//! The single-instance start lock: an flock on <data dir>/server.lock. The
//! kernel releases it when the process dies, however it dies, so a crash
//! never leaves a stale lock behind. The holder writes its pid into the
//! file for the next caller's refusal.

use std::fs::{File, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};

pub struct StartLock {
    f: File,
}

impl Drop for StartLock {
    fn drop(&mut self) {
        // SAFETY: the descriptor is open for the life of self.f.
        unsafe {
            libc::flock(self.f.as_raw_fd(), libc::LOCK_UN);
        }
    }
}

pub enum LockError {
    /// Another process holds it; the pid it recorded, or 0.
    Held(i32),
    Io(io::Error),
}

pub fn lock_file(data_dir: &Path) -> PathBuf {
    data_dir.join("server.lock")
}

fn open(path: &Path) -> io::Result<File> {
    OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)
}

/// Takes the lock without blocking.
pub fn acquire(data_dir: &Path) -> Result<StartLock, LockError> {
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(data_dir)
        .map_err(LockError::Io)?;
    let path = lock_file(data_dir);
    let mut f = open(&path).map_err(LockError::Io)?;
    // SAFETY: f is open; flock takes no pointers.
    let rc = unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
    if rc != 0 {
        return Err(LockError::Held(read_pid(&path)));
    }
    f.set_len(0).map_err(LockError::Io)?;
    f.seek(SeekFrom::Start(0)).map_err(LockError::Io)?;
    f.write_all(format!("{}\n", std::process::id()).as_bytes())
        .map_err(LockError::Io)?;
    Ok(StartLock { f })
}

/// Whether another process holds the lock now, without taking it.
pub fn held(data_dir: &Path) -> io::Result<bool> {
    let f = open(&lock_file(data_dir))?;
    // SAFETY: f is open.
    let rc = unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
    if rc != 0 {
        return Ok(true);
    }
    // SAFETY: as above.
    unsafe {
        libc::flock(f.as_raw_fd(), libc::LOCK_UN);
    }
    Ok(false)
}

fn read_pid(path: &Path) -> i32 {
    let mut s = String::new();
    if File::open(path)
        .and_then(|mut f| f.read_to_string(&mut s))
        .is_err()
    {
        return 0;
    }
    s.trim().parse().unwrap_or(0)
}
