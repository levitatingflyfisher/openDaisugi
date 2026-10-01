//! The phone page the Go tree ships, carried byte for byte, and Go's mime
//! table for the names the server serves.

use std::collections::HashMap;
use std::sync::OnceLock;

mod files {
    include!(concat!(env!("OUT_DIR"), "/web_static.rs"));
}

/// The page's files: each path under static/ and its bytes.
#[cfg(test)]
pub fn files() -> &'static [(&'static str, &'static [u8])] {
    files::FILES
}

/// The file at path under static/.
pub fn file(path: &str) -> Option<&'static [u8]> {
    files::FILES
        .iter()
        .find(|(p, _)| *p == path)
        .map(|(_, b)| *b)
}

/// The content security policy every page response carries: nothing
/// inline, nothing from another origin.
pub const CSP: &str = "default-src 'self'; connect-src 'self'; img-src 'self'; script-src 'self'; style-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'";

/// The policy a view's files carry: the floor page may frame them, and
/// they run in a sandbox with scripts only.
pub fn view_csp() -> String {
    format!(
        "{}; sandbox allow-scripts",
        CSP.replacen("frame-ancestors 'none'", "frame-ancestors 'self'", 1)
    )
}

/// Go's builtin types, which win over the system's.
const BUILTIN: &[(&str, &str)] = &[
    (".ai", "application/postscript"),
    (".apk", "application/vnd.android.package-archive"),
    (".apng", "image/apng"),
    (".avif", "image/avif"),
    (".bin", "application/octet-stream"),
    (".bmp", "image/bmp"),
    (".com", "application/octet-stream"),
    (".css", "text/css; charset=utf-8"),
    (".csv", "text/csv; charset=utf-8"),
    (".doc", "application/msword"),
    (
        ".docx",
        "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    ),
    (".ehtml", "text/html; charset=utf-8"),
    (".eml", "message/rfc822"),
    (".eps", "application/postscript"),
    (".exe", "application/octet-stream"),
    (".flac", "audio/flac"),
    (".gif", "image/gif"),
    (".gz", "application/gzip"),
    (".htm", "text/html; charset=utf-8"),
    (".html", "text/html; charset=utf-8"),
    (".ico", "image/vnd.microsoft.icon"),
    (".ics", "text/calendar; charset=utf-8"),
    (".jfif", "image/jpeg"),
    (".jpeg", "image/jpeg"),
    (".jpg", "image/jpeg"),
    (".js", "text/javascript; charset=utf-8"),
    (".json", "application/json"),
    (".m4a", "audio/mp4"),
    (".mjs", "text/javascript; charset=utf-8"),
    (".mp3", "audio/mpeg"),
    (".mp4", "video/mp4"),
    (".oga", "audio/ogg"),
    (".ogg", "audio/ogg"),
    (".ogv", "video/ogg"),
    (".opus", "audio/ogg"),
    (".pdf", "application/pdf"),
    (".pjp", "image/jpeg"),
    (".pjpeg", "image/jpeg"),
    (".png", "image/png"),
    (".ppt", "application/vnd.ms-powerpoint"),
    (
        ".pptx",
        "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    ),
    (".ps", "application/postscript"),
    (".rdf", "application/rdf+xml"),
    (".rtf", "application/rtf"),
    (".shtml", "text/html; charset=utf-8"),
    (".svg", "image/svg+xml"),
    (".text", "text/plain; charset=utf-8"),
    (".tif", "image/tiff"),
    (".tiff", "image/tiff"),
    (".txt", "text/plain; charset=utf-8"),
    (".vtt", "text/vtt; charset=utf-8"),
    (".wasm", "application/wasm"),
    (".wav", "audio/wav"),
    (".webm", "audio/webm"),
    (".webp", "image/webp"),
    (".xbl", "text/xml; charset=utf-8"),
    (".xbm", "image/x-xbitmap"),
    (".xht", "application/xhtml+xml"),
    (".xhtml", "application/xhtml+xml"),
    (".xls", "application/vnd.ms-excel"),
    (
        ".xlsx",
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    ),
    (".xml", "text/xml; charset=utf-8"),
    (".xsl", "text/xml; charset=utf-8"),
    (".zip", "application/zip"),
    // The phone server adds this one before it serves anything.
    (".webmanifest", "application/manifest+json"),
];

/// Go's setExtensionType: a text type gains a utf-8 charset.
fn with_charset(t: &str) -> String {
    if t.starts_with("text/") && !t.contains("charset=") {
        format!("{t}; charset=utf-8")
    } else {
        t.to_string()
    }
}

/// The table: Go's builtins, then the first system database that reads,
/// as Go's mime package loads them on a Unix box.
fn table() -> &'static HashMap<String, String> {
    static T: OnceLock<HashMap<String, String>> = OnceLock::new();
    T.get_or_init(|| {
        let mut m: HashMap<String, String> = BUILTIN
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        for globs in ["/usr/local/share/mime/globs2", "/usr/share/mime/globs2"] {
            let Ok(text) = std::fs::read_to_string(globs) else {
                continue;
            };
            for line in text.lines() {
                let f: Vec<&str> = line.split(':').collect();
                if f.len() < 3 || f[0].is_empty() || f[2].len() < 3 {
                    continue;
                }
                if f[0].starts_with('#') || !f[2].starts_with("*.") {
                    continue;
                }
                let ext = &f[2][1..];
                if ext.contains(['?', '*', '[']) || m.contains_key(ext) {
                    continue;
                }
                m.insert(ext.to_string(), with_charset(f[1]));
            }
            return m;
        }
        for types in [
            "/etc/mime.types",
            "/etc/apache2/mime.types",
            "/etc/apache/mime.types",
            "/etc/httpd/conf/mime.types",
        ] {
            let Ok(text) = std::fs::read_to_string(types) else {
                continue;
            };
            for line in text.lines() {
                let f: Vec<&str> = line.split_whitespace().collect();
                if f.len() <= 1 || f[0].starts_with('#') {
                    continue;
                }
                for ext in &f[1..] {
                    if ext.starts_with('#') {
                        break;
                    }
                    m.insert(format!(".{ext}"), with_charset(f[0]));
                }
            }
        }
        m
    })
}

/// Go's mime.TypeByExtension: an exact match, then one without regard to
/// case. "" when the extension is unknown.
pub fn type_by_extension(ext: &str) -> String {
    let t = table();
    if let Some(v) = t.get(ext) {
        return v.clone();
    }
    let lower = ext.to_lowercase();
    t.iter()
        .find(|(k, _)| k.to_lowercase() == lower)
        .map(|(_, v)| v.clone())
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_page_is_carried_and_the_tests_are_not() {
        assert!(file("index.html").is_some());
        assert!(file("app.js").is_some());
        assert!(file("icons/icon-192.png").is_some());
        assert!(files().iter().all(|(p, _)| !p
            .split('/')
            .any(|s| s.starts_with('_') || s.starts_with('.'))));
    }

    #[test]
    fn the_embedded_list_matches_the_go_tree() {
        // Every file the Go tree's static/ holds, less the names Go's
        // embed leaves out, is carried; nothing else is.
        let root =
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../coppice/internal/web/static");
        let mut want = Vec::new();
        fn walk(dir: &std::path::Path, rel: &str, out: &mut Vec<String>) {
            for e in std::fs::read_dir(dir).unwrap() {
                let e = e.unwrap();
                let n = e.file_name().to_string_lossy().into_owned();
                if n.starts_with('.') || n.starts_with('_') {
                    continue;
                }
                let r = if rel.is_empty() {
                    n.clone()
                } else {
                    format!("{rel}/{n}")
                };
                if e.file_type().unwrap().is_dir() {
                    walk(&e.path(), &r, out);
                } else {
                    out.push(r);
                }
            }
        }
        walk(&root, "", &mut want);
        want.sort();
        let mut got: Vec<String> = files().iter().map(|(p, _)| p.to_string()).collect();
        got.sort();
        assert_eq!(got, want);
        for (p, b) in files() {
            assert_eq!(std::fs::read(root.join(p)).unwrap(), *b, "{p}");
        }
    }

    #[test]
    fn types_are_gos() {
        assert_eq!(type_by_extension(".js"), "text/javascript; charset=utf-8");
        assert_eq!(
            type_by_extension(".webmanifest"),
            "application/manifest+json"
        );
        assert_eq!(type_by_extension(".PNG"), "image/png");
    }
}
