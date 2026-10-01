package smolvla

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type processCases struct {
	Samples [][3]int `json:"samples"`
	Images  []struct {
		Height  int       `json:"height"`
		Width   int       `json:"width"`
		Sum     float64   `json:"sum"`
		Samples []float64 `json:"samples"`
	} `json:"images"`
	Times []struct {
		Time      float64   `json:"time"`
		Embedding []float64 `json:"embedding"`
	} `json:"times"`
	Noise []struct {
		Seed uint64    `json:"seed"`
		Sum  float64   `json:"sum"`
		Head []float64 `json:"head"`
	} `json:"noise"`
}

func loadProcess(t *testing.T) processCases {
	t.Helper()
	b, err := os.ReadFile("../../../fixtures/vla/process.json")
	if err != nil {
		t.Fatal(err)
	}
	var pc processCases
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	return pc
}

// testImage is vla_cases.test_image: a uint8 RGB pattern, top row first.
func testImage(h, w int) []byte {
	out := make([]byte, h*w*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < 3; c++ {
				out[(y*w+x)*3+c] = byte((x*7 + y*13 + c*29 + x*y) % 256)
			}
		}
	}
	return out
}

func TestImageMatchesTheOracle(t *testing.T) {
	pc := loadProcess(t)
	for _, im := range pc.Images {
		got, err := Image(testImage(im.Height, im.Width), im.Height, im.Width)
		if err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for _, v := range got {
			sum += float64(v)
		}
		if math.Abs(sum-im.Sum) > 1e-3 {
			t.Errorf("%dx%d: sum %v, want %v", im.Height, im.Width, sum, im.Sum)
		}
		for i, s := range pc.Samples {
			v := got[(s[0]*ImageSize+s[1])*ImageSize+s[2]]
			if math.Abs(float64(v)-im.Samples[i]) > 1e-6 {
				t.Fatalf("%dx%d at %v: %v, want %v", im.Height, im.Width, s, v, im.Samples[i])
			}
		}
	}
	if _, err := Image(make([]byte, 5), 2, 2); err == nil {
		t.Error("a short image is not an error")
	}
}

func TestTimeEmbeddingMatchesTheOracle(t *testing.T) {
	pc := loadProcess(t)
	if len(pc.Times) != Steps {
		t.Fatalf("%d times", len(pc.Times))
	}
	for step, tc := range pc.Times {
		tm := StepTime(step)
		if float64(tm) != tc.Time {
			t.Errorf("step %d: time %v, want %v", step, tm, tc.Time)
		}
		emb := TimeEmbedding(tm)
		for i, v := range emb {
			if math.Abs(float64(v)-tc.Embedding[i]) > 1e-6 {
				t.Fatalf("step %d [%d]: %v, want %v", step, i, v, tc.Embedding[i])
			}
		}
	}
}

func TestNoiseMatchesTheOracle(t *testing.T) {
	for _, nc := range loadProcess(t).Noise {
		n := Noise(nc.Seed)
		sum := 0.0
		for _, v := range n {
			sum += float64(v)
		}
		if math.Abs(sum-nc.Sum) > 1e-3 {
			t.Errorf("seed %d: sum %v, want %v", nc.Seed, sum, nc.Sum)
		}
		for i, v := range nc.Head {
			if math.Abs(float64(n[i])-v) > 1e-6 {
				t.Fatalf("seed %d [%d]: %v, want %v", nc.Seed, i, n[i], v)
			}
		}
	}
}

// safetensors writes a file of F32 tensors.
func safetensors(t *testing.T, tensors map[string][]float32) []byte {
	t.Helper()
	header := map[string]any{}
	var data []byte
	for _, name := range []string{"observation.state.mean", "observation.state.std", "action.mean", "action.std"} {
		v, ok := tensors[name]
		if !ok {
			continue
		}
		start := len(data)
		for _, f := range v {
			data = binary.LittleEndian.AppendUint32(data, math.Float32bits(f))
		}
		header[name] = map[string]any{"dtype": "F32", "shape": []int{len(v)}, "data_offsets": []int{start, len(data)}}
	}
	h, _ := json.Marshal(header)
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(h)))
	return append(append(out, h...), data...)
}

func TestStatsNormalizeWhenPresentAndPassThroughWhenMissing(t *testing.T) {
	st, err := ParseStats(safetensors(t, map[string][]float32{
		"observation.state.mean": {1, 2}, "observation.state.std": {2, 4},
		"action.mean": {10, 20}, "action.std": {0.5, 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.State([]float64{3, 2})
	if err != nil {
		t.Fatal(err)
	}
	if s[0] != float32(2)/float32(2+1e-8) || s[1] != 0 || s[2] != 0 {
		t.Errorf("state %v", s[:3])
	}
	if _, err := st.State([]float64{1, 2, 3}); err == nil {
		t.Error("a state of the wrong length is not an error")
	}
	chunk := make([]float32, Chunk*ActionDim)
	chunk[0], chunk[1], chunk[ActionDim] = 2, 1, -2
	a := st.Actions(chunk, 2)
	if len(a) != Chunk || a[0][0] != 11 || a[0][1] != 22 || a[1][0] != 9 {
		t.Errorf("actions %v %v", a[0], a[1])
	}
	none, err := ParseStats(safetensors(t, map[string][]float32{}))
	if err != nil {
		t.Fatal(err)
	}
	s, _ = none.State([]float64{3, 2, 1})
	if s[0] != 3 || s[2] != 1 {
		t.Errorf("identity state %v", s[:3])
	}
	if a := none.Actions(chunk, 3); a[0][0] != 2 || a[0][2] != 0 {
		t.Errorf("identity actions %v", a[0])
	}
}

func TestBaseCheckpointStatsAreIdentity(t *testing.T) {
	st, err := LoadStats(filepath.Join(modelDir(t), "stats.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := st.State([]float64{0.5, -0.25})
	if s[0] != 0.5 || s[1] != -0.25 {
		t.Errorf("state %v", s[:2])
	}
}

// raw is a safetensors file with header h and n zero bytes of data.
func raw(h string, n int) []byte {
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(h)))
	return append(append(out, h...), make([]byte, n)...)
}

func TestBadStatsFilesAreErrors(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{1, 0, 0, 0, 0, 0, 0, 0, '{'},
		append(binary.LittleEndian.AppendUint64(nil, 2), "{}"...)[:9],
		raw(`{"a":{"dtype":"F64","shape":[1],"data_offsets":[0,8]}}`, 8),
		raw(`{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,8]}}`, 4),
		raw(`{"a":{"dtype":"F32","shape":[3],"data_offsets":[0,8]}}`, 8),
		raw(`{"a":{"dtype":"F32","shape":[2],"data_offsets":[4,0]}}`, 8),
	} {
		if _, err := ParseStats(b); err == nil {
			t.Errorf("%q: no error", b)
		}
	}
}
