package install

import (
	"strings"
	"testing"
)

func TestComponentLayersUnknownNamesTheList(t *testing.T) {
	_, err := ComponentLayers([]string{"warden,journal"})
	want := "unknown component 'journal'. Components: skill, mcp, capture, instructions, warden, pando, router. Run: daisugi install --list"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

func TestComponentLayersSplitsAndFolds(t *testing.T) {
	got, err := ComponentLayers([]string{"warden,capture", "warden"})
	if err != nil || len(got) != 2 || !got[LayerWarden] || !got[LayerCapture] {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListComponentsMatchesOracleText(t *testing.T) {
	got := ListComponents()
	for _, s := range []string{"skill        ", "router       ", "Default: skill, mcp, capture, instructions, warden, pando.", "--harness pi|opencode"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in %q", s, got)
		}
	}
}
