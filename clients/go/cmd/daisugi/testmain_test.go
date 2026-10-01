package main

import (
	"os"
	"testing"

	"daisugi-verify/internal/testtmp"
)

// TestMain gives the package one private TMPDIR, removed after the last test.
func TestMain(m *testing.M) { os.Exit(testtmp.Main(m)) }
