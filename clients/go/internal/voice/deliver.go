package voice

import (
	"fmt"

	"daisugi-verify/internal/pystr"
)

// DeliverResult is deliver.DeliverResult. Reason is nil when there is
// none.
type DeliverResult struct {
	Delivered string
	Reason    *string
}

func strp(s string) *string { return &s }

// Deliver is deliver.deliver. It makes the one arm decision itself and
// calls send, which builds the pane backend and sends, only right after
// that decision says send. Preview, an unknown mode and a refused send
// never call it. now is the arming check's clock.
func Deliver(paneID, text, mode, armedDir string, paneKey *string, now float64,
	send func(text string) error) (DeliverResult, error) {
	if mode != "preview" && mode != "send" {
		return DeliverResult{"refused", strp(fmt.Sprintf("unknown mode %s. Use preview or send.", pystr.Repr(mode)))}, nil
	}
	key := paneID
	if paneKey != nil {
		key = *paneKey
	}
	if mode == "preview" {
		return DeliverResult{Delivered: "preview"}, nil
	}
	if !IsArmed(key, armedDir, now) {
		return DeliverResult{"refused", strp("pane not armed for direct send. Run: daisugi voice arm " + key + " --for 30m")}, nil
	}
	if err := send(text); err != nil {
		return DeliverResult{}, err
	}
	return DeliverResult{Delivered: "sent"}, nil
}
