//go:build mujoco

// Package mujoco is a thin wrapper over MuJoCo's C API: what the robotics
// executors need and nothing more. Load MJCF from a file or a string, make
// data, reset, forward, step, read and write qpos, qvel and ctrl, read body
// and site positions, look names up, the body Jacobian, the contacts, and
// offscreen rendering of a camera to RGB.
//
// It links the libmujoco.so that clients/go/scripts/native.sh --mujoco
// installs (through the clients/go/.mujoco link) and the small C layer in
// clients/native/mujoco, which the Rust wrapper shares. The package builds
// only with the mujoco build tag, so the shipped binaries never link
// MuJoCo. At run time the dynamic loader must find libmujoco.so
// (LD_LIBRARY_PATH).
//
// A Model or Data is not safe for use from two goroutines at once.
package mujoco

/*
#cgo CFLAGS: -I${SRCDIR}/../../.mujoco/include -I${SRCDIR}/../../../native/mujoco
#cgo LDFLAGS: -L${SRCDIR}/../../.mujoco/lib -lmujoco -lEGL
#include <stdlib.h>
#include "dmj.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

const errLen = 1000

// ObjType is mjtObj.
type ObjType int

// The object types the executors look up.
const (
	ObjBody     ObjType = C.mjOBJ_BODY
	ObjJoint    ObjType = C.mjOBJ_JOINT
	ObjGeom     ObjType = C.mjOBJ_GEOM
	ObjSite     ObjType = C.mjOBJ_SITE
	ObjCamera   ObjType = C.mjOBJ_CAMERA
	ObjActuator ObjType = C.mjOBJ_ACTUATOR
)

// The joint types (mjtJoint).
const (
	JntFree  = int(C.mjJNT_FREE)
	JntBall  = int(C.mjJNT_BALL)
	JntSlide = int(C.mjJNT_SLIDE)
	JntHinge = int(C.mjJNT_HINGE)
)

// Version is mj_version(): 3012000 for MuJoCo 3.12.0.
func Version() int { return int(C.dmj_version()) }

// Model is an mjModel.
type Model struct {
	m *C.mjModel
	v C.dmj_model_view
}

func newModel(m *C.mjModel, err []byte) (*Model, error) {
	if m == nil {
		return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&err[0]))))
	}
	out := &Model{m: m}
	C.dmj_model_view_get(m, &out.v)
	return out, nil
}

// LoadXML compiles the MJCF file at path. The error is MuJoCo's message.
func LoadXML(path string) (*Model, error) {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	err := make([]byte, errLen)
	return newModel(C.dmj_load_xml(cp, (*C.char)(unsafe.Pointer(&err[0])), errLen), err)
}

// LoadXMLString compiles MJCF held in a string.
func LoadXMLString(xml string) (*Model, error) {
	cx := C.CString(xml)
	defer C.free(unsafe.Pointer(cx))
	err := make([]byte, errLen)
	return newModel(C.dmj_load_xml_string(cx, (*C.char)(unsafe.Pointer(&err[0])), errLen), err)
}

// Close frees the model.
func (m *Model) Close() {
	if m.m != nil {
		C.mj_deleteModel(m.m)
		m.m = nil
	}
}

func (m *Model) NQ() int           { return int(m.v.nq) }
func (m *Model) NV() int           { return int(m.v.nv) }
func (m *Model) NU() int           { return int(m.v.nu) }
func (m *Model) NJnt() int         { return int(m.v.njnt) }
func (m *Model) NBody() int        { return int(m.v.nbody) }
func (m *Model) NSite() int        { return int(m.v.nsite) }
func (m *Model) NGeom() int        { return int(m.v.ngeom) }
func (m *Model) NCam() int         { return int(m.v.ncam) }
func (m *Model) Timestep() float64 { return float64(m.v.timestep) }
func ints(p *C.int, n int) []C.int { return unsafe.Slice(p, n) }
func f64s(p *C.double, n int) []float64 {
	if p == nil || n == 0 {
		return nil
	}
	return unsafe.Slice((*float64)(unsafe.Pointer(p)), n)
}
func bytesOf(p *C.uchar, n int) []C.uchar { return unsafe.Slice(p, n) }

// ActuatorTrnID is actuator_trnid[a, 0]: the joint actuator a drives.
func (m *Model) ActuatorTrnID(a int) int { return int(ints(m.v.actuator_trnid, 2*m.NU())[2*a]) }

// JntType is jnt_type[j].
func (m *Model) JntType(j int) int { return int(ints(m.v.jnt_type, m.NJnt())[j]) }

// JntQposAdr is jnt_qposadr[j].
func (m *Model) JntQposAdr(j int) int { return int(ints(m.v.jnt_qposadr, m.NJnt())[j]) }

// JntLimited is jnt_limited[j].
func (m *Model) JntLimited(j int) bool { return bytesOf(m.v.jnt_limited, m.NJnt())[j] != 0 }

// JntRange is jnt_range[j].
func (m *Model) JntRange(j int) (lo, hi float64) {
	r := f64s(m.v.jnt_range, 2*m.NJnt())
	return r[2*j], r[2*j+1]
}

// ActuatorCtrlLimited is actuator_ctrllimited[a].
func (m *Model) ActuatorCtrlLimited(a int) bool {
	return bytesOf(m.v.actuator_ctrllimited, m.NU())[a] != 0
}

// ActuatorCtrlRange is actuator_ctrlrange[a].
func (m *Model) ActuatorCtrlRange(a int) (lo, hi float64) {
	r := f64s(m.v.actuator_ctrlrange, 2*m.NU())
	return r[2*a], r[2*a+1]
}

// Name2ID is mj_name2id: -1 when no object of that type has the name.
func (m *Model) Name2ID(kind ObjType, name string) int {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	return int(C.mj_name2id(m.m, C.int(kind), cn))
}

// ID2Name is mj_id2name; ok is false when the object has no name or the
// id is out of range.
func (m *Model) ID2Name(kind ObjType, id int) (string, bool) {
	p := C.mj_id2name(m.m, C.int(kind), C.int(id))
	if p == nil {
		return "", false
	}
	s := C.GoString(p)
	return s, s != ""
}

// Data is an mjData.
type Data struct {
	d *C.mjData
	m *Model
	v C.dmj_data_view
}

// NewData is mj_makeData. It runs no computation: positions read zero
// until Forward or Step.
func NewData(m *Model) *Data {
	out := &Data{d: C.mj_makeData(m.m), m: m}
	C.dmj_data_view_get(m.m, out.d, &out.v)
	return out
}

// Close frees the data.
func (d *Data) Close() {
	if d.d != nil {
		C.mj_deleteData(d.d)
		d.d = nil
	}
}

// QPos, QVel, Ctrl and ActuatorForce are views of the C arrays: writes go
// straight to the simulation.
func (d *Data) QPos() []float64          { return f64s(d.v.qpos, d.m.NQ()) }
func (d *Data) QVel() []float64          { return f64s(d.v.qvel, d.m.NV()) }
func (d *Data) Ctrl() []float64          { return f64s(d.v.ctrl, d.m.NU()) }
func (d *Data) ActuatorForce() []float64 { return f64s(d.v.actuator_force, d.m.NU()) }

// XPos is the world position of body b.
func (d *Data) XPos(b int) [3]float64 {
	x := f64s(d.v.xpos, 3*d.m.NBody())
	return [3]float64{x[3*b], x[3*b+1], x[3*b+2]}
}

// SiteXPos is the world position of site s.
func (d *Data) SiteXPos(s int) [3]float64 {
	x := f64s(d.v.site_xpos, 3*d.m.NSite())
	return [3]float64{x[3*s], x[3*s+1], x[3*s+2]}
}

// NCon is the number of contacts.
func (d *Data) NCon() int { return int(C.dmj_ncon(d.d)) }

// ContactGeoms is contact i's geom1 and geom2.
func (d *Data) ContactGeoms(i int) (int, int) {
	var g1, g2 C.int
	C.dmj_contact_geoms(d.d, C.int(i), &g1, &g2)
	return int(g1), int(g2)
}

// Reset is mj_resetData.
func Reset(m *Model, d *Data) { C.mj_resetData(m.m, d.d) }

// Forward is mj_forward.
func Forward(m *Model, d *Data) { C.mj_forward(m.m, d.d) }

// Step is mj_step.
func Step(m *Model, d *Data) { C.mj_step(m.m, d.d) }

// JacBody is the translational Jacobian of body b's frame (mj_jacBody's
// jacp): 3 x nv, row major.
func JacBody(m *Model, d *Data, b int) []float64 {
	nv := m.NV()
	jacp := make([]float64, 3*nv)
	if nv == 0 {
		return jacp
	}
	C.mj_jacBody(m.m, d.d, (*C.mjtNum)(unsafe.Pointer(&jacp[0])), nil, C.int(b))
	return jacp
}
