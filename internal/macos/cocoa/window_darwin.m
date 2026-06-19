// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CGDirectDisplay.h>  // shield-level Quartz function lives here, NB: not in CGWindowLevel.h
#import <stdint.h>
#import <string.h>
#import "matrixview_darwin.h"  // @interface MatrixView (cgo compiles each .m as a separate TU, so a bare @class forward decl is not enough)

// kGlassBlurOpacity tunes the "glass" frost strength. NSVisualEffectView's
// behind-window blur has a SYSTEM-FIXED radius (no public API to soften it), so
// we lighten it by lowering the effect view's opacity over a non-opaque window:
// the sharp desktop bleeds partially back through, so the result is
// kGlassBlurOpacity*blurred + (1-kGlassBlurOpacity)*sharp — text stays
// hard-to-read rather than fully erased. 0.0 = no frost (invisible), 1.0 = the
// full strong blur; ~0.5 = light frosted glass.
static const CGFloat kGlassBlurOpacity = 0.5;

// cocoa_create_overlay_window allocates and fully configures one black
// full-screen NSWindow for the NSScreen identified by displayID.
//
// Returns a CoreFoundation-bridged retained handle on success — caller (Go
// side) owns exactly one strong reference and MUST pass the handle to
// cocoa_close_overlay_window exactly once.
//
// On failure returns NULL and writes a strdup'd C string to *outErr; caller
// must free(*outErr) after copying via C.GoString.
//
// Configuration covers (see body for line-tagged mapping). The
// window is shield-level so it sits above menu bar, Dock, Mission Control,
// Spotlight, Cmd+Tab UI and Force Quit dialog. Phase 2 keeps the overlay
// visual-only (ignoresMouseEvents YES); Phase 4 adds CGEventTap for input
// blocking.
//
// `style` selects the overlay content: "matrix" (QUICK-gh8) installs an animated
// MatrixView over an opaque black base; "glass" (QUICK-glass) makes the window
// transparent and installs an NSVisualEffectView that blurs whatever is behind
// it (frosted glass — the ONLY non-opaque style, the desktop shows through
// blurred); for "black", NULL, or anything else the plain opaque-black path is
// untouched. black + matrix keep setOpaque:YES (T-gh8-03 no bleed-through);
// glass deliberately relaxes that for the look while input stays blocked.
//
// Critical pitfall (the design notes): default releasedWhenClosed=YES
// combined with ARC + __bridge_retained ownership causes double-free on
// [w close]. We explicitly disable it immediately after init below.
//
// All exact symbol names for (the shield-level Quartz function,
// the four CollectionBehavior flags, opaque-black background, front-ordering
// without activation) appear exactly once in the body for unambiguous grep
// audits.
void* cocoa_create_overlay_window(uint32_t displayID, const char* style, char** outErr) {
    NSScreen *target = nil;
    for (NSScreen *s in [NSScreen screens]) {
        NSNumber *n = [[s deviceDescription] objectForKey:@"NSScreenNumber"];
        if (n && [n unsignedIntValue] == displayID) {
            target = s;
            break;
        }
    }
    if (!target) {
        if (outErr) *outErr = strdup("no NSScreen matches displayID");
        return NULL;
    }

    NSWindow *w = [[NSWindow alloc]
        initWithContentRect:[target frame]                // full physical frame
                  styleMask:NSWindowStyleMaskBorderless    // 
                    backing:NSBackingStoreBuffered         // 
                      defer:NO                             // 
                     screen:target];

    if (!w) {
        if (outErr) *outErr = strdup("[NSWindow alloc] returned nil");
        return NULL;
    }

    // CRITICAL — fix; must set BEFORE any other configuration.
    [w setReleasedWhenClosed:NO];

    [w setLevel:CGShieldingWindowLevel()];                 // 

    [w setCollectionBehavior:                              // (4 flags)
        NSWindowCollectionBehaviorCanJoinAllSpaces       // 1<<0
      | NSWindowCollectionBehaviorStationary             // 1<<4
      | NSWindowCollectionBehaviorFullScreenAuxiliary    // 1<<8
      | NSWindowCollectionBehaviorIgnoresCycle];         // 1<<6

    [w setHasShadow:NO];                                   // 
    [w setCanHide:NO];                                     // 
    [w setHidesOnDeactivate:NO];                           // 
    [w setIgnoresMouseEvents:YES];                         // 

    // QUICK-glass: "glass" is the ONE style that is intentionally NOT opaque — a
    // transparent frosted panel that blurs whatever is behind it. Input stays
    // fully blocked (ignoresMouseEvents above + the Phase 4 CGEventTap); only the
    // VISUAL coverage relaxes (the desktop shows through, blurred), so it
    // deliberately trades the T-gh8-03 no-bleed-through guarantee for the look.
    // NSVisualEffectView with BehindWindow blending samples + blurs the content
    // behind the shield window; state=Active forces the blur to render even
    // though the overlay window is never key/active.
    if (style != NULL && strcmp(style, "glass") == 0) {
        [w setOpaque:NO];
        [w setBackgroundColor:[NSColor clearColor]];
        NSVisualEffectView *ve =
            [[NSVisualEffectView alloc] initWithFrame:[[w contentView] bounds]];
        [ve setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
        [ve setBlendingMode:NSVisualEffectBlendingModeBehindWindow];
        [ve setMaterial:NSVisualEffectMaterialFullScreenUI];
        [ve setState:NSVisualEffectStateActive];
        // Soften the system-fixed blur: drop opacity so the sharp desktop bleeds
        // partly back through (kGlassBlurOpacity is the knob; see its comment).
        [ve setAlphaValue:kGlassBlurOpacity];
        [w setContentView:ve];
    } else {
        [w setOpaque:YES];                                 // 
        [w setBackgroundColor:[NSColor blackColor]];       // 
        // QUICK-gh8: matrix swaps in an animated digital-rain contentView over
        // the opaque black base (an opaque layer on top, never transparent). For
        // "black"/NULL/anything else the default black contentView is left
        // untouched (byte-identical black path). MatrixView's @interface comes
        // from matrixview_darwin.h.
        if (style != NULL && strcmp(style, "matrix") == 0) {
            MatrixView *mv = [[MatrixView alloc] initWithFrame:[[w contentView] bounds]];
            [mv setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
            [w setContentView:mv];
        }
    }

    [w orderFrontRegardless];                              // 

    // Transfer ownership to Go side. Go MUST eventually call
    // cocoa_close_overlay_window with this handle to balance the retain.
    return (void*)CFBridgingRetain(w);
}

// cocoa_close_overlay_window orders out + closes the NSWindow and releases
// the strong reference held since cocoa_create_overlay_window returned.
//
// Idempotency contract: calling with NULL is a no-op (silent). Caller is
// responsible for calling exactly once with each non-NULL handle (controller
// uses the windows map + delete-after-close to enforce).
void cocoa_close_overlay_window(void* windowHandle) {
    if (!windowHandle) return;
    NSWindow *w = (__bridge_transfer NSWindow*)windowHandle;
    [w orderOut:nil];
    [w close];
    // ARC drops the bridged reference when this scope exits.
}

// cocoa_window_level returns the level of the NSWindow handle (post-create).
// Used by smoke tests to verify. Returns 0 on NULL handle.
long cocoa_window_level(void* windowHandle) {
    if (!windowHandle) return 0;
    NSWindow *w = (__bridge NSWindow*)windowHandle;  // bridge without ownership transfer
    return (long)[w level];
}

// cocoa_window_is_visible returns 1 if the NSWindow is on screen, 0 else.
// Used by smoke tests to verify (window made it to the front).
int cocoa_window_is_visible(void* windowHandle) {
    if (!windowHandle) return 0;
    NSWindow *w = (__bridge NSWindow*)windowHandle;
    return [w isVisible] ? 1 : 0;
}

// cocoa_window_collection_behavior returns the raw NSWindowCollectionBehavior
// bitmask of the NSWindow handle. Used by smoke tests to verify
// (exactly 4 flags ORed: CanJoinAllSpaces|Stationary|FullScreenAuxiliary|
// IgnoresCycle = (1<<0)|(1<<4)|(1<<8)|(1<<6) = 0x151).
//
// Returns 0 on NULL handle. NSWindowCollectionBehavior is typedef'd as
// NSUInteger; cast to unsigned long for stable cgo bridging.
unsigned long cocoa_window_collection_behavior(void* windowHandle) {
    if (!windowHandle) return 0UL;
    NSWindow *w = (__bridge NSWindow*)windowHandle;
    return (unsigned long)[w collectionBehavior];
}

// cocoa_first_attached_display_id returns the CGDirectDisplayID of the first
// attached NSScreen (deviceDescription[NSScreenNumber] of [NSScreen screens][0]),
// or 0 with *outFound=0 when no screens are attached.
//
// Test-only helper bridging smoke tests to NSScreen enumeration
// without depending on internal/macos/cocoa/screens_darwin.go (parallel
//). Once lands its production-grade enumerateScreens
// helper, this minimal single-screen lookup remains useful as an isolated
// alternative for the low-level NSWindow round-trip layer.
uint32_t cocoa_first_attached_display_id(int* outFound) {
    NSArray<NSScreen*> *screens = [NSScreen screens];
    if (screens == nil || [screens count] == 0) {
        if (outFound) *outFound = 0;
        return 0;
    }
    NSScreen *s = [screens objectAtIndex:0];
    NSNumber *n = [[s deviceDescription] objectForKey:@"NSScreenNumber"];
    if (n == nil) {
        if (outFound) *outFound = 0;
        return 0;
    }
    if (outFound) *outFound = 1;
    return (uint32_t)[n unsignedIntValue];
}
