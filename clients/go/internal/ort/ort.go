//go:build mujoco

// Package ort is a thin wrapper over ONNX Runtime's C API: open a session
// on a model file and run it on float32 and int64 inputs into float32
// outputs. That is all the VLA executors need.
//
// It links the libonnxruntime.so that clients/go/scripts/native.sh
// --onnxruntime installs (through the clients/go/.onnxruntime link) and
// the small C layer in clients/native/ort, which the Rust wrapper shares.
// The package builds only with the mujoco build tag, the robotics build
// option, so the shipped binaries never link ONNX Runtime. At run time the
// dynamic loader must find libonnxruntime.so.1 (LD_LIBRARY_PATH).
//
// A Session may be run from one goroutine at a time.
package ort

/*
#cgo CFLAGS: -I${SRCDIR}/../../.onnxruntime/include -I${SRCDIR}/../../../native/ort
#cgo LDFLAGS: -L${SRCDIR}/../../.onnxruntime/lib -lonnxruntime -lpthread
#include <stdlib.h>
#include "dort.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

const errLen = 2000

// Version is the ONNX Runtime version the loaded library reports.
func Version() string { return C.GoString(C.dort_version()) }

// Session is one model, loaded.
type Session struct {
	s *C.dort_session
}

// Open loads the model at path, to run with threads intra-op threads.
func Open(path string, threads int) (*Session, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var buf [errLen]C.char
	s := C.dort_open(cpath, C.int(threads), &buf[0], errLen)
	if s == nil {
		return nil, errors.New(C.GoString(&buf[0]))
	}
	return &Session{s: s}, nil
}

// Close frees the session.
func (s *Session) Close() {
	if s != nil && s.s != nil {
		C.dort_close(s.s)
		s.s = nil
	}
}

// Input is one input tensor: exactly one of F32 and I64, and its shape.
type Input struct {
	Name  string
	F32   []float32
	I64   []int64
	Shape []int64
}

// Output is one float32 output, written into Data, which must hold
// exactly the output's element count.
type Output struct {
	Name string
	Data []float32
}

// Run runs the session.
func (s *Session) Run(in []Input, out []Output) error {
	if s == nil || s.s == nil {
		return errors.New("ort: the session is closed")
	}
	var pin runtime.Pinner
	defer pin.Unpin()
	var cstrs []*C.char
	defer func() {
		for _, p := range cstrs {
			C.free(unsafe.Pointer(p))
		}
	}()
	cstr := func(s string) *C.char {
		p := C.CString(s)
		cstrs = append(cstrs, p)
		return p
	}
	cin := (*[1 << 20]C.dort_input)(C.calloc(C.size_t(len(in)+1), C.size_t(unsafe.Sizeof(C.dort_input{}))))[: len(in)+1 : len(in)+1]
	defer C.free(unsafe.Pointer(&cin[0]))
	for i, x := range in {
		n := int64(1)
		for _, d := range x.Shape {
			n *= d
		}
		var data unsafe.Pointer
		var typ C.int
		switch {
		case x.F32 != nil && x.I64 == nil && int64(len(x.F32)) == n && n > 0:
			data, typ = unsafe.Pointer(&x.F32[0]), C.DORT_FLOAT
		case x.I64 != nil && x.F32 == nil && int64(len(x.I64)) == n && n > 0:
			data, typ = unsafe.Pointer(&x.I64[0]), C.DORT_INT64
		default:
			return fmt.Errorf("ort: input %s: give one of F32 and I64, with %d values", x.Name, n)
		}
		pin.Pin(data)
		var shape *C.int64_t
		if len(x.Shape) > 0 {
			shape = (*C.int64_t)(unsafe.Pointer(&x.Shape[0]))
			pin.Pin(shape)
		}
		cin[i] = C.dort_input{name: cstr(x.Name), _type: typ, data: data, shape: shape, rank: C.int(len(x.Shape))}
	}
	cout := (*[1 << 20]C.dort_output)(C.calloc(C.size_t(len(out)+1), C.size_t(unsafe.Sizeof(C.dort_output{}))))[: len(out)+1 : len(out)+1]
	defer C.free(unsafe.Pointer(&cout[0]))
	for i, y := range out {
		if len(y.Data) == 0 {
			return fmt.Errorf("ort: output %s has no buffer", y.Name)
		}
		p := (*C.float)(unsafe.Pointer(&y.Data[0]))
		pin.Pin(p)
		cout[i] = C.dort_output{name: cstr(y.Name), data: p, count: C.int64_t(len(y.Data))}
	}
	var buf [errLen]C.char
	if C.dort_run(s.s, &cin[0], C.int(len(in)), &cout[0], C.int(len(out)), &buf[0], errLen) != 0 {
		return errors.New("ort: " + C.GoString(&buf[0]))
	}
	return nil
}
