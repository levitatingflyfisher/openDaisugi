/* dmj.c: see dmj.h. */
#include "dmj.h"

#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void dmj_err(char* err, int errlen, const char* fmt, ...) {
  if (!err || errlen <= 0) return;
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(err, (size_t)errlen, fmt, ap);
  va_end(ap);
}

int dmj_version(void) { return mj_version(); }

mjModel* dmj_load_xml(const char* path, char* err, int errlen) {
  if (err && errlen > 0) err[0] = 0;
  mjModel* m = mj_loadXML(path, NULL, err, errlen);
  if (!m && err && errlen > 0 && !err[0]) dmj_err(err, errlen, "could not load %s", path);
  return m;
}

mjModel* dmj_load_xml_string(const char* xml, char* err, int errlen) {
  static const char name[] = "model.xml";
  if (err && errlen > 0) err[0] = 0;
  mjVFS vfs;
  mj_defaultVFS(&vfs);
  if (mj_addBufferVFS(&vfs, name, xml, (int)strlen(xml)) != 0) {
    mj_deleteVFS(&vfs);
    dmj_err(err, errlen, "could not hold the model in a VFS");
    return NULL;
  }
  mjModel* m = mj_loadXML(name, &vfs, err, errlen);
  mj_deleteVFS(&vfs);
  if (!m && err && errlen > 0 && !err[0]) dmj_err(err, errlen, "could not load the model");
  return m;
}

void dmj_model_view_get(const mjModel* m, dmj_model_view* out) {
  out->nq = (int)m->nq;
  out->nv = (int)m->nv;
  out->nu = (int)m->nu;
  out->njnt = (int)m->njnt;
  out->nbody = (int)m->nbody;
  out->nsite = (int)m->nsite;
  out->ngeom = (int)m->ngeom;
  out->ncam = (int)m->ncam;
  out->timestep = m->opt.timestep;
  out->actuator_trnid = m->actuator_trnid;
  out->jnt_type = m->jnt_type;
  out->jnt_qposadr = m->jnt_qposadr;
  out->jnt_limited = (const unsigned char*)m->jnt_limited;
  out->jnt_range = m->jnt_range;
  out->actuator_ctrllimited = (const unsigned char*)m->actuator_ctrllimited;
  out->actuator_ctrlrange = m->actuator_ctrlrange;
}

void dmj_data_view_get(const mjModel* m, mjData* d, dmj_data_view* out) {
  (void)m;
  out->qpos = d->qpos;
  out->qvel = d->qvel;
  out->ctrl = d->ctrl;
  out->xpos = d->xpos;
  out->site_xpos = d->site_xpos;
  out->actuator_force = d->actuator_force;
}

int dmj_ncon(const mjData* d) { return d->ncon; }

void dmj_contact_geoms(const mjData* d, int i, int* geom1, int* geom2) {
  *geom1 = d->contact[i].geom1;
  *geom2 = d->contact[i].geom2;
}

/* ---- offscreen rendering through EGL ---------------------------------- */

struct dmj_renderer {
  EGLDisplay dpy;
  EGLContext ctx;
  int width, height;
  mjvScene scn;
  mjvOption opt;
  mjrContext con;
};

static const EGLint dmj_egl_config[] = {
    EGL_RED_SIZE,          8,
    EGL_GREEN_SIZE,        8,
    EGL_BLUE_SIZE,         8,
    EGL_ALPHA_SIZE,        8,
    EGL_DEPTH_SIZE,        24,
    EGL_STENCIL_SIZE,      8,
    EGL_COLOR_BUFFER_TYPE, EGL_RGB_BUFFER,
    EGL_SURFACE_TYPE,      EGL_PBUFFER_BIT,
    EGL_RENDERABLE_TYPE,   EGL_OPENGL_BIT,
    EGL_NONE};

/* The first EGL device that initializes, as MuJoCo's Python EGL context
 * picks it; then the default display, as sample/record.cc does. */
static EGLDisplay dmj_egl_display(void) {
  PFNEGLQUERYDEVICESEXTPROC query =
      (PFNEGLQUERYDEVICESEXTPROC)eglGetProcAddress("eglQueryDevicesEXT");
  PFNEGLGETPLATFORMDISPLAYEXTPROC platform =
      (PFNEGLGETPLATFORMDISPLAYEXTPROC)eglGetProcAddress("eglGetPlatformDisplayEXT");
  if (query && platform) {
    EGLDeviceEXT devices[16];
    EGLint n = 0;
    if (query(16, devices, &n) == EGL_TRUE) {
      for (EGLint i = 0; i < n; i++) {
        EGLDisplay dpy = platform(EGL_PLATFORM_DEVICE_EXT, devices[i], NULL);
        if (dpy == EGL_NO_DISPLAY || eglGetError() != EGL_SUCCESS) continue;
        EGLint major, minor;
        if (eglInitialize(dpy, &major, &minor) == EGL_TRUE && eglGetError() == EGL_SUCCESS) {
          return dpy;
        }
      }
    }
  }
  EGLDisplay dpy = eglGetDisplay(EGL_DEFAULT_DISPLAY);
  if (dpy == EGL_NO_DISPLAY) return EGL_NO_DISPLAY;
  EGLint major, minor;
  if (eglInitialize(dpy, &major, &minor) != EGL_TRUE) return EGL_NO_DISPLAY;
  return dpy;
}

static int dmj_current(dmj_renderer* r, char* err, int errlen) {
  if (eglMakeCurrent(r->dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, r->ctx) != EGL_TRUE) {
    dmj_err(err, errlen, "could not make the EGL context current (EGL error 0x%x)", eglGetError());
    return 1;
  }
  return 0;
}

/* A context is current on one thread at a time; every call releases it,
 * so the next call may come from another thread. */
static void dmj_release(dmj_renderer* r) {
  eglMakeCurrent(r->dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
}

dmj_renderer* dmj_renderer_new(const mjModel* m, int width, int height, char* err, int errlen) {
  if (err && errlen > 0) err[0] = 0;
  if (width <= 0 || height <= 0) {
    dmj_err(err, errlen, "the image size %dx%d is not positive", width, height);
    return NULL;
  }
  if (width > m->vis.global.offwidth || height > m->vis.global.offheight) {
    dmj_err(err, errlen, "the image size %dx%d is larger than the offscreen buffer %dx%d",
            width, height, m->vis.global.offwidth, m->vis.global.offheight);
    return NULL;
  }
  EGLDisplay dpy = dmj_egl_display();
  if (dpy == EGL_NO_DISPLAY) {
    dmj_err(err, errlen, "no EGL display could be initialized (EGL error 0x%x)", eglGetError());
    return NULL;
  }
  EGLConfig cfg;
  EGLint ncfg = 0;
  if (eglChooseConfig(dpy, dmj_egl_config, &cfg, 1, &ncfg) != EGL_TRUE || ncfg < 1) {
    dmj_err(err, errlen, "no EGL config fits (EGL error 0x%x)", eglGetError());
    return NULL;
  }
  if (eglBindAPI(EGL_OPENGL_API) != EGL_TRUE) {
    dmj_err(err, errlen, "could not bind the OpenGL API (EGL error 0x%x)", eglGetError());
    return NULL;
  }
  EGLContext ctx = eglCreateContext(dpy, cfg, EGL_NO_CONTEXT, NULL);
  if (ctx == EGL_NO_CONTEXT) {
    dmj_err(err, errlen, "could not create an EGL context (EGL error 0x%x)", eglGetError());
    return NULL;
  }
  dmj_renderer* r = (dmj_renderer*)calloc(1, sizeof(dmj_renderer));
  if (!r) {
    eglDestroyContext(dpy, ctx);
    dmj_err(err, errlen, "out of memory");
    return NULL;
  }
  r->dpy = dpy;
  r->ctx = ctx;
  r->width = width;
  r->height = height;
  if (dmj_current(r, err, errlen)) {
    eglDestroyContext(dpy, ctx);
    free(r);
    return NULL;
  }
  mjv_defaultScene(&r->scn);
  mjv_makeScene(m, &r->scn, 10000);
  mjv_defaultOption(&r->opt);
  mjr_defaultContext(&r->con);
  mjr_makeContext(m, &r->con, mjFONTSCALE_150);
  mjr_setBuffer(mjFB_OFFSCREEN, &r->con);
  dmj_release(r);
  return r;
}

int dmj_renderer_render(dmj_renderer* r, const mjModel* m, mjData* d, int camid,
                        unsigned char* rgb, char* err, int errlen) {
  if (err && errlen > 0) err[0] = 0;
  if (camid < -1 || camid >= m->ncam) {
    dmj_err(err, errlen, "the camera id %d is out of range [-1, %d)", camid, m->ncam);
    return 1;
  }
  if (dmj_current(r, err, errlen)) return 1;
  mjvCamera cam;
  mjv_defaultCamera(&cam);
  cam.fixedcamid = camid;
  if (camid == -1) {
    cam.type = mjCAMERA_FREE;
    mjv_defaultFreeCamera(m, &cam);
  } else {
    cam.type = mjCAMERA_FIXED;
  }
  mjv_updateScene(m, d, &r->opt, NULL, &cam, mjCAT_ALL, &r->scn);
  mjrRect rect = {0, 0, r->width, r->height};
  mjr_setBuffer(mjFB_OFFSCREEN, &r->con);
  mjr_render(rect, &r->scn, &r->con);
  size_t row = (size_t)r->width * 3;
  unsigned char* tmp = (unsigned char*)malloc(row * (size_t)r->height);
  if (!tmp) {
    dmj_release(r);
    dmj_err(err, errlen, "out of memory");
    return 1;
  }
  mjr_readPixels(tmp, NULL, rect, &r->con);
  dmj_release(r);
  /* OpenGL reads the bottom row first. */
  for (int y = 0; y < r->height; y++) {
    memcpy(rgb + (size_t)y * row, tmp + (size_t)(r->height - 1 - y) * row, row);
  }
  free(tmp);
  return 0;
}

void dmj_renderer_free(dmj_renderer* r) {
  if (!r) return;
  if (eglMakeCurrent(r->dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, r->ctx) == EGL_TRUE) {
    mjr_freeContext(&r->con);
    dmj_release(r);
  }
  mjv_freeScene(&r->scn);
  eglDestroyContext(r->dpy, r->ctx);
  free(r);
}
