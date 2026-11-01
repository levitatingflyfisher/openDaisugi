package recipes

import (
	"os"
	"path/filepath"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func TestShippedFilesAreTheOraclesBytes(t *testing.T) {
	for _, name := range Names() {
		want, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "src", "opendaisugi", "recipes", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := Text(name)
		if got != string(want) {
			t.Errorf("%s differs from the oracle's file", name)
		}
	}
	if len(Names()) != 4 {
		t.Fatalf("names = %v", Names())
	}
}

func TestEveryShippedRecipeValidates(t *testing.T) {
	for _, name := range Names() {
		if _, err := Load(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoaderRefusesAShellStep(t *testing.T) {
	v, _ := pyjson.Loads(`{"name":"x","title":"t","doc":"d","slots":{},"params":{},"steps":[{"kind":"shell","command":"rm -rf /"}]}`)
	_, err := Validate(v, "x.json")
	if err == nil || err.Error() != "x.json: steps[0].kind 'shell' is not one of install, harness, config, warden-init, note" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoaderRefusesAnUndeclaredParam(t *testing.T) {
	v, _ := pyjson.Loads(`{"name":"x","title":"t","doc":"d","slots":{},"params":{},"steps":[{"kind":"warden-init","workspace":"{workspace}"}]}`)
	_, err := Validate(v, "x.json")
	if err == nil || err.Error() != "x.json: steps[0].workspace: param 'workspace' is not declared" {
		t.Fatalf("err = %v", err)
	}
}
