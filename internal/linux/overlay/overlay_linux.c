#include <gtk/gtk.h>
#include <pango/pangocairo.h>
#include <pthread.h>
#include <string.h>

#include "overlay_linux.h"
#include "_cgo_export.h"

// Debounce for monitor changes: plugging a display fires several
// items-changed in a row; the shield is rebuilt once they settle.
#define MONITOR_DEBOUNCE_MS 250


static pthread_t ui_thread;
static GMainLoop *loop;
static gulong monitors_handler;
static guint debounce_source;

int overlay_init(void) {
	ui_thread = pthread_self();
	g_set_prgname("dndmode");
	if (!gtk_init_check()) {
		return -1;
	}
	GtkCssProvider *css = gtk_css_provider_new();
	gtk_css_provider_load_from_string(css,
		"window.dndmode-shield { background-color: #000000; }");
	gtk_style_context_add_provider_for_display(gdk_display_get_default(),
		GTK_STYLE_PROVIDER(css), GTK_STYLE_PROVIDER_PRIORITY_APPLICATION);
	g_object_unref(css);
	loop = g_main_loop_new(NULL, FALSE);
	return 0;
}

int overlay_on_main_thread(void) {
	return pthread_equal(pthread_self(), ui_thread);
}

static gboolean idle_cb(gpointer data) {
	goOverlayIdle((uintptr_t)data);
	return G_SOURCE_REMOVE;
}

void overlay_dispatch(uintptr_t handle) {
	g_idle_add_full(G_PRIORITY_HIGH_IDLE, idle_cb, (gpointer)handle, NULL);
}

void overlay_run(void) { g_main_loop_run(loop); }

void overlay_quit(void) { g_main_loop_quit(loop); }

int overlay_iterate(void) { return g_main_context_iteration(NULL, FALSE); }

int overlay_monitor_count(void) {
	GListModel *mons = gdk_display_get_monitors(gdk_display_get_default());
	return (int)g_list_model_get_n_items(mons);
}

// The shield must not be closable by the compositor or a stray
// close-window binding.
static gboolean refuse_close(GtkWindow *w, gpointer data) {
	(void)w;
	(void)data;
	return TRUE;
}

// An animated style attached to one window: a drawing area advanced on
// every frame of the GTK frame clock, i.e. in step with the display refresh
// rather than on a free-running timer that drifts against it and judders,
// and repainted whenever the Go side reports that the frame changed. Everything style-specific lives on the Go side behind
// handle; C only owns the widget, the tick and the drawing primitives below.
typedef struct {
	GtkWidget *area;
	uintptr_t handle;  // Go-side animation (cgo.Handle)
	guint tick;        // tick callback id
	gint64 last_frame; // frame time of the previous tick, microseconds
} anim;

static void anim_draw(GtkDrawingArea *area, cairo_t *cr, int w, int h, gpointer data) {
	(void)area;
	(void)w;
	(void)h;
	anim *a = data;
	cairo_set_source_rgb(cr, 0, 0, 0);
	cairo_paint(cr);
	goAnimDraw(a->handle, cr);
}

static gboolean anim_tick(GtkWidget *area, GdkFrameClock *clock, gpointer data) {
	anim *a = data;
	gint64 now = gdk_frame_clock_get_frame_time(clock);
	double dt = a->last_frame == 0 ? 0.0 : (double)(now - a->last_frame) / G_USEC_PER_SEC;
	a->last_frame = now;
	if (goAnimTick(a->handle, (double)gtk_widget_get_width(area), (double)gtk_widget_get_height(area), dt)) {
		gtk_widget_queue_draw(area);
	}
	return G_SOURCE_CONTINUE;
}

// Stops the ticks and hands the Go state back when the drawing area goes
// away (it is destroyed with its window).
static void anim_destroy(GtkWidget *area, gpointer data) {
	anim *a = data;
	if (a->tick != 0) {
		gtk_widget_remove_tick_callback(area, a->tick);
		a->tick = 0;
	}
	goAnimFree(a->handle);
	g_free(a);
}

// stencil paints the current source colour through an A8 mask, scaled into
// the box (x, y, w, h).
static void stencil(cairo_t *cr, cairo_surface_t *mask, double x, double y, double w, double h,
	double r, double g, double b) {
	cairo_save(cr);
	cairo_set_source_rgb(cr, r, g, b);
	cairo_translate(cr, x, y);
	double sx, sy;
	cairo_surface_get_device_scale(mask, &sx, &sy);
	cairo_scale(cr, w / (cairo_image_surface_get_width(mask) / sx),
		h / (cairo_image_surface_get_height(mask) / sy));
	cairo_mask_surface(cr, mask, 0, 0);
	cairo_restore(cr);
}

// --- dvd ----------------------------------------------------------------

static cairo_surface_t *dvd_mask;

void overlay_set_dvd_mask(const unsigned char *alpha, int w, int h) {
	if (dvd_mask != NULL) {
		return;
	}
	cairo_surface_t *s = cairo_image_surface_create(CAIRO_FORMAT_A8, w, h);
	int stride = cairo_image_surface_get_stride(s);
	unsigned char *dst = cairo_image_surface_get_data(s);
	cairo_surface_flush(s);
	for (int y = 0; y < h; y++) {
		memcpy(dst + (size_t)y * stride, alpha + (size_t)y * w, (size_t)w);
	}
	cairo_surface_mark_dirty(s);
	dvd_mask = s;
}

void overlay_dvd_draw(cairo_t *cr, double x, double y, double w, double h, double r, double g, double b) {
	if (dvd_mask != NULL && w > 0 && h > 0) {
		stencil(cr, dvd_mask, x, y, w, h, r, g, b);
	}
}

// --- matrix -------------------------------------------------------------

// Glyph atlas: every glyph pre-rendered once per size as an A8 mask, then
// recoloured per frame with stencil — no text layout in the frame loop.
// Rendered at 2x so it stays crisp on HiDPI monitors.
#define ATLAS_SCALE 2

static cairo_surface_t **atlas;
static int atlas_sizes, atlas_glyphs;

int overlay_matrix_atlas(const double *sizes, int nsizes, const char *const *glyphs, int nglyphs) {
	if (atlas != NULL) {
		return 0;
	}
	cairo_surface_t **a = g_new0(cairo_surface_t *, (gsize)nsizes * nglyphs);
	for (int s = 0; s < nsizes; s++) {
		PangoFontDescription *fd = pango_font_description_from_string("Monospace Medium");
		pango_font_description_set_absolute_size(fd, sizes[s] * ATLAS_SCALE * PANGO_SCALE);
		int cw = (int)(sizes[s] * ATLAS_SCALE * 1.2) + 1;
		int ch = (int)(sizes[s] * ATLAS_SCALE * 1.4) + 1;
		for (int g = 0; g < nglyphs; g++) {
			cairo_surface_t *m = cairo_image_surface_create(CAIRO_FORMAT_A8, cw, ch);
			cairo_t *cr = cairo_create(m);
			PangoLayout *l = pango_cairo_create_layout(cr);
			pango_layout_set_font_description(l, fd);
			pango_layout_set_text(l, glyphs[g], -1);
			pango_cairo_show_layout(cr, l);
			g_object_unref(l);
			cairo_destroy(cr);
			cairo_surface_set_device_scale(m, ATLAS_SCALE, ATLAS_SCALE);
			a[s * nglyphs + g] = m;
		}
		pango_font_description_free(fd);
	}
	atlas = a;
	atlas_sizes = nsizes;
	atlas_glyphs = nglyphs;
	return 0;
}

void overlay_matrix_glyph(cairo_t *cr, int size, int glyph, double x, double y, double r, double g, double b) {
	if (atlas == NULL || size < 0 || size >= atlas_sizes || glyph < 0 || glyph >= atlas_glyphs) {
		return;
	}
	cairo_surface_t *m = atlas[size * atlas_glyphs + glyph];
	cairo_save(cr);
	cairo_set_source_rgb(cr, r, g, b);
	cairo_mask_surface(cr, m, x, y);
	cairo_restore(cr);
}

// --- terminal -----------------------------------------------------------

static PangoContext *term_ctx;
static PangoFontDescription *term_font;

void overlay_term_init(double size, double *cell_w, double *line_h) {
	if (term_ctx == NULL) {
		term_ctx = pango_font_map_create_context(pango_cairo_font_map_get_default());
		term_font = pango_font_description_from_string("Monospace");
		pango_font_description_set_absolute_size(term_font, size * PANGO_SCALE);
	}
	PangoLayout *l = pango_layout_new(term_ctx);
	pango_layout_set_font_description(l, term_font);
	pango_layout_set_text(l, "M", -1);
	PangoRectangle logical;
	pango_layout_get_extents(l, NULL, &logical);
	*cell_w = (double)logical.width / PANGO_SCALE;
	*line_h = (double)logical.height / PANGO_SCALE;
	g_object_unref(l);
}

void *overlay_term_layout(const char *text, const int *starts, const int *lens, const double *rgb, int n) {
	PangoLayout *l = pango_layout_new(term_ctx);
	pango_layout_set_font_description(l, term_font);
	pango_layout_set_text(l, text, -1);
	PangoAttrList *attrs = pango_attr_list_new();
	for (int i = 0; i < n; i++) {
		PangoAttribute *a = pango_attr_foreground_new(
			(guint16)(rgb[3 * i] * 65535), (guint16)(rgb[3 * i + 1] * 65535), (guint16)(rgb[3 * i + 2] * 65535));
		a->start_index = (guint)starts[i];
		a->end_index = (guint)(starts[i] + lens[i]);
		pango_attr_list_insert(attrs, a);
	}
	pango_layout_set_attributes(l, attrs);
	pango_attr_list_unref(attrs);
	return l;
}

// Draws the layout at (x, y), showing only the columns between clip_x and
// clip_x + clip_w: the code column, or the typed part of the bottom line.
void overlay_term_draw(cairo_t *cr, void *layout, double x, double y, double clip_x, double clip_w) {
	if (layout == NULL || clip_w <= 0) {
		return;
	}
	cairo_save(cr);
	cairo_rectangle(cr, clip_x, y, clip_w, 1e6);
	cairo_clip(cr);
	cairo_move_to(cr, x, y);
	pango_cairo_show_layout(cr, layout);
	cairo_restore(cr);
}

void overlay_term_free(void *layout) {
	if (layout != NULL) {
		g_object_unref(layout);
	}
}

void overlay_fill_rect(cairo_t *cr, double x, double y, double w, double h, double r, double g, double b) {
	cairo_set_source_rgb(cr, r, g, b);
	cairo_rectangle(cr, x, y, w, h);
	cairo_fill(cr);
}

// --- gold ---------------------------------------------------------------

static cairo_surface_t **gold_sprites;
static int gold_sprite_n, gold_sprite_px;

void overlay_gold_sprites(const uint32_t *argb, int n, int px) {
	if (gold_sprites != NULL) {
		return;
	}
	cairo_surface_t **s = g_new0(cairo_surface_t *, n);
	for (int i = 0; i < n; i++) {
		cairo_surface_t *img = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, px, px);
		cairo_surface_flush(img);
		int stride = cairo_image_surface_get_stride(img);
		unsigned char *dst = cairo_image_surface_get_data(img);
		for (int y = 0; y < px; y++) {
			memcpy(dst + (size_t)y * stride, argb + ((size_t)i * px + y) * px, (size_t)px * 4);
		}
		cairo_surface_mark_dirty(img);
		s[i] = img;
	}
	gold_sprites = s;
	gold_sprite_n = n;
	gold_sprite_px = px;
}

void overlay_gold_coins(cairo_t *cr, const double *xys, const int *sprite, int n) {
	if (gold_sprites == NULL) {
		return;
	}
	double px = gold_sprite_px;
	for (int i = 0; i < n; i++) {
		if (sprite[i] < 0 || sprite[i] >= gold_sprite_n) {
			continue;
		}
		cairo_save(cr);
		cairo_translate(cr, xys[3 * i], xys[3 * i + 1]);
		cairo_scale(cr, xys[3 * i + 2] / px, xys[3 * i + 2] / px);
		cairo_set_source_surface(cr, gold_sprites[sprite[i]], 0, 0);
		cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_NEAREST);
		cairo_rectangle(cr, 0, 0, px, px);
		cairo_fill(cr);
		cairo_restore(cr);
	}
}

// --- sprite sets and canvases -------------------------------------------

typedef struct {
	int n;
	cairo_surface_t **s;
} sprite_set;

void *overlay_sprites_new(const uint32_t *argb, const int *wh, int n) {
	sprite_set *set = g_new0(sprite_set, 1);
	set->n = n;
	set->s = g_new0(cairo_surface_t *, n);
	size_t off = 0;
	for (int i = 0; i < n; i++) {
		int w = wh[2 * i], h = wh[2 * i + 1];
		if (w <= 0 || h <= 0) {
			continue;
		}
		cairo_surface_t *img = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, w, h);
		cairo_surface_flush(img);
		int stride = cairo_image_surface_get_stride(img);
		unsigned char *dst = cairo_image_surface_get_data(img);
		for (int y = 0; y < h; y++) {
			memcpy(dst + (size_t)y * stride, argb + off + (size_t)y * w, (size_t)w * 4);
		}
		cairo_surface_mark_dirty(img);
		set->s[i] = img;
		off += (size_t)w * h;
	}
	return set;
}

void overlay_sprites_free(void *p) {
	sprite_set *set = p;
	if (set == NULL) {
		return;
	}
	for (int i = 0; i < set->n; i++) {
		if (set->s[i] != NULL) {
			cairo_surface_destroy(set->s[i]);
		}
	}
	g_free(set->s);
	g_free(set);
}

void *overlay_canvas_new(int w, int h) {
	return cairo_image_surface_create(CAIRO_FORMAT_RGB24, w > 0 ? w : 1, h > 0 ? h : 1);
}

void overlay_canvas_free(void *canvas) {
	if (canvas != NULL) {
		cairo_surface_destroy(canvas);
	}
}

void overlay_canvas_paint(void *canvas, void *p, const int *ops, int n) {
	sprite_set *set = p;
	if (canvas == NULL || set == NULL) {
		return;
	}
	cairo_t *cr = cairo_create(canvas);
	for (int i = 0; i < n; i++) {
		const int *op = ops + 6 * i;
		if (op[0] < 0 || op[0] >= set->n || set->s[op[0]] == NULL || op[5] <= 0) {
			continue;
		}
		cairo_surface_t *s = set->s[op[0]];
		int sw = cairo_image_surface_get_width(s), sh = cairo_image_surface_get_height(s);
		cairo_save(cr);
		cairo_translate(cr, op[1], op[2]);
		if (op[3] != sw || op[4] != sh) {
			cairo_scale(cr, (double)op[3] / sw, (double)op[4] / sh);
		}
		cairo_set_source_surface(cr, s, 0, 0);
		cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_NEAREST);
		cairo_rectangle(cr, 0, 0, sw, sh);
		if (op[5] >= 255) {
			cairo_fill(cr);
		} else {
			cairo_clip(cr);
			cairo_paint_with_alpha(cr, op[5] / 255.0);
		}
		cairo_restore(cr);
	}
	cairo_destroy(cr);
	cairo_surface_flush(canvas);
}

void overlay_canvas_show(cairo_t *cr, void *canvas, double x, double y, double scale) {
	if (canvas == NULL) {
		return;
	}
	cairo_save(cr);
	cairo_translate(cr, x, y);
	cairo_scale(cr, scale, scale);
	cairo_set_source_surface(cr, canvas, 0, 0);
	cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_NEAREST);
	cairo_paint(cr);
	cairo_restore(cr);
}

void overlay_line(cairo_t *cr, double x0, double y0, double x1, double y1, double width,
	double r, double g, double b, double a, double dash) {
	cairo_save(cr);
	cairo_set_source_rgba(cr, r, g, b, a);
	cairo_set_line_width(cr, width);
	if (dash > 0) {
		cairo_set_dash(cr, &dash, 1, 0);
	}
	cairo_move_to(cr, x0, y0);
	cairo_line_to(cr, x1, y1);
	cairo_stroke(cr);
	cairo_restore(cr);
}

void overlay_polyline(cairo_t *cr, const double *xy, int n, double width, double r, double g, double b, double a) {
	if (n < 2) {
		return;
	}
	cairo_save(cr);
	cairo_set_source_rgba(cr, r, g, b, a);
	cairo_set_line_width(cr, width);
	cairo_set_line_join(cr, CAIRO_LINE_JOIN_ROUND);
	cairo_move_to(cr, xy[0], xy[1]);
	for (int i = 1; i < n; i++) {
		cairo_line_to(cr, xy[2 * i], xy[2 * i + 1]);
	}
	cairo_stroke(cr);
	cairo_restore(cr);
}

void overlay_round_rect(cairo_t *cr, double x, double y, double w, double h, double radius,
	double r, double g, double b) {
	cairo_new_sub_path(cr);
	cairo_arc(cr, x + w - radius, y + radius, radius, -G_PI / 2, 0);
	cairo_arc(cr, x + w - radius, y + h - radius, radius, 0, G_PI / 2);
	cairo_arc(cr, x + radius, y + h - radius, radius, G_PI / 2, G_PI);
	cairo_arc(cr, x + radius, y + radius, radius, G_PI, 3 * G_PI / 2);
	cairo_close_path(cr);
	cairo_set_source_rgb(cr, r, g, b);
	cairo_fill(cr);
}

void overlay_clip(cairo_t *cr, double x, double y, double w, double h) {
	cairo_save(cr);
	cairo_rectangle(cr, x, y, w, h);
	cairo_clip(cr);
}

void overlay_unclip(cairo_t *cr) { cairo_restore(cr); }

// --- text ---------------------------------------------------------------

typedef struct {
	PangoFontDescription *desc;
	PangoAttrList *attrs; // tabular digits, so changing prices do not jitter
} font;

static PangoContext *text_ctx;

void *overlay_font_new(const char *family, int weight, double size) {
	if (text_ctx == NULL) {
		text_ctx = pango_font_map_create_context(pango_cairo_font_map_get_default());
	}
	font *f = g_new0(font, 1);
	f->desc = pango_font_description_new();
	pango_font_description_set_family(f->desc, family);
	pango_font_description_set_weight(f->desc, (PangoWeight)weight);
	pango_font_description_set_absolute_size(f->desc, size * PANGO_SCALE);
	f->attrs = pango_attr_list_new();
	pango_attr_list_insert(f->attrs, pango_attr_font_features_new("tnum"));
	return f;
}

void overlay_font_free(void *fp) {
	font *f = fp;
	if (f == NULL) {
		return;
	}
	pango_font_description_free(f->desc);
	pango_attr_list_unref(f->attrs);
	g_free(f);
}

static PangoLayout *text_layout(font *f, const char *text) {
	PangoLayout *l = pango_layout_new(text_ctx);
	pango_layout_set_font_description(l, f->desc);
	pango_layout_set_attributes(l, f->attrs);
	pango_layout_set_text(l, text, -1);
	return l;
}

void overlay_text_size(void *fp, const char *text, double *w, double *h) {
	PangoLayout *l = text_layout(fp, text);
	PangoRectangle logical;
	pango_layout_get_extents(l, NULL, &logical);
	*w = (double)logical.width / PANGO_SCALE;
	*h = (double)logical.height / PANGO_SCALE;
	g_object_unref(l);
}

// cairo has no blur, so the macOS shadow halo is approximated by a few
// strokes of the glyph outlines, wide and faint to narrow and darker.
void overlay_text(cairo_t *cr, void *fp, const char *text, double x, double y,
	double r, double g, double b, double halo) {
	PangoLayout *l = text_layout(fp, text);
	cairo_save(cr);
	if (halo > 0) {
		static const double widths[] = {1.5, 1.0, 0.5};
		static const double alphas[] = {0.15, 0.25, 0.4};
		cairo_move_to(cr, x, y);
		pango_cairo_layout_path(cr, l);
		cairo_set_line_join(cr, CAIRO_LINE_JOIN_ROUND);
		for (int i = 0; i < 3; i++) {
			cairo_set_source_rgba(cr, 0, 0, 0, alphas[i]);
			cairo_set_line_width(cr, halo * widths[i]);
			cairo_stroke_preserve(cr);
		}
		cairo_new_path(cr);
	}
	cairo_set_source_rgb(cr, r, g, b);
	cairo_move_to(cr, x, y);
	pango_cairo_show_layout(cr, l);
	cairo_restore(cr);
	g_object_unref(l);
}

void *overlay_window_create(int i, const char *startup_id, uintptr_t animation) {
	GListModel *mons = gdk_display_get_monitors(gdk_display_get_default());
	GdkMonitor *mon = g_list_model_get_item(mons, (guint)i);
	if (mon == NULL) {
		return NULL;
	}
	GtkWidget *w = gtk_window_new();
	gtk_window_set_title(GTK_WINDOW(w), "dndmode");
	gtk_window_set_decorated(GTK_WINDOW(w), FALSE);
	gtk_widget_add_css_class(w, "dndmode-shield");
	gtk_widget_set_cursor_from_name(w, "none");
	g_signal_connect(w, "close-request", G_CALLBACK(refuse_close), NULL);
	if (animation != 0) {
		anim *a = g_new0(anim, 1);
		a->handle = animation;
		a->area = gtk_drawing_area_new();
		gtk_widget_set_cursor_from_name(a->area, "none");
		gtk_drawing_area_set_draw_func(GTK_DRAWING_AREA(a->area), anim_draw, a, NULL);
		gtk_window_set_child(GTK_WINDOW(w), a->area);
		a->tick = gtk_widget_add_tick_callback(a->area, anim_tick, a, NULL);
		// On the area, not the window: GTK destroys the child before the
		// window's own destroy signal runs, so by then a->area is gone.
		g_signal_connect(a->area, "destroy", G_CALLBACK(anim_destroy), a);
	}
	if (startup_id != NULL && startup_id[0] != '\0') {
		gtk_window_set_startup_id(GTK_WINDOW(w), startup_id);
	}
	gtk_window_fullscreen_on_monitor(GTK_WINDOW(w), mon);
	gtk_window_present(GTK_WINDOW(w));
	g_object_unref(mon);
	return w;
}

void overlay_window_destroy(void *w) {
	if (w != NULL) {
		gtk_window_destroy(GTK_WINDOW(w));
	}
}

static gboolean debounce_fire(gpointer data) {
	(void)data;
	debounce_source = 0;
	goOverlayMonitorsChanged();
	return G_SOURCE_REMOVE;
}

static void monitors_changed(GListModel *m, guint pos, guint removed, guint added, gpointer data) {
	(void)m;
	(void)pos;
	(void)removed;
	(void)added;
	(void)data;
	if (debounce_source != 0) {
		g_source_remove(debounce_source);
	}
	debounce_source = g_timeout_add(MONITOR_DEBOUNCE_MS, debounce_fire, NULL);
}

void overlay_watch_monitors(int on) {
	GListModel *mons = gdk_display_get_monitors(gdk_display_get_default());
	if (on && monitors_handler == 0) {
		monitors_handler = g_signal_connect(mons, "items-changed", G_CALLBACK(monitors_changed), NULL);
	} else if (!on && monitors_handler != 0) {
		g_signal_handler_disconnect(mons, monitors_handler);
		monitors_handler = 0;
		if (debounce_source != 0) {
			g_source_remove(debounce_source);
			debounce_source = 0;
		}
	}
}

void overlay_beep(void) { gdk_display_beep(gdk_display_get_default()); }
