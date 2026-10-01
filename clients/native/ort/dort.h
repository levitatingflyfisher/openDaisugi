/* dort.h: the small C layer the Go and Rust ONNX Runtime wrappers share.
 *
 * Go reaches it through cgo, Rust through hand-written FFI. ONNX Runtime's
 * C API is a table of function pointers, which neither language can call
 * from its own side without generated bindings; this layer calls the table
 * and offers only what the VLA executors need:
 *
 *   - open a session on a model file, on the CPU, with a fixed number of
 *     intra-op threads, one inter-op thread, sequential execution and every
 *     graph optimization;
 *   - run it on float32 and int64 inputs whose memory the caller owns,
 *     and copy each float32 output into a buffer the caller owns, which
 *     must hold exactly the output's element count.
 *
 * One ONNX Runtime environment serves the whole process; it is made on the
 * first open and never freed. Every function that can fail writes a
 * message into err (at most errlen bytes, always terminated) and returns
 * NULL or a non-zero code.
 */
#ifndef DORT_H
#define DORT_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* The element types of a tensor, as ONNX numbers them. */
enum { DORT_FLOAT = 1, DORT_INT64 = 7 };

typedef struct dort_session dort_session;

/* The ONNX Runtime version the library reports, such as "1.23.2". */
const char* dort_version(void);

/* A session on the model at path, run with threads intra-op threads. */
dort_session* dort_open(const char* path, int threads, char* err, int errlen);

/* Free a session. NULL is allowed. */
void dort_close(dort_session* s);

/* One tensor: its name, element type, data and shape. */
typedef struct {
  const char* name;
  int type;
  const void* data;
  const int64_t* shape;
  int rank;
} dort_input;

/* An output: its name, the caller's buffer and its element count. */
typedef struct {
  const char* name;
  float* data;
  int64_t count;
} dort_output;

/* Run the session. Every output must be float32 and have count elements. */
int dort_run(dort_session* s, const dort_input* in, int nin, const dort_output* out, int nout,
             char* err, int errlen);

#ifdef __cplusplus
}
#endif

#endif
