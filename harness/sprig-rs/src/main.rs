//! sprig: a minimal, automatable coding-agent harness with a fail-closed
//! gate. The thin binary; the harness is the library.

use sprig::agent::Model;
use sprig::cli::ModelOptions;
use sprig::model_api::new_api_model_for;
use sprig::model_claude::ClaudeCodeModel;
use sprig::model_openai::new_openai_model;
use sprig::osx::Fd;
use std::os::unix::ffi::OsStrExt;

fn main() {
    let args: Vec<Vec<u8>> = std::env::args_os()
        .skip(1)
        .map(|a| a.as_bytes().to_vec())
        .collect();
    // The default backend is claude -p. --wire anthropic (or
    // SPRIG_BACKEND=api) opts into the direct Messages API, and --wire
    // openai into chat completions; each takes a key from --key-fd unless
    // --base-url names a server that needs none. --model
    // overrides the backend's default model (and SPRIG_MODEL on the API);
    // --tools sets the tools the backend offers.
    let new_model = |o: &ModelOptions| -> Result<Box<dyn Model>, Vec<u8>> {
        if o.wire == "anthropic" {
            let mut m = new_api_model_for(&o.key, o.key_set, &o.base_url)?;
            if !o.model.is_empty() {
                m.model = o.model.clone();
            }
            m.tools = o.tools.clone();
            (m.no_tools, m.system, m.system_set) = (o.no_tools, o.system.clone(), o.system_set);
            m.extra = o.extra.clone();
            return Ok(Box::new(m));
        }
        if o.wire == "openai" {
            let mut m = new_openai_model(&o.key, o.key_set, &o.base_url, &o.model)?;
            m.tools = o.tools.clone();
            (m.no_tools, m.system, m.system_set) = (o.no_tools, o.system.clone(), o.system_set);
            m.extra = o.extra.clone();
            return Ok(Box::new(m));
        }
        let mut m = ClaudeCodeModel::new();
        if !o.model.is_empty() {
            m.model = o.model.clone();
        }
        m.tools = o.tools.clone();
        (m.no_tools, m.system, m.system_set) = (o.no_tools, o.system.clone(), o.system_set);
        (m.mcp_config, m.mcp_servers) = (o.mcp_config.clone(), o.mcp_servers.clone());
        Ok(Box::new(m))
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
