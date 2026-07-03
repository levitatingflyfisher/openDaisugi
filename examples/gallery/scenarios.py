"""Gallery scenarios — each backed by a REAL opendaisugi verify / swarm check.

Every function returns a list of rendered frames (same size), color-coded
green=accepted, amber=out-of-bounds refused+fallback, red=hard hold/refusal.
"""
from harness import AMBER, BLUE, GREEN, GREY, RED, Stage, play

from opendaisugi import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    aabb_disjoint,
    envelope_subsumes,
    partition_and_assign,
    verify,
    verify_swarm_tasking,
)
from opendaisugi.models import CartesianMoveStep


def _ws_env(box, obstacles=None):
    inv = [Invariant(type="end_effector_in_workspace", description="in zone")]
    if obstacles:
        inv.append(Invariant(type="no_obstacle_penetration", description="avoid"))
    return Envelope(generated_by="t", task="x", stakes="physical",
                    permissions=Permission(workspace_bounds=box, obstacles=obstacles or []),
                    invariants=inv)


def _move(target):
    return ActionPlan(source="t", task="x",
                      steps=[CartesianMoveStep(id="s", target_position=(target[0], target[1], 4.0))])


def _accepts(env, target):
    return verify(_move(target), env).ok


def _path(wps):
    return ActionPlan(source="t", task="x", steps=[
        CartesianMoveStep(id=f"s{i}", target_position=(x, y, 4.0),
                          depends_on=([f"s{i-1}"] if i else []))
        for i, (x, y) in enumerate(wps)])


# 1 ── keep-in workspace ──────────────────────────────────────────────────────
def keep_in():
    st = Stage()
    box = ((6, 3, 0), (16, 9, 8))
    st.zone(0, 11, 6, 5, 3, (*BLUE, 0.30))
    env = _ws_env(box)
    title = "KEEP-IN · workspace bound"
    keys = [([(8, 4, GREEN)], "patrolling in-zone")]
    for wp in [(14, 8), (9, 7)]:
        keys.append(([(wp[0], wp[1], GREEN)], "verify(move ⊆ zone): accepted"))
    tgt = (24, 6)                                   # outside the zone
    reject = not _accepts(env, tgt)
    fb = (15.4, 6)                                  # clamped back in-zone
    keys.append(([(20, 6, AMBER)], f"reach to {tgt}: {'REFUSED' if reject else '?'} (out of zone)"))
    keys.append(([(fb[0], fb[1], AMBER)], "-> clamped back inside the bound"))
    play(st, title, keys)
    return st.close()


# 2 ── no-fly / keep-out zone (trajectory-sampled) ────────────────────────────
def no_fly():
    st = Stage()
    box = ((0, 0, 0), (30, 12, 8))
    obs = [((13, 4, 0), (17, 8, 8))]                # keep-out slab in the middle
    st.zone(0, 15, 6, 15, 6, (0.16, 0.17, 0.2, 0.5))
    st.zone(1, 15, 6, 2, 2, (*RED, 0.5))            # the no-fly box
    env = _ws_env(box, obstacles=obs)
    title = "KEEP-OUT · no-fly zone"
    straight = verify(_path([(4, 6), (24, 6)]), env).ok   # horizontal segment clips box → refused
    keys = [([(4, 6, GREEN)], "cross the property, avoid the keep-out")]
    keys.append(([(11, 6, GREEN)], "approaching..."))
    keys.append(([(11, 6, RED)], f"straight path through no-fly: {'REFUSED' if not straight else '?'}"))
    for wp, s in [((15, 10.5), "reroute over the top (verified clear)"), ((22, 8), "clear"), ((25, 6), "arrived, no-fly untouched")]:
        keys.append(([(wp[0], wp[1], GREEN)], s))
    play(st, title, keys)
    return st.close()


# 3 ── swarm deconfliction ────────────────────────────────────────────────────
def deconflict():
    st = Stage()
    st.zone(0, 15, 6, 14, 6, (0.16, 0.17, 0.2, 0.45))
    title = "DECONFLICT · swarm safety bubble"
    R, bubble = 0.6, 3.5

    def box(p):
        return ((p[0] - R, p[1] - R, 0), (p[0] + R, p[1] + R, 8))
    keys = [([(6, 6, GREEN), (24, 6, GREEN)], "two drones, converging tasks")]
    keys.append(([(11, 6, GREEN), (19, 6, GREEN)], "closing..."))
    a, b = (13.5, 6), (16.5, 6)
    conflict = not aabb_disjoint(box(a), box(b), margin=bubble)   # would breach the bubble
    keys.append(([(a[0], a[1], GREEN), (b[0], b[1], RED)],
                 f"would breach {bubble}m bubble: {'HOLD' if conflict else '?'}"))
    keys.append(([(a[0], a[1], GREEN), (b[0], b[1], RED)], "the second drone holds — no collision"))
    play(st, title, keys)
    return st.close()


# 4 ── delegation over-reach ──────────────────────────────────────────────────
def delegation():
    st = Stage()
    mission = _ws_env(((0, 0, 0), (30, 12, 8)))
    fleet = partition_and_assign(mission, ["a", "b", "c"], axis=0, margin=0.5)
    grant = fleet["a"]
    (glx, gly, _), (ghx, ghy, _) = grant.permissions.workspace_bounds
    st.zone(0, (glx + ghx) / 2, 6, (ghx - glx) / 2, 6, (*BLUE, 0.32))   # the granted slice
    title = "DELEGATE · authority containment"
    task_in = _ws_env(((glx + 1, 3, 0), (ghx - 1, 9, 8)))
    task_out = _ws_env(((glx + 1, 3, 0), (ghx + 12, 9, 8)))            # spills past the grant
    ok = envelope_subsumes(grant, task_in).holds
    bad = envelope_subsumes(grant, task_out).holds
    keys = [([((glx + ghx) / 2, 6, GREEN)], "coordinator delegates a slice")]
    keys.append(([((glx + ghx) / 2, 5, GREEN)], f"task ⊆ grant? envelope_subsumes: {'ACCEPTED' if ok else '?'}"))
    keys.append(([(ghx + 5, 6, RED)], f"task spills past the grant: {'REJECTED' if not bad else '?'}"))
    keys.append(([((glx + ghx) / 2, 6, GREEN)], "you cannot delegate authority you were not given"))
    play(st, title, keys)
    return st.close()


# 5 ── formation lane discipline ─────────────────────────────────────────────
def formation():
    st = Stage()
    lanes = [((i * 7.5, 2, 0), (i * 7.5 + 6.5, 10, 8)) for i in range(4)]
    cols = [BLUE, GREEN, AMBER, (0.8, 0.5, 1.0)]
    for i, ((lx, _, _), (hx, _, _)) in enumerate(lanes):
        st.zone(i, (lx + hx) / 2, 6, (hx - lx) / 2, 4, (*cols[i], 0.22))
    envs = [_ws_env(b) for b in lanes]
    title = "FORMATION · lane discipline"

    def home(k):
        return [((lanes[i][0][0] + lanes[i][1][0]) / 2, 6, GREEN) for i in range(4)]
    keys = [(home(0), "four drones, four lanes")]
    keys.append(([home(0)[i] if i != 1 else (15.5, 6, AMBER) for i in range(4)],
                 "drone 2 drifts toward lane 3..."))
    drift_ok = _accepts(envs[1], (16.0, 6))          # 16 is well inside lane 3, out of lane 2
    keys.append(([home(0)[i] if i != 1 else (13.5, 6, RED) for i in range(4)],
                 f"crosses its lane: {'REFUSED' if not drift_ok else '?'} -> held"))
    keys.append((home(0), "formation holds — every drone in its lane"))
    play(st, title, keys)
    return st.close()


# 6 ── dynamic human keep-out ─────────────────────────────────────────────────
def human_keepout():
    st = Stage()
    box = ((0, 0, 0), (30, 12, 8))
    st.zone(0, 15, 6, 15, 6, (0.16, 0.17, 0.2, 0.45))
    title = "SAFETY · dynamic keep-out (person)"
    drone = (4, 4)
    frames_path = [(4, 4), (10, 5), (14, 6), (14, 6), (14, 6), (20, 7), (26, 6)]
    person_x = [24, 21, 18, 16, 16, 12, 6]           # person walks toward the drone's path
    for k, ((dx, dy), px) in enumerate(zip(frames_path, person_x, strict=True)):
        person = ((px - 2, 4, 0), (px + 2, 8, 8))
        st.zone(1, px, 6, 2, 2, (*RED, 0.5))
        env = _ws_env(box, obstacles=[person])
        blocked = not _accepts(env, (dx, dy))
        col = RED if (blocked and k in (2, 3, 4)) else GREEN
        sub = ("person enters the drone's path -> HOLD" if col is RED
               else "patrolling; keep-out clear")
        st.push([(drone[0] if col is RED else dx, dy, col)], title, sub, hold=2)
        if col is not RED:
            drone = (dx, dy)
    return st.close()


# 7 ── perimeter intercept (clamped to boundary) ──────────────────────────────
def intercept():
    st = Stage()
    box = ((0, 0, 0), (28, 12, 8))
    st.zone(0, 14, 6, 14, 6, (0.18, 0.30, 0.5, 0.4))     # the property
    st.zone(1, 31, 6, 1.2, 1.2, (*RED, 0.9))             # intruder OUTSIDE the fence
    env = _ws_env(box)
    title = "INTERCEPT · geofence"
    intruder = (31, 6)
    clamp_ok = _accepts(env, intruder)
    keys = [([(8, 6, GREEN)], "intruder detected beyond the fence")]
    keys.append(([(20, 6, GREEN)], "vectoring to intercept..."))
    keys.append(([(27, 6, AMBER)], f"intercept point is off-property: {'REFUSED' if not clamp_ok else '?'}"))
    keys.append(([(26.5, 6, AMBER)], "-> holds at the fence line, does not chase out"))
    play(st, title, keys)
    return st.close()


# 8 ── comms-loss reassignment ────────────────────────────────────────────────
def reassignment():
    st = Stage()
    mission = _ws_env(((0, 0, 0), (30, 12, 8)))
    fleet = partition_and_assign(mission, ["w", "m", "e"], axis=0, margin=0.5)
    (wlo, _), (mlo, mhi) = fleet["w"].permissions.workspace_bounds, fleet["m"].permissions.workspace_bounds
    ehi = fleet["e"].permissions.workspace_bounds[1]
    west_exp = _ws_env((wlo, mhi))
    ok = verify_swarm_tasking(mission, {"w": west_exp, "e": fleet["e"]}, margin=0.5).ok
    bad = verify_swarm_tasking(mission, {"w": west_exp, "e": _ws_env((mlo, ehi))}, margin=0.5).ok
    title = "REASSIGN · comms-loss cover"
    st.zone(0, 5, 6, 4.7, 6, (*BLUE, 0.32)); st.zone(1, 15, 6, 4.7, 6, (*GREEN, 0.3)); st.zone(2, 25.1, 6, 4.7, 6, (*AMBER, 0.3))

    def frame(sub, mid_col, west_x, expand, warn, hold=3):
        st.zone(1, 15, 6, 4.7, 6, (*GREEN, max(0.06, 0.3 * (1 - expand))))
        st.zone(0, 4.9 + expand * 5.0, 6, 4.7 + expand * 5.1, 6, (*BLUE, 0.32))
        st.zone(3, 15, 6, 4.7, 6, (*RED, warn))
        st.push([(west_x, 6, GREEN), (15, 6, mid_col), (25.1, 6, AMBER)], title, sub, hold=hold)
    frame("three drones, one sector each", GREEN, 4.9, 0, 0)
    frame("drone_mid: COMMS LOST", GREY, 4.9, 0, 0)
    for e in [0.3, 0.6, 1.0]:
        frame(f"expand WEST to cover: verify {'ACCEPTED' if ok else '?'}", GREY, 4.9 + e * 5, e, 0, hold=1)
    frame("west now covers the gap (contained + disjoint)", GREY, 9.9, 1, 0)
    frame(f"...but hand it to BOTH? overlap {'REJECTED' if not bad else '?'}", GREY, 9.9, 1, 0.5)
    frame("delegation transfer is a checked containment proof", GREY, 9.9, 1, 0)
    return st.close()


# 9 ── cross-swarm airspace ───────────────────────────────────────────────────
def cross_swarm():
    st = Stage()
    alpha = ((0, 0, 0), (15, 12, 8))
    st.zone(0, 7.5, 6, 7.5, 6, (*BLUE, 0.3))
    title = "CROSS-SWARM · airspace"
    clear = ((16, 0, 0), (30, 12, 8))
    over = ((11, 0, 0), (30, 12, 8))
    ok = aabb_disjoint(alpha, clear, margin=0.5)
    bad = aabb_disjoint(alpha, over, margin=0.5)
    keys = [([(7, 6, BLUE), (23, 6, AMBER)], "swarm Alpha (left) + Bravo (right)")]
    st.zone(1, 23, 6, 7, 6, (*AMBER, 0.3))
    keys.append(([(7, 6, BLUE), (23, 6, AMBER)], f"Bravo clear of Alpha? {'ACCEPTED' if ok else '?'}"))
    keys.append(([(7, 6, BLUE), (13, 6, RED)], f"Bravo enters Alpha's volume: {'REJECTED' if not bad else '?'}"))
    keys.append(([(7, 6, BLUE), (20, 6, AMBER)], "each swarm proves disjointness before entry"))
    play(st, title, keys)
    return st.close()


ALL = {
    "keep_in": keep_in, "no_fly": no_fly, "deconflict": deconflict, "delegation": delegation,
    "formation": formation, "human_keepout": human_keepout, "intercept": intercept,
    "reassignment": reassignment, "cross_swarm": cross_swarm,
}
