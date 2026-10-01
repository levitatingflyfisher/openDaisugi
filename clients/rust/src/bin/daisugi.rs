//! `daisugi`: the openDaisugi CLI in one Rust binary, the gate commands
//! and the gate half of install. See `cli`.

fn main() {
    // `fence` first, before any CLI state: it costs one exec.
    if std::env::args_os().nth(1).as_deref() == Some(std::ffi::OsStr::new("fence")) {
        let rest: Vec<String> =
            std::env::args_os().skip(2).map(|a| a.to_string_lossy().into_owned()).collect();
        daisugi_verify::fence::main(rest);
    }
    daisugi_verify::cli::main()
}
