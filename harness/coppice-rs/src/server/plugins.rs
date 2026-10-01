//! What the server knows of the plugins: the verbs and events each enabled
//! policy holds, and the pid of each running policy, so a policy's own
//! connection is placed as that plugin.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, MutexGuard};

use super::Server;
use crate::proto::{self, Request, Response};
use crate::sys;

/// The plugin state, under one lock that placing a connection takes.
#[derive(Default)]
pub struct PlugState {
    /// Each enabled policy's needs and listens.
    pub specs: HashMap<String, (Vec<String>, Vec<String>)>,
    /// The pid of each policy, kept while its session has a process.
    pub pids: HashMap<i32, String>,
    /// The pids of policies that exited.
    pub exited: HashSet<i32>,
}

impl PlugState {
    /// Forgets each exited policy whose session is empty. A session the
    /// scan cannot read counts as not empty.
    pub fn prune(&mut self) {
        if self.exited.is_empty() {
            return;
        }
        let Ok(rd) = std::fs::read_dir("/proc") else {
            return;
        };
        let mut live = HashSet::new();
        for e in rd.flatten() {
            let Ok(pid) = e.file_name().to_string_lossy().parse::<i32>() else {
                continue;
            };
            if let Ok((_, sid)) = sys::proc_stat(pid) {
                if self.exited.contains(&sid) {
                    live.insert(sid);
                }
            }
        }
        let gone: Vec<i32> = self
            .exited
            .iter()
            .filter(|p| !live.contains(p))
            .copied()
            .collect();
        for pid in gone {
            self.exited.remove(&pid);
            self.pids.remove(&pid);
        }
    }
}

/// Verbs that only read. A plugin that runs one makes no note.
pub fn read_verb(cmd: &str) -> bool {
    matches!(
        cmd,
        "pane.list"
            | "session.list"
            | "pane.read"
            | "task.list"
            | "agent.list"
            | "agent.get"
            | "server.status"
            | "floor.notes"
            | "pane.explain"
            | "floor.facts"
    )
}

fn refusal(id: &str, verb: &str) -> String {
    format!("plugin {id} did not ask for {verb} in its manifest")
}

impl Server {
    pub fn plug_lock(&self) -> MutexGuard<'_, PlugState> {
        self.plug.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Records the enabled policies. A plugin connection whose id is not
    /// here holds no verb at all.
    pub fn set_plugins(&self, specs: HashMap<String, (Vec<String>, Vec<String>)>) {
        self.plug_lock().specs = specs;
    }

    pub fn plugin_spec(&self, id: &str) -> Option<(Vec<String>, Vec<String>)> {
        self.plug_lock().specs.get(id).cloned()
    }

    /// Refuses a verb plugin id did not ask for.
    pub fn plugin_guard(&self, id: &str, r: &Request) -> Result<(), Response> {
        let refuse = |m: &str| Err(proto::err_resp(&r.id, proto::ERR_UNAUTHORIZED, m));
        let Some((needs, listens)) = self.plugin_spec(id) else {
            return refuse(&format!("plugin {id} is not enabled. It holds no verb."));
        };
        if r.cmd == "pane.report_state" || r.cmd == "pane.report_child" {
            return refuse(&format!(
                "a plugin reports no state. {}",
                super::role::PANE_REFUSAL
            ));
        }
        if r.cmd == "events.subscribe" {
            let kinds = match r.str_slice("kinds") {
                Some(k) if !k.is_empty() => k,
                _ => vec!["state".to_string(), "layout".to_string()],
            };
            for k in kinds {
                if !listens.contains(&k) {
                    return refuse(&format!(
                        "plugin {id} does not listen to {k} in its manifest"
                    ));
                }
            }
            return Ok(());
        }
        if r.cmd == "events.pause" || r.cmd == "events.resume" {
            if listens.is_empty() {
                return refuse(&refusal(id, &r.cmd));
            }
            return Ok(());
        }
        if needs.contains(&r.cmd) {
            return Ok(());
        }
        refuse(&refusal(id, &r.cmd))
    }
}

/// The server as the plugin runner's host.
pub struct PluginHost(pub Arc<Server>);

impl crate::plugins::Host for PluginHost {
    fn launch(
        &self,
        id: &str,
        start: &mut dyn FnMut() -> Result<i32, String>,
    ) -> Result<i32, String> {
        let mut st = self.0.plug_lock();
        let pid = start()?;
        if pid <= 0 {
            return Err(format!("plugin {id} started with no pid"));
        }
        st.pids.insert(pid, id.to_string());
        st.exited.remove(&pid);
        Ok(pid)
    }

    fn exited(&self, pid: i32) {
        let mut st = self.0.plug_lock();
        if !st.pids.contains_key(&pid) {
            return;
        }
        st.exited.insert(pid);
        st.prune();
    }
}
