//go:build mujoco

/* cgo compiles only the C files of the package's own directory; the C
 * layer itself lives in clients/native/mujoco, where Rust builds it too. */
#include "../../../native/mujoco/dmj.c"
