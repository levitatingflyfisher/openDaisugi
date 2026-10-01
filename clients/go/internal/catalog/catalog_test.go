package catalog

import (
	"os"
	"strings"
	"testing"
)

// The embedded catalog must be the file Python ships.
func TestCatalogMatchesPython(t *testing.T) {
	want, err := os.ReadFile("../../../../src/opendaisugi/model_catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != Raw {
		t.Error("src/opendaisugi/model_catalog.json drifted from the embedded copy: copy it into internal/catalog")
	}
}

func f(x float64) *float64 { return &x }

func TestDefaultFollowsHardware(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ram        *float64
		vram       float64
		class, def string
	}{
		{f(16), 0, "capable", "ibm-granite/granite-4.1-3b"},
		{f(4), 0, "weak", "ibm-granite/granite-4.0-1b"},
		{f(4), 8, "capable", "ibm-granite/granite-4.1-3b"},
		{nil, 0, "weak", "ibm-granite/granite-4.0-1b"},
	} {
		if got := c.Class(tc.ram, tc.vram); got != tc.class {
			t.Errorf("class %v %v: %s", tc.ram, tc.vram, got)
		}
		if got := c.Default(tc.ram, tc.vram); got != tc.def {
			t.Errorf("default %v %v: %s", tc.ram, tc.vram, got)
		}
	}
	if got := c.HardwareLine(f(4), 8); got != "This box has 4 GB of RAM and a GPU with 8 GB, so the default is ibm-granite/granite-4.1-3b." {
		t.Error(got)
	}
	if got := c.HardwareLine(nil, 0); got != "This box has an unknown amount of RAM, so the default is ibm-granite/granite-4.0-1b." {
		t.Error(got)
	}
}

func TestEveryEntryHasTheNeutralFields(t *testing.T) {
	c, _ := Load()
	if len(c.Models) == 0 {
		t.Fatal("no models")
	}
	for _, m := range c.Models {
		if got := strings.Join(m.Object().Keys(), ","); got != strings.Join(EntryKeys, ",") {
			t.Errorf("%s: keys %s", m.ID, got)
		}
	}
}

func TestSearchTarget(t *testing.T) {
	got := SearchTarget("granite é/4 & x+y~_.-")
	want := "/api/models?search=granite%20%C3%A9%2F4%20%26%20x%2By~_.-&limit=100&sort=downloads&direction=-1" +
		"&expand%5B%5D=cardData&expand%5B%5D=gguf&expand%5B%5D=pipeline_tag&expand%5B%5D=safetensors&expand%5B%5D=tags"
	if got != want {
		t.Errorf("%s", got)
	}
}

func TestParseListingAndFilter(t *testing.T) {
	c, _ := Load()
	raw := `[{"id":"a/st","cardData":{"license":"apache-2.0"},"safetensors":{"total":3000000000},"pipeline_tag":"text-generation"},
	{"id":"b/gguf","gguf":{"total":1000000000,"context_length":32768},"tags":["gguf","license:mit"]},
	{"id":"c/other","cardData":{"license":"other","license_name":"custom-x"}},
	{"id":"d/float","safetensors":{"total":3e9}},
	{"no":"id"},5,{"id":true}]`
	rows, err := c.ParseListing(raw)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rs []Row) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(rows); got != "a/st,b/gguf,c/other,d/float" {
		t.Fatal(got)
	}
	if rows[1].License != "mit" || rows[1].GGUF != "b/gguf" || rows[1].Suits != "weak" || rows[2].License != "custom-x" {
		t.Errorf("%+v %+v", rows[1], rows[2])
	}
	if rows[3].Params != nil {
		t.Error("a float size is not a size")
	}
	if got := ids(Filter(rows, 8, nil, 20)); got != "a/st,b/gguf" {
		t.Error(got)
	}
	if got := ids(Filter(rows, 0, []string{"mit", "custom-x"}, 20)); got != "b/gguf,c/other" {
		t.Error(got)
	}
	if got := ids(Filter(rows, 0, nil, 1)); got != "a/st" {
		t.Error(got)
	}
	for _, bad := range []string{"{}", "not json", `"x"`, ""} {
		if _, err := c.ParseListing(bad); err == nil {
			t.Errorf("%q read as a listing", bad)
		}
	}
}

func TestTable(t *testing.T) {
	c, _ := Load()
	lines := TableLines(c.Models[:2])
	want := "  ibm-granite/granite-4.1-3b  3.4B  apache-2.0  131072   yes   capable  Tool calling and following instructions."
	if lines[1] != want {
		t.Errorf("%q", lines[1])
	}
	for _, ln := range lines {
		if strings.HasSuffix(ln, " ") {
			t.Errorf("trailing space: %q", ln)
		}
	}
}
