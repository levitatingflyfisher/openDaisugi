//! Workers go where there is room (FM-R-93 to FM-R-101). A worker profile
//! may name a pool, from pools.toml beside coppice.toml: several (machine,
//! base URL) entries that serve ONE model. A spawn takes the first entry
//! with a free slot, else the job waits in a queue the owner can read. An
//! entry that does not answer cools down with a back-off and is tried again
//! when it ends. A quota moves the job to another entry of the same pool,
//! which serves the same model; when every entry is out the job is requeued
//! and the foreman says so in one line. Nothing here moves a job to another
//! model. A background job waits for an entry that has had no foreground job
//! for idle_after, and is paused (its process group stopped) when
//! foreground work needs the room. The state is files and memory of the
//! server: no new daemon, and no network code beyond the base URL the
//! worker already uses. The twin of Go's foreman_pool.go.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use serde_json::Value;

use super::foreman_chat::ForemanChat;
use super::foreman_work::{
    base, envelope_dir, read_project_envelope, WorkState, WORK_FAILED, WORK_PAUSED, WORK_QUEUED,
    WORK_RAN, WORK_RUNNING,
};
use super::Server;
use crate::config;
use crate::foreman::chat as fchat;
use crate::foreman::ctx::Ctx;
use crate::foreman::failover::{classify, safe_reason, Outcome};
use crate::foreman::pool::{
    load_pools_or_aside, pick_entry, read_queue_or_aside, take_queue_requests, write_queue, Health, HeldJob, Load, Pool,
    PoolEntry, PoolSet, QueueEntry, QueueJob, QueueState, POOLS_FILE,
    QUEUE_REQUEST_DIR,
};

/// How long after a resume a failure counts as the pause's (FM-R-107).
const RESUME_GRACE: i64 = 60;

/// A job that is not running: it waits, or it is paused.
pub struct HeldEntry {
    pub job: HeldJob,
    pub fc: Arc<ForemanChat>,
}

/// The pool state of the server. The work lock guards all of it.
#[derive(Default)]
pub struct PoolRuntime {
    pub health: BTreeMap<String, Health>,
    pub last_fg: BTreeMap<String, i64>,
    pub held: Vec<HeldEntry>,
    pub pids: BTreeMap<String, i32>,
    /// The process groups a yield stopped, by label (FM-R-107).
    pub frozen: BTreeMap<String, Vec<i32>>,
    /// When a paused job last went on, by label (FM-R-107).
    pub resumed: BTreeMap<String, i64>,
    /// Jobs that went on in place because their pool or entry left the pools
    /// file; no yield pauses them again (FM-R-108).
    pub no_yield: std::collections::BTreeSet<String>,
    pub timer_gen: u64,
    pub fc: Option<Arc<ForemanChat>>,
}

/// One line for the foreman's chat, logged after the lock is off.
pub type Line = (Arc<ForemanChat>, String);

fn entry_key(pool: &str, entry: &str) -> String {
    format!("{pool}/{entry}")
}

fn entry_of<'a>(pool: &'a Pool, name: &str) -> Option<&'a PoolEntry> {
    pool.entries.iter().find(|e| e.name == name)
}

fn clock_text(secs: i64) -> String {
    let t = fchat::date_text(SystemTime::UNIX_EPOCH + Duration::from_secs(secs.max(0) as u64));
    format!("{} UTC", &t[11..16])
}

fn date_of(secs: i64) -> String {
    fchat::date_text(SystemTime::UNIX_EPOCH + Duration::from_secs(secs.max(0) as u64))
}

/// Sets the base URL of one profile in the profile bundle. The key is
/// copied with it and never leaves the bytes.
pub fn patch_bundle_url(bundle: &mut [u8], profile: &str, base_url: &str) -> Result<Vec<u8>, String> {
    let mut doc: Value = serde_json::from_slice(bundle)
        .map_err(|_| "the agent profiles do not read".to_string())?;
    let Some(e) = doc.get_mut(profile).and_then(Value::as_object_mut) else {
        return Err(format!("the profile {profile} is not in the agent profiles"));
    };
    e.insert("base_url".into(), Value::String(base_url.into()));
    let out = serde_json::to_vec(&doc).map_err(|e| e.to_string())?;
    bundle.fill(0);
    Ok(out)
}

impl Server {
    pub(super) fn pools_path(&self) -> PathBuf {
        config::path()
            .parent()
            .map(|p| p.to_path_buf())
            .unwrap_or_default()
            .join(POOLS_FILE)
    }

    pub(super) fn pool_now(&self) -> i64 {
        self.clock_seconds() as i64
    }

    /// The pool the profile names, or None. The default profile is never a
    /// pool. A pools file that does not read is moved aside and the foreman
    /// goes on with no pools (FM-R-112), so a profile that names no pool
    /// never fails because of it.
    pub(super) fn pool_for(self: &Arc<Server>, fc: &Arc<ForemanChat>, profile: &str) -> Option<Pool> {
        if profile == config::DEFAULT_COMPACTOR_PROFILE {
            return None;
        }
        let (set, note) = self.load_pool_set();
        if !note.is_empty() {
            self.log_work(fc, &note);
        }
        set.pools.get(profile).cloned()
    }

    /// Reads the pools file. When it does not read it is moved to
    /// pools.toml.broken-<time>, never deleted, and the result is no pools;
    /// the note is the one line that says where the file went.
    pub(super) fn load_pool_set(&self) -> (PoolSet, String) {
        load_pools_or_aside(&self.pools_path(), self.fm_now())
    }

    pub(super) fn flush_lines(self: &Arc<Server>, lines: Vec<Line>) {
        for (fc, text) in lines {
            self.log_work(&fc, &text);
        }
    }

    /// Records the pid of a worker's process group. A foreground job may
    /// wait for the room this worker holds, and could not pause it before
    /// its process group was known.
    pub(super) fn pool_started(self: &Arc<Server>, label: &str, pid: i32) {
        let lines = {
            let mut w = self.work_lock();
            w.pool.pids.insert(label.to_string(), pid);
            match w.pool.fc.clone() {
                Some(fc) if !w.pool.held.is_empty() => self.pool_pump_locked(&mut w, &fc, ""),
                _ => vec![],
            }
        };
        self.flush_lines(lines);
    }

    /// Drops the pid of a run that ended. A run that replaced it under the
    /// same label (a quota restart) keeps its own pid: the entry goes only
    /// when it still holds this run's pid.
    pub(super) fn pool_forget(&self, label: &str, pid: i32) {
        let mut w = self.work_lock();
        if w.pool.pids.get(label).copied() != Some(pid) {
            return;
        }
        w.pool.pids.remove(label);
        w.pool.frozen.remove(label);
        w.pool.resumed.remove(label);
        w.pool.no_yield.remove(label);
    }

    /// Records a worker for a pool, puts it in the queue and runs the queue
    /// once. The answer says where it started or why it waits.
    pub(super) fn spawn_pooled(
        self: &Arc<Server>,
        fc: &Arc<ForemanChat>,
        dir: String,
        brief: &str,
        pool: &Pool,
        background: bool,
    ) -> Result<String, String> {
        let name = base(&dir);
        if !self.work_lock().loaded {
            self.load_workers(fc);
        }
        let (res, lines) = {
            let mut w = self.work_lock();
            if self.is_closed() {
                return Err("the server is closing".into());
            }
            let n = {
                let e = w.next.entry(name.clone()).or_insert(0);
                *e += 1;
                *e
            };
            let label = format!("{name}-{n}");
            let now = self.fm_date();
            w.work.push(super::foreman_work::WorkerRec {
                label: label.clone(),
                project: name.clone(),
                dir,
                profile: pool.name.clone(),
                model: pool.model.clone(),
                started: now.clone(),
                state: WORK_QUEUED.into(),
                pool: pool.name.clone(),
                background,
                ..Default::default()
            });
            w.pool.held.push(HeldEntry {
                job: HeldJob {
                    label: label.clone(),
                    project: name,
                    pool: pool.name.clone(),
                    model: pool.model.clone(),
                    brief: brief.to_string(),
                    background,
                    added: now,
                    cut: false,
                },
                fc: fc.clone(),
            });
            let lines = self.pool_pump_locked(&mut w, fc, &label);
            self.save_workers_locked(fc, &w);
            let rec = w.work.iter().find(|r| r.label == label).cloned().unwrap_or_default();
            let res = if rec.state == WORK_RUNNING {
                format!("started {label} on {}", rec.entry)
            } else {
                format!(
                    "queued {label}: {} ({} waiting)",
                    wait_reason(pool, background),
                    w.pool.held.len()
                )
            };
            (res, lines)
        };
        self.flush_lines(lines);
        Ok(res)
    }

    /// The load of each entry of pool.
    fn loads_locked(&self, w: &WorkState, pool: &Pool) -> BTreeMap<String, Load> {
        let mut loads = BTreeMap::new();
        for e in &pool.entries {
            let key = entry_key(&pool.name, &e.name);
            loads.insert(
                e.name.clone(),
                Load {
                    last_fg: w.pool.last_fg.get(&key).copied().unwrap_or(0),
                    until: w.pool.health.get(&key).map(|h| h.until).unwrap_or(0),
                    ..Default::default()
                },
            );
        }
        for r in &w.work {
            if r.pool != pool.name || r.state != WORK_RUNNING {
                continue;
            }
            if let Some(l) = loads.get_mut(&r.entry) {
                l.running += 1;
                if !r.background {
                    l.fg_running += 1;
                }
            }
        }
        loads
    }

    /// Runs the queue once. Tests and the timer call it.
    pub(super) fn pool_pump(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        let lines = {
            let mut w = self.work_lock();
            self.pool_pump_locked(&mut w, fc, "")
        };
        self.flush_lines(lines);
    }

    /// Places what can be placed, in this order: foreground jobs (pausing a
    /// background worker for one that has no room), then the background
    /// jobs. skip names a label whose own start the caller reports.
    pub(super) fn pool_pump_locked(
        self: &Arc<Server>,
        w: &mut WorkState,
        fc: &Arc<ForemanChat>,
        skip: &str,
    ) -> Vec<Line> {
        w.pool.fc = Some(fc.clone());
        if self.is_closed() {
            return vec![];
        }
        let (set, note) = self.load_pool_set();
        let now = self.pool_now();
        let mut lines: Vec<Line> = vec![];
        if !note.is_empty() {
            lines.push((fc.clone(), note));
        }
        for bg in [false, true] {
            let mut i = 0;
            while i < w.pool.held.len() {
                if w.pool.held[i].job.background != bg {
                    i += 1;
                    continue;
                }
                let pool_name = w.pool.held[i].job.pool.clone();
                let label = w.pool.held[i].job.label.clone();
                let hfc = w.pool.held[i].fc.clone();
                // A paused job has a stopped process. When its pool or its
                // entry has gone from the pools file it can never be picked
                // again: it goes on and runs to its end (FM-R-108).
                let paused_entry = w
                    .work
                    .iter()
                    .find(|r| r.label == label && r.state == WORK_PAUSED)
                    .map(|r| r.entry.clone());
                if let Some(entry) = paused_entry {
                    if set.pools.get(&pool_name).and_then(|p| entry_of(p, &entry)).is_none() {
                        self.thaw_locked(w, &label, now);
                        if let Some(r) = w.work.iter_mut().find(|r| r.label == label) {
                            r.state = WORK_RUNNING.into();
                        }
                        w.pool.no_yield.insert(label.clone());
                        lines.push((
                            hfc,
                            format!(
                                "[{label}] resumed on {entry}: it is no longer in {POOLS_FILE}, so it runs on and is not paused again"
                            ),
                        ));
                        w.pool.held.remove(i);
                        continue;
                    }
                }
                let Some(pool) = set.pools.get(&pool_name) else {
                    if let Some(r) = w.work.iter_mut().find(|r| r.label == label) {
                        r.state = WORK_FAILED.into();
                        r.entry.clear();
                    }
                    lines.push((
                        hfc,
                        format!(
                            "[{label}] failed: the pool {pool_name} is not in {POOLS_FILE} any more"
                        ),
                    ));
                    w.pool.held.remove(i);
                    continue;
                };
                let mut pick = self.pick_locked(w, pool, &set, i, now);
                if pick.is_none() && !bg {
                    if let Some((vlabel, ventry)) = self.yield_locked(w, pool, now) {
                        lines.push((
                            hfc.clone(),
                            format!(
                                "[{vlabel}] paused on {ventry} to make room for foreground work; it resumes when the entry has been idle for {}",
                                crate::foreman::compactor::go_duration(Duration::from_secs(
                                    set.idle_after as u64
                                ))
                            ),
                        ));
                        pick = self.pick_locked(w, pool, &set, i, now);
                    }
                }
                let Some(entry) = pick else {
                    i += 1;
                    continue;
                };
                let h = w.pool.held.remove(i);
                let tell = h.job.label != skip;
                let text = self.start_locked(w, h, pool, &entry, now, tell);
                if !text.is_empty() {
                    lines.push((hfc, text));
                }
            }
        }
        self.persist_queue_locked(w, fc, &set, now);
        self.arm_timer_locked(w, &set, now);
        lines
    }

    /// The entry job i may take now. A paused job can only go back to the
    /// entry its process lives on.
    fn pick_locked(
        &self,
        w: &WorkState,
        pool: &Pool,
        set: &PoolSet,
        i: usize,
        now: i64,
    ) -> Option<String> {
        let loads = self.loads_locked(w, pool);
        let h = &w.pool.held[i];
        let rec = w.work.iter().find(|r| r.label == h.job.label)?;
        if rec.state == WORK_PAUSED {
            let l = loads.get(&rec.entry).cloned().unwrap_or_default();
            let e = entry_of(pool, &rec.entry)?;
            if l.until > now || l.running >= e.slots || !l.idle(now, set.idle_after) {
                return None;
            }
            return Some(rec.entry.clone());
        }
        pick_entry(pool, &loads, now, h.job.background, set.idle_after)
    }

    /// Stops the process group of one running background worker of the pool,
    /// if one has an entry that is not cooling down. It returns the label
    /// and the entry of the job, now paused and held. The group is stopped,
    /// not killed, and goes on with SIGCONT: nothing is cut mid-write
    /// (FM-R-98).
    pub(super) fn yield_locked(&self, w: &mut WorkState, pool: &Pool, now: i64) -> Option<(String, String)> {
        let loads = self.loads_locked(w, pool);
        let fc = w.pool.fc.clone()?;
        for k in 0..w.work.len() {
            let r = &w.work[k];
            let pid = w.pool.pids.get(&r.label).copied().unwrap_or(0);
            if r.pool != pool.name
                || !r.background
                || r.state != WORK_RUNNING
                || pid <= 0
                || w.pool.no_yield.contains(&r.label)
                || loads.get(&r.entry).is_some_and(|l| l.until > now)
            {
                continue;
            }
            // Every process group of the worker stops, the model process
            // included. The room counts as free only when all of them have.
            let Some(groups) = super::foreman_freeze::freeze_tree(pid) else {
                continue;
            };
            let label = w.work[k].label.clone();
            w.pool.frozen.insert(label, groups);
            let r = &mut w.work[k];
            r.state = WORK_PAUSED.into();
            let (label, entry) = (r.label.clone(), r.entry.clone());
            let job = HeldJob {
                label: label.clone(),
                project: r.project.clone(),
                pool: r.pool.clone(),
                model: r.model.clone(),
                background: true,
                added: r.started.clone(),
                ..Default::default()
            };
            w.pool.held.push(HeldEntry { job, fc });
            return Some((label, entry));
        }
        None
    }

    /// Resumes the process groups of a paused job. A stream that was open
    /// when the job stopped can fail the moment it goes on, and the node
    /// timeout of the tree counts the time it stood still, so the time of the
    /// resume is kept for pool_out (FM-R-107).
    pub(super) fn thaw_locked(&self, w: &mut WorkState, label: &str, now: i64) {
        w.pool.resumed.insert(label.to_string(), now);
        let groups = w.pool.frozen.remove(label).unwrap_or_default();
        let pid = w.pool.pids.get(label).copied().unwrap_or(0);
        super::foreman_freeze::thaw_tree(pid, &groups);
    }

    /// Starts, or resumes, job h on the entry. It returns the line for the
    /// chat; an empty line is a start that the spawn call reports itself.
    fn start_locked(
        self: &Arc<Server>,
        w: &mut WorkState,
        h: HeldEntry,
        pool: &Pool,
        entry: &str,
        now: i64,
        tell: bool,
    ) -> String {
        let label = h.job.label.clone();
        let key = entry_key(&pool.name, entry);
        if !h.job.background {
            w.pool.last_fg.insert(key, now);
        }
        let Some(idx) = w.work.iter().position(|r| r.label == label) else {
            return String::new();
        };
        if w.work[idx].state == WORK_PAUSED {
            self.thaw_locked(w, &label, now);
            w.work[idx].state = WORK_RUNNING.into();
            return format!("[{label}] resumed on {entry}");
        }
        let project = w.work[idx].project.clone();
        let pe = match read_project_envelope(&envelope_dir().join(format!("{project}.json")), &project)
        {
            Ok(pe) => pe,
            Err(e) => {
                w.work[idx].state = WORK_FAILED.into();
                w.work[idx].entry.clear();
                return format!("[{label}] failed: {e}");
            }
        };
        w.work[idx].state = WORK_RUNNING.into();
        w.work[idx].entry = entry.to_string();
        let mut brief = h.job.brief.clone();
        if h.job.cut {
            brief = format!(
                "Your earlier run on this brief was cut off. The worktree holds the work it did. Continue it.\n\n{brief}"
            );
        }
        let base_url = entry_of(pool, entry).map(|e| e.base_url.clone()).unwrap_or_default();
        self.work_wg.add();
        let ctx = w.ctx.clone().unwrap_or_default();
        let rec = w.work[idx].clone();
        let (s, fc) = (self.clone(), h.fc.clone());
        std::thread::spawn(move || {
            s.run_worker(&ctx, &fc, rec, pe, &brief, &base_url);
            s.work_wg.done();
        });
        if tell {
            format!("[{label}] started on {entry}")
        } else {
            String::new()
        }
    }

    /// The pool part of a worker's end: the entry answered, a foreground job
    /// counts as the entry's last foreground work, and the queue runs again.
    /// The caller holds the work lock.
    pub(super) fn pool_ended_locked(
        self: &Arc<Server>,
        w: &mut WorkState,
        fc: &Arc<ForemanChat>,
        label: &str,
        state: &str,
    ) -> Vec<Line> {
        let Some(r) = w.work.iter().find(|r| r.label == label).cloned() else {
            return vec![];
        };
        let key = entry_key(&r.pool, &r.entry);
        if state == WORK_RAN {
            if let Some(h) = w.pool.health.get_mut(&key) {
                h.ok();
            }
        }
        if !r.background {
            let now = self.pool_now();
            w.pool.last_fg.insert(key, now);
        }
        self.pool_pump_locked(w, fc, "")
    }

    /// Reads the failure of a pool worker. When its entry is out or did not
    /// answer, the entry cools down and the job goes back in the queue at the
    /// front, to run on another entry of the same pool; it returns true. Any
    /// other failure, and a key problem, are the worker's own end (false).
    pub(super) fn pool_out(
        self: &Arc<Server>,
        ctx: &Ctx,
        fc: &Arc<ForemanChat>,
        label: &str,
        brief: &str,
        detail: &str,
    ) -> bool {
        if ctx.err().is_some() {
            return false;
        }
        if self
            .work_lock()
            .work
            .iter()
            .find(|r| r.label == label)
            .is_none_or(|r| r.pool.is_empty())
        {
            return false;
        }
        let now = self.pool_now();
        // A failure that comes within RESUME_GRACE of a resume is the pause:
        // the node timeout counted the time the job stood still, or its
        // stream died while it was stopped. The entry did nothing wrong: it
        // does not cool, and the job runs again once (FM-R-107).
        let resumed = {
            let mut w = self.work_lock();
            w.pool.resumed.remove(label)
        };
        if resumed.is_some_and(|at| now - at < RESUME_GRACE) {
            let lines = {
                let mut w = self.work_lock();
                let Some(idx) = w.work.iter().position(|r| r.label == label) else {
                    return false;
                };
                let r = w.work[idx].clone();
                w.work[idx].state = WORK_QUEUED.into();
                w.work[idx].entry.clear();
                w.pool.held.insert(
                    0,
                    HeldEntry {
                        job: HeldJob {
                            label: label.to_string(),
                            project: r.project.clone(),
                            pool: r.pool.clone(),
                            model: r.model.clone(),
                            brief: brief.to_string(),
                            background: r.background,
                            added: r.started.clone(),
                            cut: true,
                        },
                        fc: fc.clone(),
                    },
                );
                let mut lines: Vec<Line> = vec![(
                    fc.clone(),
                    format!(
                        "[{label}] stopped right after it resumed on {0}, so {0} is not blamed; it runs again",
                        r.entry
                    ),
                )];
                lines.extend(self.pool_pump_locked(&mut w, fc, ""));
                self.save_workers_locked(fc, &w);
                lines
            };
            self.flush_lines(lines);
            return true;
        }
        let v = classify(detail, now);
        if v.outcome != Outcome::Out {
            return false;
        }
        let lines = {
            let mut w = self.work_lock();
            let Some(idx) = w.work.iter().position(|r| r.label == label) else {
                return false;
            };
            let r = w.work[idx].clone();
            let key = entry_key(&r.pool, &r.entry);
            w.pool.health.entry(key).or_default().fail(now, &v);
            let conn = v.reason == "cannot connect";
            w.work[idx].state = WORK_QUEUED.into();
            w.work[idx].entry.clear();
            w.pool.held.insert(
                0,
                HeldEntry {
                    job: HeldJob {
                        label: label.to_string(),
                        project: r.project.clone(),
                        pool: r.pool.clone(),
                        model: r.model.clone(),
                        brief: brief.to_string(),
                        background: r.background,
                        added: r.started.clone(),
                        cut: !conn,
                    },
                    fc: fc.clone(),
                },
            );
            let first = if conn {
                format!("[{label}] {} did not answer", r.entry)
            } else {
                format!(
                    "[{label}] {} is out of quota ({})",
                    r.entry,
                    safe_reason(&v.reason)
                )
            };
            let mut first = first;
            let more = self.pool_pump_locked(&mut w, fc, label);
            let mut lines: Vec<Line> = vec![];
            let after = w.work.iter().find(|x| x.label == label).cloned().unwrap_or_default();
            if after.state == WORK_RUNNING {
                first.push_str(&format!("; moved to {}", after.entry));
                lines.push((fc.clone(), first));
            } else if let Some(pool) = PoolSet::load(&self.pools_path())
                .ok()
                .and_then(|s| s.pools.get(&r.pool).cloned())
            {
                match all_out_locked(&w, &pool, now) {
                    Some(until) if !conn => {
                        lines.push((fc.clone(), first));
                        lines.push((
                            fc.clone(),
                            format!(
                                "[{label}] paused: every account of the model {} is out; requeued until {}",
                                pool.model,
                                clock_text(until)
                            ),
                        ));
                    }
                    Some(until) => {
                        first.push_str(&format!(
                            "; every entry of the pool {} is down, so the job is requeued until {}",
                            pool.name,
                            clock_text(until)
                        ));
                        lines.push((fc.clone(), first));
                    }
                    None => {
                        first.push_str("; the job waits for a free entry");
                        lines.push((fc.clone(), first));
                    }
                }
            } else {
                lines.push((fc.clone(), first));
            }
            lines.extend(more);
            self.save_workers_locked(fc, &w);
            lines
        };
        self.flush_lines(lines);
        true
    }

    /// The state of each entry of each pool.
    fn entries_locked(&self, w: &WorkState, set: &PoolSet, now: i64) -> Vec<QueueEntry> {
        let mut out = vec![];
        for pool in set.pools.values() {
            let loads = self.loads_locked(w, pool);
            for e in &pool.entries {
                let l = loads.get(&e.name).cloned().unwrap_or_default();
                let mut qe = QueueEntry {
                    pool: pool.name.clone(),
                    name: e.name.clone(),
                    base_url: e.base_url.clone(),
                    slots: e.slots,
                    running: l.running,
                    state: "free".into(),
                    ..Default::default()
                };
                if l.until > now {
                    qe.state = "cooling".into();
                    qe.until = date_of(l.until);
                    if let Some(h) = w.pool.health.get(&entry_key(&pool.name, &e.name)) {
                        qe.reason = safe_reason(&h.reason);
                    }
                } else if l.running >= e.slots {
                    qe.state = "busy".into();
                }
                out.push(qe);
            }
        }
        out
    }

    /// The entries for the Machines view: empty when there are no pools, or
    /// the pools file does not read.
    pub(super) fn pool_entries_view(&self) -> Vec<QueueEntry> {
        let Ok(set) = PoolSet::load(&self.pools_path()) else {
            return vec![];
        };
        if set.pools.is_empty() {
            return vec![];
        }
        let w = self.work_lock();
        self.entries_locked(&w, &set, self.pool_now())
    }

    /// Writes the queue file the command and the Machines view read, and the
    /// held jobs that a restart keeps.
    fn persist_queue_locked(&self, w: &WorkState, fc: &ForemanChat, set: &PoolSet, now: i64) {
        let mut q = QueueState::default();
        for r in &w.work {
            if r.pool.is_empty()
                || !(r.state == WORK_QUEUED || r.state == WORK_PAUSED || r.state == WORK_RUNNING)
            {
                continue;
            }
            let state = match r.state.as_str() {
                WORK_QUEUED => "waiting",
                WORK_PAUSED => "paused",
                _ => "running",
            };
            q.jobs.push(QueueJob {
                label: r.label.clone(),
                project: r.project.clone(),
                pool: r.pool.clone(),
                background: r.background,
                state: state.into(),
                entry: r.entry.clone(),
                added: r.started.clone(),
                note: String::new(),
            });
        }
        for h in &w.pool.held {
            let queued = w
                .work
                .iter()
                .any(|r| r.label == h.job.label && r.state == WORK_QUEUED);
            if queued {
                q.held.push(h.job.clone());
            }
        }
        q.entries = self.entries_locked(w, set, now);
        let _ = write_queue(&self.work_dir(fc), &q);
    }

    /// The held background jobs that the last server left in the queue file.
    pub(super) fn restore_held(&self, fc: &ForemanChat) -> (BTreeMap<String, HeldJob>, String) {
        let mut out = BTreeMap::new();
        // A file that does not read stays as evidence: it is moved aside,
        // and the next write starts a fresh one (FM-R-112).
        let (q, note) = read_queue_or_aside(&self.work_dir(fc), self.fm_now());
        for h in q.held {
            if h.background {
                out.insert(h.label.clone(), h);
            }
        }
        (out, note)
    }

    /// Wakes the queue when something that holds a job back ends: a
    /// cool-down, or the idle time a background job waits for. A thread
    /// that holds only a weak hold of the server sleeps until then; the next
    /// pump makes a new one and the old one ends.
    fn arm_timer_locked(self: &Arc<Server>, w: &mut WorkState, set: &PoolSet, now: i64) {
        w.pool.timer_gen += 1;
        let gen = w.pool.timer_gen;
        if w.pool.held.is_empty() {
            return;
        }
        let mut next = 0i64;
        let mut see = |t: i64| {
            if t > now && (next == 0 || t < next) {
                next = t;
            }
        };
        for h in &w.pool.held {
            let Some(pool) = set.pools.get(&h.job.pool) else {
                continue;
            };
            for e in &pool.entries {
                let key = entry_key(&pool.name, &e.name);
                if let Some(hh) = w.pool.health.get(&key) {
                    see(hh.until);
                }
                if h.job.background {
                    if let Some(t) = w.pool.last_fg.get(&key).copied().filter(|t| *t != 0) {
                        see(t + set.idle_after);
                    }
                }
            }
        }
        let Some(fc) = w.pool.fc.clone() else { return };
        if next == 0 {
            return;
        }
        let wait = Duration::from_secs((next - now).max(0) as u64) + Duration::from_millis(50);
        let weak = Arc::downgrade(self);
        std::thread::spawn(move || {
            let end = std::time::Instant::now() + wait;
            loop {
                let left = end.saturating_duration_since(std::time::Instant::now());
                if left.is_zero() {
                    break;
                }
                std::thread::sleep(left.min(Duration::from_millis(500)));
                let Some(s) = weak.upgrade() else { return };
                if s.is_closed() || s.work_lock().pool.timer_gen != gen {
                    return;
                }
            }
            if let Some(s) = weak.upgrade() {
                if !s.is_closed() && s.work_lock().pool.timer_gen == gen {
                    s.pool_pump(&fc);
                }
            }
        });
    }

    /// Looks at the queue on the same beat as the import file, until the
    /// chat closes.
    pub(super) fn pool_loop(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        let poll = self.poll_every();
        while !fc.cancel.sleep(poll) {
            self.pool_poll(fc);
        }
    }

    /// One look at the queue: it takes the jobs that `coppice
    /// foreman queue add` left, as background jobs, and runs the queue.
    pub(super) fn pool_poll(self: &Arc<Server>, fc: &Arc<ForemanChat>) {
        // A pools file that broke since the last look is moved aside now,
        // not at the next spawn.
        let (_, note) = self.load_pool_set();
        if !note.is_empty() {
            self.log_work(fc, &note);
        }
        let (reqs, bad) = take_queue_requests(&self.work_dir(fc));
        if bad > 0 {
            self.log_work(
                fc,
                &format!("[queue add] {bad} request(s) did not read and were moved aside as {QUEUE_REQUEST_DIR}/*.broken-*"),
            );
        }
        for r in reqs {
            match self.tool_spawn_opts(fc, &r.project, &r.brief, &r.profile, &r.model, true) {
                Ok(res) => self.log_work(fc, &format!("[queue add] {res}")),
                Err(e) => self.log_work(fc, &format!("[queue add] refused: {}", e.trim())),
            }
        }
        if !self.work_lock().pool.held.is_empty() {
            self.pool_pump(fc);
        }
    }

    /// Ends the queue's timer.
    pub(super) fn close_pools(&self) {
        self.work_lock().pool.timer_gen += 1;
    }
}

fn wait_reason(pool: &Pool, background: bool) -> String {
    if background {
        format!(
            "a background job waits for an entry of the pool {} that has been idle",
            pool.name
        )
    } else {
        format!(
            "every entry of the pool {} is busy or not answering",
            pool.name
        )
    }
}

/// When the first entry of the pool is ready again, if every entry is
/// cooling down.
fn all_out_locked(w: &WorkState, pool: &Pool, now: i64) -> Option<i64> {
    let mut first = 0i64;
    for e in &pool.entries {
        let until = w.pool.health.get(&entry_key(&pool.name, &e.name)).map(|h| h.until)?;
        if until <= now {
            return None;
        }
        if first == 0 || until < first {
            first = until;
        }
    }
    Some(first)
}
