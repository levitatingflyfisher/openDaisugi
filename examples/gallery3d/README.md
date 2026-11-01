# The 3D gallery: three scenes with a person near the machine

Three simulated scenes. In each one a planner offers plans, the real verifier judges
every plan, and the picture shows the answers: a refused plan turns red and fades, an
accepted plan stays green.

| Scene | What it shows | Checks | Picture script |
|---|---|---|---|
| Delivery | A delivery robot on a sidewalk. A child runs out after a ball. The plan in hand is refused, six of eight new plans are refused, and the robot stops short and waits. | `delivery.py` | `render_delivery.py` |
| Patrol | A quadrotor crosses a property with a no-fly box over the house and a person on the path. The same flight twice: without the warden, and with it. | `patrol.py` | `render_patrol.py` |
| Many plans | Six hundred random plans for the patrol's flight, each one checked. The counts are on the picture: checked, refused, accepted, breaches among accepted. | `many.py` | `render_many.py` |

## The rule every scene keeps

A picture draws what the verifier decided and nothing else.

- Each scene has a `prove()`. It fails unless the verifier refuses the unsafe plan and
  accepts the safe one. Every picture script calls it before it makes a renderer.
- A plan's colour comes from the verifier's answer, never from the plan's name.
- Every number on a picture is worked out from the data shown on that frame.
- The tests check each accepted plan a second time without the verifier, against the
  true boxes. In the many-plans scene that count, breaches among accepted, must be zero
  or `prove()` fails.

## What the pictures do not show

- **A scripted planner drives.** No learned policy runs in these scenes. The many-plans
  scene uses a random generator. The pictures say so in their footers.
- **The simulator gives the person's position.** No vision model runs. On a real robot
  a vision model would have to see the person, and that part is judged, not proven.
- **The motion is drawn, not flown.** The machines are moved along their plans. Thrust,
  wind, wheel slip and tracking error are not simulated.
- **"Proven" is about the plan as a line.** The verifier tests sample points on each
  leg, so each scene grows its boxes by half the gap between samples and keeps its legs
  short enough for that margin to cover them. The tests check both.
- **The quadrotor is drawn six times its size**, because at true size it would be a few
  pixels. The checks use the true size, and a test keeps the drawn body clear of the
  boxes.

The quadrotor and the delivery robot are plain shapes made in the scripts. They are no
real product, and the scenes load no file.

## Run the checks

They need nothing beyond the package:

```
pytest examples/gallery3d/test_delivery.py examples/gallery3d/test_patrol.py \
       examples/gallery3d/test_many.py -q
```

## Make the pictures

The pictures need MuJoCo, NumPy and Pillow. MuJoCo comes with the `robotics` extra.
Pillow is not in the lock file; `uv run --with pillow` brings it for the one run.

```
uv sync --locked --extra robotics --extra dev
MUJOCO_GL=egl uv run --no-sync --with pillow python examples/gallery3d/render_delivery.py
MUJOCO_GL=egl uv run --no-sync --with pillow python examples/gallery3d/render_patrol.py
MUJOCO_GL=egl uv run --no-sync --with pillow python examples/gallery3d/render_many.py
```

Each script writes into `examples/gallery3d/out/` (ignored by git), or into the folder
you give with `--out`: an animated WebP at 480 px and at most 150 KB, a GIF, a PNG still
for readers who ask for reduced motion, two check pictures at 360 px on a light and a
dark page, and a contact sheet. The labels use Liberation Sans; a script fails if the
font is missing.

To render on the CPU, with no graphics card, put Mesa's software driver in front:

```
__EGL_VENDOR_LIBRARY_FILENAMES=/usr/share/glvnd/egl_vendor.d/50_mesa.json \
LIBGL_ALWAYS_SOFTWARE=1 MUJOCO_GL=egl \
uv run --no-sync --with pillow python examples/gallery3d/render_patrol.py
```

## Run every test

```
MUJOCO_GL=egl uv run --no-sync --with pillow python -m pytest examples/gallery3d -q
```

The default suite collects these tests too, through `tests/test_gallery3d.py`. Where
MuJoCo or Pillow is absent the picture tests are skipped and say what is missing. Where
MuJoCo cannot make a renderer, the tests that draw skip by their own marks.
