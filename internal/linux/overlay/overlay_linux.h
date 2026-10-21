#ifndef DNDMODE_OVERLAY_LINUX_H
#define DNDMODE_OVERLAY_LINUX_H

#include <stdint.h>

// Initialises GTK on the calling thread, which becomes the UI thread.
// Returns 0 on success, -1 when no display can be opened.
int overlay_init(void);

// Non-zero when called on the UI thread.
int overlay_on_main_thread(void);

// Schedules goOverlayIdle(handle) on the UI thread.
void overlay_dispatch(uintptr_t handle);

// Runs the main loop until overlay_quit. Returns when it stops.
void overlay_run(void);
void overlay_quit(void);

// Runs one non-blocking main-context iteration; returns non-zero if
// something was dispatched.
int overlay_iterate(void);

// Number of monitors currently attached.
int overlay_monitor_count(void);

#include <cairo.h>

// Creates a fullscreen shield window on monitor i. startup_id may be NULL.
// animation is the Go handle of the window's animation, or 0 for plain
// black. Returns NULL when the monitor has gone away.
void *overlay_window_create(int i, const char *startup_id, uintptr_t animation);

// dvd: installs the logo as an alpha mask (w*h bytes, 255 = paint) once,
// then draws it recoloured into a box.
void overlay_set_dvd_mask(const unsigned char *alpha, int w, int h);
void overlay_dvd_draw(cairo_t *cr, double x, double y, double w, double h, double r, double g, double b);

// matrix: renders every glyph at every size into the atlas once, then draws
// atlas entries recoloured at (x, y).
int overlay_matrix_atlas(const double *sizes, int nsizes, const char *const *glyphs, int nglyphs);
void overlay_matrix_glyph(cairo_t *cr, int size, int glyph, double x, double y, double r, double g, double b);

// terminal: corpus access (terminalcorpus_linux.c) and text drawing.
int overlay_term_block_count(int lang);
int overlay_term_block_len(int lang, int block);
const char *overlay_term_line(int lang, int block, int line);
// Sets up the monospaced font once; reports the cell width and line height.
void overlay_term_init(double size, double *cell_w, double *line_h);
// A layout of text coloured per segment (byte ranges, rgb triples).
void *overlay_term_layout(const char *text, const int *starts, const int *lens, const double *rgb, int n);
void overlay_term_draw(cairo_t *cr, void *layout, double x, double y, double clip_x, double clip_w);
void overlay_term_free(void *layout);
void overlay_fill_rect(cairo_t *cr, double x, double y, double w, double h, double r, double g, double b);

void overlay_window_destroy(void *w);

// Starts / stops forwarding monitor changes (debounced) to
// goOverlayMonitorsChanged.
void overlay_watch_monitors(int on);

void overlay_beep(void);

#endif
