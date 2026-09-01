package signing

import (
	"encoding/hex"
	"testing"
)

// The oracle's base64.b64decode answers, recorded from Python.
func TestB64DecodeReadsAsPythonDoes(t *testing.T) {
	cases := []struct {
		in, hex, err string
	}{
		{"QQ==", "41", ""},
		{"QQ=", "", "binascii.Error: Incorrect padding"},
		{"QQ", "", "binascii.Error: Incorrect padding"},
		{"Q", "", "binascii.Error: Invalid base64-encoded string: number of data characters (1) cannot be 1 more than a multiple of 4"},
		{"AAAAAAAAAAAAA==", "", "binascii.Error: Invalid base64-encoded string: number of data characters (13) cannot be 1 more than a multiple of 4"},
		{"QUI=", "4142", ""},
		{"=QUJD", "414243", ""},
		{"Q=Q=", "", "binascii.Error: Incorrect padding"},
		{"QU=I=", "4142", ""},
		{"QUJ=D", "4142", ""},
		{"====", "", ""},
		{"!!!", "", ""},
		{"AB==CD==", "00", ""},
		{"YQ==YQ==", "61", ""},
		{"Y=Q==", "61", ""},
		{"Y\tQ=\r\n=", "61", ""},
		{"QU-_JD", "414243", ""},
	}
	for _, c := range cases {
		got, err := B64Decode(c.in)
		switch {
		case c.err != "":
			if err == nil || err.Type+": "+err.Msg != c.err {
				t.Errorf("%q: error %v, want %s", c.in, err, c.err)
			}
		case err != nil:
			t.Errorf("%q: %v", c.in, err)
		case hex.EncodeToString(got) != c.hex:
			t.Errorf("%q: %x, want %s", c.in, got, c.hex)
		}
	}
	if _, err := B64Decode("é"); err == nil || err.Type != "ValueError" || err.Context == "" {
		t.Errorf("non-ASCII input: %v", err)
	}
}

// A signature made with the raw 32-byte seed verifies under its raw
// public key, and under no other; a key of the wrong size is an error
// when signing and a false when verifying, never a panic.
func TestSignAndVerifyTheOraclesKeyFormat(t *testing.T) {
	priv, pub, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := PublicOf(priv); p != pub {
		t.Fatalf("PublicOf %s, want %s", p, pub)
	}
	sig, perr := SignBytes([]byte("hello"), priv)
	if perr != nil {
		t.Fatal(perr)
	}
	if !VerifyBytes([]byte("hello"), sig, pub) || VerifyBytes([]byte("hellO"), sig, pub) {
		t.Fatal("verify")
	}
	_, other, _ := GenerateKeypair()
	if VerifyBytes([]byte("hello"), sig, other) {
		t.Fatal("verified under another key")
	}
	short := B64Encode(make([]byte, 31))
	if _, perr := SignBytes([]byte("x"), short); perr == nil || perr.Msg != "An Ed25519 private key is 32 bytes long" {
		t.Fatalf("short private key: %v", perr)
	}
	if VerifyBytes([]byte("hello"), sig, short) || VerifyBytes([]byte("hello"), "abc", pub) {
		t.Fatal("a bad key or signature verified")
	}
	if VerifyWith([]byte("hello"), sig, 5) {
		t.Fatal("a registry value that is not a str verified")
	}
}
