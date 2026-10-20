#include <gtk/gtk.h>
#include <pango/pangocairo.h>
#include <pthread.h>
#include <string.h>

#include "overlay_linux.h"
#include "_cgo_export.h"

// Debounce for monitor changes: plugging a display fires several
// items-changed in a row; the shield is rebuilt once they settle.
#define MONITOR_DEBOUNCE_MS 250

// Frame interval of animated styles (~30 FPS).
#define ANIM_FRAME_MS 33

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

// An animated style attached to one window: a drawing area repainted on a
// timer. Everything style-specific lives on the Go side behind handle; C
// only owns the widget, the timer and the drawing primitives below.
typedef struct {
	GtkWidget *area;
	uintptr_t handle; // Go-side animation (cgo.Handle)
	guint timer;
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

static gboolean anim_tick(gpointer data) {
	anim *a = data;
	goAnimTick(a->handle, (double)gtk_widget_get_width(a->area), (double)gtk_widget_get_height(a->area));
	gtk_widget_queue_draw(a->area);
	return G_SOURCE_CONTINUE;
}

// Stops the timer and hands the Go state back when the window goes away.
static void anim_destroy(GtkWidget *w, gpointer data) {
	(void)w;
	anim *a = data;
	if (a->timer != 0) {
		g_source_remove(a->timer);
		a->timer = 0;
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
		a->timer = g_timeout_add(ANIM_FRAME_MS, anim_tick, a);
		g_signal_connect(w, "destroy", G_CALLBACK(anim_destroy), a);
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
