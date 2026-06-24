# PI / LeRobot / VLA integration recipe (v0.26+)

`VLAExecutorBase` (`src/opendaisugi/vla_executor.py`) is the abstract
scaffolding for any Vision-Language-Action policy. The base class
handles MuJoCo loading, simulation stepping, evidence packaging, and
the integrity-check participation. Subclasses implement one method:

```python
def _predict_actions(
    self, step: VLAStep, observation: dict[str, Any],
) -> list[dict[str, Any]]:
    """Return a list of action dicts. Each is {joint_name: target}."""
```

## π0 via LeRobot (recommended starting point)

LeRobot is HuggingFace's robot-learning library; π0 weights are
distributed through it. License: Apache 2.0 last we checked — verify
on the model card before commercial use.

```python
# requirements: torch, transformers, lerobot, mujoco
# GPU recommended; ~8GB VRAM for π0

from opendaisugi.vla_executor import VLAExecutorBase
from PIL import Image
import torch


class LeRobotPi0Executor(VLAExecutorBase):
    """Concrete VLAExecutor that drives π0 via LeRobot's API.

    The model consumes (RGB image, language instruction, proprioception)
    and emits a chunk of actions. We cache the chunk and step the sim
    one action at a time so contact dynamics integrate properly.
    """

    def __init__(self, *, mjcf_path: str, model_id: str = "lerobot/pi0",
                 device: str = "cuda"):
        super().__init__(mjcf_path=mjcf_path)
        from lerobot.common.policies.pi0.modeling_pi0 import PI0Policy
        self.policy = PI0Policy.from_pretrained(model_id).to(device)
        self.device = device
        # MuJoCo offscreen renderer for the visual observation.
        import mujoco
        self.renderer = mujoco.Renderer(self._model, height=480, width=640)
        self.camera = "wrist_cam"  # MJCF must declare it

    def _capture_image(self):
        self.renderer.update_scene(self._data, camera=self.camera)
        return Image.fromarray(self.renderer.render())

    def _predict_actions(self, step, observation):
        image = self._capture_image()
        batch = self.policy.normalize_inputs({
            "observation.image": torch.from_numpy(np.array(image)).to(self.device),
            "observation.state": torch.tensor(observation["qpos"]).to(self.device),
            "task": [step.task],
        })
        with torch.no_grad():
            action_chunk = self.policy.select_action(batch)
        # action_chunk is shape (1, T, action_dim); convert each timestep
        # to the {joint_name: target} dict the executor wants.
        result = []
        for t in range(action_chunk.shape[1]):
            row = action_chunk[0, t].cpu().numpy()
            result.append({
                "j1": float(row[0]),
                "j2": float(row[1]),
                "j_grip": float(row[2]),
            })
        return result[: step.max_actions]
```

Wire it into the kit:

```python
exe = LeRobotPi0Executor(
    mjcf_path="path/to/aloha.xml",
    model_id="lerobot/pi0",
)
sup = Supervisor(executors={"vla": exe}, journal=j, ...)
session = await sup.run(plan, env)
```

## TransformersVLAExecutor — generic HuggingFace path (v0.26.1+)

For VLAs published via standard transformers APIs (``AutoProcessor`` +
``AutoModel``), v0.26.1 ships a generic executor:

```python
from opendaisugi.vla_executor import TransformersVLAExecutor

exe = TransformersVLAExecutor(
    mjcf_path="path/to/robot.xml",
    model_id="lerobot/smolvla_base",  # or your model id
    device="cpu",                      # or "cuda" if GPU
    action_horizon=16,
)
```

The model is **lazy-loaded on first ``run()``** — instantiating the
executor doesn't touch ``torch`` or pull weights. This matters on
memory-constrained hosts: a ``Daisugi(...)`` import path that never
actually invokes a VLAStep stays cheap.

**Memory profile** for ``lerobot/smolvla_base`` (~450M params):
- ~2 GB download (one-time, cached at ``$HF_HOME`` or ``cache_dir``)
- ~1–2 GB host RAM during inference on CPU
- ~1 GB VRAM if ``device="cuda"``
- Few seconds per call on CPU, sub-second on GPU

For ``lerobot/pi0`` (~3.3B params): ~8 GB VRAM, GPU required for usable
latency.

**Smoke testing:**

```bash
OPENDAISUGI_SMOLVLA_SMOKE=1 pytest tests/test_vla_executor.py::test_smolvla_real_load_smoke -v
```

Opt-in because the load profile pushes <8 GB-free-RAM laptops into
swap. Run on machines with headroom.

**When to subclass instead:** non-standard processor signature, custom
image preprocessing, multi-camera input, hierarchical actions, or
custom image capture (specific MuJoCo camera, real robot cameras).
Subclass ``VLAExecutorBase`` directly — ``TransformersVLAExecutor`` is
the 80%-case shortcut, not the only path.

## Aloha bimanual rig (production target)

PI demos π0 / π0.5 on a Trossen Aloha bimanual rig (~$5–8k). The
MJCF lives in `mujoco_menagerie` (Apache 2.0):

```bash
git clone https://github.com/google-deepmind/mujoco_menagerie
ls mujoco_menagerie/aloha/scene.xml
```

Don't vendor `mujoco_menagerie` into openDaisugi — it's GBs of meshes
+ textures. Reference it from the user's clone path:

```python
exe = LeRobotPi0Executor(
    mjcf_path="/path/to/mujoco_menagerie/aloha/scene.xml",
)
```

The Aloha MJCF declares 14 joints (7 per arm) plus two grippers; your
`_predict_actions` returns dicts keyed on those joint names.

## RT-2 / OpenVLA / other policies

Same shape: subclass `VLAExecutorBase`, implement `_predict_actions`,
swap the model load. The executor base is policy-agnostic.

## What the substrate gates that a bare VLA doesn't

| concern | bare VLA inference | with openDaisugi |
|---|---|---|
| Workspace bounds | enforced (or not) inside the model's training distribution | `_check_workspace_containment` rejects pre-execution if `target_pose` is outside the envelope — independent of what the model learned |
| Max action count | none; a policy can loop forever | `step.max_actions` + `executor.max_actions_global` cap |
| Per-rollout audit | model logs whatever it logs | structured `Receipt` with content-addressed evidence; v0.18 integrity check confirms every skill produced one |
| Skill-level retry | none | v0.18 supervisor halts on a failed skill, fallback handler decides next |
| Cross-rollout learning | model retrains | journaled rollouts feed `daisugi tend` → `CompiledPathway` → Gardener fitness |
| Cross-instance sharing | n/a | v0.25 git-backed registry (signed pathway bundles) |

## GPU / cost notes

- π0 inference: ~8GB VRAM, ~30 Hz on an A100 / 4090. Slower on consumer
  cards.
- The v0.26 abstract base does NOT require a GPU — `MockVLAExecutor`
  runs CPU-only. Tests stay fast.
- For batch dataset curation (running historical rollouts back through
  the substrate to populate receipts), CPU is fine.

## License notes

PI's π0 is published under Apache 2.0 last we checked. LeRobot's
distributions and model cards may carry additional terms (e.g. for
specific fine-tunes). Verify per use; we don't track upstream license
changes here.
