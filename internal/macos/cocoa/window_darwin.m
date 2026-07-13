// +build darwin

#import <Cocoa/Cocoa.h>
#import <CoreGraphics/CGDirectDisplay.h>  // shield-level Quartz function lives here, NB: not in CGWindowLevel.h
#import <ScreenCaptureKit/ScreenCaptureKit.h>  // SCScreenshotManager one-shot desktop capture (glass)
#import <CoreImage/CoreImage.h>                 // CIGaussianBlur for the glass blur
#import <dispatch/dispatch.h>                   // semaphore: make the async SCK capture synchronous
#import <stdint.h>
#import <string.h>
#import "matrixview_darwin.h"  // @interface MatrixView (cgo compiles each .m as a separate TU, so a bare @class forward decl is not enough)
#import "terminalview_darwin.h"  // @interface TerminalView (same separate-TU rule as MatrixView above)

// kGlassBlurRadius is the Gaussian blur radius (in points) for the "glass" style.
// The overlay grabs a ONE-SHOT screenshot of the desktop (ScreenCaptureKit) and
// blurs it with CIGaussianBlur at this radius, then shows the STATIC blurred
// image. Unlike NSVisualEffectView (fixed, too-large radius that washes shapes
// into a flat frost) this radius is EXACT and tunable — we can sit in the band
// where large shapes stay recognizable but text is unreadable — and unlike a
// live CABackdropLayer a static image cannot "vanish" mid-session (the earlier
// bug). Lower = sharper/more legible, higher = shapes dissolve. ~16 is that band
// for typical body text; drop toward ~8 for more detail, raise toward ~30 to
// erase almost everything. Scaled by the display backing factor at capture time
// so it stays perceptually constant across Retina / non-Retina.
static const CGFloat kGlassBlurRadius = 16.0;

// kGlassTintAlpha is a FAINT flat darkening laid OVER the blurred snapshot so
// text edges do not stay crisp at small radii and the panel reads as "glass".
// 0.0 = none, 1.0 = near-solid black. Keep it low — the blur does the work.
static const CGFloat kGlassTintAlpha = 0.15;

// captureBlurredDesktopImage grabs the current desktop of `target` (the NSScreen
// for displayID) via ScreenCaptureKit, Gaussian-blurs it at `radius` points, and
// returns an NSImage sized to the display's point frame — or nil on ANY failure
// (Screen Recording permission not granted, timeout, SCK/CoreImage error) so the
// caller falls back to the opaque frost and the shield is never left transparent.
//
// It MUST be called on the main thread, BEFORE the overlay window is ordered
// front, so the capture sees the clean desktop (our window is not yet on screen —
// no self-capture, no window to exclude). SCK's two steps are async; we make them
// synchronous with a semaphore bounded by a 2s timeout. This is safe even though
// [NSApp run] has not started yet: SCK runs its completion on its OWN queue, not
// the (un-pumped) main queue, so there is no deadlock — and if it ever did land
// on main, the timeout still releases us into the frost fallback.
static NSImage *captureBlurredDesktopImage(uint32_t displayID, NSScreen *target,
                                           CGFloat radius) {
    __block CGImageRef captured = NULL;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);

    // Step 1: enumerate shareable content to resolve the SCDisplay for displayID.
    [SCShareableContent getShareableContentWithCompletionHandler:
        ^(SCShareableContent *content, NSError *err) {
        if (err != nil || content == nil) {
            if (getenv("DNDMODE_TRACE_SCREENS"))
                NSLog(@"[dndmode] glass capture: shareable content failed (likely Screen Recording not granted): %@", err);
            dispatch_semaphore_signal(sem);
            return;
        }
        SCDisplay *scDisplay = nil;
        for (SCDisplay *d in content.displays) {
            if (d.displayID == displayID) { scDisplay = d; break; }
        }
        if (scDisplay == nil) {
            if (getenv("DNDMODE_TRACE_SCREENS"))
                NSLog(@"[dndmode] glass capture: no SCDisplay matches displayID=%u", displayID);
            dispatch_semaphore_signal(sem);
            return;
        }

        // Whole display, exclude nothing (our window is not on screen yet).
        SCContentFilter *filter =
            [[SCContentFilter alloc] initWithDisplay:scDisplay excludingWindows:@[]];
        SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
        CGFloat scale = [target backingScaleFactor];
        cfg.width       = (size_t)([target frame].size.width * scale);
        cfg.height      = (size_t)([target frame].size.height * scale);
        cfg.showsCursor = NO;

        // Step 2: capture the still.
        [SCScreenshotManager captureImageWithFilter:filter
                                      configuration:cfg
                                  completionHandler:^(CGImageRef img, NSError *cerr) {
            if (cerr == nil && img != NULL) {
                captured = CGImageRetain(img);
            } else if (getenv("DNDMODE_TRACE_SCREENS")) {
                NSLog(@"[dndmode] glass capture: captureImage failed: %@", cerr);
            }
            dispatch_semaphore_signal(sem);
        }];
    }];

    // Bounded wait so a denied/hung capture never blocks startup indefinitely.
    if (dispatch_semaphore_wait(sem,
            dispatch_time(DISPATCH_TIME_NOW, (int64_t)(2.0 * NSEC_PER_SEC))) != 0) {
        if (getenv("DNDMODE_TRACE_SCREENS"))
            NSLog(@"[dndmode] glass capture: timed out after 2s → frost fallback");
        return nil; // timed out → caller uses frost
    }
    if (captured == NULL) {
        return nil; // permission denied / no matching display / SCK error
    }

    // Gaussian-blur via CoreImage. Clamp the edges to extent BEFORE blurring so
    // the border does not bleed to transparent/dark, then crop back to size. The
    // radius is in pixels here, so scale the point value by the backing factor.
    CIImage *src = [CIImage imageWithCGImage:captured];
    CGRect extent = [src extent];
    CIImage *clamped = [src imageByClampingToExtent];
    CIFilter *gb = [CIFilter filterWithName:@"CIGaussianBlur"];
    [gb setValue:clamped forKey:kCIInputImageKey];
    [gb setValue:@(radius * [target backingScaleFactor]) forKey:kCIInputRadiusKey];
    CIImage *blurred = [[gb outputImage] imageByCroppingToRect:extent];

    CIContext *ctx = [CIContext contextWithOptions:nil];
    CGImageRef outCG = [ctx createCGImage:blurred fromRect:extent];
    CGImageRelease(captured);
    if (outCG == NULL) {
        return nil;
    }
    NSImage *img = [[NSImage alloc] initWithCGImage:outCG
                                               size:NSMakeSize([target frame].size.width,
                                                               [target frame].size.height)];
    CGImageRelease(outCG);
    return img;
}

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
// MatrixView over an opaque black base; "terminal" installs an animated
// TerminalView (scrolling syntax-highlighted source) over the same opaque black
// base — the `language` arg (go/python/typescript/rust; NULL => go) selects the
// corpus + highlighting; "glass" (QUICK-glass) shows a STATIC,
// tunable-radius CIGaussianBlur of a one-shot ScreenCaptureKit screenshot of the
// desktop (frosted glass — the ONLY non-opaque style; falls back to
// NSVisualEffectView frost if Screen Recording is not granted or the capture
// fails); for "black", NULL, or anything else the plain
// opaque-black path is untouched. black + matrix + terminal keep setOpaque:YES
// (T-gh8-03 no bleed-through); glass deliberately relaxes that for the look while
// input stays blocked.
//
// Critical pitfall (the design notes): default releasedWhenClosed=YES
// combined with ARC + __bridge_retained ownership causes double-free on
// [w close]. We explicitly disable it immediately after init below.
//
// All exact symbol names for (the shield-level Quartz function,
// the four CollectionBehavior flags, opaque-black background, front-ordering
// without activation) appear exactly once in the body for unambiguous grep
// audits.
void* cocoa_create_overlay_window(uint32_t displayID, const char* style,
                                  double blurRadius, const char* language,
                                  char** outErr) {
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

    // CRITICAL multi-monitor fix: initWithContentRect:...:screen: interprets the
    // contentRect origin RELATIVE to `target`'s bottom-left corner, NOT in the
    // global screen coordinate system (per Apple docs: "The origin is relative to
    // the origin of the provided screen"). Passing [target frame] — whose origin
    // is ALREADY the screen's global origin — therefore DOUBLE-OFFSETS the window
    // by that origin. The primary display (origin 0,0) lands correctly, but every
    // secondary display (non-zero, possibly negative origin) gets shoved that far
    // off itself and is left UNCOVERED. Pass a screen-LOCAL rect (origin 0,0 +
    // the screen's size) so the window covers exactly `target` on any display.
    NSRect frame = [target frame];                         // global frame (origin + size)
    NSWindow *w = [[NSWindow alloc]
        initWithContentRect:NSMakeRect(0, 0,
                                       frame.size.width,
                                       frame.size.height)   // screen-LOCAL rect
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
    // transparent panel that blurs whatever is behind it. Input stays fully
    // blocked (ignoresMouseEvents above + the Phase 4 CGEventTap); only the
    // VISUAL coverage relaxes (the desktop shows through, blurred), so it
    // deliberately trades the T-gh8-03 no-bleed-through guarantee for the look.
    //
    // We blur a ONE-SHOT ScreenCaptureKit screenshot with CIGaussianBlur at a
    // tunable radius (kGlassBlurRadius) and show the STATIC image: NSVisualEffect-
    // View's radius is system-fixed and too large (washes shapes into a flat
    // frost), and a live CABackdropLayer did not survive app activation (it
    // vanished after ~1s). A captured image is exact-radius AND cannot vanish.
    // Cost: it needs the Screen Recording permission; if the capture fails (not
    // granted / timeout / error) we fall back to the fixed-radius NSVisualEffect-
    // View frost so the shield is NEVER left transparent (a coverage hole).
    // VISUAL confirmation on a real GUI session is required (the WindowServer
    // owns the pixels; see the glass smoke test).
    if (style != NULL && strcmp(style, "glass") == 0) {
        [w setOpaque:NO];
        [w setBackgroundColor:[NSColor clearColor]];

        // Resolve the blur radius: the caller (config glass_blur / --style
        // glass:N) passes it in; a non-positive value means "unset" and falls
        // back to the built-in default.
        CGFloat radius = (blurRadius > 0.0) ? (CGFloat)blurRadius : kGlassBlurRadius;

        // Capture the desktop NOW — the window is not ordered front yet, so the
        // shot is the clean desktop (no self-capture, nothing to exclude) — blur
        // it, and show the STATIC image. A static NSImageView cannot "vanish"
        // the way the live CABackdropLayer did on app activation.
        NSImage *blurred = captureBlurredDesktopImage(displayID, target, radius);

        if (blurred != nil) {
            NSImageView *iv =
                [[NSImageView alloc] initWithFrame:[[w contentView] bounds]];
            [iv setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
            [iv setImage:blurred];
            [iv setImageScaling:NSImageScaleAxesIndependently]; // fill exactly
            [iv setImageAlignment:NSImageAlignCenter];

            // Faint flat darkening on top (kGlassTintAlpha) — a normal
            // layer-backed subview (AppKit-managed, robust; not a raw sublayer).
            NSView *tint = [[NSView alloc] initWithFrame:[iv bounds]];
            [tint setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
            [tint setWantsLayer:YES];
            [[tint layer] setBackgroundColor:
                [[NSColor colorWithWhite:0.0 alpha:kGlassTintAlpha] CGColor]];
            [iv addSubview:tint];

            [w setContentView:iv];
        } else {
            // Capture failed (Screen Recording not granted / timeout / SCK error):
            // fall back to the fixed-radius NSVisualEffectView frost so the shield
            // is NEVER transparent. Not tunable and shapes wash out, but SECURE.
            // Grant Screen Recording in System Settings to get the tunable blur.
            NSVisualEffectView *ve =
                [[NSVisualEffectView alloc] initWithFrame:[[w contentView] bounds]];
            [ve setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
            [ve setBlendingMode:NSVisualEffectBlendingModeBehindWindow];
            [ve setMaterial:NSVisualEffectMaterialFullScreenUI];
            [ve setState:NSVisualEffectStateActive];
            [w setContentView:ve];
        }
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
        // terminal swaps in an animated scrolling-source contentView over the
        // SAME opaque black base (opaque TerminalView layer on top, never
        // transparent) — same T-gh8-03 no-bleed-through guarantee as matrix.
        // TerminalView's @interface comes from terminalview_darwin.h.
        } else if (style != NULL && strcmp(style, "terminal") == 0) {
            TerminalView *tv = [[TerminalView alloc] initWithFrame:[[w contentView] bounds]
                                                          language:language];
            [tv setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
            [w setContentView:tv];
        }
    }

    // AUTHORITATIVE positioning: pin the window to the display's exact GLOBAL
    // frame in absolute (primary-origin-based) screen coordinates. Unlike the
    // screen-relative initializer above, -setFrame:display: is unambiguous and
    // consistent across macOS versions, so this is the real coverage guarantee:
    // whatever AppKit did during init (screen-relative offset, cascade nudge,
    // constrainFrameRect), this snaps the window back onto `target` in full. The
    // shield-level set earlier means constrainFrameRect:toScreen: does NOT pull
    // the top down below the menu bar (that clamp only applies below menu level).
    [w setFrame:frame display:YES];

    // Opt-in geometry trace for multi-monitor debugging (silent unless the env
    // var is set — keeps the security-stance "silent by default"). Prints the
    // TARGET frame vs the ACTUAL window frame so a mismatch (offset/shrink) is
    // visible at a glance: `DNDMODE_TRACE_SCREENS=1 dndmode --debug`.
    if (getenv("DNDMODE_TRACE_SCREENS")) {
        NSRect got = [w frame];
        NSLog(@"[dndmode] displayID=%u scale=%.1f target=(%.0f,%.0f %.0fx%.0f) window=(%.0f,%.0f %.0fx%.0f)",
              displayID, [target backingScaleFactor],
              frame.origin.x, frame.origin.y, frame.size.width, frame.size.height,
              got.origin.x, got.origin.y, got.size.width, got.size.height);
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
