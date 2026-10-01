package voice

import (
	"bytes"
	"encoding/binary"
	"math"
)

// pcmGUID is KSDATAFORMAT_SUBTYPE_PCM, the subformat of a
// WAVE_FORMAT_EXTENSIBLE file that Python's wave module reads.
var pcmGUID = []byte("\x01\x00\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71")

// AudioError is a clip that cannot be normalized. Kind is the Python
// exception the oracle raises for it: "unsupported"
// (AudioFormatUnsupported), "wave" (wave.Error), "value" (ValueError),
// "runtime" (RuntimeError), "eof" (EOFError), "zerodiv"
// (ZeroDivisionError) or "struct" (struct.error).
type AudioError struct {
	Kind string
	Msg  string
}

func (e *AudioError) Error() string { return e.Msg }

// BadAudio reports whether the server answers this failure 400 bad_audio.
// Any other failure is a 500, as the oracle's handler gives it.
func (e *AudioError) BadAudio() bool {
	switch e.Kind {
	case "unsupported", "wave", "value", "runtime":
		return true
	}
	return false
}

func audioErr(kind, msg string) *AudioError { return &AudioError{Kind: kind, Msg: msg} }

// reader is a byte stream as Python's wave module reads one: a BytesIO, or
// a chunk inside it.
type reader interface {
	read(n int64) []byte
	seek(pos int64, whence int) error
	tell() int64
}

type pyFile struct {
	b   []byte
	pos int64
}

func (f *pyFile) read(n int64) []byte {
	if f.pos >= int64(len(f.b)) {
		return nil
	}
	end := int64(len(f.b))
	if n >= 0 && f.pos+n < end {
		end = f.pos + n
	}
	out := f.b[f.pos:end]
	f.pos = end
	return out
}

func (f *pyFile) seek(pos int64, whence int) error {
	if whence == 1 {
		pos += f.pos
	}
	f.pos = pos
	return nil
}

func (f *pyFile) tell() int64 { return f.pos }

// chunk is wave._Chunk, little-endian and word-aligned.
type chunk struct {
	f        reader
	name     []byte
	size     int64
	sizeRead int64
	offset   int64
}

func newChunk(f reader) (*chunk, *AudioError) {
	name := f.read(4)
	if len(name) < 4 {
		return nil, audioErr("eof", "EOFError")
	}
	sz := f.read(4)
	if len(sz) < 4 {
		return nil, audioErr("eof", "EOFError")
	}
	return &chunk{f: f, name: append([]byte(nil), name...), size: int64(binary.LittleEndian.Uint32(sz)), offset: f.tell()}, nil
}

func (c *chunk) read(n int64) []byte {
	if c.sizeRead >= c.size {
		return nil
	}
	if n < 0 || n > c.size-c.sizeRead {
		n = c.size - c.sizeRead
	}
	data := c.f.read(n)
	c.sizeRead += int64(len(data))
	if c.sizeRead == c.size && c.size&1 == 1 {
		c.sizeRead += int64(len(c.f.read(1)))
	}
	return data
}

func (c *chunk) seek(pos int64, whence int) error {
	switch whence {
	case 1:
		pos += c.sizeRead
	case 2:
		pos += c.size
	}
	if pos < 0 || pos > c.size {
		return audioErr("runtime", "RuntimeError")
	}
	if err := c.f.seek(c.offset+pos, 0); err != nil {
		return err
	}
	c.sizeRead = pos
	return nil
}

func (c *chunk) tell() int64 { return c.sizeRead }

func (c *chunk) skip() *AudioError {
	n := c.size - c.sizeRead
	if c.size&1 == 1 {
		n++
	}
	if err := c.f.seek(n, 1); err != nil {
		return err.(*AudioError)
	}
	c.sizeRead += n
	return nil
}

// waveRead is what wave.open(io.BytesIO(raw)) reads: the format and the
// data chunk.
type waveRead struct {
	channels  int64
	rate      int64
	sampwidth int64
	framesize int64
	nframes   int64
	data      *chunk
}

func openWave(raw []byte) (*waveRead, *AudioError) {
	outer, err := newChunk(&pyFile{b: raw})
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(outer.name, []byte("RIFF")) {
		return nil, audioErr("wave", "file does not start with RIFF id")
	}
	if !bytes.Equal(outer.read(4), []byte("WAVE")) {
		return nil, audioErr("wave", "not a WAVE file")
	}
	w := &waveRead{}
	fmtRead := false
	for {
		sub, err := newChunk(outer)
		if err != nil {
			break
		}
		switch string(sub.name) {
		case "fmt ":
			if err := w.readFmt(sub); err != nil {
				return nil, err
			}
			fmtRead = true
		case "data":
			if !fmtRead {
				return nil, audioErr("wave", "data chunk before fmt chunk")
			}
			w.data = sub
			w.nframes = sub.size / w.framesize
		}
		if w.data != nil {
			break
		}
		if err := sub.skip(); err != nil {
			return nil, err
		}
	}
	if !fmtRead || w.data == nil {
		return nil, audioErr("wave", "fmt chunk and/or data chunk missing")
	}
	return w, nil
}

func (w *waveRead) readFmt(c *chunk) *AudioError {
	b := c.read(14)
	if len(b) < 14 {
		return audioErr("eof", "EOFError")
	}
	tag := binary.LittleEndian.Uint16(b[0:])
	w.channels = int64(binary.LittleEndian.Uint16(b[2:]))
	w.rate = int64(binary.LittleEndian.Uint32(b[4:]))
	if tag != 1 && tag != 0xFFFE {
		return audioErr("wave", "unknown format")
	}
	b = c.read(2)
	if len(b) < 2 {
		return audioErr("eof", "EOFError")
	}
	bits := int64(binary.LittleEndian.Uint16(b))
	if tag == 0xFFFE {
		if len(c.read(8)) < 8 {
			return audioErr("eof", "EOFError")
		}
		sub := c.read(16)
		if len(sub) < 16 {
			return audioErr("eof", "EOFError")
		}
		if !bytes.Equal(sub, pcmGUID) {
			return audioErr("wave", "unknown extended format")
		}
	}
	w.sampwidth = (bits + 7) / 8
	if w.sampwidth == 0 {
		return audioErr("wave", "bad sample width")
	}
	if w.channels == 0 {
		return audioErr("wave", "bad # of channels")
	}
	w.framesize = w.channels * w.sampwidth
	return nil
}

// readFrames is the first readframes(n) after open: the file already sits
// at the start of the data, so no seek is made.
func (w *waveRead) readFrames(n int64) ([]byte, *AudioError) {
	if n == 0 {
		return nil, nil
	}
	return w.data.read(n * w.framesize), nil
}

// WavDurationS is audio.wav_duration_s: the frames actually present,
// over the frame rate.
func WavDurationS(raw []byte) (float64, *AudioError) {
	w, err := openWave(raw)
	if err != nil {
		return 0, err
	}
	frames, err := w.readFrames(w.nframes)
	if err != nil {
		return 0, err
	}
	n := int64(len(frames)) / (w.sampwidth * w.channels)
	if w.rate == 0 {
		return 0, audioErr("zerodiv", "float division by zero")
	}
	return float64(n) / float64(w.rate), nil
}

// readMonoSamples is audio._read_wav_mono_samples: 16-bit samples, the
// channels averaged.
func readMonoSamples(raw []byte) (int64, []float64, *AudioError) {
	w, err := openWave(raw)
	if err != nil {
		return 0, nil, err
	}
	frames, err := w.readFrames(w.nframes)
	if err != nil {
		return 0, nil, err
	}
	if w.sampwidth != 2 {
		return 0, nil, audioErr("unsupported", "unsupported sample width")
	}
	if len(frames)%2 != 0 {
		return 0, nil, audioErr("value", "buffer size must be a multiple of element size")
	}
	count := int64(len(frames) / 2)
	ints := make([]int64, count)
	for i := range ints {
		ints[i] = int64(int16(binary.LittleEndian.Uint16(frames[2*i:])))
	}
	if w.channels <= 1 {
		out := make([]float64, count)
		for i, v := range ints {
			out[i] = float64(v)
		}
		return w.rate, out, nil
	}
	if count%w.channels != 0 {
		return 0, nil, audioErr("value", "cannot reshape array")
	}
	rows := count / w.channels
	out := make([]float64, rows)
	for r := int64(0); r < rows; r++ {
		var sum int64
		for c := int64(0); c < w.channels; c++ {
			sum += ints[r*w.channels+c]
		}
		// The sum of int16 values is exact in a float64, whatever order
		// numpy adds them in; the mean is one rounded division.
		out[r] = float64(sum) / float64(w.channels)
	}
	return w.rate, out, nil
}

// writeWavMono is audio.write_wav_16k_mono: the samples clipped and cut
// to int16, in a 44-byte header PCM WAV at the given rate.
func writeWavMono(samples []float64, rate int64) ([]byte, *AudioError) {
	if rate <= 0 {
		return nil, audioErr("wave", "bad frame rate")
	}
	if rate*2 > math.MaxUint32 {
		return nil, audioErr("struct", "argument out of range")
	}
	data := make([]byte, 2*len(samples))
	for i, s := range samples {
		if s < -32768 {
			s = -32768
		} else if s > 32767 {
			s = 32767
		}
		binary.LittleEndian.PutUint16(data[2*i:], uint16(int16(math.Trunc(s))))
	}
	return writeWavPCM(data, rate)
}

// writeWavPCM is a mono 16-bit wave writer at rate given these raw frame
// bytes: the header's sizes count every byte written, an odd one too.
func writeWavPCM(data []byte, rate int64) ([]byte, *AudioError) {
	if int64(len(data))+36 > math.MaxUint32 {
		return nil, audioErr("struct", "argument out of range")
	}
	var h bytes.Buffer
	h.WriteString("RIFF")
	binary.Write(&h, binary.LittleEndian, uint32(36+len(data)))
	h.WriteString("WAVEfmt ")
	binary.Write(&h, binary.LittleEndian, uint32(16))
	binary.Write(&h, binary.LittleEndian, uint16(1))
	binary.Write(&h, binary.LittleEndian, uint16(1))
	binary.Write(&h, binary.LittleEndian, uint32(rate))
	binary.Write(&h, binary.LittleEndian, uint32(rate*2))
	binary.Write(&h, binary.LittleEndian, uint16(2))
	binary.Write(&h, binary.LittleEndian, uint16(16))
	h.WriteString("data")
	binary.Write(&h, binary.LittleEndian, uint32(len(data)))
	h.Write(data)
	return h.Bytes(), nil
}

// resampleLinear is audio._resample_linear: numpy.interp of the samples
// at the target rate's times, with no anti-aliasing.
func resampleLinear(samples []float64, orig, target int64) ([]float64, *AudioError) {
	if orig == target {
		return samples, nil
	}
	if orig == 0 {
		return nil, audioErr("zerodiv", "division by zero")
	}
	duration := float64(len(samples)) / float64(orig)
	nTarget := int64(math.RoundToEven(float64(duration * float64(target))))
	if nTarget < 1 {
		nTarget = 1
	}
	n := len(samples)
	if n == 0 {
		return nil, audioErr("value", "array of sample points is empty")
	}
	xp := make([]float64, n)
	for i := range xp {
		xp[i] = float64(i) / float64(orig)
	}
	out := make([]float64, nTarget)
	if n == 1 {
		for i := range out {
			out[i] = samples[0]
		}
		return out, nil
	}
	slopes := make([]float64, n-1)
	for i := range slopes {
		slopes[i] = (samples[i+1] - samples[i]) / (xp[i+1] - xp[i])
	}
	j := 0
	for i := range out {
		x := float64(i) / float64(target)
		// The largest j with xp[j] <= x; x is never below xp[0] = 0.
		for j+1 < n && xp[j+1] <= x {
			j++
		}
		switch {
		case x > xp[n-1]:
			out[i] = samples[n-1]
		case j == n-1:
			out[i] = samples[j]
		case xp[j] == x:
			out[i] = samples[j]
		default:
			slope := slopes[j]
			v := float64(slope*(x-xp[j])) + samples[j]
			if math.IsNaN(v) {
				v = float64(slope*(x-xp[j+1])) + samples[j+1]
				if math.IsNaN(v) && samples[j] == samples[j+1] {
					v = samples[j]
				}
			}
			out[i] = v
		}
	}
	return out, nil
}

// isWav is audio._is_wav.
func isWav(raw []byte) bool {
	return len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WAVE"
}
