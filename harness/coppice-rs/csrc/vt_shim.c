// A small C layer over libghostty-vt's C API for the Rust port of coppice.
// It makes the same calls in the same order as go-libghostty does for
// coppice's internal/vt. The C compiler checks every struct against the
// real headers, so the Rust side never declares a ghostty struct itself.
// Every function here takes a cop_vt the caller holds its own lock on.

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include <ghostty/vt.h>

typedef struct {
  GhosttyTerminal term;
  GhosttyRenderState rs;
  GhosttyRenderStateRowIterator ri;
  GhosttyRenderStateRowCells rc;
  // The last OSC 9;4 progress report: has is 0 until one arrives.
  int has_progress;
  int progress_state;
  int progress_value;
} cop_vt;

static void cop_progress(GhosttyTerminal t, void *userdata,
                         const GhosttyTerminalProgressReport *report) {
  (void)t;
  cop_vt *v = (cop_vt *)userdata;
  // A sized struct: read only the fields this build knows.
  if (v == NULL || report == NULL ||
      report->size < sizeof(GhosttyTerminalProgressReport)) {
    return;
  }
  v->has_progress = 1;
  v->progress_state = (int)report->state;
  v->progress_value = (int)report->progress;
}

// cop_vt_new returns NULL on failure, with the step that failed in *step:
// 1 terminal, 2 scrollback, 3 effects, 4 render state, 5 row iterator,
// 6 row cells.
cop_vt *cop_vt_new(uint16_t cols, uint16_t rows, size_t scrollback_lines,
                   int *step) {
  cop_vt *v = calloc(1, sizeof(cop_vt));
  if (v == NULL) {
    *step = 1;
    return NULL;
  }
  if (ghostty_terminal_new(NULL, &v->term, cols, rows) != GHOSTTY_SUCCESS) {
    *step = 1;
    free(v);
    return NULL;
  }
  size_t lines = scrollback_lines;
  if (ghostty_terminal_set(v->term, GHOSTTY_TERMINAL_OPT_SCROLLBACK_MAX_LINES,
                           &lines) != GHOSTTY_SUCCESS) {
    *step = 2;
    ghostty_terminal_free(v->term);
    free(v);
    return NULL;
  }
  // The only effect coppice asks for is the progress report. The userdata
  // pointer is this struct, which outlives the terminal.
  if (ghostty_terminal_set(v->term, GHOSTTY_TERMINAL_OPT_USERDATA, v) !=
          GHOSTTY_SUCCESS ||
      ghostty_terminal_set(v->term, GHOSTTY_TERMINAL_OPT_PROGRESS_REPORT,
                           (const void *)cop_progress) != GHOSTTY_SUCCESS) {
    *step = 3;
    ghostty_terminal_free(v->term);
    free(v);
    return NULL;
  }
  if (ghostty_render_state_new(NULL, &v->rs) != GHOSTTY_SUCCESS) {
    *step = 4;
    ghostty_terminal_free(v->term);
    free(v);
    return NULL;
  }
  if (ghostty_render_state_row_iterator_new(NULL, &v->ri) != GHOSTTY_SUCCESS) {
    *step = 5;
    ghostty_render_state_free(v->rs);
    ghostty_terminal_free(v->term);
    free(v);
    return NULL;
  }
  if (ghostty_render_state_row_cells_new(NULL, &v->rc) != GHOSTTY_SUCCESS) {
    *step = 6;
    ghostty_render_state_row_iterator_free(v->ri);
    ghostty_render_state_free(v->rs);
    ghostty_terminal_free(v->term);
    free(v);
    return NULL;
  }
  *step = 0;
  return v;
}

// cop_vt_free releases the handles in the reverse order they were made.
void cop_vt_free(cop_vt *v) {
  if (v == NULL) {
    return;
  }
  ghostty_render_state_row_cells_free(v->rc);
  ghostty_render_state_row_iterator_free(v->ri);
  ghostty_render_state_free(v->rs);
  ghostty_terminal_free(v->term);
  free(v);
}

void cop_vt_write(cop_vt *v, const uint8_t *data, size_t len) {
  if (len == 0) {
    return;
  }
  ghostty_terminal_vt_write(v->term, data, len);
}

int cop_vt_resize(cop_vt *v, uint16_t cols, uint16_t rows) {
  return (int)ghostty_terminal_resize(v->term, cols, rows, 0, 0);
}

int cop_vt_cols(cop_vt *v, uint16_t *out) {
  return (int)ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_COLS, out);
}

int cop_vt_rows(cop_vt *v, uint16_t *out) {
  return (int)ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_ROWS, out);
}

int cop_vt_cursor_x(cop_vt *v, uint16_t *out) {
  return (int)ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_CURSOR_X,
                                   out);
}

int cop_vt_cursor_y(cop_vt *v, uint16_t *out) {
  return (int)ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_CURSOR_Y,
                                   out);
}

int cop_vt_cursor_visible(cop_vt *v, bool *out) {
  return (int)ghostty_terminal_get(v->term,
                                   GHOSTTY_TERMINAL_DATA_CURSOR_VISIBLE, out);
}

// cop_vt_title points *ptr at the terminal's own title bytes. They stay
// valid until the next call that changes the terminal.
int cop_vt_title(cop_vt *v, const uint8_t **ptr, size_t *len) {
  GhosttyString s = {0};
  int r = (int)ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_TITLE, &s);
  *ptr = s.ptr;
  *len = s.len;
  return r;
}

int cop_vt_progress(cop_vt *v, int *state, int *progress) {
  *state = v->progress_state;
  *progress = v->progress_value;
  return v->has_progress;
}

// cop_vt_plain formats the terminal as plain text, trimmed. With all set it
// formats the selection of the whole buffer, scrollback included, that
// select_all returns. Otherwise it formats the active area only: the cursor
// movable screen from (0,0) to (cols-1, rows-1). unwrap joins soft-wrapped
// rows. The text is allocated by libghostty; free it with cop_vt_free_buf.
// The result is a GhosttyResult, or 100 when the terminal has zero size.
int cop_vt_plain(cop_vt *v, int all, bool unwrap, uint8_t **out,
                 size_t *out_len) {
  *out = NULL;
  *out_len = 0;
  GhosttySelection sel = GHOSTTY_INIT_SIZED(GhosttySelection);
  bool have_sel = true;
  int r;
  if (all) {
    // No value means nothing to select. go-libghostty hands back no
    // selection then, and the formatter formats the whole buffer.
    r = (int)ghostty_terminal_select_all(v->term, &sel);
    if (r == GHOSTTY_NO_VALUE) {
      have_sel = false;
    } else if (r != GHOSTTY_SUCCESS) {
      return r;
    }
  } else {
    uint16_t cols = 0, rows = 0;
    if (ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_COLS, &cols) !=
            GHOSTTY_SUCCESS ||
        ghostty_terminal_get(v->term, GHOSTTY_TERMINAL_DATA_ROWS, &rows) !=
            GHOSTTY_SUCCESS) {
      cols = 0;
      rows = 0;
    }
    if (cols == 0 || rows == 0) {
      return 100;
    }
    GhosttyPoint p = {0};
    p.tag = GHOSTTY_POINT_TAG_ACTIVE;
    p.value.coordinate.x = 0;
    p.value.coordinate.y = 0;
    GhosttyGridRef start = GHOSTTY_INIT_SIZED(GhosttyGridRef);
    r = (int)ghostty_terminal_grid_ref(v->term, p, &start);
    if (r != GHOSTTY_SUCCESS) {
      return r;
    }
    p.value.coordinate.x = (uint16_t)(cols - 1);
    p.value.coordinate.y = (uint32_t)(rows - 1);
    GhosttyGridRef end = GHOSTTY_INIT_SIZED(GhosttyGridRef);
    r = (int)ghostty_terminal_grid_ref(v->term, p, &end);
    if (r != GHOSTTY_SUCCESS) {
      return r;
    }
    sel.start = start;
    sel.end = end;
    sel.rectangle = false;
  }
  GhosttyFormatterTerminalOptions opts =
      GHOSTTY_INIT_SIZED(GhosttyFormatterTerminalOptions);
  opts.extra.size = sizeof(GhosttyFormatterTerminalExtra);
  opts.extra.screen.size = sizeof(GhosttyFormatterScreenExtra);
  opts.emit = GHOSTTY_FORMATTER_FORMAT_PLAIN;
  opts.trim = true;
  opts.unwrap = unwrap;
  opts.selection = have_sel ? &sel : NULL;
  GhosttyFormatter f = NULL;
  r = (int)ghostty_formatter_terminal_new(NULL, &f, v->term, opts);
  if (r != GHOSTTY_SUCCESS) {
    return r;
  }
  r = (int)ghostty_formatter_format_alloc(f, NULL, out, out_len);
  ghostty_formatter_free(f);
  return r;
}

void cop_vt_free_buf(uint8_t *ptr, size_t len) {
  if (ptr != NULL) {
    ghostty_free(NULL, ptr, len);
  }
}

// cop_vt_rows_begin updates the render state from the terminal and points
// the row iterator at its first row.
int cop_vt_rows_begin(cop_vt *v) {
  int r = (int)ghostty_render_state_update(v->rs, v->term);
  if (r != GHOSTTY_SUCCESS) {
    return r;
  }
  return (int)ghostty_render_state_get(
      v->rs, GHOSTTY_RENDER_STATE_DATA_ROW_ITERATOR, &v->ri);
}

bool cop_vt_row_next(cop_vt *v) {
  return ghostty_render_state_row_iterator_next(v->ri);
}

int cop_vt_row_cells(cop_vt *v) {
  return (int)ghostty_render_state_row_get(
      v->ri, GHOSTTY_RENDER_STATE_ROW_DATA_CELLS, &v->rc);
}

bool cop_vt_cell_next(cop_vt *v) {
  return ghostty_render_state_row_cells_next(v->rc);
}

// cop_vt_cell_text writes the cell's grapheme cluster as UTF-8 into buf.
// On GHOSTTY_OUT_OF_SPACE, *len is the size needed.
int cop_vt_cell_text(cop_vt *v, uint8_t *buf, size_t cap, size_t *len) {
  GhosttyBuffer b = {.ptr = buf, .cap = cap, .len = 0};
  int r = (int)ghostty_render_state_row_cells_get(
      v->rc, GHOSTTY_RENDER_STATE_ROW_CELLS_DATA_GRAPHEMES_UTF8, &b);
  *len = b.len;
  return r;
}

// The attribute bits coppice sends on the wire, in this order.
enum {
  COP_BOLD = 1 << 0,
  COP_FAINT = 1 << 1,
  COP_ITALIC = 1 << 2,
  COP_UNDERLINE = 1 << 3,
  COP_BLINK = 1 << 4,
  COP_INVERSE = 1 << 5,
  COP_STRIKE = 1 << 6,
};

// cop_vt_cell_style sets *attrs from the cell's style. A failed read leaves
// *attrs zero and returns the error.
int cop_vt_cell_style(cop_vt *v, uint16_t *attrs) {
  *attrs = 0;
  GhosttyStyle st = GHOSTTY_INIT_SIZED(GhosttyStyle);
  int r = (int)ghostty_render_state_row_cells_get(
      v->rc, GHOSTTY_RENDER_STATE_ROW_CELLS_DATA_STYLE, &st);
  if (r != GHOSTTY_SUCCESS) {
    return r;
  }
  uint16_t a = 0;
  if (st.bold) a |= COP_BOLD;
  if (st.faint) a |= COP_FAINT;
  if (st.italic) a |= COP_ITALIC;
  if (st.underline != GHOSTTY_SGR_UNDERLINE_NONE) a |= COP_UNDERLINE;
  if (st.blink) a |= COP_BLINK;
  if (st.inverse) a |= COP_INVERSE;
  if (st.strikethrough) a |= COP_STRIKE;
  *attrs = a;
  return r;
}

// cop_vt_cell_color reads the foreground (bg 0) or background (bg 1)
// colour as 0xRRGGBB. A cell with the terminal's default colour answers
// GHOSTTY_INVALID_VALUE.
int cop_vt_cell_color(cop_vt *v, int bg, uint32_t *rgb) {
  GhosttyColorRgb c = {0};
  int r = (int)ghostty_render_state_row_cells_get(
      v->rc,
      bg ? GHOSTTY_RENDER_STATE_ROW_CELLS_DATA_BG_COLOR
         : GHOSTTY_RENDER_STATE_ROW_CELLS_DATA_FG_COLOR,
      &c);
  *rgb = ((uint32_t)c.r << 16) | ((uint32_t)c.g << 8) | (uint32_t)c.b;
  return r;
}
