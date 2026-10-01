// Package catalog is opendaisugi.model_catalog: the curated models for
// the garden, the default by hardware, and the Hugging Face model list
// read into the same neutral fields.
package catalog

import (
	_ "embed"
	"errors"
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Raw is src/opendaisugi/model_catalog.json, copied here (a test checks
// the copy).
//
//go:embed catalog.json
var Raw string

// EntryKeys are the fields of an entry, in their order.
var EntryKeys = []string{"id", "params", "license", "context_length", "gguf", "suits", "note"}

// ChoiceFile is where `models use` records the choice, in the data dir.
const ChoiceFile = "garden_model.json"

// DefaultEndpoint is the API HF_ENDPOINT replaces.
const DefaultEndpoint = "https://huggingface.co"

const searchLimit = 100

var expand = []string{"cardData", "gguf", "pipeline_tag", "safetensors", "tags"}

// Row is one model in the catalog's fields. License, GGUF and Suits are
// nil or a string; Params and Context nil or an integer.
type Row struct {
	ID      string
	Params  *pyjson.Int
	License any
	Context *pyjson.Int
	GGUF    any
	Suits   any
	Note    string
}

// Object is the row as the oracle dumps it.
func (r Row) Object() *pyjson.Object {
	o := pyjson.NewObject().Set("id", r.ID)
	if r.Params != nil {
		o.Set("params", *r.Params)
	} else {
		o.Set("params", nil)
	}
	o.Set("license", r.License)
	if r.Context != nil {
		o.Set("context_length", *r.Context)
	} else {
		o.Set("context_length", nil)
	}
	return o.Set("gguf", r.GGUF).Set("suits", r.Suits).Set("note", r.Note)
}

// Catalog is the loaded file.
type Catalog struct {
	Models         []Row
	capableMinRAM  float64
	capableMinVRAM float64
	weakMaxParams  float64
	defaults       *pyjson.Object
	searchMax      *pyjson.Object
}

func num(v any) float64 {
	switch x := v.(type) {
	case pyjson.Float:
		return float64(x)
	case pyjson.Int:
		f, _ := strconv.ParseFloat(x.Text, 64)
		return f
	}
	return 0
}

func intOf(v any) *pyjson.Int {
	if x, ok := v.(pyjson.Int); ok {
		return &x
	}
	return nil
}

// Load reads the embedded catalog.
func Load() (*Catalog, error) {
	v, derr := pyjson.LoadsPy(Raw, 100)
	if derr != nil {
		return nil, derr
	}
	doc, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errors.New("the model catalog is not an object")
	}
	c := &Catalog{
		capableMinRAM:  num(doc.Value("capable_min_ram_gb")),
		capableMinVRAM: num(doc.Value("capable_min_vram_gb")),
		weakMaxParams:  num(doc.Value("weak_max_params")),
	}
	c.defaults, _ = doc.Value("defaults").(*pyjson.Object)
	c.searchMax, _ = doc.Value("search_max_params_b").(*pyjson.Object)
	models, _ := doc.Value("models").([]any)
	for _, m := range models {
		o, _ := m.(*pyjson.Object)
		r := Row{Params: intOf(o.Value("params")), License: o.Value("license"),
			Context: intOf(o.Value("context_length")), GGUF: o.Value("gguf"), Suits: o.Value("suits")}
		r.ID, _ = o.Value("id").(string)
		r.Note, _ = o.Value("note").(string)
		c.Models = append(c.Models, r)
	}
	return c, nil
}

// Class is hardware_class: capable with enough GPU memory or RAM.
func (c *Catalog) Class(ram *float64, vram float64) string {
	if vram >= c.capableMinVRAM {
		return "capable"
	}
	if ram != nil && *ram >= c.capableMinRAM {
		return "capable"
	}
	return "weak"
}

// Default is default_model.
func (c *Catalog) Default(ram *float64, vram float64) string {
	s, _ := c.defaults.Value(c.Class(ram, vram)).(string)
	return s
}

// SearchMax is the default --max-params for this hardware.
func (c *Catalog) SearchMax(ram *float64, vram float64) float64 {
	return num(c.searchMax.Value(c.Class(ram, vram)))
}

// PyG is format(f, "g").
func PyG(f float64) string {
	s := strconv.FormatFloat(f, 'g', 6, 64)
	if strings.Contains(s, "e") {
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		for len(digits) < 2 {
			digits = "0" + digits
		}
		return mant + "e" + sign + digits
	}
	return s
}

// HardwareLine is hardware_line.
func (c *Catalog) HardwareLine(ram *float64, vram float64) string {
	r := "an unknown amount of RAM"
	if ram != nil {
		r = PyG(*ram) + " GB of RAM"
	}
	gpu := ""
	if vram > 0 {
		gpu = " and a GPU with " + PyG(vram) + " GB"
	}
	return "This box has " + r + gpu + ", so the default is " + c.Default(ram, vram) + "."
}

// FmtParams is fmt_params.
func FmtParams(n *pyjson.Int) string {
	if n == nil {
		return "?"
	}
	f, _ := strconv.ParseFloat(n.Text, 64)
	return strconv.FormatFloat(f/1e9, 'f', 1, 64) + "B"
}

// Endpoint is endpoint(env): HF_ENDPOINT without a trailing slash.
func Endpoint(env map[string]string) string {
	e := env["HF_ENDPOINT"]
	if e == "" {
		e = DefaultEndpoint
	}
	return strings.TrimRight(e, "/")
}

// OfflineEnv is offline_env: HF_HUB_OFFLINE as huggingface_hub reads it.
func OfflineEnv(env map[string]string) bool {
	switch strings.ToUpper(env["HF_HUB_OFFLINE"]) {
	case "1", "ON", "YES", "TRUE":
		return true
	}
	return false
}

// quote is urllib.parse.quote(s, safe=""): unreserved ASCII kept, every
// other byte of the UTF-8 as %XX.
func quote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)>>4, 16)) + strings.ToUpper(strconv.FormatInt(int64(c)&15, 16)))
		}
	}
	return b.String()
}

// SearchTarget is search_target: the request target in one fixed order.
func SearchTarget(query string) string {
	parts := []string{"search=" + quote(query), "limit=" + strconv.Itoa(searchLimit), "sort=downloads", "direction=-1"}
	for _, e := range expand {
		parts = append(parts, "expand%5B%5D="+e)
	}
	return "/api/models?" + strings.Join(parts, "&")
}

// ErrBadAnswer is BadAnswer: the answer is not a model list.
var ErrBadAnswer = errors.New("not a model list")

func license(row, card *pyjson.Object) any {
	lic := card.Value("license")
	if name, ok := card.Value("license_name").(string); ok && lic == "other" {
		return name
	}
	if s, ok := lic.(string); ok {
		return s
	}
	tags, _ := row.Value("tags").([]any)
	for _, t := range tags {
		if s, ok := t.(string); ok && strings.HasPrefix(s, "license:") {
			return strings.TrimPrefix(s, "license:")
		}
	}
	return nil
}

func objOf(v any) *pyjson.Object {
	if o, ok := v.(*pyjson.Object); ok {
		return o
	}
	return pyjson.NewObject()
}

// ParseListing is parse_listing.
func (c *Catalog) ParseListing(raw string) ([]Row, error) {
	v, derr := pyjson.LoadsPy(raw, 900)
	if derr != nil {
		return nil, ErrBadAnswer
	}
	list, ok := v.([]any)
	if !ok {
		return nil, ErrBadAnswer
	}
	rows := []Row{}
	for _, item := range list {
		row, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		id, ok := row.Value("id").(string)
		if !ok {
			continue
		}
		st, gg := objOf(row.Value("safetensors")), objOf(row.Value("gguf"))
		params := intOf(st.Value("total"))
		if params == nil {
			params = intOf(gg.Value("total"))
		}
		_, ggufObj := row.Value("gguf").(*pyjson.Object)
		hasGGUF := ggufObj
		if tags, ok := row.Value("tags").([]any); ok {
			for _, t := range tags {
				if t == "gguf" {
					hasGGUF = true
				}
			}
		}
		r := Row{ID: id, Params: params, License: license(row, objOf(row.Value("cardData"))),
			Context: intOf(gg.Value("context_length"))}
		if hasGGUF {
			r.GGUF = id
		}
		if params != nil {
			r.Suits = "capable"
			if lessEq(params.Text, c.weakMaxParams) {
				r.Suits = "weak"
			}
		}
		r.Note, _ = row.Value("pipeline_tag").(string)
		rows = append(rows, r)
	}
	return rows, nil
}

// lessEq is int(text) <= f, exactly for the sizes a model has.
func lessEq(text string, f float64) bool {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && n < 1<<53 && n > -(1<<53) {
		return float64(n) <= f
	}
	x, _ := strconv.ParseFloat(text, 64)
	return x <= f
}

// Filter is filter_rows: at most maxB billion parameters (0 or less: any
// size, and an unknown size is kept only then), under one of licenses
// when it names any, the first limit of them.
func Filter(rows []Row, maxB float64, licenses []string, limit int) []Row {
	out := []Row{}
	for _, r := range rows {
		if maxB > 0 && (r.Params == nil || !lessEq(r.Params.Text, maxB*1e9)) {
			continue
		}
		if len(licenses) > 0 {
			s, ok := r.License.(string)
			found := false
			for _, l := range licenses {
				found = found || (ok && l == s)
			}
			if !found {
				continue
			}
		}
		out = append(out, r)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func orQ(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "?"
}

func cells(r Row) []string {
	ctx := "?"
	if r.Context != nil {
		ctx = r.Context.Text
	}
	gguf := "no"
	if s, ok := r.GGUF.(string); ok && s != "" {
		gguf = "yes"
	}
	return []string{r.ID, FmtParams(r.Params), orQ(r.License), ctx, gguf, orQ(r.Suits), r.Note}
}

// TableLines is table_lines: a header and a line per row, every column
// padded but the last, each line without trailing spaces.
func TableLines(rows []Row) []string {
	grid := [][]string{{"ID", "SIZE", "LICENSE", "CONTEXT", "GGUF", "SUITS", "GOOD AT"}}
	for _, r := range rows {
		grid = append(grid, cells(r))
	}
	n := len(grid[0])
	widths := make([]int, n-1)
	for _, row := range grid {
		for i := 0; i < n-1; i++ {
			if w := pystr.Len(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var lines []string
	for _, row := range grid {
		parts := make([]string, n)
		for i := 0; i < n-1; i++ {
			parts[i] = row[i] + strings.Repeat(" ", widths[i]-pystr.Len(row[i]))
		}
		parts[n-1] = row[n-1]
		lines = append(lines, pystr.RStrip("  "+strings.Join(parts, "  ")))
	}
	return lines
}
