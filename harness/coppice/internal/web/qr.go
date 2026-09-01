package web

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// MaxQRColumns is the drawn width a QR must stay inside. A wrapped QR is not
// a QR, and 120 columns is what a terminal on a laptop reliably has.
const MaxQRColumns = 120

// WriteQR draws a QR code with half blocks, so it fits a normal terminal at
// a size a phone camera can still read. Error correction stays low on
// purpose. Less redundancy makes a smaller code, and the operator is
// holding a phone close to the screen, not reading a poster from across a
// room.
func WriteQR(w io.Writer, text string) {
	qrterminal.GenerateWithConfig(text, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         w,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      2,
	})
}
