//! A pane an agent starts is a worker for Pando (the twin of the Go
//! `panes_pando.go`).

use std::collections::BTreeMap;
use std::sync::Arc;

use super::Server;
use crate::layout;

impl Server {
    /// Sets the two Pando opt-out variables in env. A pane that a pane, a
    /// plugin or an unplaced peer asks for (an agent, never the operator) is
    /// a worker: it gets no memory view and none of its chat is logged into
    /// the owner's work. The variables ride on the layout record, so a
    /// resume, a restore and a fork keep them.
    ///
    /// Edge: the owner who types `coppice pane create` in a shell inside a
    /// coppice pane is a pane-role caller too, and the new pane is marked.
    /// Chats the owner starts from the floor, the phone or a plain terminal
    /// are not.
    pub(super) fn pando_agent_env(&self, env: &mut BTreeMap<String, String>) {
        env.insert(pando::live::SKIP_VAR.into(), "1".into());
        env.insert(
            pando::live::SKIP_LIST_VAR.into(),
            pando::live::skip_list_in(&crate::steward::store_root(&self.cfg.data_dir))
                .to_string_lossy()
                .into_owned(),
        );
    }

    /// A marked claude pty pane's argv with a minted --session-id after the
    /// program, and that id. The id is on the skip list before the process
    /// runs, so no follow poll can take the first line of its transcript.
    /// The saved argv never holds the flag: a restart mints a new id, and a
    /// resume (which has its own --resume) is left alone. None when the pane
    /// is not such a pane or the list cannot be written (its hooks list the
    /// session then).
    pub(super) fn listed_claude_argv(self: &Arc<Server>, rec: &layout::Pane) -> Option<(Vec<String>, String)> {
        let argv = rec.argv.as_ref()?;
        let on = rec.env.get(pando::live::SKIP_VAR).is_some_and(|v| pando::live::skip_value_on(v));
        if rec.kind != layout::KIND_PTY || rec.harness != "claude" || argv.is_empty() || !on {
            return None;
        }
        const OWN: [&str; 6] = ["--session-id", "--resume", "-r", "--continue", "-c", "--fork-session"];
        if argv[1..].iter().any(|a| OWN.contains(&a.as_str())) {
            return None;
        }
        let sid = pando::live::new_skipped_session(&crate::steward::store_root(&self.cfg.data_dir)).ok()?;
        let mut out = vec![argv[0].clone(), "--session-id".to_string(), sid.clone()];
        out.extend(argv[1..].iter().cloned());
        Some((out, sid))
    }
}

// Round 8b: Pando branches (the twin of the Go `panes_branch.go`). A
// long-running agent the steward starts runs in a pane of its own with
// DAISUGI_PANDO_BRANCH in its env (and no skip marker). Its chat goes into
// its branch of the steward's store, and it sees its branch and below. The
// variable rides on the layout record, so a resume, a restore and a fork
// keep it.

/// What pane.create with long_running answers for a harness other than claude.
pub const LONG_RUN_HARNESS_REFUSAL: &str = "a long-running agent starts on claude: pane.create with long_running takes harness claude, because only a Claude session can be put on the branch's session map before it starts.";

/// What a pane that only a long-running agent may ask for answers.
pub const LONG_RUN_REFUSAL: &str =
    "only a long-running agent starts a long-running agent below it: the new branch goes under the caller's own branch.";

/// The branch a pane carries and whether it is a short worker of it.
pub(super) fn pane_branch(rec: &layout::Pane) -> (String, bool) {
    let branch = rec.env.get(pando::branch::BRANCH_VAR).map(|s| s.trim().to_string()).unwrap_or_default();
    let worker = rec.env.get(pando::live::SKIP_VAR).is_some_and(|v| pando::live::skip_value_on(v));
    (branch, worker)
}

/// A long-running agent's env: the branch set and no skip marker.
pub(super) fn lead_env(env: &mut BTreeMap<String, String>, branch: &str) {
    env.remove(pando::live::SKIP_VAR);
    env.remove(pando::live::SKIP_LIST_VAR);
    env.insert(pando::branch::BRANCH_VAR.into(), branch.into());
}

impl Server {
    /// The store the branches of this server live in: the steward's store.
    pub(super) fn branch_store_dir(&self) -> std::path::PathBuf {
        self.steward_store_dir()
    }

    /// The branch of the pane that sent a request.
    pub(super) fn caller_branch(&self, ro: &super::role::Role) -> (String, bool) {
        if !ro.pane || ro.pane_id.is_empty() {
            return (String::new(), false);
        }
        match self.tree.pane(&ro.pane_id) {
            Some(rec) => pane_branch(&rec),
            None => (String::new(), false),
        }
    }

    /// A long-running agent's claude argv with a minted --session-id that
    /// is on the store's session map before it runs.
    pub(super) fn branch_claude_argv(self: &Arc<Server>, rec: &layout::Pane) -> Option<(Vec<String>, String)> {
        let argv = rec.argv.as_ref()?;
        let (branch, worker) = pane_branch(rec);
        if branch.is_empty() || worker || rec.kind != layout::KIND_PTY || rec.harness != "claude" || argv.is_empty() {
            return None;
        }
        const OWN: [&str; 6] = ["--session-id", "--resume", "-r", "--continue", "-c", "--fork-session"];
        if argv[1..].iter().any(|a| OWN.contains(&a.as_str())) {
            return None;
        }
        let sid = pando::branch::new_branch_session(&self.branch_store_dir(), &branch).ok()?;
        let mut out = vec![argv[0].clone(), "--session-id".to_string(), sid.clone()];
        out.extend(argv[1..].iter().cloned());
        Some((out, sid))
    }

    /// Puts a session coppice learned for a branch pane on the session map.
    pub(super) fn route_pane_session(&self, rec: &layout::Pane, sid: &str) {
        let (branch, worker) = pane_branch(rec);
        if branch.is_empty() || worker || sid.is_empty() {
            return;
        }
        let _ = pando::branch::add_route(&self.branch_store_dir(), &branch, &[sid]);
    }

    /// The dir a fenced pane of a branch may read, read only, or None.
    pub(super) fn pane_branch_dir(&self, rec: &layout::Pane) -> Option<std::path::PathBuf> {
        let (branch, _) = pane_branch(rec);
        if branch.is_empty() {
            return None;
        }
        pando::branch::existing_branch(&self.branch_store_dir(), &branch).ok()
    }

    /// The env of a pane an agent asks for (see the Go agentPaneEnv).
    pub(super) fn agent_pane_env(
        &self,
        ro: &super::role::Role,
        r: &crate::proto::Request,
        env: &mut BTreeMap<String, String>,
        cwd: &mut String,
    ) -> Result<(), String> {
        let (parent, parent_worker) = self.caller_branch(ro);
        let long = r.bool("long_running").unwrap_or(false);
        let sub = r.s("pando_branch");
        if !long {
            if !sub.is_empty() {
                return Err("pando_branch goes with long_running: a worker always works in its caller's branch".into());
            }
            self.pando_agent_env(env);
            if parent.is_empty() {
                env.remove(pando::branch::BRANCH_VAR);
            } else {
                env.insert(pando::branch::BRANCH_VAR.into(), parent);
            }
            return Ok(());
        }
        if parent.is_empty() || parent_worker {
            return Err(LONG_RUN_REFUSAL.into());
        }
        if r.s("harness") != "claude" {
            return Err(LONG_RUN_HARNESS_REFUSAL.into());
        }
        if sub.is_empty() {
            return Err("long_running needs pando_branch, the label of the new branch below yours".into());
        }
        let path = pando::branch::join_branch(&parent, &sub);
        pando::branch::parse_branch(&path)?;
        // A nested lead gets its own worktree and dir route (G6).
        let (wt, _) = self.lead_worktree(cwd, &path)?;
        pando::branch::reopen_branch(&self.branch_store_dir(), &path, &None)?;
        lead_env(env, &path);
        *cwd = wt;
        Ok(())
    }
}

/// A lead's worktree (and git branch) name: lead- and the branch path, a-z
/// and 0-9 kept, any other byte as a dash and two hex digits (G3).
pub(super) fn lead_worktree_name(branch: &str) -> String {
    let mut s = String::from("lead-");
    for c in branch.bytes() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() {
            s.push(c as char);
        } else {
            s.push_str(&format!("-{c:02x}"));
        }
    }
    s
}

impl Server {
    /// The dir a lead starts in and a phrase that says where (the Go
    /// leadWorktree): its own worktree in a git repo, with a dir route; a
    /// worktree that cannot be made refuses the spawn (G3).
    pub(super) fn lead_worktree(&self, dir: &str, branch: &str) -> Result<(String, String), String> {
        let Ok(top) = crate::worktree::toplevel(dir) else {
            return Ok((
                dir.to_string(),
                format!("in {dir} (not a git repo: no worktree of its own, so the owner's chats in this project share its Claude project dir)"),
            ));
        };
        let path = crate::worktree::ensure(&top, &lead_worktree_name(branch))
            .map_err(|e| format!("the lead's worktree for branch {branch} cannot be made, so it does not start: {e}"))?;
        let key = format!("{}{path}", pando::branch::DIR_ROUTE_PREFIX);
        if let Some(have) = pando::branch::load_routes(&self.branch_store_dir()).get(&key) {
            if have != branch {
                return Err(format!("the worktree {path} belongs to the branch {have}, not {branch}"));
            }
        }
        pando::branch::add_route(&self.branch_store_dir(), branch, &[&key])
            .map_err(|e| format!("the dir route of the lead's worktree cannot be written, so it does not start: {e}"))?;
        Ok((path.clone(), format!("in its worktree {path}")))
    }
}
