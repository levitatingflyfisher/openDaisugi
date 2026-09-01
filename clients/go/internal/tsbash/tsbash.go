// Package tsbash parses shell with tree-sitter-bash, the grammar the Python
// oracle uses, and hands back the whole tree as plain Go values.
//
// The C runtime (tree-sitter 0.26.0) and the grammar (tree-sitter-bash
// 0.25.1) are built from the same source archives the oracle's wheels are
// built from, by clients/go/scripts/native.sh, and linked statically.
package tsbash

/*
#cgo CFLAGS: -I${SRCDIR}/../../.native/include
#cgo LDFLAGS: ${SRCDIR}/../../.native/lib/libtree-sitter-bash.a ${SRCDIR}/../../.native/lib/libtree-sitter.a
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <tree_sitter/api.h>
#include <locale.h>
#include <langinfo.h>

// py_locale sets LC_CTYPE the way CPython 3.12 does at startup: from the
// environment, and when that gives the C locale (and LC_ALL is empty and
// PYTHONCOERCECLOCALE is not 0), coerced to the first of C.UTF-8, C.utf8
// and UTF-8 that exists (PEP 538). The grammar's scanner classifies
// characters with iswspace and iswalpha, which read LC_CTYPE. It returns
// the coerced name, which CPython also puts in LC_CTYPE for its children,
// or NULL.
static const char *py_locale(void) {
	setlocale(LC_CTYPE, "");
	const char *pcl = getenv("PYTHONCOERCECLOCALE");
	if (pcl && strcmp(pcl, "0") == 0) return NULL;
	const char *all = getenv("LC_ALL");
	if (all && *all) return NULL;
	const char *cur = setlocale(LC_CTYPE, NULL);
	if (!cur || strcmp(cur, "C") != 0) return NULL;
	static const char *targets[] = {"C.UTF-8", "C.utf8", "UTF-8", NULL};
	for (int i = 0; targets[i]; i++) {
		if (setlocale(LC_CTYPE, targets[i])) {
			const char *cs = nl_langinfo(CODESET);
			if (!cs || !*cs) {
				setlocale(LC_CTYPE, "");
				continue;
			}
			setenv("LC_CTYPE", targets[i], 1);
			setlocale(LC_CTYPE, "");
			return targets[i];
		}
	}
	setlocale(LC_CTYPE, "C");
	return NULL;
}

const TSLanguage *tree_sitter_bash(void);

typedef struct {
	uint16_t sym;
	uint8_t missing;
	uint32_t start;
	uint32_t end;
	int32_t parent;
	int32_t nchild;
	const void *id;
	const void *name_id; // child_by_field_name("name") of a command node
} flat_node;

typedef struct {
	flat_node *nodes;
	int32_t len;
	int32_t cap;
	uint8_t has_error;
	uint8_t failed;
} flat_tree;

static int push(flat_tree *t, flat_node n) {
	if (t->len == t->cap) {
		int32_t cap = t->cap ? t->cap * 2 : 256;
		flat_node *p = realloc(t->nodes, (size_t)cap * sizeof(flat_node));
		if (!p) return -1;
		t->nodes = p;
		t->cap = cap;
	}
	t->nodes[t->len] = n;
	return t->len++;
}

// flatten walks the tree in pre-order (a node, then its children in
// order), the order the oracle's recursive walks visit nodes in.
static flat_tree flatten(const char *src, uint32_t len) {
	flat_tree out = {0};
	TSParser *p = ts_parser_new();
	if (!p || !ts_parser_set_language(p, tree_sitter_bash())) {
		out.failed = 1;
		if (p) ts_parser_delete(p);
		return out;
	}
	TSTree *tree = ts_parser_parse_string(p, NULL, src, len);
	if (!tree) {
		out.failed = 1;
		ts_parser_delete(p);
		return out;
	}
	TSNode root = ts_tree_root_node(tree);
	out.has_error = ts_node_has_error(root);
	// Stack of (node, parent index) for an explicit pre-order walk.
	int32_t scap = 256, slen = 0;
	TSNode *stack = malloc((size_t)scap * sizeof(TSNode));
	int32_t *sparent = malloc((size_t)scap * sizeof(int32_t));
	if (!stack || !sparent) { out.failed = 1; goto done; }
	stack[slen] = root; sparent[slen] = -1; slen++;
	while (slen > 0) {
		slen--;
		TSNode n = stack[slen];
		int32_t parent = sparent[slen];
		uint32_t nc = ts_node_child_count(n);
		flat_node f = {ts_node_symbol(n), ts_node_is_missing(n), ts_node_start_byte(n),
			ts_node_end_byte(n), parent, (int32_t)nc, n.id, NULL};
		if (strcmp(ts_node_type(n), "command") == 0) {
			TSNode nm = ts_node_child_by_field_name(n, "name", 4);
			if (!ts_node_is_null(nm)) f.name_id = nm.id;
		}
		int32_t idx = push(&out, f);
		if (idx < 0) { out.failed = 1; goto done; }
		if (nc == 0) continue;
		if (slen + (int32_t)nc > scap) {
			while (slen + (int32_t)nc > scap) scap *= 2;
			TSNode *s2 = realloc(stack, (size_t)scap * sizeof(TSNode));
			if (!s2) { out.failed = 1; goto done; }
			stack = s2;
			int32_t *p2 = realloc(sparent, (size_t)scap * sizeof(int32_t));
			if (!p2) { out.failed = 1; goto done; }
			sparent = p2;
		}
		// Push children in reverse so the first child is walked first.
		for (int32_t i = (int32_t)nc - 1; i >= 0; i--) {
			stack[slen] = ts_node_child(n, (uint32_t)i);
			sparent[slen] = idx;
			slen++;
		}
	}
done:
	free(stack);
	free(sparent);
	ts_tree_delete(tree);
	ts_parser_delete(p);
	return out;
}

static const char *symbol_name(uint16_t sym) {
	return ts_language_symbol_name(tree_sitter_bash(), sym);
}
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

var (
	localeOnce sync.Once
	coerced    string
)

// PythonLocale sets this process's LC_CTYPE as CPython sets its own at
// startup, once, and returns the locale it coerced LC_CTYPE to (which
// CPython also exports to its children), or "". Parse calls it first.
func PythonLocale() string {
	localeOnce.Do(func() {
		if c := C.py_locale(); c != nil {
			coerced = C.GoString(c)
		}
	})
	return coerced
}

// Node is one tree-sitter node. Children are every child, named or not,
// as py-tree-sitter's Node.children lists them.
type Node struct {
	Type     string
	Missing  bool
	Start    int
	End      int
	Parent   int
	Children []int
	// Name is the index of child_by_field_name("name"), or -1. Set for
	// command nodes only.
	Name int
}

// Tree is a parsed command: its nodes in pre-order, the root first.
type Tree struct {
	Src      []byte
	Nodes    []Node
	HasError bool
}

var symbolNames = map[uint16]string{}

func symbolName(sym uint16) string {
	if s, ok := symbolNames[sym]; ok {
		return s
	}
	s := C.GoString(C.symbol_name(C.uint16_t(sym)))
	symbolNames[sym] = s
	return s
}

// ErrParse is returned when the parser itself fails (out of memory).
var ErrParse = errors.New("tree-sitter could not parse the command")

// Parse parses src with tree-sitter-bash.
func Parse(src []byte) (*Tree, error) {
	PythonLocale()
	var cs *C.char
	if len(src) > 0 {
		cs = (*C.char)(C.CBytes(src))
		defer C.free(unsafe.Pointer(cs))
	} else {
		cs = C.CString("")
		defer C.free(unsafe.Pointer(cs))
	}
	ft := C.flatten(cs, C.uint32_t(len(src)))
	defer C.free(unsafe.Pointer(ft.nodes))
	if ft.failed != 0 {
		return nil, ErrParse
	}
	n := int(ft.len)
	flat := unsafe.Slice(ft.nodes, n)
	t := &Tree{Src: src, Nodes: make([]Node, n), HasError: ft.has_error != 0}
	byID := make(map[uintptr]int, n)
	for i := 0; i < n; i++ {
		byID[uintptr(flat[i].id)] = i
	}
	for i := 0; i < n; i++ {
		f := flat[i]
		t.Nodes[i] = Node{
			Type:    symbolName(uint16(f.sym)),
			Missing: f.missing != 0,
			Start:   int(f.start),
			End:     int(f.end),
			Parent:  int(f.parent),
			Name:    -1,
		}
		if f.name_id != nil {
			if j, ok := byID[uintptr(f.name_id)]; ok {
				t.Nodes[i].Name = j
			}
		}
		if f.nchild > 0 {
			t.Nodes[i].Children = make([]int, 0, int(f.nchild))
		}
		if p := int(f.parent); p >= 0 {
			t.Nodes[p].Children = append(t.Nodes[p].Children, i)
		}
	}
	return t, nil
}

// Text is the node's source bytes.
func (t *Tree) Text(i int) []byte { return t.Src[t.Nodes[i].Start:t.Nodes[i].End] }
