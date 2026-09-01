//! `synthesizer.py`: the step outputs put together into one answer.

use std::cell::RefCell;
use std::rc::Rc;

use crate::gate::py::text::{rstrip, strip};
use crate::gate::pyjson::Object;
use crate::gateway::route::DEFAULT_CHEAP_MODEL;
use crate::llm::{Call, Client, Schema};
use crate::pathways::pmodel::Id;
use crate::supervise::{str_of, Session, SUCCEEDED};

/// `synthesizer.SYNTHESIZER_SYSTEM_PROMPT`.
const SYNTH_SYSTEM: &str = "You are a synthesizer. You are given the user's original request and the outputs
of the steps that were run to fulfill it. Write the single, complete final answer
the user asked for, drawing only on the step outputs. Do not mention the steps,
the plan, or that you are synthesizing \u{2014} just give the answer.
";

struct StepOutput {
    id: String,
    kind: String,
    status: String,
    output: String,
}

/// `synthesizer.collect_outputs`: each outcome with its step's kind, in
/// plan order.
fn collect_outputs(sess: &Session, steps: &[Object]) -> Vec<StepOutput> {
    let pos = |id: &str| steps.iter().rposition(|s| str_of(s, "id") == id).unwrap_or(steps.len());
    let mut outs: Vec<StepOutput> = sess
        .steps
        .iter()
        .map(|o| StepOutput {
            id: o.step_id.clone(),
            kind: steps
                .iter()
                .rev()
                .find(|s| str_of(s, "id") == o.step_id)
                .map(|s| str_of(s, "type"))
                .unwrap_or_else(|| "?".into()),
            status: o.status.clone(),
            output: o.stdout.clone(),
        })
        .collect();
    outs.sort_by_key(|o| pos(&o.id));
    outs
}

/// `synthesizer._deterministic_answer`.
fn deterministic(prompt: &str, outs: &[StepOutput]) -> String {
    let ok: Vec<&StepOutput> = outs.iter().filter(|o| o.status == SUCCEEDED && !strip(&o.output).is_empty()).collect();
    if ok.is_empty() {
        return format!("No step produced output for: {prompt}");
    }
    let mut lines = vec![format!("Results for: {prompt}"), String::new()];
    for o in ok {
        lines.push(format!("[{} \u{b7} {}]", o.id, o.kind));
        lines.push(strip(&o.output).to_string());
        lines.push(String::new());
    }
    rstrip(&lines.join("\n")).to_string()
}

/// `synthesizer.synthesize`: the answer, and whether a model wrote it. It
/// never fails: any failure gives the deterministic answer.
pub fn synthesize(c: &Rc<RefCell<Client>>, prompt: &str, sess: &Session, steps: &[Object], use_llm: bool) -> (String, bool) {
    let outs = collect_outputs(sess, steps);
    if !use_llm || c.borrow().preflight_model(DEFAULT_CHEAP_MODEL).is_err() {
        return (deterministic(prompt, &outs), false);
    }
    let blocks: Vec<String> =
        outs.iter().map(|o| format!("### step {} ({}, {})\n{}", o.id, o.kind, o.status, strip(&o.output))).collect();
    let user = format!("Original request:\n{prompt}\n\nStep outputs:\n{}", blocks.join("\n\n"));
    let mut call = Call::new(DEFAULT_CHEAP_MODEL, SYNTH_SYSTEM, &user, Schema { name: "_Answer", model: Id::Answer }, 0);
    call.default_retries = true;
    let reply = match c.borrow_mut().structured(&call) {
        Ok(r) => r,
        Err(_) => return (deterministic(prompt, &outs), false),
    };
    let a = strip(reply.value("answer").as_str().unwrap_or("")).to_string();
    if a.is_empty() {
        return (deterministic(prompt, &outs), false);
    }
    (a, true)
}
