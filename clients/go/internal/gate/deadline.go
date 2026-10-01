package gate

import (
	"regexp"
	"strconv"
	"time"

	"daisugi-verify/internal/pyjson"
)

// nowEnv is gate._NOW_ENV: it pins the clock the deadline check reads.
const nowEnv = "DAISUGI_GATE_NOW"

// nowPin is gate._NOW_PIN, matched in full.
var nowPin = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

// gateNow is gate.gate_now: the real time, or a later pin.
func (r *runner) gateNow() float64 {
	now := float64(time.Now().UnixNano()) / 1e9
	if pin, ok := r.env[nowEnv]; ok && nowPin.MatchString(pin) {
		if f, err := strconv.ParseFloat(pin, 64); err == nil && f > now {
			now = f
		}
	}
	return now
}

// deadlineReason is gate.deadline_reason.
func deadlineReason(deadline float64) string {
	return "the envelope's deadline " + pyjson.FloatRepr(deadline) +
		" (Unix seconds) has passed; it starts no new work"
}
