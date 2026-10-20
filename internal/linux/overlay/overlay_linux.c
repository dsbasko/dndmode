#include <gtk/gtk.h>
#include <pthread.h>

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

void *overlay_window_create(int i, const char *startup_id) {
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
