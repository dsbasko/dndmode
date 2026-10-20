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

// Creates a fullscreen shield window on monitor i. startup_id may be NULL.
// Returns NULL when the monitor has gone away.
void *overlay_window_create(int i, const char *startup_id);
void overlay_window_destroy(void *w);

// Starts / stops forwarding monitor changes (debounced) to
// goOverlayMonitorsChanged.
void overlay_watch_monitors(int on);

void overlay_beep(void);

#endif
