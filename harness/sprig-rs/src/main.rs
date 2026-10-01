//! sprig: a minimal, automatable coding-agent harness with a fail-closed
//! gate. The thin binary; the harness is the library.

use sprig::agent::Model;
use sprig::model_api::new_api_model;
use sprig::model_claude::ClaudeCodeModel;
use sprig::osx::Fd;
use std::os::unix::ffi::OsStrExt;

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    // The default backend is claude -p. SPRIG_BACKEND=api opts into the
    // direct Messages API, which needs ANTHROPIC_API_KEY.
    let new_model = || -> Result<Box<dyn Model>, Vec<u8>> {
        if std::env::var_os("SPRIG_BACKEND").is_some_and(|v| v == "api") {
            return Ok(Box::new(new_api_model()?));
        }
        Ok(Box::new(ClaudeCodeModel::new()))
    };
    let code = sprig::cli::run(
        &args,
        &mut std::io::stdin().lock(),
        &mut Fd(1),
        &mut Fd(2),
        env!("SPRIG_VERSION"),
        &new_model,
    );
    std::process::exit(code);
}
