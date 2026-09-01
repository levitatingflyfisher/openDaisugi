//! `l-probe`: a test instrument for clients/l_compare.py. It answers one
//! query (argv[1], JSON) of the stage-L library parts no command reaches
//! (the deed ledger, the strata store, batch runs, pathway bundles and
//! the signing primitives) as clients/l_probe_oracle.py answers it. It is
//! not shipped.

fn main() {
    std::process::exit(daisugi_verify::cli::lprobe::main())
}
