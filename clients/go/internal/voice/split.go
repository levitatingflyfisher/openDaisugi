package voice

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"

	"daisugi-verify/internal/pystr"
)

// Long clips. The owner dictates long prompts, so the server takes a clip up
// to MaxAudioSeconds and cuts it into windows the engine can take in one
// call. Each window is at most WindowSeconds long. A cut falls at the
// quietest 20 ms frame in the last CutSearchSeconds of a window, so it lands
// in a pause and not in a word. The windows do not overlap.
const (
	WindowSeconds    = 30.0
	CutSearchSeconds = 3.0
	cutFrameSamples  = 320 // 20 ms at 16 kHz
)

// SplitWav cuts a 16 kHz mono WAV into windows of at most WindowSeconds. A
// clip that fits in one window comes back as it is, the same bytes.
func SplitWav(wav []byte) ([][]byte, *AudioError) {
	w, err := openWave(wav)
	if err != nil {
		return nil, err
	}
	frames, err := w.readFrames(w.nframes)
	if err != nil {
		return nil, err
	}
	if w.sampwidth != 2 || w.channels != 1 || w.rate <= 0 {
		return [][]byte{wav}, nil
	}
	total := len(frames) / 2
	window := int(WindowSeconds * float64(w.rate))
	if total <= window {
		return [][]byte{wav}, nil
	}
	search := int(CutSearchSeconds * float64(w.rate))
	var out [][]byte
	pos := 0
	for total-pos > window {
		cut := quietestCut(frames, pos+window-search, pos+window)
		piece, perr := writeWavPCM(frames[2*pos:2*cut], w.rate)
		if perr != nil {
			return nil, perr
		}
		out = append(out, piece)
		pos = cut
	}
	piece, perr := writeWavPCM(frames[2*pos:], w.rate)
	if perr != nil {
		return nil, perr
	}
	return append(out, piece), nil
}

// TruncateWav keeps the first maxSeconds of a 16 kHz mono WAV. It returns
// the clip and the whole seconds cut from its end, rounded up, so the reply
// can say how much was left out. A clip that fits comes back as it is, with
// 0 cut. Frames are counted as integers, so every port cuts at one sample.
func TruncateWav(wav []byte, maxSeconds float64) ([]byte, int64, *AudioError) {
	w, err := openWave(wav)
	if err != nil {
		return nil, 0, err
	}
	frames, err := w.readFrames(w.nframes)
	if err != nil {
		return nil, 0, err
	}
	if w.sampwidth != 2 || w.channels != 1 || w.rate <= 0 {
		return wav, 0, nil
	}
	total := int64(len(frames) / 2)
	keep := int64(maxSeconds * float64(w.rate))
	if total <= keep {
		return wav, 0, nil
	}
	piece, perr := writeWavPCM(frames[:2*keep], w.rate)
	if perr != nil {
		return nil, 0, perr
	}
	cut := total - keep
	return piece, (cut + w.rate - 1) / w.rate, nil
}

// quietestCut is the sample index in [lo, hi] at the middle of the quietest
// 20 ms frame that lies inside the range. The first of equal frames wins.
func quietestCut(pcm []byte, lo, hi int) int {
	best, bestAt := uint64(math.MaxUint64), (lo+hi)/2
	for start := lo; start+cutFrameSamples <= hi; start += cutFrameSamples / 2 {
		var e uint64
		for i := start; i < start+cutFrameSamples; i++ {
			v := int64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
			e += uint64(v * v)
		}
		if e < best {
			best, bestAt = e, start+cutFrameSamples/2
		}
	}
	return bestAt
}

// JoinTexts joins the window texts in order with one space; an empty window
// adds nothing.
func JoinTexts(parts []string) string {
	var keep []string
	for _, p := range parts {
		if t := pystr.Strip(p); t != "" {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, " ")
}

// TranscribeLong transcribes a clip of any length up to the caller's cap:
// the windows in order, the texts joined. The duration is the clip's, the
// real-time factor is all the engine time over all the audio. whisper.cpp
// reads all windows in one run, so its model loads once. A window that
// fails does not cost the others: its place in the text is GapMarker. The
// call fails only when no window could be read.
func (e *Engine) TranscribeLong(wav []byte, language *string) (*Transcript, error) {
	pieces, aerr := SplitWav(wav)
	if aerr != nil {
		return nil, aerr
	}
	if len(pieces) == 1 {
		t, err := e.Transcribe(pieces[0], language)
		if err == nil {
			t.Parts = []string{t.Text}
		}
		return t, err
	}
	start := time.Now()
	total, derr := WavDurationS(wav)
	if derr != nil {
		return nil, derr
	}
	var texts []string
	var read []bool
	var failure error
	if e.res == nil {
		texts, read, failure = e.transcribeBatch(pieces, language)
	} else {
		for _, p := range pieces {
			t, err := e.Transcribe(p, language)
			if err != nil {
				if failure == nil {
					failure = err
				}
				texts, read = append(texts, ""), append(read, false)
				continue
			}
			texts, read = append(texts, t.Text), append(read, true)
		}
	}
	parts := make([]string, len(pieces))
	gaps, any := 0, false
	for i := range pieces {
		if read[i] {
			parts[i], any = texts[i], true
		} else {
			parts[i], gaps = GapMarker, gaps+1
		}
	}
	if !any {
		if failure == nil {
			failure = errors.New("the engine read no window")
		}
		return nil, failure
	}
	rtf := 0.0
	if total > 0 {
		rtf = time.Since(start).Seconds() / total
	}
	return &Transcript{Text: JoinTexts(parts), DurationS: total, RTF: rtf, Parts: parts, Gaps: gaps}, nil
}
