//go:build mujoco

package robotics

import (
	"daisugi-verify/internal/mujoco"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/smolvla"
)

// The camera TransformersVLAExecutor._capture_image renders: the default
// free camera, 320 wide and 240 high.
const (
	smolvlaCamWidth  = 320
	smolvlaCamHeight = 240
	// smolvlaActions is the checkpoint's action feature: 6 values.
	smolvlaActions = 6
)

// SmolVLA is TransformersVLAExecutor for lerobot/smolvla_base, with the
// policy run natively from its ONNX graphs: the camera image, the qpos as
// the state, and the first rows of the chunk mapped onto the MJCF's
// joints in order, capped at the step's max_actions. Chunk k draws the
// noise of seed Seed+k.
type SmolVLA struct {
	Policy *smolvla.Policy
	Seed   uint64
	// Chunks counts the chunks run; LastChunk is the last one (all 6
	// action values of each of 50 actions) and LastImage the camera image
	// it saw.
	Chunks    int
	LastChunk [][]float64
	LastImage []byte
	renderer  *mujoco.Renderer
}

// NewSmolVLA loads the MJCF and the pinned model in modelDir.
func NewSmolVLA(mjcfPath, modelDir string, threads int, seed uint64) (*VLA, *SmolVLA, error) {
	p, err := smolvla.Open(modelDir, threads)
	if err != nil {
		return nil, nil, err
	}
	s := &SmolVLA{Policy: p, Seed: seed}
	v, err := NewVLABase(mjcfPath, 200, s.predict)
	if err != nil {
		p.Close()
		return nil, nil, err
	}
	return v, s, nil
}

// Close frees the policy and the renderer.
func (s *SmolVLA) Close() {
	if s.renderer != nil {
		s.renderer.Close()
	}
	s.Policy.Close()
}

func (s *SmolVLA) predict(v *VLA, step *pyjson.Object, obs *pyjson.Object) ([]Action, error) {
	if s.renderer == nil {
		r, err := mujoco.NewRenderer(v.model, smolvlaCamWidth, smolvlaCamHeight)
		if err != nil {
			return nil, &PyError{Type: "RuntimeError", Msg: err.Error()}
		}
		s.renderer = r
	}
	rgb, err := s.renderer.Render(v.data, -1)
	if err != nil {
		return nil, &PyError{Type: "RuntimeError", Msg: err.Error()}
	}
	state := floatsOf(obs.Value("qpos"))
	chunk, err := s.Policy.Chunk(rgb, smolvlaCamHeight, smolvlaCamWidth, strOf(step, "task"), state,
		smolvla.Noise(s.Seed+uint64(s.Chunks)), smolvlaActions)
	if err != nil {
		return nil, &PyError{Type: "RuntimeError", Msg: err.Error()}
	}
	s.Chunks++
	s.LastChunk, s.LastImage = chunk, rgb
	keys := v.jointNames
	if len(keys) > smolvlaActions {
		keys = keys[:smolvlaActions]
	}
	n := intOf(step.Value("max_actions"))
	if n > len(chunk) {
		n = len(chunk)
	}
	var out []Action
	for t := 0; t < n; t++ {
		out = append(out, Action{Joints: keys, Targets: chunk[t][:len(keys)]})
	}
	return out, nil
}
