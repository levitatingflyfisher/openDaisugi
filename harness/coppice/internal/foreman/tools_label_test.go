package foreman

import "testing"

func TestTheZoomToolOffersALabelAndRequiresNothing(t *testing.T) {
	tools := toolList()["tools"].([]any)
	var zoom map[string]any
	for _, tl := range tools {
		if m := tl.(map[string]any); m["name"] == "zoom" {
			zoom = m
		}
	}
	if zoom == nil {
		t.Fatal("no zoom tool")
	}
	schema := zoom["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	for _, p := range []string{"id", "n", "label"} {
		if _, ok := props[p]; !ok {
			t.Errorf("zoom has no %s", p)
		}
	}
	if req, _ := schema["required"].([]string); len(req) != 0 {
		t.Errorf("zoom requires %v: the server checks, since a label replaces id and n", req)
	}
	if len(tools) != 4 {
		t.Errorf("%d tools, want exactly four", len(tools))
	}
}
