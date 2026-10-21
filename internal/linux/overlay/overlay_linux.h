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

// gold: installs the coin sprites once (n premultiplied ARGB32 images of
// px*px pixels), then draws n coins in one call, far to near: xys holds the
// top-left and side of each, sprite its sprite index. Nearest-neighbour, so
// the pixel art stays crisp.
void overlay_gold_sprites(const uint32_t *argb, int n, int px);
void overlay_gold_coins(cairo_t *cr, const double *xys, const int *sprite, int n);

// Shapes. dash 0 strokes a solid line; xy holds n points.
void overlay_line(cairo_t *cr, double x0, double y0, double x1, double y1, double width,
	double r, double g, double b, double a, double dash);
void overlay_polyline(cairo_t *cr, const double *xy, int n, double width, double r, double g, double b, double a);
void overlay_round_rect(cairo_t *cr, double x, double y, double w, double h, double radius,
	double r, double g, double b);
// overlay_clip saves the context and clips to the box; overlay_unclip restores it.
void overlay_clip(cairo_t *cr, double x, double y, double w, double h);
void overlay_unclip(cairo_t *cr);

// Text in a font of one family, weight (PangoWeight) and absolute size,
// with tabular digits. Drawn with its top-left at (x, y); halo > 0 first
// strokes a dark glow that wide around the glyphs.
void *overlay_font_new(const char *family, int weight, double size);
void overlay_font_free(void *font);
void overlay_text_size(void *font, const char *text, double *w, double *h);
void overlay_text(cairo_t *cr, void *font, const char *text, double x, double y,
	double r, double g, double b, double halo);

void overlay_window_destroy(void *w);

// Starts / stops forwarding monitor changes (debounced) to
// goOverlayMonitorsChanged.
void overlay_watch_monitors(int on);

void overlay_beep(void);

#endif
