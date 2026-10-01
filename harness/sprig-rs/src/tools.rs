//! The four tools: read, write, edit, bash. Each runs only after the
//! executor's gate allows the call.

use crate::gate::Tool;
use crate::goerr::quote;
use crate::gostr::{count, replace_once};
use crate::json::{Any, Map};
use crate::osx::{dir, mkdir_all, read_file, write_file, Run};
use std::collections::BTreeMap;

/// A tool input value as a string; anything else is "".
fn str_of(input: &Option<Map>, key: &str) -> Vec<u8> {
    match input.as_ref().and_then(|m| m.get(key)) {
        Some(Any::Str(s)) => s.clone().into_bytes(),
        _ => Vec::new(),
    }
}

/// Returns a file's contents. input: {"path"}.
pub struct ReadTool;

/// Writes a file, making its parent directories. input: {"path","content"}.
pub struct WriteTool;

/// Replaces a unique occurrence of old with new. input: {"path","old","new"}.
pub struct EditTool;

/// Runs a shell command; its output and errors are one stream. input: {"cmd"}.
pub struct BashTool;

impl Tool for ReadTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        match read_file(&str_of(input, "path")) {
            Ok(b) => (b, None),
            Err(e) => (Vec::new(), Some(e)),
        }
    }
}

impl Tool for WriteTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let path = str_of(input, "path");
        if let Err(e) = mkdir_all(&dir(&path), 0o755) {
            return (Vec::new(), Some(e));
        }
        if let Err(e) = write_file(&path, &str_of(input, "content"), 0o644) {
            return (Vec::new(), Some(e));
        }
        ([b"wrote ".as_slice(), &path].concat(), None)
    }
}

impl Tool for EditTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let (path, old, new) = (
            str_of(input, "path"),
            str_of(input, "old"),
            str_of(input, "new"),
        );
        let body = match read_file(&path) {
            Ok(b) => b,
            Err(e) => return (Vec::new(), Some(e)),
        };
        match count(&body, &old) {
            0 => {
                let e = [
                    format!("edit: {} not found in ", quote(&old)).as_bytes(),
                    &path,
                ]
                .concat();
                return (Vec::new(), Some(e));
            }
            1 => {}
            n => {
                let e = [
                    format!("edit: {} is not unique in ", quote(&old)).as_bytes(),
                    &path,
                    format!(" (matches {n} times)").as_bytes(),
                ]
                .concat();
                return (Vec::new(), Some(e));
            }
        }
        if let Err(e) = write_file(&path, &replace_once(&body, &old, &new), 0o644) {
            return (Vec::new(), Some(e));
        }
        ([b"edited ".as_slice(), &path].concat(), None)
    }
}

impl Tool for BashTool {
    fn run(&self, input: &Option<Map>) -> (Vec<u8>, Option<Vec<u8>>) {
        let argv = vec![b"sh".to_vec(), b"-c".to_vec(), str_of(input, "cmd")];
        let ran = Run {
            argv: &argv,
            stdin: None,
            combined: true,
            dir: None,
            timeout: None,
        }
        .run();
        (ran.stdout, ran.err.map(String::into_bytes))
    }
}

/// The whole tool surface, keyed by name.
pub fn default_tools() -> BTreeMap<String, Box<dyn Tool>> {
    let mut m: BTreeMap<String, Box<dyn Tool>> = BTreeMap::new();
    m.insert("read".into(), Box::new(ReadTool));
    m.insert("write".into(), Box::new(WriteTool));
    m.insert("edit".into(), Box::new(EditTool));
    m.insert("bash".into(), Box::new(BashTool));
    m
}

/// The four tools in sprig's own order: the order a prompt lists them in,
/// and the default of --tools.
pub const ALL_TOOL_NAMES: &[&str] = &["read", "write", "edit", "bash"];

/// A --tools value: a comma list of tool names. Returns the named tools
/// once each, in sprig's order. An empty value or a name that is not a
/// tool is an error, worded for one line on stderr.
pub fn parse_tool_wall(list: &[u8]) -> Result<Vec<String>, Vec<u8>> {
    if list.is_empty() {
        return Err(b"--tools is empty; name one or more of read, write, edit, bash".to_vec());
    }
    let mut want = Vec::new();
    for name in list.split(|&c| c == b',') {
        if !ALL_TOOL_NAMES.iter().any(|t| t.as_bytes() == name) {
            return Err([
                b"--tools: unknown tool ".as_slice(),
                quote(name).as_bytes(),
                b"; the tools are read, write, edit, bash",
            ]
            .concat());
        }
        want.push(name.to_vec());
    }
    Ok(ALL_TOOL_NAMES
        .iter()
        .filter(|t| want.iter().any(|w| w == t.as_bytes()))
        .map(|t| t.to_string())
        .collect())
}

/// The tool map for a wall: only the named tools. A call to any other tool
/// is then refused as an unknown tool.
pub fn tools_for(wall: &[String]) -> BTreeMap<String, Box<dyn Tool>> {
    let mut all = default_tools();
    let mut m = BTreeMap::new();
    for name in wall {
        if let Some(t) = all.remove(name) {
            m.insert(name.clone(), t);
        }
    }
    m
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::ffi::OsStrExt;

    #[test]
    fn tool_wall_follows_go() {
        assert_eq!(
            parse_tool_wall(b"bash,read,bash").unwrap(),
            vec!["read", "bash"]
        );
        assert_eq!(
            parse_tool_wall(b"").unwrap_err(),
            b"--tools is empty; name one or more of read, write, edit, bash"
        );
        assert_eq!(
            parse_tool_wall(b"read,").unwrap_err(),
            b"--tools: unknown tool \"\"; the tools are read, write, edit, bash"
        );
        assert_eq!(
            parse_tool_wall(b"grep").unwrap_err(),
            b"--tools: unknown tool \"grep\"; the tools are read, write, edit, bash"
        );
        let m = tools_for(&["bash".to_string()]);
        assert_eq!(m.keys().collect::<Vec<_>>(), vec!["bash"]);
    }

    fn input(pairs: &[(&str, &str)]) -> Option<Map> {
        Some(
            pairs
                .iter()
                .map(|(k, v)| (k.to_string(), Any::Str(v.to_string())))
                .collect(),
        )
    }

    #[test]
    fn tools_follow_go() {
        let d = std::env::temp_dir().join(format!("sprig-tools-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        let ds = d.to_str().unwrap();
        let f = format!("{ds}/a/b.txt");
        let (out, err) = WriteTool.run(&input(&[("path", &f), ("content", "ab ab é")]));
        assert_eq!((out, err), (format!("wrote {f}").into_bytes(), None));
        assert_eq!(
            ReadTool.run(&input(&[("path", &f)])).0,
            "ab ab é".as_bytes()
        );
        let (_, err) = EditTool.run(&input(&[("path", &f), ("old", "ab"), ("new", "x")]));
        assert_eq!(
            err.unwrap(),
            format!("edit: \"ab\" is not unique in {f} (matches 2 times)").into_bytes()
        );
        let (_, err) = EditTool.run(&input(&[("path", &f), ("old", "q\u{a0}\t"), ("new", "x")]));
        assert_eq!(
            err.unwrap(),
            format!("edit: \"q\\u00a0\\t\" not found in {f}").into_bytes()
        );
        let (out, err) = EditTool.run(&input(&[("path", &f), ("old", "é"), ("new", "E")]));
        assert_eq!((out, err), (format!("edited {f}").into_bytes(), None));
        assert_eq!(std::fs::read(&f).unwrap(), b"ab ab E");
        let (out, err) = BashTool.run(&input(&[("cmd", "echo o; echo e >&2; exit 2")]));
        assert_eq!(
            (out, err),
            (b"o\ne\n".to_vec(), Some(b"exit status 2".to_vec()))
        );
        let (_, err) = ReadTool.run(&None);
        assert_eq!(err.unwrap(), b"open : no such file or directory");
        std::fs::remove_dir_all(std::ffi::OsStr::from_bytes(d.as_os_str().as_bytes())).unwrap();
    }
}
