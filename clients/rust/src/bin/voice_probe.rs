//! `voice-probe`: a test instrument for clients/voice_cases.py, one query
//! of the voice bridge's pure parts on stdin, answered as JSON on stdout.
//! Not shipped.

fn main() {
    std::process::exit(daisugi_verify::voice::probe::main())
}
