package voice

import (
	"strconv"
	"strings"
)

// The engine a box with no voice choice gets, by its hardware (ruling
// VO-17): engines.choose_engine.
const (
	ParakeetMinRAMGB  = 8.0
	ParakeetMinCPUs   = 4
	MoonshineMinRAMGB = 2.0
	// A test sets this to "RAM_GB,CPUS,VRAM_GB" (RAM_GB may be empty), so
	// no test or case reads the real box. The voice engine choice and the
	// model catalog's default read it; nothing else does.
	HardwareEnv = "OPENDAISUGI_VOICE_HARDWARE"
)

// VoiceHardware is engines.VoiceHardware. RAMGB is nil when unknown.
type VoiceHardware struct {
	RAMGB  *float64
	CPUs   int
	VRAMGB float64
}

// pyG is format(f, "g").
func pyG(f float64) string {
	s := strconv.FormatFloat(f, 'g', 6, 64)
	if strings.Contains(s, "e") {
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		for len(digits) < 2 {
			digits = "0" + digits
		}
		return mant + "e" + sign + digits
	}
	return s
}

// HardwareOrder is engines.hardware_order: the tiers this hardware can
// run, best first.
func HardwareOrder(hw VoiceHardware) []string {
	if hw.RAMGB != nil && *hw.RAMGB >= ParakeetMinRAMGB && hw.CPUs >= ParakeetMinCPUs {
		return []string{"parakeet", "moonshine", "tiny"}
	}
	if hw.RAMGB != nil && *hw.RAMGB >= MoonshineMinRAMGB {
		return []string{"moonshine", "tiny"}
	}
	return []string{"tiny"}
}

func hardwareText(hw VoiceHardware) string {
	ram := "an unknown amount of RAM"
	if hw.RAMGB != nil {
		ram = pyG(*hw.RAMGB) + " GB of RAM"
	}
	cores := strconv.Itoa(hw.CPUs) + " cores"
	if hw.CPUs == 1 {
		cores = "1 core"
	}
	text := ram + " and " + cores
	if hw.VRAMGB > 0 {
		text += ", and a GPU with " + pyG(hw.VRAMGB) + " GB (no GPU engine is built yet)"
	}
	return text
}

var engineLabels = map[[2]string]string{
	{"parakeet", ParakeetDefaultModel}:   "Parakeet v2",
	{"moonshine", MoonshineDefaultModel}: "Moonshine small",
	{"faster-whisper", "tiny.en"}:        "faster-whisper tiny.en",
}

var engineNeeds = map[string]string{"parakeet": ParakeetBinary, "moonshine": MoonshineBinary}

// ChooseEngine is engines.choose_engine: the engine, model and one line
// for a box with no voice choice. installed holds the engines whose
// program is on PATH or whose package imports; fw is whether
// faster-whisper imports (never, in this binary).
func ChooseEngine(hw VoiceHardware, installed map[string]bool, fw bool) (string, string, string) {
	var picks [][2]string
	for _, tier := range HardwareOrder(hw) {
		var p [2]string
		switch {
		case tier == "parakeet":
			p = [2]string{"parakeet", ParakeetDefaultModel}
		case tier == "moonshine" || !fw:
			p = [2]string{"moonshine", MoonshineDefaultModel}
		default:
			p = [2]string{"faster-whisper", "tiny.en"}
		}
		seen := false
		for _, q := range picks {
			seen = seen || q == p
		}
		if !seen {
			picks = append(picks, p)
		}
	}
	chosen := picks[0]
	for _, p := range picks {
		if installed[p[0]] {
			chosen = p
			break
		}
	}
	line := "No voice engine is set, so voice uses " + engineLabels[chosen] + ": this box has " + hardwareText(hw) + "."
	if chosen != picks[0] {
		line += " " + engineLabels[picks[0]] + " would come first, but " + engineNeeds[picks[0][0]] + " is not on PATH."
	}
	return chosen[0], chosen[1], line
}

// ParseHardwareEnv reads HardwareEnv's "RAM_GB,CPUS,VRAM_GB".
func ParseHardwareEnv(raw string) (VoiceHardware, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) != 3 {
		return VoiceHardware{}, false
	}
	var hw VoiceHardware
	if parts[0] != "" {
		f, ok := PyFloat(parts[0])
		if !ok {
			return VoiceHardware{}, false
		}
		hw.RAMGB = &f
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	v, ok := PyFloat(parts[2])
	if err != nil || !ok {
		return VoiceHardware{}, false
	}
	hw.CPUs, hw.VRAMGB = n, v
	return hw, true
}

// DetectHardware is engines.detect_voice_hardware: HardwareEnv when it is
// set, else the probe the caller gives (the one tiers setup uses), else
// nothing known.
func (m ModelEnv) DetectHardware() VoiceHardware {
	if raw := m.get(HardwareEnv); raw != "" {
		if hw, ok := ParseHardwareEnv(raw); ok {
			return hw
		}
	}
	if m.Hardware != nil {
		return m.Hardware()
	}
	return VoiceHardware{CPUs: 1}
}
