//! The bearer token, the named tokens beside it, and the ban list for an
//! address that guesses.

use std::collections::HashMap;
use std::io::Write;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde_json::json;

use crate::godec::{self, StructTy, Ty};
use crate::sys;

/// The entropy behind one web token. In unpadded base64url it is 43
/// characters, each legal in a Sec-WebSocket-Protocol offer.
pub const TOKEN_BYTES: usize = 32;

/// The refusal for a name that breaks the name rule.
pub const NAME_RULE: &str = "names are one word, up to 32 characters";

/// Refuses a name that is empty, over 32 bytes, or holds a character other
/// than an ASCII letter, a digit, a dash or an underscore.
pub fn check_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > 32 {
        return Err(NAME_RULE.into());
    }
    if !name
        .chars()
        .all(|r| r.is_ascii_alphanumeric() || r == '-' || r == '_')
    {
        return Err(NAME_RULE.into());
    }
    Ok(())
}

/// No token has been minted.
pub const NO_TOKEN: &str = "no web token";

/// n random bytes from the kernel.
pub fn random_bytes(n: usize) -> Result<Vec<u8>, String> {
    let mut b = vec![0u8; n];
    let mut got = 0;
    while got < n {
        // SAFETY: the buffer is live and holds n - got bytes from got.
        let r = unsafe { libc::getrandom(b[got..].as_mut_ptr().cast(), n - got, 0) };
        if r < 0 {
            let e = std::io::Error::last_os_error();
            if e.kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return Err(e.to_string());
        }
        got += r as usize;
    }
    Ok(b)
}

/// Unpadded base64url, Go's base64.RawURLEncoding.
pub fn base64_raw_url(b: &[u8]) -> String {
    const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::new();
    for ch in b.chunks(3) {
        let n = ch
            .iter()
            .enumerate()
            .fold(0u32, |n, (i, c)| n | (*c as u32) << (16 - 8 * i));
        for i in 0..ch.len() + 1 {
            out.push(A[((n >> (18 - 6 * i)) & 63) as usize] as char);
        }
    }
    out
}

/// A fresh token.
pub fn new_token() -> Result<String, String> {
    Ok(base64_raw_url(&random_bytes(TOKEN_BYTES)?))
}

/// Compares a and b in time that depends only on their lengths.
pub fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

/// The operator's token in a file mode 0600 in a directory mode 0700, and
/// the named tokens beside it.
#[derive(Clone, Debug)]
pub struct TokenStore {
    pub path: PathBuf,
}

static NAMED_TY: StructTy = StructTy {
    name: "namedToken",
    text: "web.namedToken",
    fields: &[("name", Ty::Str), ("token", Ty::Str)],
};
static NAMED_LIST: Ty = Ty::Struct(&NAMED_TY);
static NAMES_TY: StructTy = StructTy {
    name: "",
    text: "struct { Tokens []web.namedToken \"json:\\\"tokens\\\"\" }",
    fields: &[("tokens", Ty::Slice(&NAMED_LIST, "[]web.namedToken"))],
};

impl TokenStore {
    pub fn new(path: PathBuf) -> TokenStore {
        TokenStore { path }
    }

    pub fn names_path(&self) -> PathBuf {
        PathBuf::from(format!("{}-names.json", self.path.display()))
    }

    /// The named tokens. A missing file is none; one that does not read
    /// or parse is an error.
    fn load_named(&self) -> Result<Vec<(String, String)>, String> {
        let p = self.names_path();
        let raw = match std::fs::read(&p) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(e) => return Err(sys::go_path_err("open", &p, &e)),
        };
        let (g, err) = godec::decode(&raw, &Ty::Struct(&NAMES_TY));
        if let Some(e) = err {
            return Err(format!("cannot read {}: {e}", p.display()));
        }
        Ok(g.f("tokens")
            .items()
            .iter()
            .map(|r| (r.f("name").s(), r.f("token").s()))
            .collect())
    }

    fn save_named(&self, rows: &[(String, String)]) -> Result<(), String> {
        let list: Vec<serde_json::Value> = rows
            .iter()
            .map(|(n, t)| crate::gojson::obj(vec![("name", json!(n)), ("token", json!(t))]))
            .collect();
        let body = crate::gojson::marshal(&crate::gojson::map(vec![(
            "tokens",
            serde_json::Value::Array(list),
        )]));
        write_private(&self.names_path(), ".web-names-", body.as_bytes())
    }

    /// An exclusive lock on the names file's lock file, released on drop.
    fn lock_names(&self) -> Result<std::fs::File, String> {
        let path = PathBuf::from(format!("{}.lock", self.names_path().display()));
        let dir = path.parent().map(PathBuf::from).unwrap_or_default();
        super::config::mkdir_private(&dir)?;
        let f = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(&path)
            .map_err(|e| sys::go_path_err("open", &path, &e))?;
        use std::os::fd::AsRawFd;
        // SAFETY: the fd is live while f is.
        if unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX) } != 0 {
            return Err(std::io::Error::last_os_error().to_string());
        }
        Ok(f)
    }

    /// Writes a fresh token for name and retires any token name had.
    pub fn mint_for(&self, name: &str) -> Result<String, String> {
        check_name(name)?;
        let _lock = self.lock_names()?;
        let rows = self.load_named()?;
        let tok = new_token()?;
        let mut kept = vec![(name.to_string(), tok.clone())];
        kept.extend(rows.into_iter().filter(|(n, _)| n != name));
        kept.sort_by(|a, b| a.0.cmp(&b.0));
        self.save_named(&kept)?;
        Ok(tok)
    }

    /// The token minted for name, or NO_TOKEN.
    pub fn lookup(&self, name: &str) -> Result<String, String> {
        let rows = self.load_named()?;
        for (n, t) in rows {
            if n == name && !t.is_empty() {
                return Ok(t);
            }
        }
        Err(NO_TOKEN.into())
    }

    /// Retires the token minted for name.
    pub fn revoke(&self, name: &str) -> Result<(), String> {
        let _lock = self.lock_names()?;
        let rows = self.load_named()?;
        let n = rows.len();
        let kept: Vec<(String, String)> = rows.into_iter().filter(|(r, _)| r != name).collect();
        if kept.len() == n {
            return Err(format!(
                "no token has the name {name}. Run: coppice web token list"
            ));
        }
        self.save_named(&kept)
    }

    /// The names that hold a token, in order.
    pub fn names(&self) -> Result<Vec<String>, String> {
        Ok(self.load_named()?.into_iter().map(|(n, _)| n).collect())
    }

    /// The name candidate carries: Some("") for the operator's token,
    /// Some(name) for a named one, None for no token. Every comparison
    /// runs in constant time.
    pub fn who(&self, candidate: &str) -> Option<String> {
        if candidate.is_empty() {
            return None;
        }
        let c = candidate.as_bytes();
        let mut ok = false;
        if let Ok(stored) = self.load() {
            if ct_eq(c, stored.as_bytes()) {
                ok = true;
            }
        }
        let rows = match self.load_named() {
            Ok(r) => r,
            Err(_) => return ok.then(String::new),
        };
        let mut name = String::new();
        for (n, t) in rows {
            if t.is_empty() || check_name(&n).is_err() {
                continue;
            }
            if ct_eq(c, t.as_bytes()) && !ok {
                name = n;
                ok = true;
            }
        }
        ok.then_some(name)
    }

    /// The stored token. Missing, unreadable or blank is NO_TOKEN.
    pub fn load(&self) -> Result<String, String> {
        let raw = std::fs::read(&self.path).map_err(|_| NO_TOKEN.to_string())?;
        let tok = String::from_utf8_lossy(&raw).trim().to_string();
        if tok.is_empty() {
            return Err(NO_TOKEN.into());
        }
        Ok(tok)
    }

    /// Writes a fresh token over any token there.
    pub fn mint(&self) -> Result<String, String> {
        let tok = new_token()?;
        write_private(&self.path, ".web-token-", tok.as_bytes())?;
        Ok(tok)
    }
}

/// Writes body to path mode 0600 in a directory mode 0700, through a temp
/// file in the same directory renamed into place.
pub fn write_private(path: &Path, prefix: &str, body: &[u8]) -> Result<(), String> {
    let dir = path.parent().map(PathBuf::from).unwrap_or_default();
    super::config::mkdir_private(&dir)?;
    let mut tmp = PathBuf::new();
    let mut f = None;
    for _ in 0..100 {
        let r = random_bytes(4)?;
        let n = u32::from_le_bytes([r[0], r[1], r[2], r[3]]);
        tmp = dir.join(format!("{prefix}{n}"));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
        {
            Ok(file) => {
                f = Some(file);
                break;
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(sys::go_path_err("open", &tmp, &e)),
        }
    }
    let Some(mut f) = f else {
        return Err(format!("open {}: file exists", tmp.display()));
    };
    let res = (|| {
        f.set_permissions(std::fs::Permissions::from_mode(0o600))
            .map_err(|e| sys::go_path_err("chmod", &tmp, &e))?;
        f.write_all(body)
            .map_err(|e| sys::go_path_err("write", &tmp, &e))?;
        drop(f);
        std::fs::rename(&tmp, path).map_err(|e| {
            format!(
                "rename {} {}: {}",
                tmp.display(),
                path.display(),
                sys::go_errno_text(&e)
            )
        })
    })();
    let _ = std::fs::remove_file(&tmp);
    res
}

/// Three bad tokens from one address inside a minute earn that address a
/// minute of silence. A ban holds even if a good token arrives during it.
pub struct Banlist {
    inner: Mutex<BanState>,
}

struct BanState {
    fails: HashMap<String, Vec<Instant>>,
    banned: HashMap<String, Instant>,
}

const BAN_FAILURES: usize = 3;
const BAN_WINDOW: Duration = Duration::from_secs(60);
const BAN_DURATION: Duration = Duration::from_secs(60);

impl Banlist {
    pub fn new() -> Banlist {
        Banlist {
            inner: Mutex::new(BanState {
                fails: HashMap::new(),
                banned: HashMap::new(),
            }),
        }
    }

    /// Whether addr is serving a ban. A ban whose time has passed is
    /// cleared, and old failures are swept.
    pub fn banned(&self, addr: &str) -> bool {
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        let now = Instant::now();
        st.fails.retain(|_, times| {
            times
                .last()
                .is_some_and(|t| now.duration_since(*t) < BAN_WINDOW)
        });
        let Some(until) = st.banned.get(addr).copied() else {
            return false;
        };
        if now >= until {
            st.banned.remove(addr);
            return false;
        }
        true
    }

    /// Records one bad token from addr.
    pub fn fail(&self, addr: &str) {
        let mut st = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        let now = Instant::now();
        let mut recent: Vec<Instant> = st
            .fails
            .get(addr)
            .map(|v| {
                v.iter()
                    .copied()
                    .filter(|t| now.duration_since(*t) < BAN_WINDOW)
                    .collect()
            })
            .unwrap_or_default();
        recent.push(now);
        if recent.len() >= BAN_FAILURES {
            st.banned.insert(addr.to_string(), now + BAN_DURATION);
            st.fails.remove(addr);
        } else {
            st.fails.insert(addr.to_string(), recent);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_follow_the_rule() {
        assert!(check_name("kid").is_ok());
        assert!(check_name("a-b_9").is_ok());
        assert_eq!(check_name("").unwrap_err(), NAME_RULE);
        assert_eq!(check_name("bad name").unwrap_err(), NAME_RULE);
        assert_eq!(check_name(&"a".repeat(33)).unwrap_err(), NAME_RULE);
        assert!(check_name(&"a".repeat(32)).is_ok());
    }

    #[test]
    fn a_token_is_43_url_safe_characters() {
        let t = new_token().unwrap();
        assert_eq!(t.len(), 43);
        assert!(t
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_'));
        assert_eq!(base64_raw_url(b"\xfb\xff"), "-_8");
    }

    #[test]
    fn three_strikes_ban_an_address() {
        let b = Banlist::new();
        b.fail("a");
        b.fail("a");
        assert!(!b.banned("a"));
        b.fail("a");
        assert!(b.banned("a"));
        assert!(!b.banned("b"));
    }
}
