//! tend: build the pending nodes of a chat with a summarizer. The twin of
//! Go's pando tend.go and redo.go.

use super::chat::{finish, run_job, Chat, RedoSet, Summarizer};
use super::compactor::CLIP_MARKER;
use super::ctx::Ctx;
use super::order::NodeId;

/// How many model calls at the start of a run may all fall back to a clipped
/// node before the run stops. A model that returns no text for the first few
/// nodes will do so for all of them.
pub const CLIP_STOP_AFTER: usize = 5;

/// Ends a run whose first `CLIP_STOP_AFTER` model calls all gave no line. The
/// nodes are written as clipped copies of the source; redo them with
/// `tend --redo-clipped` once the model is fixed.
pub const ERR_ALL_CLIPPED: &str = "the model returned no text for the first 5 nodes; if it is a thinking model, raise max_tokens in its profile or use a non-thinking model";

/// Says whether a node's text is a clipped fallback: the marker and the start
/// of the source (FM-R-64).
pub fn is_clipped(text: &str) -> bool {
    text.starts_with(CLIP_MARKER)
}

/// What a tend run did, over the store and its children: the nodes it wrote,
/// the model calls it made, and how many of those gave a clipped fallback.
/// With `stop_on_clips`, a run whose first `CLIP_STOP_AFTER` calls all
/// clipped stops with `ERR_ALL_CLIPPED`.
/// A shared writer for the progress lines of a run (the process's stderr).
pub type ProgressSink = std::sync::Arc<std::sync::Mutex<dyn std::io::Write + Send>>;

#[derive(Default, Clone)]
pub struct TendRun {
    pub stop_on_clips: bool,
    pub built: usize,
    pub calls: usize,
    pub clipped: usize,
    /// The model calls the run made, every re-ask included, when the
    /// summarizer counts them; else one for each job. --max caps it.
    pub spent: usize,
    /// Where one line goes for each node written, or None for none.
    pub progress: Option<ProgressSink>,
    /// The profile of a redo run, for its marker.
    pub model: String,
    /// Child stores a redo run skipped because they were locked.
    pub locked: Vec<std::path::PathBuf>,
}

impl std::fmt::Debug for TendRun {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TendRun").field("built", &self.built).field("calls", &self.calls).field("spent", &self.spent).finish()
    }
}

impl TendRun {
    /// Runs one job and adds the model calls it made to `spent`. A
    /// summarizer that counts its calls is believed; any other counts one
    /// call for each job.
    fn job(&mut self, ctx: &Ctx, s: &dyn Summarizer, j: &super::chat::Job) -> Result<String, String> {
        let before = s.calls_made();
        let r = run_job(ctx, s, j);
        match (before, s.calls_made()) {
            (Some(b), Some(a)) => self.spent += a - b,
            _ => self.spent += 1,
        }
        r
    }

    fn report(&self, id: NodeId, took: std::time::Duration) {
        if let Some(p) = &self.progress {
            let mut w = p.lock().unwrap_or_else(|e| e.into_inner());
            let _ = writeln!(
                w,
                "built level {} index {} in {:.1}s ({} model calls so far)",
                id.l,
                id.i,
                took.as_secs_f64(),
                self.spent
            );
            let _ = w.flush();
        }
    }

    fn all_clipped(&self) -> bool {
        self.stop_on_clips && self.calls == CLIP_STOP_AFTER && self.clipped == self.calls
    }
}

/// Builds up to max pending nodes of c with s, one call at a time, in queue
/// order. max 0 means all. It stops at the first failure of a model call:
/// the node waits by the retry schedule and the error comes back with the
/// count of nodes written, with or without the model. The caller holds the
/// store (Chat::open) and no other lock is taken here.
pub fn tend(ctx: &Ctx, c: &mut Chat, s: &dyn Summarizer, max: usize) -> (usize, Option<String>) {
    let mut run = TendRun::default();
    let err = tend_calls(ctx, c, s, max, &mut run);
    (run.built, err)
}

/// `tend` that also counts, in run, the model calls it made and how many
/// fell back to a clipped node. max caps the calls of this store: a node the
/// store can build without a model does not count.
pub fn tend_calls(
    ctx: &Ctx,
    c: &mut Chat,
    s: &dyn Summarizer,
    max: usize,
    run: &mut TendRun,
) -> Option<String> {
    let before = c.pending();
    let start = run.spent;
    let mut err = None;
    loop {
        if max > 0 && run.spent - start >= max {
            break;
        }
        c.prioritize();
        let (job, nerr) = c.next();
        if let Some(e) = nerr {
            err = Some(e);
            break;
        }
        let Some(job) = job else { break };
        if let Some(e) = ctx.err() {
            err = Some(e.text().into());
            break;
        }
        let t0 = std::time::Instant::now();
        match run.job(ctx, s, &job) {
            Err(e) => {
                if ctx.err().is_none() {
                    c.fail_err(&job, &e);
                }
                err = Some(e);
                break;
            }
            Ok(text) => {
                run.calls += 1;
                if is_clipped(&text) {
                    run.clipped += 1;
                }
                if let Err(e) = c.commit(&job, &text) {
                    err = Some(e);
                    break;
                }
                run.report(job.id, t0.elapsed());
                if run.all_clipped() {
                    err = Some(ERR_ALL_CLIPPED.into());
                    break;
                }
            }
        }
    }
    run.built += before.saturating_sub(c.pending());
    err
}

/// Builds again the nodes `Chat::redo_nodes` names for `set`, lowest level first, with
/// s. A node whose line is a clipped fallback, or is stale after a child was
/// written again, gets a new line; an ancestor is made again only when a
/// child was. max caps the model calls of this store; a node made without
/// the model does not count. A node whose new text is the same clipped text
/// it had is left. Counts go into run.
pub fn redo_calls(
    ctx: &Ctx,
    c: &mut Chat,
    s: &dyn Summarizer,
    set: RedoSet,
    max: usize,
    run: &mut TendRun,
) -> Option<String> {
    let start = run.spent;
    c.redo_model = run.model.clone();
    if !c.redo_nodes(set).is_empty() {
        if let Err(e) = c.redo_begin(set) {
            return Some(e);
        }
    }
    for id in c.redo_nodes(set) {
        if max > 0 && run.spent - start >= max {
            break;
        }
        let t0 = std::time::Instant::now();
        let old = c.node(id).map(|n| n.text.clone()).unwrap_or_default();
        let stale = c.is_stale(id);
        // An ancestor is made again only when a child was written again,
        // unless the level rule picked it.
        if !stale && !is_clipped(&old) && !set.in_level(id) {
            continue;
        }
        let (job, mut text) = match c.prepare(id) {
            Ok(x) => x,
            Err(e) => return Some(e),
        };
        if let Some(job) = job {
            if let Some(e) = ctx.err() {
                return Some(e.text().into());
            }
            let got = match run.job(ctx, s, &job) {
                Ok(t) => t,
                Err(e) => return Some(e),
            };
            run.calls += 1;
            text = finish(&got, job.limit);
            if is_clipped(&text) {
                run.clipped += 1;
            }
        }
        if text != old || stale {
            if let Err(e) = c.rewrite(id, &text) {
                return Some(e);
            }
            run.built += 1;
            run.report(id, t0.elapsed());
        }
        if run.all_clipped() {
            return Some(ERR_ALL_CLIPPED.into());
        }
    }
    None
}

impl Chat {
    /// Names the pending nodes in build order.
    pub fn pending_node_ids(&self) -> Vec<NodeId> {
        self.pending_ids()
    }
}
