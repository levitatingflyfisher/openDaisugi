/* dmj.h: the small C layer the Go and Rust MuJoCo wrappers share.
 *
 * Go reaches it through cgo, Rust through hand-written FFI. Both call
 * MuJoCo's own functions (mj_step, mj_forward, mj_name2id, ...) directly;
 * this layer adds only what a language without the C headers cannot do
 * safely:
 *
 *   - load MJCF from a file or from a string, with the parser's message;
 *   - read the model and data fields the executors use, as flat views
 *     (plain counts and pointers, one fixed struct each);
 *   - render a camera offscreen to RGB with no display, through EGL, as
 *     MuJoCo's sample/record.cc does.
 *
 * Every function that can fail writes a message into err (at most errlen
 * bytes, always terminated) and returns NULL or a non-zero code.
 */
#ifndef DMJ_H
#define DMJ_H

#include <mujoco/mujoco.h>

#ifdef __cplusplus
extern "C" {
#endif

/* The MuJoCo version the library reports, such as 3012000 for 3.12.0. */
int dmj_version(void);

/* mj_loadXML on a path. NULL on failure, with the parser's message. */
mjModel* dmj_load_xml(const char* path, char* err, int errlen);

/* mj_loadXML on a string, through a VFS holding one file. */
mjModel* dmj_load_xml_string(const char* xml, char* err, int errlen);

/* The model fields the executors read. Arrays are row major: actuator_trnid
 * is nu x 2, jnt_range and actuator_ctrlrange are njnt x 2 and nu x 2. */
typedef struct {
  int nq, nv, nu, njnt, nbody, nsite, ngeom, ncam;
  double timestep;
  const int* actuator_trnid;
  const int* jnt_type;
  const int* jnt_qposadr;
  const unsigned char* jnt_limited; /* mjtBool, one byte each */
  const double* jnt_range;
  const unsigned char* actuator_ctrllimited;
  const double* actuator_ctrlrange;
} dmj_model_view;

void dmj_model_view_get(const mjModel* m, dmj_model_view* out);

/* The data fields the executors read and write. xpos is nbody x 3,
 * site_xpos is nsite x 3. The pointers stay valid as long as d lives. */
typedef struct {
  double* qpos;
  double* qvel;
  double* ctrl;
  double* xpos;
  double* site_xpos;
  double* actuator_force;
} dmj_data_view;

void dmj_data_view_get(const mjModel* m, mjData* d, dmj_data_view* out);

/* The number of contacts, and the two geom ids of contact i (the
 * geom1 and geom2 fields). */
int dmj_ncon(const mjData* d);
void dmj_contact_geoms(const mjData* d, int i, int* geom1, int* geom2);

/* An offscreen renderer: one EGL context, one scene, one MuJoCo render
 * context, at a fixed size no larger than the model's offscreen buffer
 * (visual/global offwidth, offheight). */
typedef struct dmj_renderer dmj_renderer;

dmj_renderer* dmj_renderer_new(const mjModel* m, int width, int height, char* err, int errlen);

/* Render camera camid (-1 for the default free camera) of the state in d,
 * and copy width x height x 3 bytes of RGB into rgb, top row first. */
int dmj_renderer_render(dmj_renderer* r, const mjModel* m, mjData* d, int camid,
                        unsigned char* rgb, char* err, int errlen);

void dmj_renderer_free(dmj_renderer* r);

#ifdef __cplusplus
}
#endif

#endif
