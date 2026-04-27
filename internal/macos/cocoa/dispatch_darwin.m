// +build darwin

#import <stdint.h>
#import <pthread.h>
#import <dispatch/dispatch.h>
#import "_cgo_export.h"   // generated header for //export-ed Go functions

// cocoa_is_main_thread returns 1 if the calling thread is the process's main
// thread (the thread that ran main()), 0 otherwise.
//
// pthread_main_np is documented at usr/include/pthread.h:539 and is the
// canonical Apple-supported test for "are we on main". It is a single C call
// with no Objective-C dispatch overhead — preferred over [NSThread isMainThread]
// (the design notes).
int cocoa_is_main_thread(void) {
    return pthread_main_np();
}

// cocoa_dispatch_main schedules the goCocoaDispatchCallback Go function to
// run on the main thread (via dispatch_get_main_queue()). The handle is an
// opaque uintptr representing a runtime/cgo.Handle that boxes the original
// Go closure passed to DispatchMain. The Go callback un-boxes and invokes
// the closure, then deletes the handle.
//
// dispatch_async returns immediately; the block runs when the main runloop
// next processes its queue. If the main runloop is stopped before the block
// is dequeued, the closure leaks (the design notes mitigated by Phase 2
// design: Release runs inline, debouncer.Stop cancels pending blocks).
void cocoa_dispatch_main(uintptr_t handle) {
    dispatch_async(dispatch_get_main_queue(), ^{
        goCocoaDispatchCallback(handle);
    });
}
