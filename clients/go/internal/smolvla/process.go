package smolvla

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
)

// The shapes and constants of the checkpoint (ruling VL-R-3).
const (
	ImageSize = 512   // resize_imgs_with_padding
	StateDim  = 32    // max_state_dim
	ActionDim = 32    // max_action_dim
	Chunk     = 50    // chunk_size
	Steps     = 10    // num_steps
	TimeDim   = 720   // the action expert's hidden size
	minPeriod = 0.004 // min_period
	maxPeriod = 4.0   // max_period
	eps       = 1e-8  // the normalizer's eps
)

// Image is the camera image as the vision graph takes it: rgb is h x w
// RGB bytes, top row first; the result is [3,512,512] in [0,1], resized
// bilinearly (align_corners=False, no antialias) to fit with its aspect
// kept and padded with 0 on the left and top, as lerobot's
// resize_with_pad does.
func Image(rgb []byte, h, w int) ([]float32, error) {
	if h <= 0 || w <= 0 || len(rgb) != h*w*3 {
		return nil, fmt.Errorf("smolvla: an image of %d bytes is not %dx%d RGB", len(rgb), h, w)
	}
	ratio := math.Max(float64(w)/ImageSize, float64(h)/ImageSize)
	rh, rw := int(float64(h)/ratio), int(float64(w)/ratio)
	if h == ImageSize && w == ImageSize {
		rh, rw = h, w
	}
	padH, padW := ImageSize-rh, ImageSize-rw
	ys, xs := axis(h, rh), axis(w, rw)
	out := make([]float32, 3*ImageSize*ImageSize)
	px := func(c, y, x int) float32 { return float32(rgb[(y*w+x)*3+c]) / 255 }
	for c := 0; c < 3; c++ {
		for oy, ay := range ys {
			row := out[(c*ImageSize+padH+oy)*ImageSize+padW:]
			for ox, ax := range xs {
				top := ax.l0*px(c, ay.i0, ax.i0) + ax.l1*px(c, ay.i0, ax.i1)
				bottom := ax.l0*px(c, ay.i1, ax.i0) + ax.l1*px(c, ay.i1, ax.i1)
				row[ox] = ay.l0*top + ay.l1*bottom
			}
		}
	}
	return out, nil
}

// tap is one output index of PyTorch's bilinear upsampling along one
// axis: the two source indexes and their weights.
type tap struct {
	i0, i1 int
	l0, l1 float32
}

// axis is upsample_bilinear2d's source index computation for one axis,
// with align_corners=False and no scale given: scale = in/out, src =
// scale*(o+0.5)-0.5 clamped at 0, in float32.
func axis(in, out int) []tap {
	taps := make([]tap, out)
	scale := float32(in) / float32(out)
	for o := range taps {
		src := scale*(float32(o)+0.5) - 0.5
		if src < 0 {
			src = 0
		}
		i0 := int(src)
		i1 := i0
		if i0 < in-1 {
			i1 = i0 + 1
		}
		l1 := src - float32(i0)
		taps[o] = tap{i0, i1, 1 - l1, l1}
	}
	return taps
}

// StepTime is the time of Euler step step: 1 + step*dt with dt = -1/10,
// in float64, then float32, as lerobot's euler_integrate computes it.
func StepTime(step int) float32 {
	return float32(1.0 + float64(step)*(-1.0/Steps))
}

// TimeEmbedding is create_sinusoidal_pos_embedding for one time: the sine
// then the cosine of t / period over TimeDim/2 periods from 0.004 to 4,
// in float64 and cast to float32. The fraction follows torch.linspace,
// which counts the second half down from the end.
func TimeEmbedding(t float32) []float32 {
	const half = TimeDim / 2
	out := make([]float32, TimeDim)
	step := 1.0 / float64(half-1)
	for i := 0; i < half; i++ {
		frac := float64(i) * step
		if i >= half/2 {
			frac = 1.0 - float64(half-1-i)*step
		}
		period := minPeriod * math.Pow(maxPeriod/minPeriod, frac)
		x := (1.0 / period * 2 * math.Pi) * float64(t)
		out[i] = float32(math.Sin(x))
		out[half+i] = float32(math.Cos(x))
	}
	return out
}

// Noise is the flow-matching noise [1,50,32] for a seed: splitmix64, and
// Box-Muller in float64 on each pair of draws, cast to float32. The
// Python cases (clients/vla_cases.py) and the Rust port draw the same.
func Noise(seed uint64) []float32 {
	out := make([]float32, 0, Chunk*ActionDim)
	state := seed
	next := func() uint64 {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	for len(out) < Chunk*ActionDim {
		u1 := float64((next()>>11)+1) * 0x1p-53
		u2 := float64(next()>>11) * 0x1p-53
		r := math.Sqrt(-2 * math.Log(u1))
		out = append(out, float32(r*math.Cos(2*math.Pi*u2)), float32(r*math.Sin(2*math.Pi*u2)))
	}
	return out
}

// Stats are the checkpoint's normalizer stats, by key ("action.mean").
type Stats struct {
	m map[string][]float32
}

// LoadStats reads stats.safetensors.
func LoadStats(path string) (*Stats, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseStats(b)
}

// ParseStats parses a safetensors file of F32 tensors.
func ParseStats(b []byte) (*Stats, error) {
	if len(b) < 8 {
		return nil, errors.New("stats: shorter than a safetensors header")
	}
	n := binary.LittleEndian.Uint64(b)
	if n > uint64(len(b)-8) {
		return nil, errors.New("stats: the header runs past the end")
	}
	var h map[string]json.RawMessage
	if err := json.Unmarshal(b[8:8+n], &h); err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	data := b[8+n:]
	st := &Stats{m: map[string][]float32{}}
	for name, raw := range h {
		if name == "__metadata__" {
			continue
		}
		var t struct {
			Dtype   string   `json:"dtype"`
			Shape   []int    `json:"shape"`
			Offsets []uint64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("stats: %s: %w", name, err)
		}
		if t.Dtype != "F32" {
			return nil, fmt.Errorf("stats: %s is %s, not F32", name, t.Dtype)
		}
		count := 1
		for _, d := range t.Shape {
			if d < 0 || d > len(data) {
				return nil, fmt.Errorf("stats: %s has a bad shape", name)
			}
			count *= d
		}
		if len(t.Offsets) != 2 || t.Offsets[0] > t.Offsets[1] || t.Offsets[1] > uint64(len(data)) ||
			t.Offsets[1]-t.Offsets[0] != uint64(count*4) {
			return nil, fmt.Errorf("stats: %s has bad data offsets", name)
		}
		v := make([]float32, count)
		for i := range v {
			v[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[t.Offsets[0]+uint64(i*4):]))
		}
		st.m[name] = v
	}
	return st, nil
}

// pair is the mean and std of a feature, or nil when the stats lack
// either: lerobot then leaves the feature as it is.
func (s *Stats) pair(feature string) (mean, std []float32) {
	mean, std = s.m[feature+".mean"], s.m[feature+".std"]
	if mean == nil || std == nil || len(mean) != len(std) {
		return nil, nil
	}
	return mean, std
}

// State is the prefix graph's state input: the robot state normalized by
// the observation.state stats ((x - mean) / (std + 1e-8), in float32),
// then padded with zeros to StateDim.
func (s *Stats) State(x []float64) ([StateDim]float32, error) {
	var out [StateDim]float32
	if len(x) > StateDim {
		return out, fmt.Errorf("smolvla: a state of %d values is longer than %d", len(x), StateDim)
	}
	mean, std := s.pair("observation.state")
	if mean != nil && len(mean) != len(x) {
		return out, fmt.Errorf("smolvla: a state of %d values, but the stats have %d", len(x), len(mean))
	}
	for i, v := range x {
		out[i] = float32(v)
		if mean != nil {
			out[i] = (out[i] - mean[i]) / (std[i] + eps)
		}
	}
	return out, nil
}

// Actions is the chunk [50,32] cut to its first dim values per row and
// unnormalized by the action stats (x * std + mean, in float32).
func (s *Stats) Actions(chunk []float32, dim int) [][]float64 {
	mean, std := s.pair("action")
	out := make([][]float64, Chunk)
	for r := range out {
		out[r] = make([]float64, dim)
		for i := 0; i < dim; i++ {
			v := chunk[r*ActionDim+i]
			if mean != nil && i < len(mean) {
				v = v*std[i] + mean[i]
			}
			out[r][i] = float64(v)
		}
	}
	return out
}
