//go:build mujoco

// Command vla-probe is a test instrument for clients/vla_compare.py: the
// SmolVLA executor on the closed-loop case of clients/vla_cases.py. It
// runs the case's steps in its MJCF (a path relative to the working
// directory) and writes one JSON object: for each run, the chunk, the
// camera image (zlib then base64) and its SHA-256, the qpos after the run
// and the run's time in milliseconds. With --replay it runs no simulation:
// it feeds each recorded run's own state and image to the policy, with the
// same noise seed, and writes the chunks and their times. It is not
// shipped.
//
//	vla-probe [--replay] CASE.json MODEL_DIR
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/robotics"
	"daisugi-verify/internal/smolvla"
)

type loopCase struct {
	MJCF       string `json:"mjcf"`
	Task       string `json:"task"`
	Seed       uint64 `json:"seed"`
	Steps      int    `json:"steps"`
	MaxActions int    `json:"max_actions"`
	Threads    int    `json:"threads"`
	Records    []struct {
		State     []float64 `json:"state"`
		ImageZlib string    `json:"image_zlib"`
	} `json:"records"`
}

// The camera image of the case: 320 wide, 240 high.
const camWidth, camHeight = 320, 240

type record struct {
	ImageZlib   string      `json:"image_zlib,omitempty"`
	ImageSHA256 string      `json:"image_sha256,omitempty"`
	Chunk       [][]float64 `json:"chunk"`
	QPos        []float64   `json:"qpos,omitempty"`
	Ms          int64       `json:"ms"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "vla-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	replay := len(args) == 3 && args[0] == "--replay"
	if replay {
		args = args[1:]
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: vla-probe [--replay] CASE.json MODEL_DIR")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var c loopCase
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	if replay {
		return replayRuns(c, args[1])
	}
	v, s, err := robotics.NewSmolVLA(c.MJCF, args[1], c.Threads, c.Seed)
	if err != nil {
		return err
	}
	defer v.Close()
	defer s.Close()
	text, _ := json.Marshal(map[string]any{
		"source": "vla-probe", "task": c.Task,
		"steps": []any{map[string]any{"type": "vla", "id": "v", "task": c.Task, "max_actions": c.MaxActions, "timeout_s": 3600}},
	})
	raw, err := pyjson.Loads(string(text))
	if err != nil {
		return err
	}
	plan, verr := pmodel.Validate("ActionPlan", pmodel.ActionPlan, raw, pmodel.Python)
	if verr != nil {
		return fmt.Errorf("the step does not validate: %v", verr)
	}
	step := plan.(*pyjson.Object).Value("steps").([]any)[0].(*pyjson.Object)
	var out []record
	for k := 0; k < c.Steps; k++ {
		t := time.Now()
		r, err := v.Run(step, 3600, 1<<20)
		ms := time.Since(t).Milliseconds()
		if err != nil {
			return err
		}
		if r.RC != 0 {
			return fmt.Errorf("run %d: rc %d: %s", k, r.RC, r.Stdout)
		}
		h := sha256.Sum256(s.LastImage)
		var z bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
		zw.Write(s.LastImage)
		zw.Close()
		out = append(out, record{base64.StdEncoding.EncodeToString(z.Bytes()), hex.EncodeToString(h[:]),
			s.LastChunk, append([]float64(nil), v.Data().QPos()...), ms})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"records": out})
}

// replayRuns feeds each recorded run's state and image to the policy.
func replayRuns(c loopCase, modelDir string) error {
	p, err := smolvla.Open(modelDir, c.Threads)
	if err != nil {
		return err
	}
	defer p.Close()
	var out []record
	for k, r := range c.Records {
		z, err := base64.StdEncoding.DecodeString(r.ImageZlib)
		if err != nil {
			return fmt.Errorf("run %d: %w", k, err)
		}
		zr, err := zlib.NewReader(bytes.NewReader(z))
		if err != nil {
			return fmt.Errorf("run %d: %w", k, err)
		}
		var rgb bytes.Buffer
		if _, err := rgb.ReadFrom(zr); err != nil {
			return fmt.Errorf("run %d: %w", k, err)
		}
		t := time.Now()
		chunk, err := p.Chunk(rgb.Bytes(), camHeight, camWidth, c.Task, r.State, smolvla.Noise(c.Seed+uint64(k)), 6)
		if err != nil {
			return fmt.Errorf("run %d: %w", k, err)
		}
		out = append(out, record{Chunk: chunk, Ms: time.Since(t).Milliseconds()})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"records": out})
}
