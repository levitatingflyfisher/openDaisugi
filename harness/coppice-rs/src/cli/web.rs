//! coppice web: the floor in a browser on this machine, the phone server,
//! its certificates and its tokens. Every subcommand works on files under
//! the data dir the global flags resolved.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use super::goflag::FlagSet;
use super::{errln, out, sys_signals, Cli};
use crate::sys;
use crate::web::{self, config, localca, serve, tls, token::TokenStore};

const WEB_USAGE: &str = "coppice web - the floor in a browser

  coppice web [--no-open] [--listen ADDR]
      Start the server if it is not running, serve the floor on this
      machine only, and open it in the browser, signed in.

  coppice web cert init [--name NAME]... [--ip ADDR]... [--ca-dir DIR] [--ca-listen ADDR] [--qr url|pem|off]
      Make the local CA if there is none, then issue a server certificate.
      Prints a QR the phone scans to install the CA.
  coppice web cert show [--ca-dir DIR]
      Print what the current certificate covers and when it expires.
  coppice web cert tailscale [NAME.TAILNET.ts.net] [--dir DIR]
      Ask tailscale for a Let's Encrypt certificate and write it where
      coppice web serve --tls tailscale looks for it. With no name, this
      box's MagicDNS name.
  coppice web serve [--listen ADDR] [--tls tailscale|localca|files|off]
                    [--cert FILE --key FILE] [--ca-dir DIR] [--ca-listen ADDR]
                    [--external-url URL] [--gate-root DIR] [--voice-url URL [--voice-token-file FILE]]
                    [--ntfy URL --ntfy-topic NAME --ntfy-token-env VAR]
                    [--persist|--forget] [--web-push] [--qr]
      Serve the phone client. Mints a token on the first run and prints it.
      With --tls tailscale it does the whole job: it gets the certificate
      for this box's MagicDNS name, renews it when under 30 days are left,
      and listens on the tailnet address and loopback only. A --listen
      with no host (:8443) keeps that and sets the port; a --listen with a
      host (0.0.0.0:8443) listens there instead.
  coppice web token [--for NAME] [--rotate] [--url URL] [--qr] [--listen ADDR]
      Print the current token, its sign-in URL, and a QR to scan. With
      --for, the token is the one minted for NAME, and every allow and
      deny made with it carries that name.
  coppice web token list
      Print the names that hold a token. It never prints a token.
  coppice web token --revoke NAME
      Retire the token minted for NAME.
";

/// Where coppice web serves when the port is free.
const DEFAULT_WEB_LISTEN: &str = "127.0.0.1:9443";

const VOICE_URL_ERR: &str =
    "--voice-url needs an http:// or https:// address, for example http://127.0.0.1:7477.";

fn eout(s: &str) {
    let mut e = std::io::stderr().lock();
    let _ = e.write_all(s.as_bytes());
}

fn path_s(p: &Path) -> String {
    p.to_string_lossy().into_owned()
}

/// This machine's name, as Go's os.Hostname reads it.
fn hostname() -> String {
    let mut buf = [0u8; 256];
    // SAFETY: the buffer is live and its length is passed.
    let r = unsafe { libc::gethostname(buf.as_mut_ptr().cast(), buf.len()) };
    if r != 0 {
        return String::new();
    }
    let n = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    String::from_utf8_lossy(&buf[..n]).into_owned()
}

/// The address the phone opens; the scheme follows the TLS source.
fn sign_in_url(base: &str, listen: &str, tls_source: &str) -> String {
    if !base.is_empty() {
        return base.trim_end_matches('/').to_string();
    }
    let scheme = if tls_source == tls::OFF {
        "http"
    } else {
        "https"
    };
    let mut host = hostname();
    if host.is_empty() {
        host = "localhost".into();
    }
    if scheme == "http" {
        host = "127.0.0.1".into();
    }
    let port = match web::split_host_port(listen) {
        Ok((_, p)) if !p.is_empty() => p,
        _ => "8443".into(),
    };
    format!("{scheme}://{host}:{port}")
}

fn print_sign_in(token: &str, external_url: &str, listen: &str, tls_source: &str) {
    let url = format!(
        "{}/#t={token}",
        sign_in_url(external_url, listen, tls_source)
    );
    out(&format!(
        "token     {token}\nsign in   {url}\n\nScan this on the phone.\n"
    ));
    out(&web::qr::render(&url));
}

/// Whether raw is empty, or an absolute http or https URL with a host.
fn valid_voice_url(raw: &str) -> bool {
    if raw.is_empty() {
        return true;
    }
    let Some((scheme, rest)) = raw.split_once("://") else {
        return false;
    };
    // Go's url.Parse lowers the scheme.
    if !scheme.eq_ignore_ascii_case("http") && !scheme.eq_ignore_ascii_case("https") {
        return false;
    }
    let host = rest.split(['/', '?', '#']).next().unwrap_or("");
    let host = host.rsplit('@').next().unwrap_or(host);
    !host.is_empty()
}

/// Runs serve until SIGINT or SIGTERM. The signals are blocked before any
/// thread starts, so only the waiter takes them.
fn serve_until_signal(c: &config::Config, opts: serve::Options) -> Result<(), String> {
    let signals = sys_signals::block();
    let stop = Arc::new(AtomicBool::new(false));
    let st = stop.clone();
    std::thread::spawn(move || {
        sys_signals::wait(&signals);
        st.store(true, Ordering::SeqCst);
    });
    serve::serve(c, opts, stop)
}

/// The pair a running tailscale serve uses, else the pair a saved --tls
/// tailscale setup names, else cert_file and key_file.
fn served_pair(data_dir: &Path, cert_file: &Path, key_file: &Path) -> (PathBuf, PathBuf) {
    if let Some((c, k)) = web::tailscale::serving_pair(&web::tailscale::serving_path(data_dir)) {
        if !k.is_empty() {
            return (PathBuf::from(c), PathBuf::from(k));
        }
    }
    if let Ok(cfg) = config::load(&config::config_path(data_dir)) {
        if cfg.tls == tls::TAILSCALE && !cfg.cert_file.is_empty() && !cfg.key_file.is_empty() {
            return (PathBuf::from(cfg.cert_file), PathBuf::from(cfg.key_file));
        }
    }
    (cert_file.to_path_buf(), key_file.to_path_buf())
}

fn opts_for(cli: &Cli, gate_root: &str, voice_url: &str, voice_token_file: &str) -> serve::Options {
    let (plugins, _) = super::load_plugins_and_config();
    serve::Options {
        dial: web::upstream::Dialer {
            path: cli.socket.clone(),
        },
        tokens: TokenStore::new(config::token_path(&cli.data_dir)),
        gate_root: PathBuf::from(gate_root),
        push: None,
        voice_url: voice_url.into(),
        voice_token_file: voice_token_file.into(),
        views: serve::views_of(&plugins),
        events: None,
        listening: None,
        cert_check_every: None,
        wait_for_tailnet: false,
        tailnet_retry: None,
        serving_file: String::new(),
    }
}

impl Cli {
    pub(super) fn run_web(&self, remote: &str, argv: &[String]) -> i32 {
        if !remote.is_empty() {
            errln("coppice web does not take --remote. Run it on the box the phone reaches.");
            return 1;
        }
        let Some(first) = argv.first() else {
            return self.web_open(argv);
        };
        if first.starts_with('-') {
            return self.web_open(argv);
        }
        match first.as_str() {
            "cert" => self.web_cert(&argv[1..]),
            "serve" => self.web_serve(&argv[1..]),
            "token" => self.web_token(&argv[1..]),
            _ => {
                eout(&format!(
                    "Unknown command: coppice web {first}\n\n{WEB_USAGE}"
                ));
                1
            }
        }
    }

    fn web_cert(&self, args: &[String]) -> i32 {
        let Some(first) = args.first() else {
            eout(WEB_USAGE);
            return 1;
        };
        match first.as_str() {
            "init" => self.web_cert_init(&args[1..]),
            "show" => self.web_cert_show(&args[1..]),
            "tailscale" => self.web_cert_tailscale(&args[1..]),
            _ => {
                eout(&format!(
                    "Unknown command: coppice web cert {first}\n\n{WEB_USAGE}"
                ));
                1
            }
        }
    }

    fn web_cert_init(&self, args: &[String]) -> i32 {
        let mut fs = FlagSet::new("cert init");
        fs.list("name", "a DNS name the phone will use. Repeatable.");
        fs.list("ip", "an address the phone will use. Repeatable.");
        fs.string(
            "ca-dir",
            &path_s(&config::local_ca_dir(&self.data_dir)),
            "where the CA lives",
        );
        fs.string(
            "ca-listen",
            ":8080",
            "the plain HTTP port that hands the CA to the phone",
        );
        fs.string("qr", "url", "url, pem, or off");
        if fs.parse(args).is_err() {
            return 1;
        }
        let qr_kind = fs.str("qr");
        if qr_kind != "url" && qr_kind != "pem" && qr_kind != "off" {
            errln(&format!(
                "--qr {} is not one of url, pem, off.",
                sys::go_quote(&qr_kind)
            ));
            return 1;
        }
        let mut ips = Vec::new();
        for raw in fs.values("ip") {
            match web::parse_ip(&raw) {
                Some(ip) => ips.push(ip),
                None => {
                    errln(&format!(
                        "{} is not an address. Use --ip 192.168.1.20.",
                        sys::go_quote(&raw)
                    ));
                    return 1;
                }
            }
        }
        let names = fs.values("name");
        let ca_dir = fs.str("ca-dir");
        let ca_listen = fs.str("ca-listen");
        let d = localca::CaDir::new(PathBuf::from(&ca_dir));
        let meta = match d.init(&names, &ips) {
            Ok(m) => m,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        let covers: Vec<String> = meta.names.iter().chain(meta.ips.iter()).cloned().collect();
        out(&format!(
            "CA        {}\nserver    {}\ncovers    {}\nexpires   {}\n",
            path_s(&Path::new(&ca_dir).join("ca.crt")),
            path_s(&Path::new(&ca_dir).join("leaf.crt")),
            covers.join(" "),
            rfc3339_of(&meta.not_after)
        ));
        let host = if let Some(n) = meta.names.first() {
            n.clone()
        } else if let Some(i) = meta.ips.first() {
            i.clone()
        } else {
            "127.0.0.1".into()
        };
        let url = localca::ca_cert_url(&host, &ca_listen);
        out(&format!(
            "\nInstall the CA on the phone. Scan this, save the file, then open it.\n{url}\n\n"
        ));
        match qr_kind.as_str() {
            "off" => {}
            "pem" => match d.ca_pem() {
                Ok(pem) => out(&web::qr::render(&String::from_utf8_lossy(&pem))),
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            },
            _ => out(&web::qr::render(&url)),
        }
        out(&format!(
            "\nStart the server with: coppice web serve --tls localca --ca-listen {ca_listen}\n"
        ));
        0
    }

    fn web_cert_show(&self, args: &[String]) -> i32 {
        let mut fs = FlagSet::new("cert show");
        fs.string(
            "ca-dir",
            &path_s(&config::local_ca_dir(&self.data_dir)),
            "where the CA lives",
        );
        if fs.parse(args).is_err() {
            return 1;
        }
        let ca_dir = fs.str("ca-dir");
        let d = localca::CaDir::new(PathBuf::from(&ca_dir));
        let Ok(meta) = d.meta() else {
            errln(&format!(
                "No local CA in {ca_dir}. Run coppice web cert init."
            ));
            return 1;
        };
        let (cert_file, _) = d.leaf_paths();
        let Ok(not_after) = localca::load_leaf(&cert_file) else {
            errln(&format!(
                "The certificate in {ca_dir} will not parse. Run coppice web cert init."
            ));
            return 1;
        };
        let covers: Vec<String> = meta.names.iter().chain(meta.ips.iter()).cloned().collect();
        out(&format!(
            "CA        {}\ncovers    {}\nexpires   {}\n",
            path_s(&Path::new(&ca_dir).join("ca.crt")),
            covers.join(" "),
            localca::rfc3339(not_after)
        ));
        let warn = localca::expiry_warning(not_after, localca::now().0);
        if !warn.is_empty() {
            out(&format!("warning   {warn}\n"));
        }
        0
    }

    fn web_cert_tailscale(&self, args: &[String]) -> i32 {
        let (cert_default, key_default) = config::tailscale_paths(&self.data_dir);
        let mut fs = FlagSet::new("cert tailscale");
        let dir_default = cert_default.parent().map(path_s).unwrap_or_default();
        fs.string("dir", &dir_default, "where to write the pair");
        if fs.parse(args).is_err() {
            return 1;
        }
        if fs.args().len() > 1 {
            errln("Give one MagicDNS name, or none to use this box's. Example: coppice web cert tailscale box.tail1234.ts.net");
            return 1;
        }
        let name = match fs.args().first() {
            Some(n) => n.clone(),
            // No name: this box's own, as tailscale names it.
            None => match web::tailscale::read_tailscale() {
                Ok(ts) => ts.name,
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            },
        };
        let dir = PathBuf::from(fs.str("dir"));
        let mut cert_file = dir.join(cert_default.file_name().unwrap_or_default());
        let mut key_file = dir.join(key_default.file_name().unwrap_or_default());
        // With no --dir, the pair a running serve uses, else the pair
        // web.json saves: Renew types this command, and it must renew the
        // pair that is served.
        if !fs.was_set("dir") {
            let (c, k) = served_pair(&self.data_dir, &cert_file, &key_file);
            cert_file = c;
            key_file = k;
        }
        if let Err(e) = web::tailscale::check_pair_files(&path_s(&cert_file), &path_s(&key_file)) {
            errln(&e);
            return 1;
        }
        for f in [&cert_file, &key_file] {
            let d = f.parent().map(PathBuf::from).unwrap_or_default();
            let made = {
                use std::os::unix::fs::DirBuilderExt;
                std::fs::DirBuilder::new()
                    .recursive(true)
                    .mode(0o700)
                    .create(&d)
            };
            if let Err(e) = made {
                errln(&sys::go_path_err("mkdir", &d, &e));
                return 1;
            }
        }
        let _ = std::io::stdout().flush();
        let status = web::tailscale::look_path().map(|bin| {
            std::process::Command::new(bin)
                .arg("cert")
                .arg(format!("--cert-file={}", cert_file.display()))
                .arg(format!("--key-file={}", key_file.display()))
                .arg(&name)
                .status()
        });
        if !matches!(status, Some(Ok(s)) if s.success()) {
            errln("tailscale cert failed. Turn on MagicDNS and HTTPS in the admin console, then try again.");
            return 1;
        }
        out(&format!(
            "cert      {}\nkey       {}\n",
            cert_file.display(),
            key_file.display()
        ));
        out(&format!(
            "\nStart the server with:\n  coppice web serve --tls tailscale --cert {} --key {}\n",
            cert_file.display(),
            key_file.display()
        ));
        out("Let's Encrypt issues this and it lasts 90 days. A running coppice web serve --tls tailscale renews it and serves the new one.\n");
        0
    }

    fn web_serve(&self, args: &[String]) -> i32 {
        let mut fs = FlagSet::new("web serve");
        fs.string("listen", ":8443", "address to serve the phone client on");
        fs.string("tls", "localca", "tailscale, localca, files, or off");
        fs.string(
            "cert",
            "",
            "certificate file for --tls files or --tls tailscale",
        );
        fs.string("key", "", "key file for --tls files or --tls tailscale");
        fs.string(
            "ca-dir",
            &path_s(&config::local_ca_dir(&self.data_dir)),
            "where the local CA lives",
        );
        fs.string(
            "ca-listen",
            ":8080",
            "plain HTTP port that hands the CA to the phone, or off",
        );
        fs.string(
            "external-url",
            "",
            "the URL the phone uses, for notification links",
        );
        fs.string("ntfy", "", "your ntfy server, for push");
        fs.string("ntfy-topic", "", "the ntfy topic to publish to");
        fs.string(
            "ntfy-token-env",
            "",
            "name of the environment variable holding the ntfy token",
        );
        fs.string(
            "gate-root",
            &path_s(&config::gate_root(&self.data_dir)),
            "the gate's ask directory",
        );
        fs.string(
            "voice-url",
            "",
            "the address of daisugi voice serve, for the phone's record button",
        );
        fs.string(
            "voice-token-file",
            "",
            "the token file that voice server checks; needed when --voice-url is on another machine",
        );
        fs.bool("web-push", false, "not built. Use ntfy.");
        fs.bool(
            "persist",
            false,
            "start with the coppice server from now on",
        );
        fs.bool(
            "forget",
            false,
            "stop starting with the coppice server, then exit",
        );
        fs.bool("qr", true, "print the sign-in QR on start");
        if fs.parse(args).is_err() {
            return 1;
        }
        if fs.flag("web-push") {
            errln("Web push is not built. Push goes through your own ntfy.\nStart again with --ntfy URL --ntfy-topic NAME.");
            return 1;
        }
        if !valid_voice_url(&fs.str("voice-url")) {
            errln(VOICE_URL_ERR);
            return 1;
        }
        let tls_source = fs.str("tls");
        let (mut cert, mut key) = (fs.str("cert"), fs.str("key"));
        if tls_source == tls::TAILSCALE && cert.is_empty() && key.is_empty() {
            let (c, k) = config::tailscale_paths(&self.data_dir);
            cert = path_s(&c);
            key = path_s(&k);
        } else if tls_source == tls::TAILSCALE && cert.is_empty() != key.is_empty() {
            errln("--tls tailscale needs both --cert and --key, or neither. Leave both blank to use the pair coppice web cert tailscale wrote, or set both --cert and --key yourself.");
            return 1;
        }
        let config_path = config::config_path(&self.data_dir);
        let mut cfg = config::Config {
            enabled: true,
            listen: fs.str("listen"),
            tls: tls_source.clone(),
            cert_file: cert.clone(),
            key_file: key.clone(),
            ca_dir: fs.str("ca-dir"),
            ca_listen: fs.str("ca-listen"),
            ntfy: fs.str("ntfy"),
            ntfy_topic: fs.str("ntfy-topic"),
            ntfy_token_env: fs.str("ntfy-token-env"),
            external_url: fs.str("external-url"),
            gate_root: fs.str("gate-root"),
            voice_url: fs.str("voice-url"),
            voice_token_file: fs.str("voice-token-file"),
        };
        if fs.flag("forget") {
            let mut saved = match config::load(&config_path) {
                Ok(s) => s,
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            };
            if saved.listen.is_empty() {
                saved = cfg.clone();
            }
            saved.enabled = false;
            if let Err(e) = config::save(&config_path, &saved) {
                errln(&e);
                return 1;
            }
            out("The phone server will not start with the coppice server.\n");
            return 0;
        }
        // --tls tailscale does the whole job before anything else is
        // printed or saved, and the MagicDNS name is the sign-in address
        // unless --external-url names another.
        let auto = tls_source == tls::TAILSCALE;
        if auto {
            let ts = match web::tailscale::read_tailscale() {
                Ok(ts) => ts,
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            };
            if let Err(e) = web::tailscale::listen_addrs(&cfg.listen, &ts.ips) {
                errln(&e);
                return 1;
            }
            let res = match web::tailscale::ensure_cert(
                &ts.name,
                &cert,
                &key,
                web::tailscale::now_nanos(),
            ) {
                Ok(r) => r,
                Err(e) if web::tailscale::valid_pair(&cert, &key, web::tailscale::now_nanos()) => {
                    // The pair on disk still serves; the server tries
                    // again once a day.
                    let days = localca::load_leaf(Path::new(&cert))
                        .map(|n| web::tailscale::cert_days(n, web::tailscale::now_nanos()))
                        .unwrap_or_default();
                    errln(&format!(
                        "The certificate renewal failed, so the certificate on disk serves. It runs out in {days} {}. {e}",
                        web::tailscale::plural(days, "day")
                    ));
                    web::tailscale::CertResult::default()
                }
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            };
            for line in web::tailscale::cert_lines(&ts.name, &res) {
                out(&format!("{line}\n"));
            }
            if cfg.external_url.is_empty() {
                let port = web::split_host_port(&cfg.listen)
                    .map(|(_, p)| p)
                    .unwrap_or_default();
                cfg.external_url = format!("https://{}", web::join_host_port(&ts.name, &port));
            }
        }
        if let Err(e) = tls::resolve(&tls::Options {
            source: &tls_source,
            cert_file: &cert,
            key_file: &key,
            ca_dir: &cfg.ca_dir,
            listen: &cfg.listen,
        }) {
            errln(&e);
            return 1;
        }
        if fs.flag("persist") {
            if let Err(e) = config::save(&config_path, &cfg) {
                errln(&e);
                return 1;
            }
            out(&format!(
                "Saved {}. The coppice server will start the phone server.\n",
                config_path.display()
            ));
        }
        let store = TokenStore::new(config::token_path(&self.data_dir));
        let tok = match store.load() {
            Ok(t) => t,
            Err(_) => match store.mint() {
                Ok(t) => {
                    out("Minted a new web token.\n");
                    t
                }
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            },
        };
        if fs.flag("qr") {
            print_sign_in(&tok, &cfg.external_url, &cfg.listen, &tls_source);
        }
        let mut opts = opts_for(self, &cfg.gate_root, &cfg.voice_url, &cfg.voice_token_file);
        opts.serving_file = path_s(&web::tailscale::serving_path(&self.data_dir));
        if auto {
            // The one line that says what now listens where, once it does.
            let base = sign_in_url(&cfg.external_url, &cfg.listen, &tls_source);
            let only = web::tailscale::blank_host(&cfg.listen);
            opts.listening = Some(Box::new(move |addrs: &[String]| {
                out(&format!(
                    "{}\n",
                    web::tailscale::listening_line(&base, addrs, only)
                ));
            }));
        }
        match serve_until_signal(&cfg, opts) {
            Ok(()) => 0,
            Err(e) => {
                errln(&e);
                1
            }
        }
    }

    fn web_token_for(
        &self,
        store: &TokenStore,
        name: &str,
        rotate: bool,
    ) -> Result<String, String> {
        if name.is_empty() {
            return match store.load() {
                Ok(t) if !rotate => Ok(t),
                _ => store.mint(),
            };
        }
        web::token::check_name(name)?;
        match store.lookup(name) {
            Err(e) if e == web::token::NO_TOKEN => store.mint_for(name),
            Ok(_) if rotate => store.mint_for(name),
            r => r,
        }
    }

    fn web_token_list(&self, store: &TokenStore, args: &[String]) -> i32 {
        if !args.is_empty() {
            errln("coppice web token list takes no arguments.");
            return 1;
        }
        let names = match store.names() {
            Ok(n) => n,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        if names.is_empty() {
            out("No named tokens. Run: coppice web token --for NAME\n");
            return 0;
        }
        for n in names {
            out(&format!("{n}\n"));
        }
        0
    }

    fn web_token(&self, args: &[String]) -> i32 {
        let store = TokenStore::new(config::token_path(&self.data_dir));
        if args.first().map(String::as_str) == Some("list") {
            return self.web_token_list(&store, &args[1..]);
        }
        let mut fs = FlagSet::new("web token");
        fs.bool("rotate", false, "mint a new token and retire the old one");
        fs.string("for", "", "the name of the person the token is for");
        fs.string("revoke", "", "retire the token minted for this name");
        fs.string(
            "url",
            "",
            "the URL the phone uses, overriding the saved one",
        );
        fs.bool("qr", true, "print the sign-in QR");
        fs.string(
            "listen",
            "",
            "the port the phone connects to, overriding the saved one",
        );
        if fs.parse(args).is_err() {
            return 1;
        }
        if let Some(a) = fs.args().first() {
            errln(&format!(
                "coppice web token takes no argument {}. To list the names, run: coppice web token list",
                sys::go_quote(a)
            ));
            return 1;
        }
        let revoke = fs.str("revoke");
        if !revoke.is_empty() {
            if let Err(e) = store.revoke(&revoke) {
                errln(&e);
                return 1;
            }
            out(&format!("Retired the token for {revoke}.\n"));
            return 0;
        }
        let for_name = fs.str("for");
        let tok = match self.web_token_for(&store, &for_name, fs.flag("rotate")) {
            Ok(t) => t,
            Err(e) => {
                errln(&e);
                return 1;
            }
        };
        if !for_name.is_empty() {
            out(&format!("name      {for_name}\n"));
        }
        let cfg = match config::load(&config::config_path(&self.data_dir)) {
            Ok(c) => c,
            Err(e) => {
                errln(&format!(
                    "web: could not read the saved settings, guessing at the address: {e}"
                ));
                config::Config::default()
            }
        };
        let mut base = fs.str("url");
        let mut addr = fs.str("listen");
        if base.is_empty() {
            base = cfg.external_url.clone();
        }
        if addr.is_empty() {
            addr = cfg.listen.clone();
        }
        if addr.is_empty() {
            addr = ":8443".into();
        }
        if fs.flag("qr") {
            print_sign_in(&tok, &base, &addr, &cfg.tls);
        } else {
            out(&format!(
                "token     {tok}\nsign in   {}/#t={tok}\n",
                sign_in_url(&base, &addr, &cfg.tls)
            ));
        }
        0
    }

    fn web_open(&self, args: &[String]) -> i32 {
        let mut fs = FlagSet::new("web");
        fs.bool(
            "no-open",
            false,
            "print the URL and do not open the browser",
        );
        fs.string(
            "listen",
            "",
            "the loopback address to serve on (default 127.0.0.1:9443, or a free port)",
        );
        if fs.parse(args).is_err() {
            return 1;
        }
        match super::client::dial(&self.socket, &self.data_dir) {
            Ok(mut cl) => cl.close(),
            Err(e) => {
                errln(&format!(
                    "The coppice server is not running and did not start: {e}"
                ));
                return 3;
            }
        }
        let mut addr = fs.str("listen");
        if addr.is_empty() {
            addr = free_loopback();
        }
        let cfg = config::Config {
            enabled: true,
            listen: addr.clone(),
            tls: tls::OFF.into(),
            gate_root: path_s(&config::gate_root(&self.data_dir)),
            ..Default::default()
        };
        if let Err(e) = tls::resolve(&tls::Options {
            source: tls::OFF,
            cert_file: "",
            key_file: "",
            ca_dir: "",
            listen: &addr,
        }) {
            errln(&e);
            return 1;
        }
        let store = TokenStore::new(config::token_path(&self.data_dir));
        let tok = match store.load() {
            Ok(t) => t,
            Err(_) => match store.mint() {
                Ok(t) => t,
                Err(e) => {
                    errln(&e);
                    return 1;
                }
            },
        };
        let link = format!("http://{addr}/#t={tok}");
        out(&format!(
            "The floor is at {link}\nPress ctrl-c to stop serving it.\n"
        ));
        if !fs.flag("no-open") {
            if let Err(e) = open_in_browser(&link) {
                errln(&format!(
                    "Could not open a browser: {e}. Open the address above."
                ));
            }
        }
        let opts = opts_for(self, &cfg.gate_root, "", "");
        match serve_until_signal(&cfg, opts) {
            Ok(()) => 0,
            Err(e) => {
                errln(&e);
                1
            }
        }
    }
}

/// RFC 3339 of a time written with nanoseconds: the fraction cut.
fn rfc3339_of(t: &str) -> String {
    match t.find('.') {
        Some(i) => format!("{}Z", &t[..i]),
        None => t.to_string(),
    }
}

/// The default address when its port is free, else one the kernel picks.
fn free_loopback() -> String {
    if std::net::TcpListener::bind(DEFAULT_WEB_LISTEN).is_ok() {
        return DEFAULT_WEB_LISTEN.into();
    }
    match std::net::TcpListener::bind("127.0.0.1:0").and_then(|l| l.local_addr()) {
        Ok(a) => a.to_string(),
        Err(_) => DEFAULT_WEB_LISTEN.into(),
    }
}

/// The desktop's opener for this OS, as Go's openInBrowser picks it: `open`
/// on macOS, else `xdg-open`, which needs a display.
fn opener(os: &str, display: bool) -> Result<&'static str, String> {
    if os == "macos" {
        return Ok("open");
    }
    if !display {
        return Err("no display".into());
    }
    Ok("xdg-open")
}

/// Hands url to the desktop's opener. With no display there is none.
fn open_in_browser(url: &str) -> Result<(), String> {
    let empty = |k: &str| std::env::var(k).map(|v| v.is_empty()).unwrap_or(true);
    let name = opener(
        std::env::consts::OS,
        !(empty("DISPLAY") && empty("WAYLAND_DISPLAY")),
    )?;
    let mut child = std::process::Command::new(name)
        .arg(url)
        .spawn()
        .map_err(|e| format!("exec: \"{name}\": {e}"))?;
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

#[cfg(test)]
mod opener_tests {
    use super::opener;

    #[test]
    fn macos_opens_with_open_and_needs_no_display() {
        assert_eq!(opener("macos", false), Ok("open"));
    }

    #[test]
    fn other_systems_use_xdg_open_only_with_a_display() {
        assert_eq!(opener("linux", true), Ok("xdg-open"));
        assert_eq!(opener("freebsd", true), Ok("xdg-open"));
        assert_eq!(opener("linux", false), Err("no display".to_string()));
    }
}
