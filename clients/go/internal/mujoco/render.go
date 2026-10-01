//go:build mujoco

package mujoco

/*
#include <stdlib.h>
#include "dmj.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Renderer draws a camera offscreen through EGL, with no display. Each
// call makes its context current and releases it again, so a Renderer may
// be used from any goroutine, one call at a time.
type Renderer struct {
	r             *C.dmj_renderer
	width, height int
}

// NewRenderer makes a width x height renderer for m. The size must fit
// the model's offscreen buffer (visual/global offwidth and offheight).
func NewRenderer(m *Model, width, height int) (*Renderer, error) {
	err := make([]byte, errLen)
	r := C.dmj_renderer_new(m.m, C.int(width), C.int(height), (*C.char)(unsafe.Pointer(&err[0])), errLen)
	if r == nil {
		return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&err[0]))))
	}
	return &Renderer{r: r, width: width, height: height}, nil
}

// Render draws camera cam (-1 for the default free camera) of the state
// in d and returns width x height x 3 bytes of RGB, top row first.
func (r *Renderer) Render(d *Data, cam int) ([]byte, error) {
	rgb := C.malloc(C.size_t(r.width * r.height * 3))
	defer C.free(rgb)
	err := make([]byte, errLen)
	if C.dmj_renderer_render(r.r, d.m.m, d.d, C.int(cam), (*C.uchar)(rgb), (*C.char)(unsafe.Pointer(&err[0])), errLen) != 0 {
		return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&err[0]))))
	}
	return C.GoBytes(rgb, C.int(r.width*r.height*3)), nil
}

// Close frees the renderer and its EGL context.
func (r *Renderer) Close() {
	if r.r != nil {
		C.dmj_renderer_free(r.r)
		r.r = nil
	}
}
