//! Go's flag package, as `coppice web` uses it: a FlagSet with
//! ContinueOnError whose output is stderr. The parse rules, the refusal
//! lines and the usage text are Go's, word for word.

use crate::sys;

/// What a flag holds.
enum Kind {
    Str(String),
    Bool(bool),
    /// A repeatable flag: Go's flag.Var over a slice.
    List(Vec<String>),
}

struct Flag {
    name: &'static str,
    usage: &'static str,
    kind: Kind,
    /// The default as Go's flag.DefValue writes it.
    def: String,
}

/// One flag set.
pub struct FlagSet {
    name: &'static str,
    flags: Vec<Flag>,
    args: Vec<String>,
    set: Vec<&'static str>,
}

/// How a parse ended without success: -h or -help (Go's ErrHelp), or an
/// error. Both have printed what Go prints.
pub enum Stop {
    Help,
    Error,
}

impl FlagSet {
    pub fn new(name: &'static str) -> FlagSet {
        FlagSet {
            name,
            flags: Vec::new(),
            args: Vec::new(),
            set: Vec::new(),
        }
    }

    pub fn string(&mut self, name: &'static str, def: &str, usage: &'static str) {
        self.flags.push(Flag {
            name,
            usage,
            kind: Kind::Str(def.to_string()),
            def: def.to_string(),
        });
    }

    pub fn bool(&mut self, name: &'static str, def: bool, usage: &'static str) {
        self.flags.push(Flag {
            name,
            usage,
            kind: Kind::Bool(def),
            def: def.to_string(),
        });
    }

    pub fn list(&mut self, name: &'static str, usage: &'static str) {
        self.flags.push(Flag {
            name,
            usage,
            kind: Kind::List(Vec::new()),
            def: String::new(),
        });
    }

    fn find(&self, name: &str) -> Option<usize> {
        self.flags.iter().position(|f| f.name == name)
    }

    pub fn str(&self, name: &str) -> String {
        match self.find(name).map(|i| &self.flags[i].kind) {
            Some(Kind::Str(s)) => s.clone(),
            _ => String::new(),
        }
    }

    pub fn flag(&self, name: &str) -> bool {
        matches!(
            self.find(name).map(|i| &self.flags[i].kind),
            Some(Kind::Bool(true))
        )
    }

    pub fn values(&self, name: &str) -> Vec<String> {
        match self.find(name).map(|i| &self.flags[i].kind) {
            Some(Kind::List(v)) => v.clone(),
            _ => Vec::new(),
        }
    }

    /// The arguments left after the flags.
    pub fn args(&self) -> &[String] {
        &self.args
    }

    fn fail(&self, msg: &str) -> Stop {
        let mut e = String::from(msg);
        e.push('\n');
        e.push_str(&self.usage_text());
        eprint!("{e}");
        Stop::Error
    }

    /// Go's defaultUsage: the title, then PrintDefaults.
    pub fn usage_text(&self) -> String {
        let mut out = if self.name.is_empty() {
            "Usage:\n".to_string()
        } else {
            format!("Usage of {}:\n", self.name)
        };
        let mut sorted: Vec<&Flag> = self.flags.iter().collect();
        sorted.sort_by(|a, b| a.name.cmp(b.name));
        for f in sorted {
            let mut b = format!("  -{}", f.name);
            let (tname, usage) = unquote_usage(f);
            if !tname.is_empty() {
                b.push(' ');
                b.push_str(&tname);
            }
            if b.len() <= 4 {
                b.push('\t');
            } else {
                b.push_str("\n    \t");
            }
            b.push_str(&usage.replace('\n', "\n    \t"));
            let zero = match f.kind {
                Kind::Bool(_) => f.def == "false",
                _ => f.def.is_empty(),
            };
            if !zero {
                if matches!(f.kind, Kind::Str(_)) {
                    b.push_str(&format!(" (default {})", sys::go_quote(&f.def)));
                } else {
                    b.push_str(&format!(" (default {})", f.def));
                }
            }
            out.push_str(&b);
            out.push('\n');
        }
        out
    }

    /// Go's FlagSet.Parse.
    pub fn parse(&mut self, args: &[String]) -> Result<(), Stop> {
        let mut rest: Vec<String> = args.to_vec();
        while let Some(s) = rest.first().cloned() {
            let b = s.as_bytes();
            if b.len() < 2 || b[0] != b'-' {
                break;
            }
            let mut minuses = 1;
            if b[1] == b'-' {
                minuses = 2;
                if b.len() == 2 {
                    rest.remove(0);
                    break;
                }
            }
            let mut name = s[minuses..].to_string();
            if name.is_empty() || name.starts_with('-') || name.starts_with('=') {
                return Err(self.fail(&format!("bad flag syntax: {s}")));
            }
            rest.remove(0);
            let mut value = None;
            if let Some(i) = name[1..].find('=') {
                let i = i + 1;
                value = Some(name[i + 1..].to_string());
                name.truncate(i);
            }
            let Some(idx) = self.find(&name) else {
                if name == "help" || name == "h" {
                    eprint!("{}", self.usage_text());
                    return Err(Stop::Help);
                }
                return Err(self.fail(&format!("flag provided but not defined: -{name}")));
            };
            let fname = self.flags[idx].name;
            if let Kind::Bool(_) = self.flags[idx].kind {
                match value {
                    Some(v) => match parse_bool(&v) {
                        Some(x) => self.flags[idx].kind = Kind::Bool(x),
                        None => {
                            return Err(self.fail(&format!(
                                "invalid boolean value {} for -{name}: parse error",
                                sys::go_quote(&v)
                            )))
                        }
                    },
                    None => self.flags[idx].kind = Kind::Bool(true),
                }
            } else {
                let v = match value {
                    Some(v) => v,
                    None => {
                        if rest.is_empty() {
                            return Err(self.fail(&format!("flag needs an argument: -{name}")));
                        }
                        rest.remove(0)
                    }
                };
                match &mut self.flags[idx].kind {
                    Kind::Str(s) => *s = v,
                    Kind::List(l) => l.push(v),
                    Kind::Bool(_) => {}
                }
            }
            if !self.set.contains(&fname) {
                self.set.push(fname);
            }
        }
        self.args = rest;
        Ok(())
    }
}

/// Go's strconv.ParseBool.
fn parse_bool(s: &str) -> Option<bool> {
    match s {
        "1" | "t" | "T" | "TRUE" | "true" | "True" => Some(true),
        "0" | "f" | "F" | "FALSE" | "false" | "False" => Some(false),
        _ => None,
    }
}

/// Go's flag.UnquoteUsage: a back-quoted name in the usage, else the type.
fn unquote_usage(f: &Flag) -> (String, String) {
    let u = f.usage;
    if let Some(i) = u.find('`') {
        if let Some(j) = u[i + 1..].find('`') {
            let j = i + 1 + j;
            let name = &u[i + 1..j];
            return (
                name.to_string(),
                format!("{}{}{}", &u[..i], name, &u[j + 1..]),
            );
        }
    }
    let name = match f.kind {
        Kind::Bool(_) => "",
        Kind::Str(_) => "string",
        Kind::List(_) => "value",
    };
    (name.to_string(), u.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set() -> FlagSet {
        let mut fs = FlagSet::new("web serve");
        fs.string("listen", ":8443", "address to serve the phone client on");
        fs.bool("qr", true, "print the sign-in QR on start");
        fs.bool("x", false, "one letter");
        fs.list("name", "a DNS name the phone will use. Repeatable.");
        fs
    }

    #[test]
    fn usage_is_gos() {
        assert_eq!(
            set().usage_text(),
            "Usage of web serve:\n  -listen string\n    \taddress to serve the phone client on (default \":8443\")\n  -name value\n    \ta DNS name the phone will use. Repeatable.\n  -qr\n    \tprint the sign-in QR on start (default true)\n  -x\tone letter\n"
        );
    }

    #[test]
    fn parses_as_go_does() {
        let mut fs = set();
        let a: Vec<String> = [
            "--listen=:1",
            "-qr=false",
            "--name",
            "a",
            "-name=b",
            "rest",
            "-x",
        ]
        .iter()
        .map(|s| s.to_string())
        .collect();
        assert!(fs.parse(&a).is_ok());
        assert_eq!(fs.str("listen"), ":1");
        assert!(!fs.flag("qr"));
        assert!(!fs.flag("x"));
        assert_eq!(fs.values("name"), vec!["a", "b"]);
        assert_eq!(fs.args(), &["rest".to_string(), "-x".to_string()]);
    }
}
