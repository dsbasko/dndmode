#include <gtk/gtk.h>
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
static cairo_surface_t *dvd_mask;
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

// dvd animation attached to one window's drawing area.
typedef struct {
	GtkWidget *area;
	uintptr_t handle; // Go-side state (cgo.Handle)
	guint timer;
	dvd_frame frame;
} dvd_anim;

static void dvd_draw(GtkDrawingArea *area, cairo_t *cr, int w, int h, gpointer data) {
	(void)area;
	(void)w;
	(void)h;
	dvd_anim *a = data;
	cairo_set_source_rgb(cr, 0, 0, 0);
	cairo_paint(cr);
	if (dvd_mask == NULL || a->frame.w <= 0 || a->frame.h <= 0) {
		return;
	}
	cairo_set_source_rgb(cr, a->frame.r, a->frame.g, a->frame.b);
	cairo_translate(cr, a->frame.x, a->frame.y);
	cairo_scale(cr, a->frame.w / cairo_image_surface_get_width(dvd_mask),
		a->frame.h / cairo_image_surface_get_height(dvd_mask));
	cairo_mask_surface(cr, dvd_mask, 0, 0);
}

static gboolean dvd_tick(gpointer data) {
	dvd_anim *a = data;
	int w = gtk_widget_get_width(a->area);
	int h = gtk_widget_get_height(a->area);
	goDVDTick(a->handle, (double)w, (double)h, &a->frame);
	gtk_widget_queue_draw(a->area);
	return G_SOURCE_CONTINUE;
}

// Stops the timer and hands the Go state back when the window goes away.
static void dvd_destroy(GtkWidget *w, gpointer data) {
	(void)w;
	dvd_anim *a = data;
	if (a->timer != 0) {
		g_source_remove(a->timer);
		a->timer = 0;
	}
	goDVDFree(a->handle);
	g_free(a);
}

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

void *overlay_window_create(int i, const char *startup_id, int style, uintptr_t anim) {
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
	if (style == OVERLAY_STYLE_DVD) {
		dvd_anim *a = g_new0(dvd_anim, 1);
		a->handle = anim;
		a->area = gtk_drawing_area_new();
		gtk_widget_set_cursor_from_name(a->area, "none");
		gtk_drawing_area_set_draw_func(GTK_DRAWING_AREA(a->area), dvd_draw, a, NULL);
		gtk_window_set_child(GTK_WINDOW(w), a->area);
		a->timer = g_timeout_add(ANIM_FRAME_MS, dvd_tick, a);
		g_signal_connect(w, "destroy", G_CALLBACK(dvd_destroy), a);
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
