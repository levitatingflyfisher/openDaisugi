//! `opendaisugi.pack.ustar`: the bundle tar, written byte for byte as
//! Python writes it, and read by the same rules; and the CPython tarball
//! unpack of `manage.unpack_python`.

use std::io::{Read, Write};
use std::path::Path;

const BLOCK: usize = 512;

fn octal(n: u64, width: usize) -> Vec<u8> {
    let mut s = format!("{n:o}").into_bytes();
    while s.len() < width - 1 {
        s.insert(0, b'0');
    }
    s.push(0);
    s
}

fn split_name(name: &str) -> Result<(&[u8], &[u8]), String> {
    let raw = name.as_bytes();
    if raw.len() <= 100 {
        return Ok((&[], raw));
    }
    for i in (1..raw.len()).rev() {
        if raw[i] == b'/' && i <= 155 && raw.len() - i - 1 <= 100 {
            return Ok((&raw[..i], &raw[i + 1..]));
        }
    }
    Err(format!("a name too long for a ustar header: {name}"))
}

/// Writes the header checksum of a 512-byte block.
pub fn fix_checksum(h: &mut [u8]) {
    h[148..156].copy_from_slice(b"        ");
    let sum: u64 = h.iter().map(|&b| b as u64).sum();
    let mut s = format!("{sum:o}").into_bytes();
    while s.len() < 6 {
        s.insert(0, b'0');
    }
    s.push(0);
    s.push(b' ');
    h[148..156].copy_from_slice(&s);
}

const PAX_NAME: &[u8] = b"././@PaxHeader";

fn block(prefix: &[u8], short: &[u8], size: u64, kind: u8) -> Vec<u8> {
    let mut h = vec![0u8; BLOCK];
    h[..short.len()].copy_from_slice(short);
    h[100..108].copy_from_slice(&octal(0o644, 8));
    h[108..116].copy_from_slice(&octal(0, 8));
    h[116..124].copy_from_slice(&octal(0, 8));
    h[124..136].copy_from_slice(&octal(size, 12));
    h[136..148].copy_from_slice(&octal(0, 12));
    h[156] = kind;
    h[257..263].copy_from_slice(b"ustar\0");
    h[263..265].copy_from_slice(b"00");
    h[345..345 + prefix.len()].copy_from_slice(prefix);
    fix_checksum(&mut h);
    h
}

/// `ustar.pax_record`.
fn pax_record(key: &str, value: &str) -> Vec<u8> {
    let body = format!(" {key}={value}\n");
    let mut n = body.len() + 1;
    loop {
        let rec = format!("{n}{body}");
        if rec.len() == n {
            return rec.into_bytes();
        }
        n = rec.len();
    }
}

/// `ustar.header`: after an extended header when the name does not fit
/// the ustar fields.
fn header(name: &str, size: u64) -> Result<Vec<u8>, String> {
    if let Ok((prefix, short)) = split_name(name) {
        return Ok(block(prefix, short, size, b'0'));
    }
    let rec = pax_record("path", name);
    let mut out = block(&[], PAX_NAME, rec.len() as u64, b'x');
    out.extend_from_slice(&rec);
    out.extend(pad(rec.len() as u64));
    let raw = name.as_bytes();
    out.extend(block(&[], &raw[..raw.len().min(100)], size, b'0'));
    Ok(out)
}

/// `ustar.pax_path`.
fn pax_path(mut body: &[u8]) -> Option<String> {
    let mut path = None;
    while !body.is_empty() {
        let Some(sp) = body.iter().position(|&b| b == b' ').filter(|&sp| sp > 0) else {
            break;
        };
        let Ok(n) = String::from_utf8_lossy(&body[..sp]).parse::<usize>() else {
            break;
        };
        if n <= sp || n > body.len() {
            break;
        }
        let rec = &body[sp + 1..n];
        let rec = rec.strip_suffix(b"\n").unwrap_or(rec);
        if let Some(eq) = rec.iter().position(|&b| b == b'=') {
            if &rec[..eq] == b"path" {
                path = Some(String::from_utf8_lossy(&rec[eq + 1..]).into_owned());
            }
        }
        body = &body[n..];
    }
    path
}

fn pad(n: u64) -> Vec<u8> {
    vec![0u8; (BLOCK - (n as usize) % BLOCK) % BLOCK]
}

/// `ustar.write`: the files sorted by name.
pub fn write(files: &[(String, Vec<u8>)]) -> Result<Vec<u8>, String> {
    let mut sorted: Vec<&(String, Vec<u8>)> = files.iter().collect();
    sorted.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    let mut out = vec![];
    for (name, data) in sorted {
        out.extend(header(name, data.len() as u64)?);
        out.extend_from_slice(data);
        out.extend(pad(data.len() as u64));
    }
    out.extend(vec![0u8; 2 * BLOCK]);
    Ok(out)
}

/// `ustar.write_file`: (name, source path) pairs, one file at a time.
pub fn write_file(out: &Path, files: &[(String, std::path::PathBuf)]) -> Result<(), String> {
    let mut sorted: Vec<&(String, std::path::PathBuf)> = files.iter().collect();
    sorted.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    let mut fh = std::fs::File::create(out).map_err(|e| e.to_string())?;
    for (name, src) in sorted {
        let size = std::fs::metadata(src).map_err(|e| e.to_string())?.len();
        fh.write_all(&header(name, size)?)
            .map_err(|e| e.to_string())?;
        let mut f = std::fs::File::open(src).map_err(|e| e.to_string())?;
        std::io::copy(&mut f, &mut fh).map_err(|e| e.to_string())?;
        fh.write_all(&pad(size)).map_err(|e| e.to_string())?;
    }
    fh.write_all(&[0u8; 2 * BLOCK]).map_err(|e| e.to_string())?;
    fh.sync_all().map_err(|e| e.to_string())
}

fn field(b: &[u8]) -> &[u8] {
    match b.iter().position(|&c| c == 0) {
        Some(i) => &b[..i],
        None => b,
    }
}

const SHORT: &str = "a tar archive cut short";

fn read_full(r: &mut dyn Read, buf: &mut [u8]) -> Result<usize, String> {
    let mut n = 0;
    while n < buf.len() {
        match r.read(&mut buf[n..]) {
            Ok(0) => break,
            Ok(k) => n += k,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e.to_string()),
        }
    }
    Ok(n)
}

fn skip(r: &mut dyn Read, n: u64) -> Result<(), String> {
    let got = std::io::copy(&mut r.take(n), &mut std::io::sink()).map_err(|_| SHORT.to_string())?;
    if got < n {
        return Err(SHORT.into());
    }
    Ok(())
}

fn octal_field(b: &[u8]) -> Result<u64, String> {
    let t = String::from_utf8_lossy(field(b)).trim().to_string();
    if t.is_empty() {
        return Ok(0);
    }
    u64::from_str_radix(&t, 8).map_err(|_| "a tar header with a bad size".to_string())
}

/// Calls `each(name, size, reader)` for each regular file of a bundle tar;
/// it must read exactly `size` bytes.
fn entries(
    r: &mut dyn Read,
    each: &mut dyn FnMut(&str, u64, &mut dyn Read) -> Result<(), String>,
) -> Result<(), String> {
    let mut h = [0u8; BLOCK];
    let mut long_path: Option<String> = None;
    loop {
        if read_full(r, &mut h)? < BLOCK || h.iter().all(|&b| b == 0) {
            return Ok(());
        }
        let size = octal_field(&h[124..136])?;
        let mut name = field(&h[0..100]).to_vec();
        if &h[257..262] == b"ustar" {
            let prefix = field(&h[345..500]);
            if !prefix.is_empty() {
                let mut full = prefix.to_vec();
                full.push(b'/');
                full.extend(name);
                name = full;
            }
        }
        let mut text = String::from_utf8_lossy(&name).into_owned();
        let padding = ((BLOCK - (size as usize) % BLOCK) % BLOCK) as u64;
        if h[156] == b'x' || h[156] == b'g' {
            let mut body = vec![0u8; size as usize];
            if read_full(r, &mut body)? < body.len() {
                return Err(SHORT.into());
            }
            skip(r, padding)?;
            if h[156] == b'x' {
                long_path = pax_path(&body);
            }
            continue;
        }
        if let Some(p) = long_path.take() {
            text = p;
        }
        match h[156] {
            b'5' => {
                skip(r, size + padding)?;
                continue;
            }
            b'0' | 0 => {}
            _ => return Err(format!("a tar entry that is not a file: {text}")),
        }
        if text.is_empty() || text.starts_with('/') || text.split('/').any(|p| p == "..") {
            return Err(format!("a name outside the bundle: {text}"));
        }
        each(&text, size, r)?;
        skip(r, padding)?;
    }
}

/// `ustar.read`.
pub fn read(data: &[u8]) -> Result<Vec<(String, Vec<u8>)>, String> {
    let mut out = vec![];
    let mut cur = std::io::Cursor::new(data);
    entries(&mut cur, &mut |name, size, r| {
        let mut b = vec![0u8; size as usize];
        if read_full(r, &mut b)? < b.len() {
            return Err(SHORT.into());
        }
        out.push((name.to_string(), b));
        Ok(())
    })?;
    out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    Ok(out)
}

/// `ustar.extract`: the archive's files under dest.
pub fn extract(tar: &Path, dest: &Path) -> Result<(), String> {
    let mut fh = std::fs::File::open(tar).map_err(|e| e.to_string())?;
    entries(&mut fh, &mut |name, size, r| {
        let p = dest.join(name);
        if let Some(parent) = p.parent() {
            std::fs::create_dir_all(parent).map_err(|e| e.to_string())?;
        }
        let mut out = std::fs::File::create(&p).map_err(|e| e.to_string())?;
        let got = std::io::copy(&mut r.take(size), &mut out).map_err(|e| e.to_string())?;
        if got < size {
            return Err(SHORT.into());
        }
        Ok(())
    })
}

// ---------------------------------------------------------------------------
// The CPython tarball
// ---------------------------------------------------------------------------

fn pax_records(data: &[u8]) -> Vec<(String, String)> {
    let mut out = vec![];
    let mut rest = data;
    while !rest.is_empty() {
        let Some(sp) = rest.iter().position(|&b| b == b' ') else {
            break;
        };
        let Ok(len) = String::from_utf8_lossy(&rest[..sp]).parse::<usize>() else {
            break;
        };
        if len <= sp || len > rest.len() {
            break;
        }
        let rec = &rest[sp + 1..len];
        let rec = rec.strip_suffix(b"\n").unwrap_or(rec);
        if let Some(eq) = rec.iter().position(|&b| b == b'=') {
            out.push((
                String::from_utf8_lossy(&rec[..eq]).into_owned(),
                String::from_utf8_lossy(&rec[eq + 1..]).into_owned(),
            ));
        }
        rest = &rest[len..];
    }
    out
}

fn normpath(p: &str) -> String {
    let mut parts: Vec<&str> = vec![];
    for c in p.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            _ => parts.push(c),
        }
    }
    format!("/{}", parts.join("/"))
}

/// `manage.unpack_python`'s walk: every entry a file, a directory or a
/// symlink under python/, every link inside dest. GNU long names and PAX
/// path headers are read as Python's tarfile reads them.
pub fn unpack_tar_gz(data: &[u8], dest: &Path, label: &str) -> Result<(), String> {
    let not_tar = || format!("{label} is not a gzip tar archive.");
    let src: Box<dyn Read + Send> = Box::new(std::io::Cursor::new(data.to_vec()));
    let mut r = crate::gateway::http::gunzip(src);
    let root = std::fs::canonicalize(dest).map_err(|e| e.to_string())?;
    let root_s = root.to_string_lossy().into_owned();
    let mut long_name: Option<String> = None;
    let mut long_link: Option<String> = None;
    let mut pax: Vec<(String, String)> = vec![];
    let mut h = [0u8; BLOCK];
    let mut seen_any = false;
    loop {
        let n = read_full(&mut r, &mut h).map_err(|_| not_tar())?;
        if n == 0 && seen_any {
            break;
        }
        if n < BLOCK {
            return Err(not_tar());
        }
        if h.iter().all(|&b| b == 0) {
            break;
        }
        seen_any = true;
        let size = octal_field(&h[124..136]).map_err(|_| not_tar())?;
        let padding = ((BLOCK - (size as usize) % BLOCK) % BLOCK) as u64;
        let kind = h[156];
        if matches!(kind, b'L' | b'K' | b'x' | b'g') {
            let mut body = vec![0u8; size as usize];
            if read_full(&mut r, &mut body).map_err(|_| not_tar())? < body.len() {
                return Err(not_tar());
            }
            skip(&mut r, padding).map_err(|_| not_tar())?;
            let text = String::from_utf8_lossy(field(&body)).into_owned();
            match kind {
                b'L' => long_name = Some(text),
                b'K' => long_link = Some(text),
                b'x' => pax = pax_records(&body),
                _ => {}
            }
            continue;
        }
        let mut name = String::from_utf8_lossy(field(&h[0..100])).into_owned();
        if &h[257..262] == b"ustar" {
            let prefix = String::from_utf8_lossy(field(&h[345..500])).into_owned();
            if !prefix.is_empty() {
                name = format!("{prefix}/{name}");
            }
        }
        let mut link = String::from_utf8_lossy(field(&h[157..257])).into_owned();
        if let Some(n) = long_name.take() {
            name = n;
        }
        if let Some(l) = long_link.take() {
            link = l;
        }
        for (k, v) in pax.drain(..) {
            match k.as_str() {
                "path" => name = v,
                "linkpath" => link = v,
                _ => {}
            }
        }
        let is_dir = kind == b'5' || ((kind == 0 || kind == b'0') && name.ends_with('/'));
        let name = name.trim_end_matches('/').to_string();
        let parts: Vec<&str> = name.split('/').collect();
        if name.starts_with('/') || parts.contains(&"..") || parts[0] != "python" {
            return Err(format!("{label} holds an entry outside python/: {name}"));
        }
        let target = dest.join(&name);
        let mkparent = |t: &Path| -> Result<(), String> {
            if let Some(p) = t.parent() {
                std::fs::create_dir_all(p).map_err(|e| e.to_string())?;
            }
            Ok(())
        };
        if is_dir {
            std::fs::create_dir_all(&target).map_err(|e| e.to_string())?;
            skip(&mut r, size + padding).map_err(|_| not_tar())?;
        } else if matches!(kind, 0 | b'0' | b'7') {
            mkparent(&target)?;
            let mut out = std::fs::File::create(&target).map_err(|e| e.to_string())?;
            let got = std::io::copy(&mut (&mut r).take(size), &mut out).map_err(|_| not_tar())?;
            if got < size {
                return Err(not_tar());
            }
            drop(out);
            skip(&mut r, padding).map_err(|_| not_tar())?;
            let mode = octal_field(&h[100..108]).unwrap_or(0o644);
            use std::os::unix::fs::PermissionsExt;
            let m = if mode & 0o111 != 0 { 0o755 } else { 0o644 };
            std::fs::set_permissions(&target, std::fs::Permissions::from_mode(m))
                .map_err(|e| e.to_string())?;
        } else if kind == b'2' {
            let dir = Path::new(&name)
                .parent()
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_default();
            let wher = normpath(&format!("{root_s}/{dir}/{link}"));
            if link.starts_with('/') || !format!("{wher}/").starts_with(&format!("{root_s}/")) {
                return Err(format!("{label} holds a link outside the pack: {name}"));
            }
            mkparent(&target)?;
            let _ = std::fs::remove_file(&target);
            std::os::unix::fs::symlink(&link, &target).map_err(|e| e.to_string())?;
            skip(&mut r, size + padding).map_err(|_| not_tar())?;
        } else {
            return Err(format!(
                "{label} holds an entry that is not a file or a link: {name}"
            ));
        }
    }
    Ok(())
}
