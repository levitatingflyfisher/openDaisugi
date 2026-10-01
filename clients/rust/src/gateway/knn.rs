//! `opendaisugi.route_knn`: the knn router. It sends a turn to the model
//! that did well on similar past tasks. Rulings RT-R-1 to RT-R-7 in
//! clients/ADJUDICATIONS.md fix every number here. The Go client's
//! `gateway/knn.go` is the reference.

use rusqlite::{params, Connection};

use super::meter::{strictly_cheaper, Price, Prices};
use crate::gate::pyjson::Value;

pub const GOOD: &str = "good";
pub const BAD: &str = "bad";
pub const UNKNOWN: &str = "unknown";

/// The writers of an outcome: the gateway, a person, or a later grader model.
pub const SOURCE_AUTO: &str = "auto";
pub const SOURCE_HUMAN: &str = "human";
pub const SOURCE_GRADER: &str = "grader";

pub const K_NEIGHBOURS: usize = 8;
pub const MIN_KNOWN: usize = 3;
pub const COST_WEIGHT: f64 = 0.2;
pub const MAX_ROWS: i64 = 2000;
pub const RETRY_WINDOW_S: f64 = 600.0;
const SIM_SCALE: f64 = 1_000_000.0;

const FALLBACK: Price = Price { input: 3.0, output: 15.0 };

const SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS turns (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT UNIQUE,
    conversation TEXT NOT NULL,
    ts REAL NOT NULL,
    task_sha256 TEXT NOT NULL,
    embedder TEXT NOT NULL,
    vec BLOB NOT NULL,
    model TEXT NOT NULL,
    cost REAL NOT NULL,
    latency_ms REAL,
    outcome TEXT NOT NULL DEFAULT 'unknown',
    source TEXT NOT NULL DEFAULT 'auto'
)
";

/// Human feedback beats the grader, and the grader beats the automatic labels.
fn source_rank(source: &str) -> i64 {
    match source {
        SOURCE_HUMAN => 2,
        SOURCE_GRADER => 1,
        _ => 0,
    }
}

/// `Row`.
#[derive(Debug, Clone)]
pub struct Row {
    pub seq: i64,
    pub turn_id: String,
    pub conversation: String,
    pub ts: f64,
    pub task_sha: String,
    pub embedder: String,
    pub vec: Vec<f32>,
    pub model: String,
    pub cost: f64,
    pub latency_ms: Option<f64>,
    pub outcome: String,
}

/// `Neighbour`.
#[derive(Debug, Clone)]
pub struct Neighbour {
    pub turn_id: String,
    pub similarity: f64,
    pub model: String,
    pub outcome: String,
    pub seq: i64,
}

/// `KnnDecision`.
#[derive(Debug, Clone)]
pub struct Decision {
    pub model: String,
    pub fallback: bool,
    pub reason: String,
    pub neighbours: Vec<Neighbour>,
    pub known: usize,
    pub scores: Vec<(String, f64)>,
}

/// `pack_vec`: float32, little endian.
pub fn pack_vec(v: &[f32]) -> Vec<u8> {
    v.iter().flat_map(|x| x.to_le_bytes()).collect()
}

/// `unpack_vec`.
pub fn unpack_vec(b: &[u8]) -> Vec<f32> {
    b.chunks_exact(4).map(|c| f32::from_le_bytes([c[0], c[1], c[2], c[3]])).collect()
}

/// A float64 vector rounded to float32, as numpy does.
pub fn to_f32(v: &[f64]) -> Vec<f32> {
    v.iter().map(|x| *x as f32).collect()
}

/// `round6`: floor(x * 1e6 + 0.5) / 1e6.
pub fn round6(x: f64) -> f64 {
    (x * SIM_SCALE + 0.5).floor() / SIM_SCALE
}

/// `similarity`: the float64 dot product of two float32 vectors, rounded
/// to six places. A length mismatch gives -1.
pub fn similarity(a: &[f32], b: &[f32]) -> f64 {
    if a.len() != b.len() {
        return -1.0;
    }
    let mut total = 0.0f64;
    for i in 0..a.len() {
        total += a[i] as f64 * b[i] as f64;
    }
    round6(total)
}

/// `task_sha256`.
pub fn task_sha256(text: &str) -> String {
    crate::gate::sha256::hexdigest(text.as_bytes())
}

/// `OutcomeStore`.
#[derive(Debug, Clone)]
pub struct OutcomeStore {
    pub path: String,
}

impl OutcomeStore {
    fn connect(&self) -> Result<Connection, String> {
        if let Some(dir) = std::path::Path::new(&self.path).parent() {
            std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
        }
        let con = Connection::open(&self.path).map_err(|e| e.to_string())?;
        con.busy_timeout(std::time::Duration::from_secs(5)).map_err(|e| e.to_string())?;
        con.execute_batch(SCHEMA).map_err(|e| e.to_string())?;
        Ok(con)
    }

    fn exists(&self) -> bool {
        std::path::Path::new(&self.path).exists()
    }

    /// Inserts one row and returns its turn id, t<seq>. Keeps the newest
    /// MAX_ROWS.
    #[allow(clippy::too_many_arguments)]
    pub fn add(
        &self,
        conversation: &str,
        ts: f64,
        task_sha: &str,
        embedder: &str,
        vec: &[f32],
        model: &str,
        cost: f64,
        latency_ms: Option<f64>,
        outcome: &str,
        source: &str,
    ) -> Result<String, String> {
        let mut con = self.connect()?;
        let tx = con.transaction().map_err(|e| e.to_string())?;
        tx.execute(
            "INSERT INTO turns (conversation, ts, task_sha256, embedder, vec, model, cost, latency_ms, outcome, source) \
             VALUES (?,?,?,?,?,?,?,?,?,?)",
            params![conversation, ts, task_sha, embedder, pack_vec(vec), model, cost, latency_ms, outcome, source],
        )
        .map_err(|e| e.to_string())?;
        let seq = tx.last_insert_rowid();
        let id = format!("t{seq}");
        tx.execute("UPDATE turns SET turn_id=? WHERE seq=?", params![id, seq]).map_err(|e| e.to_string())?;
        tx.execute("DELETE FROM turns WHERE seq <= ?", params![seq - MAX_ROWS]).map_err(|e| e.to_string())?;
        tx.commit().map_err(|e| e.to_string())?;
        Ok(id)
    }

    /// Sets a row's outcome. False when no such turn id exists.
    pub fn set_outcome(&self, turn_id: &str, outcome: &str, source: &str) -> Result<bool, String> {
        if outcome != GOOD && outcome != BAD {
            return Err("outcome must be good or bad".into());
        }
        if ![SOURCE_AUTO, SOURCE_HUMAN, SOURCE_GRADER].contains(&source) {
            return Err("source must be auto, human or grader".into());
        }
        if !self.exists() {
            return Ok(false);
        }
        let con = self.connect()?;
        let n = con
            .execute(
                "UPDATE turns SET outcome=?, source=? WHERE turn_id=? AND \
                 CASE source WHEN 'human' THEN 2 WHEN 'grader' THEN 1 ELSE 0 END <= ?",
                params![outcome, source, turn_id, source_rank(source)],
            )
            .map_err(|e| e.to_string())?;
        Ok(n > 0)
    }

    fn rows(&self, where_: &str, arg: Option<&str>, limit: i64) -> Result<Vec<Row>, String> {
        if !self.exists() {
            return Ok(vec![]);
        }
        let con = self.connect()?;
        let sql = format!(
            "SELECT seq, turn_id, conversation, ts, task_sha256, embedder, vec, model, cost, latency_ms, outcome \
             FROM turns WHERE {where_} ORDER BY seq DESC LIMIT ?"
        );
        let mut st = con.prepare(&sql).map_err(|e| e.to_string())?;
        let map = |r: &rusqlite::Row| -> rusqlite::Result<Row> {
            let seq: i64 = r.get(0)?;
            let id: Option<String> = r.get(1)?;
            let blob: Vec<u8> = r.get(6)?;
            Ok(Row {
                seq,
                turn_id: id.unwrap_or_else(|| format!("t{seq}")),
                conversation: r.get(2)?,
                ts: r.get(3)?,
                task_sha: r.get(4)?,
                embedder: r.get(5)?,
                vec: unpack_vec(&blob),
                model: r.get(7)?,
                cost: r.get(8)?,
                latency_ms: r.get(9)?,
                outcome: r.get(10)?,
            })
        };
        let rows: Vec<Row> = match arg {
            Some(a) => st.query_map(params![a, limit], map),
            None => st.query_map(params![limit], map),
        }
        .map_err(|e| e.to_string())?
        .collect::<rusqlite::Result<_>>()
        .map_err(|e| e.to_string())?;
        Ok(rows)
    }

    /// The newest rows made by `embedder`, newest first.
    pub fn recent(&self, embedder: &str, limit: i64) -> Result<Vec<Row>, String> {
        self.rows("embedder=?", Some(embedder), limit)
    }

    /// Every row, newest first, up to `limit`.
    pub fn all(&self, limit: i64) -> Result<Vec<Row>, String> {
        self.rows("1=1", None, limit)
    }

    /// The newest row of a conversation, or None.
    pub fn last_in_conversation(&self, conversation: &str) -> Result<Option<Row>, String> {
        Ok(self.rows("conversation=?", Some(conversation), 1)?.into_iter().next())
    }

    /// The number of booked rows.
    pub fn count(&self) -> Result<i64, String> {
        if !self.exists() {
            return Ok(0);
        }
        let con = self.connect()?;
        con.query_row("SELECT COUNT(*) FROM turns", [], |r| r.get(0)).map_err(|e| e.to_string())
    }
}

/// `candidate_models`: the requested model first, then the cheaper rungs
/// (the local model, then the cheap model). A rung is a candidate only when
/// the price table shows it strictly cheaper.
pub fn candidate_models(requested: &str, cheap: &str, local: &str, prices: &Prices) -> Vec<String> {
    let mut out = vec![requested.to_string()];
    for rung in [local, cheap] {
        if rung.is_empty() || out.iter().any(|o| o == rung) {
            continue;
        }
        if strictly_cheaper(prices, &Value::Str(rung.into()), &Value::Str(requested.into())).unwrap_or(false) {
            out.push(rung.to_string());
        }
    }
    out
}

fn unit_price(model: &str, prices: &Prices) -> f64 {
    let p = prices.get(model).copied().unwrap_or(FALLBACK);
    p.input + p.output
}

fn f(x: f64, n: usize) -> String {
    crate::garden::format_f(x, n)
}

/// `_success`: (sum sim of good + 1) / (sum sim of known + 2), with the
/// number of good rows.
fn success(nbrs: &[Neighbour], model: &str) -> (f64, usize) {
    let (mut num, mut den, mut good) = (0.0f64, 0.0f64, 0usize);
    for n in nbrs {
        if n.model != model || (n.outcome != GOOD && n.outcome != BAD) {
            continue;
        }
        den += n.similarity;
        if n.outcome == GOOD {
            good += 1;
            num += n.similarity;
        }
    }
    ((num + 1.0) / (den + 2.0), good)
}

/// `score_candidates`: the default scorer, a pure function of the
/// neighbours and the candidates. A cheaper model is eligible only with a
/// good neighbour and a success value (before the cost term) at or above
/// the requested model's (RT-R-3a). Another scorer can replace it.
pub fn score_candidates(nbrs: &[Neighbour], candidates: &[String], requested: &str, prices: &Prices) -> (String, Vec<(String, f64)>) {
    let requested_price = unit_price(requested, prices);
    let requested_success = success(nbrs, requested).0;
    let mut scores: Vec<(String, f64)> = vec![];
    let mut best = requested.to_string();
    let mut best_score = f64::NEG_INFINITY;
    for model in candidates {
        let (succ, good) = success(nbrs, model);
        if model != requested && (good == 0 || succ < requested_success) {
            continue;
        }
        let ratio = if requested_price > 0.0 { unit_price(model, prices) / requested_price } else { 0.0 };
        let score = succ - COST_WEIGHT * ratio;
        scores.push((model.clone(), score));
        if score > best_score {
            best_score = score;
            best = model.clone();
        }
    }
    (best, scores)
}

/// `decide`.
#[allow(clippy::too_many_arguments)]
pub fn decide(
    query: &[f32],
    rows: &[Row],
    requested: &str,
    candidates: &[String],
    prices: &Prices,
    threshold: f64,
    k: usize,
    min_known: usize,
) -> Decision {
    let mut scored: Vec<(f64, &Row)> = rows
        .iter()
        .filter_map(|r| {
            let sim = similarity(query, &r.vec);
            (sim >= threshold).then_some((sim, r))
        })
        .collect();
    scored.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal).then(b.1.seq.cmp(&a.1.seq)));
    scored.truncate(k);
    let nbrs: Vec<Neighbour> = scored
        .iter()
        .map(|(s, r)| Neighbour {
            turn_id: r.turn_id.clone(),
            similarity: *s,
            model: r.model.clone(),
            outcome: r.outcome.clone(),
            seq: r.seq,
        })
        .collect();
    let known = nbrs
        .iter()
        .filter(|n| (n.outcome == GOOD || n.outcome == BAD) && candidates.contains(&n.model))
        .count();
    if known < min_known {
        return Decision {
            model: requested.into(),
            fallback: true,
            reason: format!("knn: {known} of {} neighbours have a known outcome (need {min_known})", nbrs.len()),
            neighbours: nbrs,
            known,
            scores: vec![],
        };
    }
    let (best, scores) = score_candidates(&nbrs, candidates, requested, prices);
    let mut ranked = scores;
    ranked.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
    let shown: Vec<String> = ranked.iter().take(3).map(|(m, s)| format!("{m} {}", f(*s, 2))).collect();
    Decision {
        reason: format!("knn: {best}; {} neighbours, {known} known; scores {}", nbrs.len(), shown.join(", ")),
        model: best,
        fallback: false,
        neighbours: nbrs,
        known,
        scores: ranked,
    }
}

/// `label_previous`: the outcome the next human turn gives the previous
/// turn, or None for no change.
pub fn label_previous(prev: Option<&Row>, vec: &[f32], embedder: &str, now: f64, threshold: f64) -> Option<&'static str> {
    let prev = prev?;
    if prev.outcome != UNKNOWN || prev.embedder != embedder {
        return None;
    }
    if now - prev.ts > RETRY_WINDOW_S {
        return None;
    }
    Some(if similarity(&prev.vec, vec) >= threshold { BAD } else { GOOD })
}

/// What embedding gives: the task vector and the embedder identity.
pub type EmbedFn = Box<dyn Fn(&str) -> Result<(Vec<f32>, String), String> + Send + Sync>;

/// `KnnRouter`: a store, an embedder and a threshold.
pub struct Knn {
    pub store: OutcomeStore,
    pub embed: EmbedFn,
    pub threshold: f64,
}

impl std::fmt::Debug for Knn {
    fn fmt(&self, fm: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        fm.debug_struct("Knn").field("store", &self.store).field("threshold", &self.threshold).finish()
    }
}

impl Knn {
    /// Books one finished turn and labels the previous turn of its
    /// conversation.
    #[allow(clippy::too_many_arguments)]
    pub fn record(
        &self,
        conversation: &str,
        task: &str,
        vec: &[f32],
        embedder: &str,
        model: &str,
        cost: f64,
        latency_ms: Option<f64>,
        failed: bool,
        now: f64,
    ) -> Result<String, String> {
        let prev = self.store.last_in_conversation(conversation)?;
        if let Some(label) = label_previous(prev.as_ref(), vec, embedder, now, self.threshold) {
            if let Some(p) = &prev {
                self.store.set_outcome(&p.turn_id, label, SOURCE_AUTO)?;
            }
        }
        let outcome = if failed { BAD } else { UNKNOWN };
        self.store.add(conversation, now, &task_sha256(task), embedder, vec, model, cost, latency_ms, outcome, SOURCE_AUTO)
    }
}

/// `knn_view`: the choice for a task and its nearest past turns, as plain
/// data.
#[derive(Debug, Clone, Default)]
pub struct View {
    pub err: String,
    pub model: String,
    pub fallback: bool,
    pub reason: String,
    pub known: usize,
    pub threshold: f64,
    pub neighbours: Vec<Neighbour>,
    pub scores: Vec<(String, f64)>,
}

/// `knn_view` for an embedded task.
pub fn view_of(
    vec: &[f32],
    identity: &str,
    store: &OutcomeStore,
    requested: &str,
    cheap: &str,
    prices: &Prices,
    threshold: f64,
) -> Result<View, String> {
    let rows = store.recent(identity, MAX_ROWS)?;
    let cands = candidate_models(requested, cheap, "", prices);
    let kd = decide(vec, &rows, requested, &cands, prices, threshold, K_NEIGHBOURS, MIN_KNOWN);
    let scores = kd.scores.iter().map(|(m, s)| (m.clone(), f(*s, 4).parse::<f64>().unwrap_or(*s))).collect();
    Ok(View {
        err: String::new(),
        model: kd.model,
        fallback: kd.fallback,
        reason: kd.reason,
        known: kd.known,
        threshold,
        neighbours: kd.neighbours,
        scores,
    })
}

/// `knn_view_lines`.
pub fn view_lines(v: &View) -> Vec<String> {
    if !v.err.is_empty() {
        return vec![format!("  knn:        {}", v.err)];
    }
    let mut reason = v.reason.strip_prefix("knn: ").unwrap_or(&v.reason).to_string();
    if v.fallback {
        reason.push_str("; the rules router decides");
    }
    let mut out = vec![format!("  knn:        {reason}"), format!("  threshold:  {}", f(v.threshold, 2))];
    if v.neighbours.is_empty() {
        out.push("  neighbours: none at or above the threshold".into());
        return out;
    }
    out.push(format!("  neighbours: {}", v.neighbours.len()));
    for n in &v.neighbours {
        out.push(format!("    {}  {}  {}  {}", n.turn_id, f(n.similarity, 2), n.model, n.outcome));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value as J;

    fn golden() -> J {
        let path = format!("{}/../fixtures/gateway/knn_golden.json", env!("CARGO_MANIFEST_DIR"));
        serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap()
    }

    fn vec_of(v: &J) -> Vec<f32> {
        v.as_array().unwrap().iter().map(|x| x.as_f64().unwrap() as f32).collect()
    }

    fn prices_of(g: &J) -> Prices {
        g["prices"]
            .as_object()
            .unwrap()
            .iter()
            .map(|(k, v)| (k.clone(), Price { input: v[0].as_f64().unwrap(), output: v[1].as_f64().unwrap() }))
            .collect()
    }

    #[test]
    fn golden_decisions() {
        let g = golden();
        let prices = prices_of(&g);
        for c in g["decide"].as_array().unwrap() {
            let name = c["name"].as_str().unwrap();
            let rows: Vec<Row> = c["rows"]
                .as_array()
                .unwrap()
                .iter()
                .filter(|r| r["embedder"] == "fake")
                .map(|r| Row {
                    seq: r["seq"].as_i64().unwrap(),
                    turn_id: r["turn_id"].as_str().unwrap().into(),
                    conversation: "c".into(),
                    ts: r["ts"].as_f64().unwrap(),
                    task_sha: "sha".into(),
                    embedder: "fake".into(),
                    vec: vec_of(&r["vec"]),
                    model: r["model"].as_str().unwrap().into(),
                    cost: 0.0,
                    latency_ms: None,
                    outcome: r["outcome"].as_str().unwrap().into(),
                })
                .collect();
            let requested = c["requested"].as_str().unwrap();
            let local = c["local"].as_str().unwrap_or("");
            let cands = candidate_models(requested, c["cheap"].as_str().unwrap(), local, &prices);
            let want: Vec<&str> =
                c["expect"]["candidates"].as_array().unwrap().iter().map(|x| x.as_str().unwrap()).collect();
            assert_eq!(cands, want, "{name}: candidates");
            let d = decide(
                &vec_of(&c["query"]),
                &rows,
                requested,
                &cands,
                &prices,
                c["threshold"].as_f64().unwrap(),
                K_NEIGHBOURS,
                MIN_KNOWN,
            );
            let e = &c["expect"];
            assert_eq!(d.model, e["model"].as_str().unwrap(), "{name}: model");
            assert_eq!(d.fallback, e["fallback"].as_bool().unwrap(), "{name}: fallback");
            assert_eq!(d.reason, e["reason"].as_str().unwrap(), "{name}: reason");
            assert_eq!(d.known as u64, e["known"].as_u64().unwrap(), "{name}: known");
            let ns = e["neighbours"].as_array().unwrap();
            assert_eq!(d.neighbours.len(), ns.len(), "{name}: neighbours");
            for (n, w) in d.neighbours.iter().zip(ns) {
                assert_eq!(n.turn_id, w["turn_id"].as_str().unwrap(), "{name}");
                assert_eq!(n.similarity, w["similarity"].as_f64().unwrap(), "{name}");
                assert_eq!(n.model, w["model"].as_str().unwrap(), "{name}");
                assert_eq!(n.outcome, w["outcome"].as_str().unwrap(), "{name}");
            }
            let ss = e["scores"].as_array().unwrap();
            assert_eq!(d.scores.len(), ss.len(), "{name}: scores");
            for ((m, s), w) in d.scores.iter().zip(ss) {
                assert_eq!(m, w["model"].as_str().unwrap(), "{name}");
                assert!((s - w["score"].as_f64().unwrap()).abs() < 5e-7, "{name}: score");
            }
        }
    }

    #[test]
    fn golden_labels() {
        let g = golden();
        for c in g["label"].as_array().unwrap() {
            let p = &c["prev"];
            let prev = Row {
                seq: 1,
                turn_id: "t1".into(),
                conversation: "c".into(),
                ts: p["ts"].as_f64().unwrap(),
                task_sha: "sha".into(),
                embedder: p["embedder"].as_str().unwrap().into(),
                vec: vec_of(&p["vec"]),
                model: "m".into(),
                cost: 0.0,
                latency_ms: None,
                outcome: p["outcome"].as_str().unwrap().into(),
            };
            let got = label_previous(
                Some(&prev),
                &vec_of(&c["vec"]),
                "fake",
                c["now"].as_f64().unwrap(),
                c["threshold"].as_f64().unwrap(),
            );
            assert_eq!(got, c["expect"].as_str(), "{}", c["name"]);
        }
    }

    #[test]
    fn golden_round_and_pack() {
        let g = golden();
        for p in g["round6"].as_array().unwrap() {
            assert_eq!(round6(p[0].as_f64().unwrap()), p[1].as_f64().unwrap());
        }
        let v = vec_of(&g["pack"]["vec"]);
        let hex: String = pack_vec(&v).iter().map(|b| format!("{b:02x}")).collect();
        assert_eq!(hex, g["pack"]["hex"].as_str().unwrap());
        assert_eq!(unpack_vec(&pack_vec(&v)), v);
    }

    #[test]
    fn the_store_books_rows_and_keeps_no_text() {
        let dir = std::env::temp_dir().join(format!("knn-store-{}", std::process::id()));
        let st = OutcomeStore { path: dir.join("gateway/routing.db").to_string_lossy().into_owned() };
        assert!(!st.set_outcome("t1", GOOD, SOURCE_HUMAN).unwrap());
        let id = st.add("c", 5.0, &task_sha256("secret task"), "fake", &[1.0, 0.0], "m", 0.01, Some(12.5), UNKNOWN, SOURCE_AUTO).unwrap();
        assert_eq!(id, "t1");
        assert!(st.set_outcome(&id, BAD, SOURCE_HUMAN).unwrap());
        assert!(!st.set_outcome("t9", BAD, SOURCE_HUMAN).unwrap());
        let rows = st.recent("fake", MAX_ROWS).unwrap();
        assert_eq!((rows.len(), rows[0].outcome.as_str(), rows[0].vec[0]), (1, BAD, 1.0));
        let raw = std::fs::read(&st.path).unwrap();
        assert!(!raw.windows(11).any(|w| w == b"secret task"));
        std::fs::remove_dir_all(dir).ok();
    }
}
