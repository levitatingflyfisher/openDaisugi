//go:build mujoco

package smolvla

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"daisugi-verify/internal/ort"
)

// Pins are the SHA-256 of each file of the model directory (ruling
// VL-R-2): the three graphs vla_export.py writes and the two assets it
// copies from the pinned checkpoint and backbone.
var Pins = map[string]string{
	"vision.onnx":       "eae4a86506b2f8e36f4b3efa16133964feadbc6323ecbb14c61838e080b06664",
	"prefix.onnx":       "ac391d4633dce409f050929adbe77c7a6af86dddfa0266aa4937e98b13ef3579",
	"denoise.onnx":      "f2412f7254659dcb9cf0d00dc181f05cca1299609ab1ec25042fe400ff4bda66",
	"tokenizer.json":    "5ece781dc8d2b2f3e2f289ca0ae50b17cfc27dd27bfe7971bb8241e0b964331a",
	"stats.safetensors": "490ab239d96e263687c0b2e386a0afbc235a2eceb9857c36ed32f2f162a3e7c8",
}

// ChunkTolerance is the largest difference allowed between an action of
// a port and the oracle's (ruling VL-R-6).
const ChunkTolerance = 1e-3

// The graphs' fixed shapes.
const (
	imgTokens = 64                    // the image embeddings per image
	embDim    = 960                   // the VLM's hidden size
	prefixLen = imgTokens + Width + 1 // image, text, state
	kvLayers  = 16                    // num_vlm_layers
	kvHeads   = 5                     // the key and value heads
	headDim   = 64                    // the head size
	kvSize    = kvLayers * kvHeads * prefixLen * headDim
)

// Policy is the three graphs and the two assets, loaded.
type Policy struct {
	tok                     *Tokenizer
	stats                   *Stats
	vision, prefix, denoise *ort.Session
}

// Open checks every file of dir against its pin, then loads them. threads
// is the intra-op thread count of each graph.
func Open(dir string, threads int) (*Policy, error) {
	names := make([]string, 0, len(Pins))
	for name := range Pins {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("smolvla: %w", err)
		}
		if got != Pins[name] {
			return nil, fmt.Errorf("smolvla: %s has sha256 %s, not the pinned %s", name, got, Pins[name])
		}
	}
	p := &Policy{}
	var err error
	if p.tok, err = LoadTokenizer(filepath.Join(dir, "tokenizer.json")); err != nil {
		return nil, err
	}
	if p.stats, err = LoadStats(filepath.Join(dir, "stats.safetensors")); err != nil {
		return nil, err
	}
	for _, g := range []struct {
		name string
		s    **ort.Session
	}{{"vision.onnx", &p.vision}, {"prefix.onnx", &p.prefix}, {"denoise.onnx", &p.denoise}} {
		if *g.s, err = ort.Open(filepath.Join(dir, g.name), threads); err != nil {
			p.Close()
			return nil, fmt.Errorf("smolvla: %s: %w", g.name, err)
		}
	}
	return p, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Close frees the graphs.
func (p *Policy) Close() {
	p.vision.Close()
	p.prefix.Close()
	p.denoise.Close()
}

// Chunk is one action chunk: the first dim values of each of the 50
// actions, for the camera image rgb (h x w RGB bytes, top row first), the
// task, the robot state and the noise [50*32].
func (p *Policy) Chunk(rgb []byte, h, w int, task string, state []float64, noise []float32, dim int) ([][]float64, error) {
	if len(noise) != Chunk*ActionDim {
		return nil, fmt.Errorf("smolvla: noise of %d values, not %d", len(noise), Chunk*ActionDim)
	}
	if dim < 1 || dim > ActionDim {
		return nil, fmt.Errorf("smolvla: %d action values is out of range", dim)
	}
	img, err := Image(rgb, h, w)
	if err != nil {
		return nil, err
	}
	ids, mask, err := p.tok.Task(task)
	if err != nil {
		return nil, err
	}
	st, err := p.stats.State(state)
	if err != nil {
		return nil, err
	}
	emb := make([]float32, imgTokens*embDim)
	if err := p.vision.Run(
		[]ort.Input{{Name: "image", F32: img, Shape: []int64{1, 3, ImageSize, ImageSize}}},
		[]ort.Output{{Name: "img_emb", Data: emb}},
	); err != nil {
		return nil, err
	}
	keys, values := make([]float32, kvSize), make([]float32, kvSize)
	if err := p.prefix.Run(
		[]ort.Input{
			{Name: "img_emb", F32: emb, Shape: []int64{1, imgTokens, embDim}},
			{Name: "lang_tokens", I64: ids[:], Shape: []int64{1, Width}},
			{Name: "lang_mask", I64: mask[:], Shape: []int64{1, Width}},
			{Name: "state", F32: st[:], Shape: []int64{1, StateDim}},
		},
		[]ort.Output{{Name: "kv_keys", Data: keys}, {Name: "kv_values", Data: values}},
	); err != nil {
		return nil, err
	}
	pmask := make([]int64, 0, prefixLen)
	for i := 0; i < imgTokens; i++ {
		pmask = append(pmask, 1)
	}
	pmask = append(append(pmask, mask[:]...), 1)
	kvShape := []int64{kvLayers, 1, kvHeads, prefixLen, headDim}
	x := append([]float32(nil), noise...)
	v := make([]float32, Chunk*ActionDim)
	dt := float32(-1.0 / Steps)
	for step := 0; step < Steps; step++ {
		if err := p.denoise.Run(
			[]ort.Input{
				{Name: "x_t", F32: x, Shape: []int64{1, Chunk, ActionDim}},
				{Name: "time_emb", F32: TimeEmbedding(StepTime(step)), Shape: []int64{1, TimeDim}},
				{Name: "kv_keys", F32: keys, Shape: kvShape},
				{Name: "kv_values", F32: values, Shape: kvShape},
				{Name: "prefix_mask", I64: pmask, Shape: []int64{1, prefixLen}},
			},
			[]ort.Output{{Name: "v_t", Data: v}},
		); err != nil {
			return nil, err
		}
		for i := range x {
			// The conversion keeps the product rounded, so no target fuses
			// it into a multiply-add: x + dt*v as torch computes it.
			x[i] += float32(dt * v[i])
		}
	}
	return p.stats.Actions(x, dim), nil
}
