/* dort.c: see dort.h. */
#include "dort.h"

#include <pthread.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <onnxruntime_c_api.h>

struct dort_session {
  OrtSession* session;
};

static const OrtApi* api;
static OrtEnv* env;
static OrtStatus* env_status;
static pthread_once_t env_once = PTHREAD_ONCE_INIT;

static void dort_err(char* err, int errlen, const char* fmt, ...) {
  if (err == NULL || errlen <= 0) return;
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(err, (size_t)errlen, fmt, ap);
  va_end(ap);
}

/* Write the status's message and free it. Returns 1 when there was one. */
static int dort_failed(OrtStatus* st, const char* what, char* err, int errlen) {
  if (st == NULL) return 0;
  dort_err(err, errlen, "%s: %s", what, api->GetErrorMessage(st));
  api->ReleaseStatus(st);
  return 1;
}

static void dort_init(void) {
  api = OrtGetApiBase()->GetApi(ORT_API_VERSION);
  if (api == NULL) return;
  env_status = api->CreateEnv(ORT_LOGGING_LEVEL_ERROR, "daisugi", &env);
}

static int dort_ready(char* err, int errlen) {
  pthread_once(&env_once, dort_init);
  if (api == NULL) {
    dort_err(err, errlen, "ONNX Runtime %s does not offer C API version %d",
             OrtGetApiBase()->GetVersionString(), ORT_API_VERSION);
    return 0;
  }
  if (env == NULL) {
    dort_err(err, errlen, "could not make the ONNX Runtime environment: %s",
             env_status ? api->GetErrorMessage(env_status) : "unknown error");
    return 0;
  }
  return 1;
}

const char* dort_version(void) { return OrtGetApiBase()->GetVersionString(); }

dort_session* dort_open(const char* path, int threads, char* err, int errlen) {
  if (!dort_ready(err, errlen)) return NULL;
  OrtSessionOptions* opts = NULL;
  if (dort_failed(api->CreateSessionOptions(&opts), "session options", err, errlen)) return NULL;
  OrtSession* session = NULL;
  int bad = dort_failed(api->SetIntraOpNumThreads(opts, threads), "intra-op threads", err, errlen) ||
            dort_failed(api->SetInterOpNumThreads(opts, 1), "inter-op threads", err, errlen) ||
            dort_failed(api->SetSessionExecutionMode(opts, ORT_SEQUENTIAL), "execution mode", err,
                        errlen) ||
            dort_failed(api->SetSessionGraphOptimizationLevel(opts, ORT_ENABLE_ALL),
                        "optimization level", err, errlen) ||
            dort_failed(api->CreateSession(env, path, opts, &session), "could not load the model",
                        err, errlen);
  api->ReleaseSessionOptions(opts);
  if (bad) return NULL;
  dort_session* s = calloc(1, sizeof *s);
  if (s == NULL) {
    api->ReleaseSession(session);
    dort_err(err, errlen, "out of memory");
    return NULL;
  }
  s->session = session;
  return s;
}

void dort_close(dort_session* s) {
  if (s == NULL) return;
  api->ReleaseSession(s->session);
  free(s);
}

int dort_run(dort_session* s, const dort_input* in, int nin, const dort_output* out, int nout,
             char* err, int errlen) {
  if (s == NULL || nin < 0 || nout < 0) {
    dort_err(err, errlen, "no session");
    return 1;
  }
  int rc = 1;
  OrtMemoryInfo* mem = NULL;
  OrtValue** ins = calloc((size_t)nin + 1, sizeof *ins);
  OrtValue** outs = calloc((size_t)nout + 1, sizeof *outs);
  const char** in_names = calloc((size_t)nin + 1, sizeof *in_names);
  const char** out_names = calloc((size_t)nout + 1, sizeof *out_names);
  if (!ins || !outs || !in_names || !out_names) {
    dort_err(err, errlen, "out of memory");
    goto done;
  }
  if (dort_failed(api->CreateCpuMemoryInfo(OrtArenaAllocator, OrtMemTypeDefault, &mem),
                  "memory info", err, errlen))
    goto done;
  for (int i = 0; i < nin; i++) {
    size_t width;
    ONNXTensorElementDataType t;
    if (in[i].type == DORT_FLOAT) {
      width = sizeof(float);
      t = ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT;
    } else if (in[i].type == DORT_INT64) {
      width = sizeof(int64_t);
      t = ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64;
    } else {
      dort_err(err, errlen, "input %s: element type %d is not float32 or int64", in[i].name,
               in[i].type);
      goto done;
    }
    size_t n = 1;
    for (int d = 0; d < in[i].rank; d++) {
      if (in[i].shape[d] < 0) {
        dort_err(err, errlen, "input %s: a negative dimension", in[i].name);
        goto done;
      }
      n *= (size_t)in[i].shape[d];
    }
    in_names[i] = in[i].name;
    if (dort_failed(api->CreateTensorWithDataAsOrtValue(mem, (void*)in[i].data, n * width,
                                                        in[i].shape, (size_t)in[i].rank, t,
                                                        &ins[i]),
                    in[i].name, err, errlen))
      goto done;
  }
  for (int i = 0; i < nout; i++) out_names[i] = out[i].name;
  if (dort_failed(api->Run(s->session, NULL, in_names, (const OrtValue* const*)ins, (size_t)nin,
                           out_names, (size_t)nout, outs),
                  "run", err, errlen))
    goto done;
  for (int i = 0; i < nout; i++) {
    OrtTensorTypeAndShapeInfo* info = NULL;
    if (dort_failed(api->GetTensorTypeAndShape(outs[i], &info), out[i].name, err, errlen))
      goto done;
    ONNXTensorElementDataType t;
    size_t n = 0;
    int bad = dort_failed(api->GetTensorElementType(info, &t), out[i].name, err, errlen) ||
              dort_failed(api->GetTensorShapeElementCount(info, &n), out[i].name, err, errlen);
    api->ReleaseTensorTypeAndShapeInfo(info);
    if (bad) goto done;
    if (t != ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT) {
      dort_err(err, errlen, "output %s is not float32", out[i].name);
      goto done;
    }
    if ((int64_t)n != out[i].count) {
      dort_err(err, errlen, "output %s has %zu elements, not %lld", out[i].name, n,
               (long long)out[i].count);
      goto done;
    }
    void* p = NULL;
    if (dort_failed(api->GetTensorMutableData(outs[i], &p), out[i].name, err, errlen)) goto done;
    memcpy(out[i].data, p, n * sizeof(float));
  }
  rc = 0;
done:
  for (int i = 0; ins && i < nin; i++)
    if (ins[i]) api->ReleaseValue(ins[i]);
  for (int i = 0; outs && i < nout; i++)
    if (outs[i]) api->ReleaseValue(outs[i]);
  if (mem) api->ReleaseMemoryInfo(mem);
  free(ins);
  free(outs);
  free(in_names);
  free(out_names);
  return rc;
}
