//go:build mujoco

/* cgo compiles only the C files of the package's own directory; the C
 * layer itself lives in clients/native/ort, where Rust builds it too. */
#include "../../../native/ort/dort.c"
