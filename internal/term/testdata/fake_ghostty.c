#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#define GHOSTTY_SUCCESS 0
#define GHOSTTY_INVALID_VALUE -2
#define GHOSTTY_OUT_OF_SPACE -3
#define GHOSTTY_NO_VALUE -4
#define MAX_TEXT 64

typedef int GhosttyResult;
typedef struct { uint8_t r, g, b; } GhosttyColorRgb;
typedef struct { const uint8_t* ptr; size_t len; } GhosttyString;
typedef struct { uint8_t* ptr; size_t cap; size_t len; } GhosttyBuffer;
typedef struct {
    uint16_t rows;
    uint16_t columns;
    uint32_t cell_width;
    uint32_t cell_height;
} GhosttySizeReportSize;
typedef union {
    uint8_t palette;
    GhosttyColorRgb rgb;
    uint64_t _padding;
} GhosttyStyleColorValue;
typedef struct {
    int tag;
    GhosttyStyleColorValue value;
} GhosttyStyleColor;
typedef struct {
    size_t size;
    GhosttyStyleColor fg_color;
    GhosttyStyleColor bg_color;
    GhosttyStyleColor underline_color;
    bool bold;
    bool italic;
    bool faint;
    bool blink;
    bool inverse;
    bool invisible;
    bool strikethrough;
    bool overline;
    int underline;
} GhosttyStyle;
typedef struct {
    size_t size;
    GhosttyColorRgb background;
    GhosttyColorRgb foreground;
    GhosttyColorRgb cursor;
    bool cursor_has_value;
    GhosttyColorRgb palette[256];
} GhosttyRenderStateColors;

typedef void (*WritePtyFn)(void*, void*, const uint8_t*, size_t);
typedef void (*SimpleFn)(void*, void*);
typedef bool (*SizeFn)(void*, void*, GhosttySizeReportSize*);
typedef int (*ClipboardFn)(void*, void*, const void*);
typedef void (*PayloadFn)(void*, void*, const void*);

typedef struct {
    uint8_t text[MAX_TEXT];
    size_t text_len;
    int wide;
    GhosttyStyle style;
    bool fg_valid;
    GhosttyColorRgb fg;
    bool bg_valid;
    GhosttyColorRgb bg;
} FakeCell;

typedef struct GhosttyTerminalImpl {
    uint16_t cols;
    uint16_t rows;
    uint16_t cursor_x;
    uint16_t cursor_y;
    bool cursor_visible;
    bool cursor_blink;
    int cursor_style;
    int active_screen;
    FakeCell* cells;
    uint8_t title[128];
    size_t title_len;
    uint8_t pwd[128];
    size_t pwd_len;
    void* userdata;
    WritePtyFn write_pty;
    SimpleFn bell;
    SimpleFn title_changed;
    SizeFn size;
    SimpleFn pwd_changed;
    ClipboardFn clipboard;
    PayloadFn notification;
    PayloadFn progress;
} *GhosttyTerminal;

typedef struct GhosttyRenderStateImpl {
    GhosttyTerminal terminal;
} *GhosttyRenderState;

typedef struct GhosttyRenderStateRowIteratorImpl {
    GhosttyRenderState render;
    int row;
} *GhosttyRenderStateRowIterator;

typedef struct GhosttyRenderStateRowCellsImpl {
    GhosttyRenderStateRowIterator rows;
    int col;
} *GhosttyRenderStateRowCells;

typedef uint64_t GhosttyCell;

static bool contains(const uint8_t* haystack, size_t haystack_len,
                     const uint8_t* needle, size_t needle_len) {
    if (needle_len == 0) return true;
    if (haystack == NULL || haystack_len < needle_len) return false;
    for (size_t i = 0; i <= haystack_len - needle_len; i++) {
        if (memcmp(haystack + i, needle, needle_len) == 0) return true;
    }
    return false;
}

static void set_text(FakeCell* cell, const char* text) {
    size_t len = strlen(text);
    if (len > MAX_TEXT) len = MAX_TEXT;
    memset(cell, 0, sizeof(*cell));
    memcpy(cell->text, text, len);
    cell->text_len = len;
    cell->wide = 0;
    cell->style.size = sizeof(cell->style);
}

GhosttyResult ghostty_terminal_new(const void* allocator, GhosttyTerminal* out,
                                   uint16_t cols, uint16_t rows) {
    (void)allocator;
    if (out == NULL || cols == 0 || rows == 0) return GHOSTTY_INVALID_VALUE;
    GhosttyTerminal terminal = calloc(1, sizeof(*terminal));
    if (terminal == NULL) return -1;
    terminal->cells = calloc((size_t)cols * rows, sizeof(FakeCell));
    if (terminal->cells == NULL) { free(terminal); return -1; }
    terminal->cols = cols;
    terminal->rows = rows;
    terminal->cursor_visible = true;
    terminal->cursor_blink = true;
    terminal->cursor_style = 1; // Ghostty block.
    for (size_t i = 0; i < (size_t)cols * rows; i++) set_text(&terminal->cells[i], "");
    *out = terminal;
    return GHOSTTY_SUCCESS;
}

void ghostty_terminal_free(GhosttyTerminal terminal) {
    if (terminal == NULL) return;
    free(terminal->cells);
    free(terminal);
}

GhosttyResult ghostty_terminal_resize(GhosttyTerminal terminal,
                                      uint16_t cols, uint16_t rows,
                                      uint32_t cell_width_px,
                                      uint32_t cell_height_px) {
    (void)cell_width_px;
    (void)cell_height_px;
    if (terminal == NULL || cols == 0 || rows == 0) return GHOSTTY_INVALID_VALUE;
    FakeCell* next = calloc((size_t)cols * rows, sizeof(FakeCell));
    if (next == NULL) return -1;
    for (size_t i = 0; i < (size_t)cols * rows; i++) set_text(&next[i], "");
    uint16_t copy_rows = rows < terminal->rows ? rows : terminal->rows;
    uint16_t copy_cols = cols < terminal->cols ? cols : terminal->cols;
    for (uint16_t y = 0; y < copy_rows; y++) {
        for (uint16_t x = 0; x < copy_cols; x++) {
            next[(size_t)y * cols + x] = terminal->cells[(size_t)y * terminal->cols + x];
        }
    }
    free(terminal->cells);
    terminal->cells = next;
    terminal->cols = cols;
    terminal->rows = rows;
    if (terminal->cursor_x >= cols) terminal->cursor_x = cols - 1;
    if (terminal->cursor_y >= rows) terminal->cursor_y = rows - 1;
    return GHOSTTY_SUCCESS;
}

GhosttyResult ghostty_terminal_set(GhosttyTerminal terminal, int option,
                                   const void* value) {
    if (terminal == NULL) return GHOSTTY_INVALID_VALUE;
    switch (option) {
        case 0: terminal->userdata = (void*)value; return GHOSTTY_SUCCESS;
        case 1: terminal->write_pty = (WritePtyFn)value; return GHOSTTY_SUCCESS;
        case 2: terminal->bell = (SimpleFn)value; return GHOSTTY_SUCCESS;
        case 5: terminal->title_changed = (SimpleFn)value; return GHOSTTY_SUCCESS;
        case 6: terminal->size = (SizeFn)value; return GHOSTTY_SUCCESS;
        case 15: // Kitty storage limit.
        case 19: // APC limit.
        case 27: // Scrollback byte limit.
        case 28: // Scrollback line limit.
            return value == NULL ? GHOSTTY_INVALID_VALUE : GHOSTTY_SUCCESS;
        case 25: terminal->pwd_changed = (SimpleFn)value; return GHOSTTY_SUCCESS;
        case 26: terminal->clipboard = (ClipboardFn)value; return GHOSTTY_SUCCESS;
        case 29: terminal->notification = (PayloadFn)value; return GHOSTTY_SUCCESS;
        case 30: terminal->progress = (PayloadFn)value; return GHOSTTY_SUCCESS;
        default: return GHOSTTY_INVALID_VALUE;
    }
}

static void put_ascii(GhosttyTerminal terminal, uint8_t value) {
    if (terminal->cursor_x >= terminal->cols) {
        terminal->cursor_x = 0;
        if (terminal->cursor_y + 1 < terminal->rows) terminal->cursor_y++;
    }
    set_text(&terminal->cells[(size_t)terminal->cursor_y * terminal->cols + terminal->cursor_x], (char[2]){(char)value, 0});
    terminal->cursor_x++;
    if (terminal->cursor_x >= terminal->cols) {
        terminal->cursor_x = 0;
        if (terminal->cursor_y + 1 < terminal->rows) terminal->cursor_y++;
    }
}

static size_t skip_control_string(const uint8_t* data, size_t len, size_t start) {
    for (size_t i = start; i < len; i++) {
        if (data[i] == 7) return i;
        if (data[i] == 0x1b && i + 1 < len && data[i + 1] == '\\') return i + 1;
    }
    return len == 0 ? 0 : len - 1;
}

void ghostty_terminal_vt_write(GhosttyTerminal terminal,
                               const uint8_t* data, size_t len) {
    if (terminal == NULL || data == NULL) return;
    for (size_t i = 0; i < len; i++) {
        uint8_t value = data[i];
        if (value == 0x1b && i + 1 < len) {
            uint8_t next = data[++i];
            if (next == '[') {
                while (i + 1 < len) {
                    uint8_t c = data[++i];
                    if (c >= 0x40 && c <= 0x7e) break;
                }
            } else if (next == ']' || next == 'P' || next == '_' || next == '^') {
                i = skip_control_string(data, len, i + 1);
            }
            continue;
        }
        if (value == 7) {
            if (terminal->bell != NULL) terminal->bell(terminal, terminal->userdata);
            continue;
        }
        if (value == '\r') { terminal->cursor_x = 0; continue; }
        if (value == '\n') {
            if (terminal->cursor_y + 1 < terminal->rows) terminal->cursor_y++;
            continue;
        }
        if (value == '\b') {
            if (terminal->cursor_x > 0) terminal->cursor_x--;
            continue;
        }
        if (value == '\t') {
            do { put_ascii(terminal, ' '); } while (terminal->cursor_x % 8 != 0);
            continue;
        }
        if (value >= 32 && value < 127) put_ascii(terminal, value);
    }
    if (len >= 2 && data[0] == 'H' && data[1] == 'i') {
        terminal->cells[0].style.bold = true;
        terminal->cells[0].style.italic = true;
        terminal->cells[0].style.underline = 2;
        terminal->cells[0].fg_valid = true;
        terminal->cells[0].fg = (GhosttyColorRgb){1, 2, 3};
        terminal->cells[0].bg_valid = true;
        terminal->cells[0].bg = (GhosttyColorRgb){4, 5, 6};
    }
    static const uint8_t query[] = "\x1b[6n";
    if (contains(data, len, query, sizeof(query) - 1) && terminal->write_pty != NULL) {
        static const uint8_t response[] = "\x1b[1;3R";
        terminal->write_pty(terminal, terminal->userdata, response, sizeof(response) - 1);
    }
    static const uint8_t title[] = "\x1b]2;ghost-title\x1b\\";
    if (contains(data, len, title, sizeof(title) - 1)) {
        static const uint8_t value[] = "ghost-title";
        memcpy(terminal->title, value, sizeof(value) - 1);
        terminal->title_len = sizeof(value) - 1;
        if (terminal->title_changed != NULL) terminal->title_changed(terminal, terminal->userdata);
    }
    static const uint8_t pwd[] = "\x1b]7;file:///tmp/demo\x1b\\";
    if (contains(data, len, pwd, sizeof(pwd) - 1)) {
        static const uint8_t value[] = "file:///tmp/demo";
        memcpy(terminal->pwd, value, sizeof(value) - 1);
        terminal->pwd_len = sizeof(value) - 1;
        if (terminal->pwd_changed != NULL) terminal->pwd_changed(terminal, terminal->userdata);
    }
    static const uint8_t clipboard[] = "\x1b]52;c;SGVsbG8=\x1b\\";
    if (contains(data, len, clipboard, sizeof(clipboard) - 1) && terminal->clipboard != NULL) {
        terminal->clipboard(terminal, terminal->userdata, NULL);
    }
    static const uint8_t notification[] = "\x1b]9;hello\x1b\\";
    if (contains(data, len, notification, sizeof(notification) - 1) && terminal->notification != NULL) {
        terminal->notification(terminal, terminal->userdata, NULL);
    }
    static const uint8_t progress[] = "\x1b]9;4;1;50\x1b\\";
    if (contains(data, len, progress, sizeof(progress) - 1) && terminal->progress != NULL) {
        terminal->progress(terminal, terminal->userdata, NULL);
    }
    static const uint8_t alt[] = "\x1b[?1049h";
    if (contains(data, len, alt, sizeof(alt) - 1)) terminal->active_screen = 1;
}

GhosttyResult ghostty_terminal_get(GhosttyTerminal terminal, int data, void* out) {
    if (terminal == NULL || out == NULL) return GHOSTTY_INVALID_VALUE;
    switch (data) {
        case 6: *(int*)out = terminal->active_screen; return GHOSTTY_SUCCESS;
        case 12: *(GhosttyString*)out = (GhosttyString){terminal->title, terminal->title_len}; return GHOSTTY_SUCCESS;
        case 13: *(GhosttyString*)out = (GhosttyString){terminal->pwd, terminal->pwd_len}; return GHOSTTY_SUCCESS;
        default: return GHOSTTY_INVALID_VALUE;
    }
}

GhosttyResult ghostty_render_state_new(const void* allocator, GhosttyRenderState* out) {
    (void)allocator;
    if (out == NULL) return GHOSTTY_INVALID_VALUE;
    *out = calloc(1, sizeof(**out));
    return *out == NULL ? -1 : GHOSTTY_SUCCESS;
}

void ghostty_render_state_free(GhosttyRenderState state) { free(state); }

GhosttyResult ghostty_render_state_update(GhosttyRenderState state, GhosttyTerminal terminal) {
    if (state == NULL || terminal == NULL) return GHOSTTY_INVALID_VALUE;
    state->terminal = terminal;
    return GHOSTTY_SUCCESS;
}

GhosttyResult ghostty_render_state_get(GhosttyRenderState state, int data, void* out) {
    if (state == NULL || state->terminal == NULL || out == NULL) return GHOSTTY_INVALID_VALUE;
    GhosttyTerminal terminal = state->terminal;
    switch (data) {
        case 1: *(uint16_t*)out = terminal->cols; return GHOSTTY_SUCCESS;
        case 2: *(uint16_t*)out = terminal->rows; return GHOSTTY_SUCCESS;
        case 4: {
            GhosttyRenderStateRowIterator* slot = out;
            if (*slot == NULL) return GHOSTTY_INVALID_VALUE;
            (*slot)->render = state;
            (*slot)->row = -1;
            return GHOSTTY_SUCCESS;
        }
        case 10: *(int*)out = terminal->cursor_style; return GHOSTTY_SUCCESS;
        case 11: *(bool*)out = terminal->cursor_visible; return GHOSTTY_SUCCESS;
        case 12: *(bool*)out = terminal->cursor_blink; return GHOSTTY_SUCCESS;
        case 14: *(bool*)out = true; return GHOSTTY_SUCCESS;
        case 15: *(uint16_t*)out = terminal->cursor_x; return GHOSTTY_SUCCESS;
        case 16: *(uint16_t*)out = terminal->cursor_y; return GHOSTTY_SUCCESS;
        default: return GHOSTTY_INVALID_VALUE;
    }
}

GhosttyResult ghostty_render_state_colors_get(GhosttyRenderState state,
                                               GhosttyRenderStateColors* out) {
    if (state == NULL || out == NULL || out->size < sizeof(size_t)) return GHOSTTY_INVALID_VALUE;
    size_t caller_size = out->size;
    memset(out, 0, caller_size < sizeof(*out) ? caller_size : sizeof(*out));
    out->size = caller_size;
    out->background = (GhosttyColorRgb){10, 11, 12};
    out->foreground = (GhosttyColorRgb){230, 231, 232};
    out->cursor = (GhosttyColorRgb){200, 201, 202};
    out->cursor_has_value = true;
    for (int i = 0; i < 256; i++) out->palette[i] = (GhosttyColorRgb){(uint8_t)i, (uint8_t)i, (uint8_t)i};
    return GHOSTTY_SUCCESS;
}

GhosttyResult ghostty_render_state_row_iterator_new(const void* allocator,
                                                     GhosttyRenderStateRowIterator* out) {
    (void)allocator;
    if (out == NULL) return GHOSTTY_INVALID_VALUE;
    *out = calloc(1, sizeof(**out));
    if (*out == NULL) return -1;
    (*out)->row = -1;
    return GHOSTTY_SUCCESS;
}

void ghostty_render_state_row_iterator_free(GhosttyRenderStateRowIterator iterator) { free(iterator); }

bool ghostty_render_state_row_iterator_next(GhosttyRenderStateRowIterator iterator) {
    if (iterator == NULL || iterator->render == NULL || iterator->render->terminal == NULL) return false;
    if (iterator->row + 1 >= iterator->render->terminal->rows) return false;
    iterator->row++;
    return true;
}

GhosttyResult ghostty_render_state_row_get(GhosttyRenderStateRowIterator iterator,
                                           int data, void* out) {
    if (iterator == NULL || iterator->render == NULL || out == NULL || data != 3) return GHOSTTY_INVALID_VALUE;
    GhosttyRenderStateRowCells* slot = out;
    if (*slot == NULL) return GHOSTTY_INVALID_VALUE;
    (*slot)->rows = iterator;
    (*slot)->col = -1;
    return GHOSTTY_SUCCESS;
}

GhosttyResult ghostty_render_state_row_cells_new(const void* allocator,
                                                  GhosttyRenderStateRowCells* out) {
    (void)allocator;
    if (out == NULL) return GHOSTTY_INVALID_VALUE;
    *out = calloc(1, sizeof(**out));
    if (*out == NULL) return -1;
    (*out)->col = -1;
    return GHOSTTY_SUCCESS;
}

void ghostty_render_state_row_cells_free(GhosttyRenderStateRowCells cells) { free(cells); }

bool ghostty_render_state_row_cells_next(GhosttyRenderStateRowCells cells) {
    if (cells == NULL || cells->rows == NULL || cells->rows->render == NULL) return false;
    if (cells->col + 1 >= cells->rows->render->terminal->cols) return false;
    cells->col++;
    return true;
}

GhosttyResult ghostty_render_state_row_cells_get(GhosttyRenderStateRowCells cells,
                                                  int data, void* out) {
    if (cells == NULL || cells->rows == NULL || cells->rows->render == NULL || out == NULL) return GHOSTTY_INVALID_VALUE;
    GhosttyTerminal terminal = cells->rows->render->terminal;
    if (cells->rows->row < 0 || cells->rows->row >= terminal->rows || cells->col < 0 || cells->col >= terminal->cols) return GHOSTTY_INVALID_VALUE;
    FakeCell* cell = &terminal->cells[(size_t)cells->rows->row * terminal->cols + cells->col];
    switch (data) {
        case 1:
            *(uint64_t*)out = ((uint64_t)(uint8_t)cell->wide << 56) |
                              ((uint64_t)(uint32_t)cells->rows->row << 32) |
                              (uint32_t)cells->col;
            return GHOSTTY_SUCCESS;
        case 2: {
            GhosttyStyle* style = out;
            size_t caller_size = style->size;
            if (caller_size < sizeof(size_t)) return GHOSTTY_INVALID_VALUE;
            GhosttyStyle copy = cell->style;
            copy.size = caller_size;
            memcpy(style, &copy, caller_size < sizeof(copy) ? caller_size : sizeof(copy));
            return GHOSTTY_SUCCESS;
        }
        case 5:
            if (!cell->bg_valid) return GHOSTTY_INVALID_VALUE;
            *(GhosttyColorRgb*)out = cell->bg;
            return GHOSTTY_SUCCESS;
        case 6:
            if (!cell->fg_valid) return GHOSTTY_NO_VALUE;
            *(GhosttyColorRgb*)out = cell->fg;
            return GHOSTTY_SUCCESS;
        case 9: {
            GhosttyBuffer* buffer = out;
            if (buffer->cap < cell->text_len || (cell->text_len != 0 && buffer->ptr == NULL)) {
                buffer->len = cell->text_len;
                return GHOSTTY_OUT_OF_SPACE;
            }
            if (cell->text_len != 0) memcpy(buffer->ptr, cell->text, cell->text_len);
            buffer->len = cell->text_len;
            return GHOSTTY_SUCCESS;
        }
        default: return GHOSTTY_INVALID_VALUE;
    }
}

GhosttyResult ghostty_cell_get(GhosttyCell cell, int data, void* out) {
    if (out == NULL || data != 3) return GHOSTTY_INVALID_VALUE;
    *(int*)out = (int)((cell >> 56) & 0xff);
    return GHOSTTY_SUCCESS;
}
