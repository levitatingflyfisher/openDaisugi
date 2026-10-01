package viz

import (
	"os"
	"testing"
)

// The embedded template is a copy of the oracle's.
func TestTemplateIsACopy(t *testing.T) {
	b, err := os.ReadFile("../../../../src/opendaisugi/viz_dag_template.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != template {
		t.Error("assets/viz_dag_template.html is not a copy of src/opendaisugi/viz_dag_template.html")
	}
}
