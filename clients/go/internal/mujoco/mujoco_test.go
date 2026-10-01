//go:build mujoco

package mujoco

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tiny = `<mujoco model="tiny">
  <compiler angle="radian"/>
  <option timestep="0.01" gravity="0 0 0"/>
  <visual><global offwidth="64" offheight="48"/></visual>
  <worldbody>
    <light pos="0 0 2"/>
    <camera name="cam" pos="0.25 -1 0.6" xyaxes="1 0 0 0 0.5 1"/>
    <geom name="floor" type="plane" size="1 1 0.01" pos="0 0 -0.1" rgba="0.3 0.6 0.3 1"/>
    <body name="arm">
      <joint name="hinge" type="hinge" axis="0 0 1" range="-1 1"/>
      <geom name="rod" type="capsule" fromto="0 0 0 0.5 0 0" size="0.03" rgba="0.9 0.2 0.2 1"/>
      <site name="tip" pos="0.5 0 0"/>
      <body name="tipbody" pos="0.5 0 0"/>
    </body>
  </worldbody>
  <actuator><position name="act" joint="hinge" kp="10" ctrlrange="-0.5 0.5"/></actuator>
</mujoco>`

const touching = `<mujoco>
  <option gravity="0 0 0"/>
  <worldbody>
    <geom name="a" type="box" size="0.1 0.1 0.1"/>
    <body pos="0.1 0 0"><freejoint/><geom type="box" size="0.1 0.1 0.1"/></body>
  </worldbody>
</mujoco>`

func load(t *testing.T, xml string) *Model {
	t.Helper()
	m, err := LoadXMLString(xml)
	if err != nil {
		t.Fatalf("LoadXMLString: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestVersion(t *testing.T) {
	if Version() != 3012000 {
		t.Fatalf("Version() = %d, want 3012000", Version())
	}
}

func TestModelCounts(t *testing.T) {
	m := load(t, tiny)
	got := []int{m.NQ(), m.NV(), m.NU(), m.NJnt(), m.NBody(), m.NSite(), m.NGeom(), m.NCam()}
	want := []int{1, 1, 1, 1, 3, 1, 2, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("counts = %v, want %v", got, want)
		}
	}
	if m.Timestep() != 0.01 {
		t.Fatalf("Timestep() = %v", m.Timestep())
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := LoadXMLString("<mujoco><worldbody><body></mujoco>"); err == nil {
		t.Fatal("broken XML loaded")
	}
	missing := filepath.Join(t.TempDir(), "absent.xml")
	_, err := LoadXML(missing)
	if err == nil || !strings.Contains(err.Error(), "Error opening file") {
		t.Fatalf("LoadXML(missing) error = %v", err)
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.xml")
	if err := os.WriteFile(path, []byte(tiny), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadXML(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.NU() != 1 {
		t.Fatalf("NU() = %d", m.NU())
	}
}

func TestNames(t *testing.T) {
	m := load(t, tiny)
	cases := []struct {
		kind ObjType
		name string
		id   int
	}{
		{ObjBody, "world", 0}, {ObjBody, "arm", 1}, {ObjBody, "tipbody", 2},
		{ObjJoint, "hinge", 0}, {ObjGeom, "rod", 1}, {ObjSite, "tip", 0},
		{ObjCamera, "cam", 0}, {ObjActuator, "act", 0}, {ObjJoint, "nope", -1},
	}
	for _, c := range cases {
		if got := m.Name2ID(c.kind, c.name); got != c.id {
			t.Fatalf("Name2ID(%v, %q) = %d, want %d", c.kind, c.name, got, c.id)
		}
		if c.id >= 0 {
			if got, ok := m.ID2Name(c.kind, c.id); !ok || got != c.name {
				t.Fatalf("ID2Name(%v, %d) = %q, %v", c.kind, c.id, got, ok)
			}
		}
	}
	if _, ok := m.ID2Name(ObjGeom, 99); ok {
		t.Fatal("ID2Name out of range gave a name")
	}
}

func TestModelFields(t *testing.T) {
	m := load(t, tiny)
	if m.JntType(0) != JntHinge || m.JntQposAdr(0) != 0 || !m.JntLimited(0) {
		t.Fatalf("joint 0: type %d adr %d limited %v", m.JntType(0), m.JntQposAdr(0), m.JntLimited(0))
	}
	if lo, hi := m.JntRange(0); lo != -1 || hi != 1 {
		t.Fatalf("JntRange(0) = %v, %v", lo, hi)
	}
	if m.ActuatorTrnID(0) != 0 || !m.ActuatorCtrlLimited(0) {
		t.Fatalf("actuator 0: trnid %d limited %v", m.ActuatorTrnID(0), m.ActuatorCtrlLimited(0))
	}
	if lo, hi := m.ActuatorCtrlRange(0); lo != -0.5 || hi != 0.5 {
		t.Fatalf("ActuatorCtrlRange(0) = %v, %v", lo, hi)
	}
}

func TestDataForwardStepReset(t *testing.T) {
	m := load(t, tiny)
	d := NewData(m)
	defer d.Close()
	if got := d.XPos(2); got != [3]float64{} {
		t.Fatalf("xpos before forward = %v", got)
	}
	Forward(m, d)
	if got := d.XPos(2); got != [3]float64{0.5, 0, 0} {
		t.Fatalf("tipbody xpos = %v", got)
	}
	if got := d.SiteXPos(0); got != [3]float64{0.5, 0, 0} {
		t.Fatalf("tip site xpos = %v", got)
	}
	d.Ctrl()[0] = 0.3
	for i := 0; i < 200; i++ {
		Step(m, d)
	}
	if d.QPos()[0] < 0.1 || d.QVel() == nil || len(d.ActuatorForce()) != 1 {
		t.Fatalf("after steps qpos = %v", d.QPos())
	}
	Reset(m, d)
	if d.QPos()[0] != 0 || d.Ctrl()[0] != 0 || d.QVel()[0] != 0 {
		t.Fatalf("after reset qpos %v ctrl %v", d.QPos(), d.Ctrl())
	}
}

func TestJacBody(t *testing.T) {
	m := load(t, tiny)
	d := NewData(m)
	defer d.Close()
	Forward(m, d)
	jacp := JacBody(m, d, 2)
	if len(jacp) != 3 || jacp[0] != 0 || jacp[1] != 0.5 || jacp[2] != 0 {
		t.Fatalf("jacp = %v", jacp)
	}
}

func TestContacts(t *testing.T) {
	m := load(t, touching)
	d := NewData(m)
	defer d.Close()
	Step(m, d)
	if d.NCon() == 0 {
		t.Fatal("no contact")
	}
	g1, g2 := d.ContactGeoms(0)
	if g1 != 0 || g2 != 1 {
		t.Fatalf("contact geoms = %d, %d", g1, g2)
	}
}

func TestRender(t *testing.T) {
	m := load(t, tiny)
	d := NewData(m)
	defer d.Close()
	Forward(m, d)
	r, err := NewRenderer(m, 64, 48)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	defer r.Close()
	a, err := r.Render(d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64*48*3 {
		t.Fatalf("len = %d", len(a))
	}
	if bytes.Count(a, a[:3]) == len(a)/3 {
		t.Fatal("the image is one color")
	}
	b, err := r.Render(d, 0)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("a second render differs (%v)", err)
	}
	free, err := r.Render(d, -1)
	if err != nil || bytes.Equal(a, free) {
		t.Fatalf("the free camera gave the fixed camera's image (%v)", err)
	}
	if _, err := r.Render(d, 1); err == nil {
		t.Fatal("camera 1 rendered")
	}
	if _, err := NewRenderer(m, 65, 48); err == nil {
		t.Fatal("a renderer larger than the offscreen buffer was made")
	}
}
