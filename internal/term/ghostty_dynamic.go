//go:build libghostty && cgo && (linux || darwin)

package term

/*
#cgo linux LDFLAGS: -ldl
#cgo CFLAGS: -std=c11

#include <stdbool.h>
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <dlfcn.h>

// This bridge intentionally declares only the frozen/value portions of the
// public libghostty-vt C ABI used by shenmux. Symbols are resolved at runtime,
// so the normal shenmux binary does not gain a hard loader dependency. The
// declarations correspond to Ghostty main at the API revision documented in
// docs/GHOSTTY.md; every sized struct sets its size before use.

typedef int GhosttyResult;
typedef struct GhosttyTerminalImpl* GhosttyTerminal;
typedef struct GhosttyRenderStateImpl* GhosttyRenderState;
typedef struct GhosttyRenderStateRowIteratorImpl* GhosttyRenderStateRowIterator;
typedef struct GhosttyRenderStateRowCellsImpl* GhosttyRenderStateRowCells;
typedef uint64_t GhosttyCell;

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

enum {
    SMX_GHOSTTY_SUCCESS = 0,
    SMX_GHOSTTY_OUT_OF_MEMORY = -1,
    SMX_GHOSTTY_INVALID_VALUE = -2,
    SMX_GHOSTTY_OUT_OF_SPACE = -3,
    SMX_GHOSTTY_NO_VALUE = -4,

    SMX_OPT_USERDATA = 0,
    SMX_OPT_WRITE_PTY = 1,
    SMX_OPT_BELL = 2,
    SMX_OPT_TITLE_CHANGED = 5,
    SMX_OPT_SIZE = 6,
    SMX_OPT_KITTY_IMAGE_STORAGE_LIMIT = 15,
    SMX_OPT_APC_MAX_BYTES = 19,
    SMX_OPT_PWD_CHANGED = 25,
    SMX_OPT_CLIPBOARD_WRITE = 26,
    SMX_OPT_SCROLLBACK_MAX_BYTES = 27,
    SMX_OPT_SCROLLBACK_MAX_LINES = 28,
    SMX_OPT_DESKTOP_NOTIFICATION = 29,
    SMX_OPT_PROGRESS_REPORT = 30,

    SMX_TERM_DATA_ACTIVE_SCREEN = 6,
    SMX_TERM_DATA_TITLE = 12,
    SMX_TERM_DATA_PWD = 13,

    SMX_SCREEN_ALTERNATE = 1,

    SMX_RENDER_DATA_COLS = 1,
    SMX_RENDER_DATA_ROWS = 2,
    SMX_RENDER_DATA_ROW_ITERATOR = 4,
    SMX_RENDER_DATA_CURSOR_VISUAL_STYLE = 10,
    SMX_RENDER_DATA_CURSOR_VISIBLE = 11,
    SMX_RENDER_DATA_CURSOR_BLINKING = 12,
    SMX_RENDER_DATA_CURSOR_VIEWPORT_HAS_VALUE = 14,
    SMX_RENDER_DATA_CURSOR_VIEWPORT_X = 15,
    SMX_RENDER_DATA_CURSOR_VIEWPORT_Y = 16,

    SMX_RENDER_ROW_DATA_CELLS = 3,
    SMX_RENDER_CELL_DATA_RAW = 1,
    SMX_RENDER_CELL_DATA_STYLE = 2,
    SMX_RENDER_CELL_DATA_BG_COLOR = 5,
    SMX_RENDER_CELL_DATA_FG_COLOR = 6,
    SMX_RENDER_CELL_DATA_GRAPHEMES_UTF8 = 9,

    SMX_CELL_DATA_WIDE = 3,
    SMX_CELL_WIDE_NARROW = 0,
    SMX_CELL_WIDE_WIDE = 1,
    SMX_CELL_WIDE_SPACER_TAIL = 2,
    SMX_CELL_WIDE_SPACER_HEAD = 3,

    SMX_CURSOR_BAR = 0,
    SMX_CURSOR_BLOCK = 1,
    SMX_CURSOR_UNDERLINE = 2,
    SMX_CURSOR_HOLLOW_BLOCK = 3,

    SMX_CLIPBOARD_DENIED = 1,
    SMX_MAX_CELL_BYTES = 64,
    SMX_MAX_METADATA_BYTES = 4096,
    SMX_MAX_PTY_RESPONSE_BYTES = 1 << 20,
};

typedef GhosttyResult (*smx_terminal_new_fn)(const void*, GhosttyTerminal*, uint16_t, uint16_t);
typedef void (*smx_terminal_free_fn)(GhosttyTerminal);
typedef GhosttyResult (*smx_terminal_resize_fn)(GhosttyTerminal, uint16_t, uint16_t, uint32_t, uint32_t);
typedef GhosttyResult (*smx_terminal_set_fn)(GhosttyTerminal, int, const void*);
typedef void (*smx_terminal_vt_write_fn)(GhosttyTerminal, const uint8_t*, size_t);
typedef GhosttyResult (*smx_terminal_get_fn)(GhosttyTerminal, int, void*);
typedef GhosttyResult (*smx_render_state_new_fn)(const void*, GhosttyRenderState*);
typedef void (*smx_render_state_free_fn)(GhosttyRenderState);
typedef GhosttyResult (*smx_render_state_update_fn)(GhosttyRenderState, GhosttyTerminal);
typedef GhosttyResult (*smx_render_state_get_fn)(GhosttyRenderState, int, void*);
typedef GhosttyResult (*smx_render_state_colors_get_fn)(GhosttyRenderState, GhosttyRenderStateColors*);
typedef GhosttyResult (*smx_row_iterator_new_fn)(const void*, GhosttyRenderStateRowIterator*);
typedef void (*smx_row_iterator_free_fn)(GhosttyRenderStateRowIterator);
typedef bool (*smx_row_iterator_next_fn)(GhosttyRenderStateRowIterator);
typedef GhosttyResult (*smx_row_get_fn)(GhosttyRenderStateRowIterator, int, void*);
typedef GhosttyResult (*smx_row_cells_new_fn)(const void*, GhosttyRenderStateRowCells*);
typedef void (*smx_row_cells_free_fn)(GhosttyRenderStateRowCells);
typedef bool (*smx_row_cells_next_fn)(GhosttyRenderStateRowCells);
typedef GhosttyResult (*smx_row_cells_get_fn)(GhosttyRenderStateRowCells, int, void*);
typedef GhosttyResult (*smx_cell_get_fn)(GhosttyCell, int, void*);

typedef struct {
    void* library;
    smx_terminal_new_fn terminal_new;
    smx_terminal_free_fn terminal_free;
    smx_terminal_resize_fn terminal_resize;
    smx_terminal_set_fn terminal_set;
    smx_terminal_vt_write_fn terminal_vt_write;
    smx_terminal_get_fn terminal_get;
    smx_render_state_new_fn render_state_new;
    smx_render_state_free_fn render_state_free;
    smx_render_state_update_fn render_state_update;
    smx_render_state_get_fn render_state_get;
    smx_render_state_colors_get_fn render_state_colors_get;
    smx_row_iterator_new_fn row_iterator_new;
    smx_row_iterator_free_fn row_iterator_free;
    smx_row_iterator_next_fn row_iterator_next;
    smx_row_get_fn row_get;
    smx_row_cells_new_fn row_cells_new;
    smx_row_cells_free_fn row_cells_free;
    smx_row_cells_next_fn row_cells_next;
    smx_row_cells_get_fn row_cells_get;
    smx_cell_get_fn cell_get;
} SmxGhosttyAPI;

typedef struct {
    uint8_t text[SMX_MAX_CELL_BYTES];
    uint8_t text_len;
    uint8_t width;
    uint16_t flags;
    uint8_t fg_valid;
    GhosttyColorRgb fg;
    uint8_t bg_valid;
    GhosttyColorRgb bg;
} SmxGhosttyCell;

typedef struct {
    uint16_t cols;
    uint16_t rows;
    uint16_t cursor_x;
    uint16_t cursor_y;
    uint8_t cursor_visible;
    uint8_t cursor_blink;
    uint8_t cursor_style;
    uint8_t alt_screen;
    GhosttyColorRgb default_fg;
    GhosttyColorRgb default_bg;
    uint8_t* title;
    size_t title_len;
    uint8_t* pwd;
    size_t pwd_len;
    SmxGhosttyCell* cells;
    size_t cells_len;
} SmxGhosttyFrame;

typedef struct {
    SmxGhosttyAPI* api;
    GhosttyTerminal terminal;
    GhosttyRenderState render;
    GhosttyRenderStateRowIterator rows;
    GhosttyRenderStateRowCells cells;
    uint16_t cols;
    uint16_t rows_count;
    uint8_t* pty_response;
    size_t pty_response_len;
    size_t pty_response_cap;
    int response_overflow;
    uint32_t bells;
    uint32_t title_changed;
    uint32_t pwd_changed;
    uint32_t clipboard_writes;
    uint32_t notifications;
    uint32_t progress_reports;
} SmxGhostty;

static void smx_set_error(char* out, size_t out_len, const char* message) {
    if (out == NULL || out_len == 0) return;
    if (message == NULL) message = "unknown libghostty-vt error";
    snprintf(out, out_len, "%s", message);
}

static const char* smx_result_name(GhosttyResult result) {
    switch (result) {
        case SMX_GHOSTTY_SUCCESS: return "success";
        case SMX_GHOSTTY_OUT_OF_MEMORY: return "out of memory";
        case SMX_GHOSTTY_INVALID_VALUE: return "invalid value";
        case SMX_GHOSTTY_OUT_OF_SPACE: return "out of space";
        case SMX_GHOSTTY_NO_VALUE: return "no value";
        default: return "unknown result";
    }
}

static int smx_load_symbol(void* library, void** out, const char* name, char* err, size_t err_len) {
    dlerror();
    *out = dlsym(library, name);
    const char* failure = dlerror();
    if (failure != NULL || *out == NULL) {
        char buffer[512];
        snprintf(buffer, sizeof(buffer), "resolve %s: %s", name, failure == NULL ? "missing symbol" : failure);
        smx_set_error(err, err_len, buffer);
        return -1;
    }
    return 0;
}

#define SMX_LOAD(api, member, symbol) do { \
    if (smx_load_symbol((api)->library, (void**)&((api)->member), (symbol), err, err_len) != 0) goto fail; \
} while (0)

static SmxGhosttyAPI* smx_api_open(const char* requested, char* err, size_t err_len) {
    SmxGhosttyAPI* api = (SmxGhosttyAPI*)calloc(1, sizeof(SmxGhosttyAPI));
    if (api == NULL) {
        smx_set_error(err, err_len, "allocate libghostty-vt API table");
        return NULL;
    }
    const char* env = getenv("SHENMUX_GHOSTTY_VT_LIBRARY");
    const char* names[6];
    size_t count = 0;
    if (requested != NULL && requested[0] != 0) names[count++] = requested;
    if (env != NULL && env[0] != 0 && (count == 0 || strcmp(env, names[0]) != 0)) names[count++] = env;
#if defined(__APPLE__)
    names[count++] = "libghostty-vt.dylib";
    names[count++] = "libghostty-vt.0.dylib";
#else
    names[count++] = "libghostty-vt.so";
    names[count++] = "libghostty-vt.so.0";
#endif
    char attempts[512] = {0};
    for (size_t i = 0; i < count; i++) {
        api->library = dlopen(names[i], RTLD_NOW | RTLD_LOCAL);
        if (api->library != NULL) break;
        const char* failure = dlerror();
        size_t used = strlen(attempts);
        snprintf(attempts + used, sizeof(attempts) - used, "%s%s: %s", used == 0 ? "" : "; ", names[i], failure == NULL ? "load failed" : failure);
    }
    if (api->library == NULL) {
        smx_set_error(err, err_len, attempts[0] == 0 ? "unable to load libghostty-vt" : attempts);
        free(api);
        return NULL;
    }

    SMX_LOAD(api, terminal_new, "ghostty_terminal_new");
    SMX_LOAD(api, terminal_free, "ghostty_terminal_free");
    SMX_LOAD(api, terminal_resize, "ghostty_terminal_resize");
    SMX_LOAD(api, terminal_set, "ghostty_terminal_set");
    SMX_LOAD(api, terminal_vt_write, "ghostty_terminal_vt_write");
    SMX_LOAD(api, terminal_get, "ghostty_terminal_get");
    SMX_LOAD(api, render_state_new, "ghostty_render_state_new");
    SMX_LOAD(api, render_state_free, "ghostty_render_state_free");
    SMX_LOAD(api, render_state_update, "ghostty_render_state_update");
    SMX_LOAD(api, render_state_get, "ghostty_render_state_get");
    SMX_LOAD(api, render_state_colors_get, "ghostty_render_state_colors_get");
    SMX_LOAD(api, row_iterator_new, "ghostty_render_state_row_iterator_new");
    SMX_LOAD(api, row_iterator_free, "ghostty_render_state_row_iterator_free");
    SMX_LOAD(api, row_iterator_next, "ghostty_render_state_row_iterator_next");
    SMX_LOAD(api, row_get, "ghostty_render_state_row_get");
    SMX_LOAD(api, row_cells_new, "ghostty_render_state_row_cells_new");
    SMX_LOAD(api, row_cells_free, "ghostty_render_state_row_cells_free");
    SMX_LOAD(api, row_cells_next, "ghostty_render_state_row_cells_next");
    SMX_LOAD(api, row_cells_get, "ghostty_render_state_row_cells_get");
    SMX_LOAD(api, cell_get, "ghostty_cell_get");
    return api;

fail:
    dlclose(api->library);
    free(api);
    return NULL;
}

static void smx_api_close(SmxGhosttyAPI* api) {
    if (api == NULL) return;
    if (api->library != NULL) dlclose(api->library);
    free(api);
}

static void smx_write_pty(GhosttyTerminal terminal, void* userdata, const uint8_t* data, size_t len) {
    (void)terminal;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge == NULL || data == NULL || len == 0 || bridge->response_overflow) return;
    if (len > SMX_MAX_PTY_RESPONSE_BYTES || bridge->pty_response_len > SMX_MAX_PTY_RESPONSE_BYTES - len) {
        bridge->response_overflow = 1;
        return;
    }
    size_t required = bridge->pty_response_len + len;
    if (required > bridge->pty_response_cap) {
        size_t next = bridge->pty_response_cap == 0 ? 256 : bridge->pty_response_cap;
        while (next < required && next < SMX_MAX_PTY_RESPONSE_BYTES) next *= 2;
        if (next > SMX_MAX_PTY_RESPONSE_BYTES) next = SMX_MAX_PTY_RESPONSE_BYTES;
        uint8_t* resized = (uint8_t*)realloc(bridge->pty_response, next);
        if (resized == NULL) {
            bridge->response_overflow = 1;
            return;
        }
        bridge->pty_response = resized;
        bridge->pty_response_cap = next;
    }
    memcpy(bridge->pty_response + bridge->pty_response_len, data, len);
    bridge->pty_response_len += len;
}

static void smx_bell(GhosttyTerminal terminal, void* userdata) {
    (void)terminal;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->bells++;
}
static void smx_title_changed(GhosttyTerminal terminal, void* userdata) {
    (void)terminal;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->title_changed++;
}
static void smx_pwd_changed(GhosttyTerminal terminal, void* userdata) {
    (void)terminal;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->pwd_changed++;
}
static int smx_clipboard_write(GhosttyTerminal terminal, void* userdata, const void* write) {
    (void)terminal; (void)write;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->clipboard_writes++;
    return SMX_CLIPBOARD_DENIED;
}
static void smx_notification(GhosttyTerminal terminal, void* userdata, const void* notification) {
    (void)terminal; (void)notification;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->notifications++;
}
static void smx_progress(GhosttyTerminal terminal, void* userdata, const void* report) {
    (void)terminal; (void)report;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge != NULL) bridge->progress_reports++;
}
static bool smx_size(GhosttyTerminal terminal, void* userdata, GhosttySizeReportSize* out) {
    (void)terminal;
    SmxGhostty* bridge = (SmxGhostty*)userdata;
    if (bridge == NULL || out == NULL) return false;
    out->rows = bridge->rows_count;
    out->columns = bridge->cols;
    out->cell_width = 0;
    out->cell_height = 0;
    return true;
}

static int smx_set_callback(SmxGhostty* bridge, int option, const void* value, char* err, size_t err_len) {
    GhosttyResult result = bridge->api->terminal_set(bridge->terminal, option, value);
    if (result == SMX_GHOSTTY_SUCCESS) return 0;
    char buffer[256];
    snprintf(buffer, sizeof(buffer), "ghostty_terminal_set option %d: %s (%d)", option, smx_result_name(result), result);
    smx_set_error(err, err_len, buffer);
    return -1;
}

static SmxGhostty* smx_ghostty_new(const char* library, uint16_t cols, uint16_t rows, size_t history_lines, char* err, size_t err_len) {
    SmxGhostty* bridge = (SmxGhostty*)calloc(1, sizeof(SmxGhostty));
    if (bridge == NULL) {
        smx_set_error(err, err_len, "allocate Ghostty bridge");
        return NULL;
    }
    bridge->api = smx_api_open(library, err, err_len);
    if (bridge->api == NULL) goto fail;
    bridge->cols = cols;
    bridge->rows_count = rows;
    GhosttyResult result = bridge->api->terminal_new(NULL, &bridge->terminal, cols, rows);
    if (result != SMX_GHOSTTY_SUCCESS) {
        char buffer[256];
        snprintf(buffer, sizeof(buffer), "ghostty_terminal_new: %s (%d)", smx_result_name(result), result);
        smx_set_error(err, err_len, buffer);
        goto fail;
    }
    result = bridge->api->render_state_new(NULL, &bridge->render);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_new failed"); goto fail; }
    result = bridge->api->row_iterator_new(NULL, &bridge->rows);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_row_iterator_new failed"); goto fail; }
    result = bridge->api->row_cells_new(NULL, &bridge->cells);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_row_cells_new failed"); goto fail; }

    if (smx_set_callback(bridge, SMX_OPT_USERDATA, bridge, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_WRITE_PTY, (const void*)smx_write_pty, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_BELL, (const void*)smx_bell, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_TITLE_CHANGED, (const void*)smx_title_changed, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_PWD_CHANGED, (const void*)smx_pwd_changed, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_SIZE, (const void*)smx_size, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_CLIPBOARD_WRITE, (const void*)smx_clipboard_write, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_DESKTOP_NOTIFICATION, (const void*)smx_notification, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_PROGRESS_REPORT, (const void*)smx_progress, err, err_len) != 0) goto fail;

    uint64_t zero64 = 0;
    size_t apc_limit = 1 << 20;
    size_t scrollback_bytes = 64 << 20;
    if (smx_set_callback(bridge, SMX_OPT_KITTY_IMAGE_STORAGE_LIMIT, &zero64, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_APC_MAX_BYTES, &apc_limit, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_SCROLLBACK_MAX_BYTES, &scrollback_bytes, err, err_len) != 0) goto fail;
    if (smx_set_callback(bridge, SMX_OPT_SCROLLBACK_MAX_LINES, &history_lines, err, err_len) != 0) goto fail;
    return bridge;

fail:
    if (bridge->cells != NULL && bridge->api != NULL) bridge->api->row_cells_free(bridge->cells);
    if (bridge->rows != NULL && bridge->api != NULL) bridge->api->row_iterator_free(bridge->rows);
    if (bridge->render != NULL && bridge->api != NULL) bridge->api->render_state_free(bridge->render);
    if (bridge->terminal != NULL && bridge->api != NULL) bridge->api->terminal_free(bridge->terminal);
    smx_api_close(bridge->api);
    free(bridge->pty_response);
    free(bridge);
    return NULL;
}

static void smx_ghostty_free(SmxGhostty* bridge) {
    if (bridge == NULL) return;
    if (bridge->api != NULL) {
        if (bridge->cells != NULL) bridge->api->row_cells_free(bridge->cells);
        if (bridge->rows != NULL) bridge->api->row_iterator_free(bridge->rows);
        if (bridge->render != NULL) bridge->api->render_state_free(bridge->render);
        if (bridge->terminal != NULL) bridge->api->terminal_free(bridge->terminal);
    }
    smx_api_close(bridge->api);
    free(bridge->pty_response);
    free(bridge);
}

static int smx_ghostty_feed(SmxGhostty* bridge, const uint8_t* data, size_t len, char* err, size_t err_len) {
    if (bridge == NULL || bridge->terminal == NULL) { smx_set_error(err, err_len, "Ghostty bridge is closed"); return -1; }
    bridge->pty_response_len = 0;
    bridge->response_overflow = 0;
    bridge->bells = 0;
    bridge->title_changed = 0;
    bridge->pwd_changed = 0;
    bridge->clipboard_writes = 0;
    bridge->notifications = 0;
    bridge->progress_reports = 0;
    if (data != NULL && len != 0) bridge->api->terminal_vt_write(bridge->terminal, data, len);
    if (bridge->response_overflow) {
        smx_set_error(err, err_len, "terminal PTY responses exceeded 1 MiB in one write batch");
        return -1;
    }
    return 0;
}

static int smx_ghostty_resize(SmxGhostty* bridge, uint16_t cols, uint16_t rows, char* err, size_t err_len) {
    if (bridge == NULL || bridge->terminal == NULL) { smx_set_error(err, err_len, "Ghostty bridge is closed"); return -1; }
    GhosttyResult result = bridge->api->terminal_resize(bridge->terminal, cols, rows, 0, 0);
    if (result != SMX_GHOSTTY_SUCCESS) {
        char buffer[256];
        snprintf(buffer, sizeof(buffer), "ghostty_terminal_resize: %s (%d)", smx_result_name(result), result);
        smx_set_error(err, err_len, buffer);
        return -1;
    }
    bridge->cols = cols;
    bridge->rows_count = rows;
    return 0;
}

static int smx_copy_string(GhosttyString source, uint8_t** out, size_t* out_len, char* err, size_t err_len) {
    *out = NULL;
    *out_len = 0;
    if (source.len == 0 || source.ptr == NULL) return 0;
    size_t len = source.len > SMX_MAX_METADATA_BYTES ? SMX_MAX_METADATA_BYTES : source.len;
    uint8_t* copy = (uint8_t*)malloc(len);
    if (copy == NULL) { smx_set_error(err, err_len, "allocate Ghostty metadata"); return -1; }
    memcpy(copy, source.ptr, len);
    *out = copy;
    *out_len = len;
    return 0;
}

static int smx_ghostty_snapshot(SmxGhostty* bridge, SmxGhosttyFrame* frame, char* err, size_t err_len) {
    if (bridge == NULL || frame == NULL || bridge->terminal == NULL) { smx_set_error(err, err_len, "invalid Ghostty snapshot request"); return -1; }
    memset(frame, 0, sizeof(*frame));
    GhosttyResult result = bridge->api->render_state_update(bridge->render, bridge->terminal);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_update failed"); return -1; }

#define SMX_RENDER_GET(key, target) do { \
    result = bridge->api->render_state_get(bridge->render, (key), (target)); \
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_get failed"); goto fail; } \
} while (0)
#define SMX_TERM_GET(key, target) do { \
    result = bridge->api->terminal_get(bridge->terminal, (key), (target)); \
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_terminal_get failed"); goto fail; } \
} while (0)

    SMX_RENDER_GET(SMX_RENDER_DATA_COLS, &frame->cols);
    SMX_RENDER_GET(SMX_RENDER_DATA_ROWS, &frame->rows);
    if (frame->cols == 0 || frame->rows == 0 || (size_t)frame->cols * (size_t)frame->rows > (1u << 20)) {
        smx_set_error(err, err_len, "Ghostty render dimensions exceed shenmux bounds");
        goto fail;
    }
    bool cursor_visible = false;
    bool cursor_blink = false;
    bool cursor_has_value = false;
    int cursor_style = SMX_CURSOR_BLOCK;
    SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_VISIBLE, &cursor_visible);
    SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_BLINKING, &cursor_blink);
    SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_VIEWPORT_HAS_VALUE, &cursor_has_value);
    SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_VISUAL_STYLE, &cursor_style);
    if (cursor_has_value) {
        SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_VIEWPORT_X, &frame->cursor_x);
        SMX_RENDER_GET(SMX_RENDER_DATA_CURSOR_VIEWPORT_Y, &frame->cursor_y);
    }
    frame->cursor_visible = cursor_visible && cursor_has_value;
    frame->cursor_blink = cursor_blink;
    frame->cursor_style = (uint8_t)cursor_style;

    int active_screen = 0;
    SMX_TERM_GET(SMX_TERM_DATA_ACTIVE_SCREEN, &active_screen);
    frame->alt_screen = active_screen == SMX_SCREEN_ALTERNATE;

    GhosttyRenderStateColors colors;
    memset(&colors, 0, sizeof(colors));
    colors.size = sizeof(colors);
    result = bridge->api->render_state_colors_get(bridge->render, &colors);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "ghostty_render_state_colors_get failed"); goto fail; }
    frame->default_fg = colors.foreground;
    frame->default_bg = colors.background;

    GhosttyString title = {0};
    GhosttyString pwd = {0};
    SMX_TERM_GET(SMX_TERM_DATA_TITLE, &title);
    SMX_TERM_GET(SMX_TERM_DATA_PWD, &pwd);
    if (smx_copy_string(title, &frame->title, &frame->title_len, err, err_len) != 0) goto fail;
    if (smx_copy_string(pwd, &frame->pwd, &frame->pwd_len, err, err_len) != 0) goto fail;

    frame->cells_len = (size_t)frame->cols * (size_t)frame->rows;
    frame->cells = (SmxGhosttyCell*)calloc(frame->cells_len, sizeof(SmxGhosttyCell));
    if (frame->cells == NULL) { smx_set_error(err, err_len, "allocate Ghostty frame cells"); goto fail; }

    result = bridge->api->render_state_get(bridge->render, SMX_RENDER_DATA_ROW_ITERATOR, &bridge->rows);
    if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "populate Ghostty row iterator"); goto fail; }
    for (uint16_t y = 0; y < frame->rows; y++) {
        if (!bridge->api->row_iterator_next(bridge->rows)) { smx_set_error(err, err_len, "Ghostty row iterator ended early"); goto fail; }
        result = bridge->api->row_get(bridge->rows, SMX_RENDER_ROW_DATA_CELLS, &bridge->cells);
        if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "populate Ghostty row cells"); goto fail; }
        for (uint16_t x = 0; x < frame->cols; x++) {
            if (!bridge->api->row_cells_next(bridge->cells)) { smx_set_error(err, err_len, "Ghostty cell iterator ended early"); goto fail; }
            SmxGhosttyCell* cell = &frame->cells[(size_t)y * frame->cols + x];
            GhosttyCell raw = 0;
            result = bridge->api->row_cells_get(bridge->cells, SMX_RENDER_CELL_DATA_RAW, &raw);
            if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "read Ghostty raw cell"); goto fail; }
            int wide = SMX_CELL_WIDE_NARROW;
            result = bridge->api->cell_get(raw, SMX_CELL_DATA_WIDE, &wide);
            if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "read Ghostty cell width"); goto fail; }
            cell->width = wide == SMX_CELL_WIDE_WIDE ? 2 : (wide == SMX_CELL_WIDE_SPACER_TAIL || wide == SMX_CELL_WIDE_SPACER_HEAD ? 0 : 1);

            GhosttyBuffer text = { cell->text, SMX_MAX_CELL_BYTES, 0 };
            result = bridge->api->row_cells_get(bridge->cells, SMX_RENDER_CELL_DATA_GRAPHEMES_UTF8, &text);
            if (result != SMX_GHOSTTY_SUCCESS || text.len > SMX_MAX_CELL_BYTES) { smx_set_error(err, err_len, "read Ghostty cell grapheme"); goto fail; }
            cell->text_len = cell->width == 0 ? 0 : (uint8_t)text.len;

            GhosttyStyle style;
            memset(&style, 0, sizeof(style));
            style.size = sizeof(style);
            result = bridge->api->row_cells_get(bridge->cells, SMX_RENDER_CELL_DATA_STYLE, &style);
            if (result != SMX_GHOSTTY_SUCCESS) { smx_set_error(err, err_len, "read Ghostty cell style"); goto fail; }
            if (style.bold) cell->flags |= 1u << 0;
            if (style.faint) cell->flags |= 1u << 1;
            if (style.italic) cell->flags |= 1u << 2;
            if (style.underline != 0) cell->flags |= 1u << 3;
            if (style.underline == 2) cell->flags |= 1u << 4;
            if (style.blink) cell->flags |= 1u << 5;
            if (style.inverse) cell->flags |= 1u << 6;
            if (style.invisible) cell->flags |= 1u << 7;
            if (style.strikethrough) cell->flags |= 1u << 8;
            if (style.overline) cell->flags |= 1u << 9;

            result = bridge->api->row_cells_get(bridge->cells, SMX_RENDER_CELL_DATA_FG_COLOR, &cell->fg);
            if (result == SMX_GHOSTTY_SUCCESS) cell->fg_valid = 1;
            else if (result != SMX_GHOSTTY_INVALID_VALUE && result != SMX_GHOSTTY_NO_VALUE) { smx_set_error(err, err_len, "read Ghostty cell foreground"); goto fail; }
            result = bridge->api->row_cells_get(bridge->cells, SMX_RENDER_CELL_DATA_BG_COLOR, &cell->bg);
            if (result == SMX_GHOSTTY_SUCCESS) cell->bg_valid = 1;
            else if (result != SMX_GHOSTTY_INVALID_VALUE && result != SMX_GHOSTTY_NO_VALUE) { smx_set_error(err, err_len, "read Ghostty cell background"); goto fail; }
        }
        if (bridge->api->row_cells_next(bridge->cells)) { smx_set_error(err, err_len, "Ghostty row contains more cells than reported columns"); goto fail; }
    }
    if (bridge->api->row_iterator_next(bridge->rows)) { smx_set_error(err, err_len, "Ghostty render state contains more rows than reported"); goto fail; }
    return 0;

fail:
    free(frame->title); frame->title = NULL; frame->title_len = 0;
    free(frame->pwd); frame->pwd = NULL; frame->pwd_len = 0;
    free(frame->cells); frame->cells = NULL; frame->cells_len = 0;
    return -1;
#undef SMX_RENDER_GET
#undef SMX_TERM_GET
}

static void smx_ghostty_frame_free(SmxGhosttyFrame* frame) {
    if (frame == NULL) return;
    free(frame->title);
    free(frame->pwd);
    free(frame->cells);
    memset(frame, 0, sizeof(*frame));
}

static const uint8_t* smx_ghostty_response_ptr(SmxGhostty* bridge) { return bridge == NULL ? NULL : bridge->pty_response; }
static size_t smx_ghostty_response_len(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->pty_response_len; }
static uint32_t smx_ghostty_bells(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->bells; }
static uint32_t smx_ghostty_title_changed(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->title_changed; }
static uint32_t smx_ghostty_pwd_changed(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->pwd_changed; }
static uint32_t smx_ghostty_clipboard_writes(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->clipboard_writes; }
static uint32_t smx_ghostty_notifications(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->notifications; }
static uint32_t smx_ghostty_progress_reports(SmxGhostty* bridge) { return bridge == NULL ? 0 : bridge->progress_reports; }
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

const ghosttyErrorBytes = 1024

// Ghostty is the libghostty-vt implementation of Terminal. All terminal
// mutation, effect collection, and render-state extraction are serialized by
// this mutex. Raw PTY control strings never leave this object.
type Ghostty struct {
	mu     sync.Mutex
	bridge *C.SmxGhostty
	closed bool
}

func NewGhostty(dim shenguard.Dimensions) (*Ghostty, error) {
	return newGhosttyWithLibrary(dim, os.Getenv("SHENMUX_GHOSTTY_VT_LIBRARY"))
}

func newGhosttyWithLibrary(dim shenguard.Dimensions, library string) (*Ghostty, error) {
	if dim.IsZero() {
		return nil, errors.New("Ghostty dimensions must be positive")
	}
	var libraryCString *C.char
	if library != "" {
		libraryCString = C.CString(library)
		defer C.free(unsafe.Pointer(libraryCString))
	}
	errbuf := make([]byte, ghosttyErrorBytes)
	bridge := C.smx_ghostty_new(
		libraryCString,
		C.uint16_t(dim.Cols()), C.uint16_t(dim.Rows()),
		C.size_t(screen.MaxHistoryRows),
		(*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)),
	)
	if bridge == nil {
		return nil, fmt.Errorf("initialize libghostty-vt: %s", cError(errbuf))
	}
	return &Ghostty{bridge: bridge}, nil
}

func (g *Ghostty) Feed(data []byte) (Effects, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.bridge == nil {
		return Effects{}, errors.New("terminal is closed")
	}
	var ptr *C.uint8_t
	if len(data) != 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&data[0]))
	}
	errbuf := make([]byte, ghosttyErrorBytes)
	if C.smx_ghostty_feed(g.bridge, ptr, C.size_t(len(data)), (*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf))) != 0 {
		return Effects{}, fmt.Errorf("libghostty-vt write: %s", cError(errbuf))
	}
	var writes [][]byte
	responseLen := int(C.smx_ghostty_response_len(g.bridge))
	if responseLen != 0 {
		responsePtr := C.smx_ghostty_response_ptr(g.bridge)
		if responsePtr == nil {
			return Effects{}, errors.New("libghostty-vt reported response bytes with nil pointer")
		}
		writes = [][]byte{C.GoBytes(unsafe.Pointer(responsePtr), C.int(responseLen))}
	}
	return Effects{
		PTYWrites:       writes,
		Bells:           uint32(C.smx_ghostty_bells(g.bridge)),
		TitleChanged:    C.smx_ghostty_title_changed(g.bridge) != 0,
		PWDChanged:      C.smx_ghostty_pwd_changed(g.bridge) != 0,
		ClipboardWrites: uint32(C.smx_ghostty_clipboard_writes(g.bridge)),
		Notifications:   uint32(C.smx_ghostty_notifications(g.bridge)),
		ProgressReports: uint32(C.smx_ghostty_progress_reports(g.bridge)),
	}, nil
}

func (g *Ghostty) Resize(dim shenguard.Dimensions) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.bridge == nil {
		return errors.New("terminal is closed")
	}
	errbuf := make([]byte, ghosttyErrorBytes)
	if C.smx_ghostty_resize(g.bridge, C.uint16_t(dim.Cols()), C.uint16_t(dim.Rows()), (*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf))) != 0 {
		return fmt.Errorf("libghostty-vt resize: %s", cError(errbuf))
	}
	return nil
}

func (g *Ghostty) Snapshot() (screen.Frame, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.bridge == nil {
		return screen.Frame{}, errors.New("terminal is closed")
	}
	var raw C.SmxGhosttyFrame
	errbuf := make([]byte, ghosttyErrorBytes)
	if C.smx_ghostty_snapshot(g.bridge, &raw, (*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf))) != 0 {
		return screen.Frame{}, fmt.Errorf("libghostty-vt snapshot: %s", cError(errbuf))
	}
	defer C.smx_ghostty_frame_free(&raw)

	cols, rows := uint16(raw.cols), uint16(raw.rows)
	cellCount := int(raw.cells_len)
	if cols == 0 || rows == 0 || cellCount != int(cols)*int(rows) || cellCount > screen.MaxCells {
		return screen.Frame{}, fmt.Errorf("libghostty-vt returned invalid frame shape %dx%d with %d cells", cols, rows, cellCount)
	}
	cells := unsafe.Slice((*C.SmxGhosttyCell)(unsafe.Pointer(raw.cells)), cellCount)
	lines := make([]screen.Row, int(rows))
	for y := 0; y < int(rows); y++ {
		row := make(screen.Row, int(cols))
		for x := 0; x < int(cols); x++ {
			rawCell := cells[y*int(cols)+x]
			textLen := int(rawCell.text_len)
			if textLen < 0 || textLen > screen.MaxCellBytes {
				return screen.Frame{}, fmt.Errorf("libghostty-vt cell (%d,%d) text length %d exceeds bound", x, y, textLen)
			}
			textBytes := unsafe.Slice((*byte)(unsafe.Pointer(&rawCell.text[0])), screen.MaxCellBytes)
			cell := screen.Cell{Text: string(textBytes[:textLen]), Width: uint8(rawCell.width), Style: ghosttyStyle(uint16(rawCell.flags))}
			if rawCell.fg_valid != 0 {
				cell.Style.FG = screen.Color{Valid: true, R: uint8(rawCell.fg.r), G: uint8(rawCell.fg.g), B: uint8(rawCell.fg.b)}
			}
			if rawCell.bg_valid != 0 {
				cell.Style.BG = screen.Color{Valid: true, R: uint8(rawCell.bg.r), G: uint8(rawCell.bg.g), B: uint8(rawCell.bg.b)}
			}
			row[x] = cell
		}
		lines[y] = row
	}

	cursorStyle, err := ghosttyCursorStyle(uint8(raw.cursor_style))
	if err != nil {
		return screen.Frame{}, err
	}
	frame := screen.Frame{
		Cols: cols, Rows: rows, Lines: lines,
		Cursor: screen.Cursor{
			X: uint16(raw.cursor_x), Y: uint16(raw.cursor_y),
			Visible: raw.cursor_visible != 0, Blink: raw.cursor_blink != 0,
			Style: cursorStyle,
		},
		AltScreen:        raw.alt_screen != 0,
		Title:            sanitizeMetadata(cBytes(raw.title, raw.title_len)),
		WorkingDirectory: sanitizeMetadata(cBytes(raw.pwd, raw.pwd_len)),
		DefaultFG:        screen.Color{Valid: true, R: uint8(raw.default_fg.r), G: uint8(raw.default_fg.g), B: uint8(raw.default_fg.b)},
		DefaultBG:        screen.Color{Valid: true, R: uint8(raw.default_bg.r), G: uint8(raw.default_bg.g), B: uint8(raw.default_bg.b)},
	}
	if err := frame.Validate(); err != nil {
		return screen.Frame{}, fmt.Errorf("validate libghostty-vt frame: %w", err)
	}
	return frame, nil
}

func (g *Ghostty) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if g.bridge != nil {
		C.smx_ghostty_free(g.bridge)
		g.bridge = nil
	}
	return nil
}

func ghosttyStyle(flags uint16) screen.Style {
	return screen.Style{
		Bold: flags&(1<<0) != 0, Faint: flags&(1<<1) != 0,
		Italic: flags&(1<<2) != 0, Underline: flags&(1<<3) != 0,
		DoubleUnderline: flags&(1<<4) != 0, Blink: flags&(1<<5) != 0,
		Inverse: flags&(1<<6) != 0, Invisible: flags&(1<<7) != 0,
		Strikethrough: flags&(1<<8) != 0, Overline: flags&(1<<9) != 0,
	}
}

func ghosttyCursorStyle(value uint8) (screen.CursorStyle, error) {
	switch value {
	case C.SMX_CURSOR_BAR:
		return screen.CursorBar, nil
	case C.SMX_CURSOR_BLOCK:
		return screen.CursorBlock, nil
	case C.SMX_CURSOR_UNDERLINE:
		return screen.CursorUnderline, nil
	case C.SMX_CURSOR_HOLLOW_BLOCK:
		return screen.CursorHollowBlock, nil
	default:
		return 0, fmt.Errorf("libghostty-vt returned unknown cursor style %d", value)
	}
}

func cBytes(ptr *C.uint8_t, length C.size_t) string {
	if ptr == nil || length == 0 {
		return ""
	}
	if uint64(length) > screen.MaxTitleBytes {
		length = screen.MaxTitleBytes
	}
	return string(C.GoBytes(unsafe.Pointer(ptr), C.int(length)))
}

func cError(buffer []byte) string {
	for i, value := range buffer {
		if value == 0 {
			if i == 0 {
				return "unknown error"
			}
			return string(buffer[:i])
		}
	}
	return string(buffer)
}
