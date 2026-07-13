# Terminal Overlay Style (scrolling source)

## Overview

Add a new `overlay_style: terminal` to dndmode: an opaque full-screen shield whose
content view renders a **scrolling stream of source code** — pseudo-code lines that
type themselves out with a blinking caret, then scroll up as new lines arrive, with
light syntax highlighting. It is a purely cosmetic content swap on top of the
existing opaque-black shield (exactly like `matrix`), so every blocking guarantee
(HID event tap, shield window level, collection behavior, no-bleed-through) is
byte-for-byte identical to `black`.

**Problem it solves / benefit.** `dndmode` covers an unattended machine while a
background job (an AI agent in YOLO mode) keeps running. The `terminal` look
reinforces that narrative — the screen reads as "work is happening" — and gives
users who find `matrix` too loud a second animated option that still looks
deliberately locked.

**How it integrates.** It plugs into the same extension point as `matrix`:
`cocoa_create_overlay_window()` in `window_darwin.m` selects the content view by the
`style` string. A new `TerminalView : NSView` (mirroring `MatrixView`) is installed
over the `setOpaque:YES` black window. The style flows through the already-existing
plumbing (`config.OverlayStyle` → `main.go` validation → `NewController(style,…)` →
`cgoWindowFactory` → `createOverlayWindowStyled` → the cgo call), so no interface or
controller logic changes — only additive branches and string/whitelist updates.

## Context (from discovery)

- **Files/components involved:**
  - `internal/config/config.go` — overlay-style constants, `ValidateOverlayStyle`, config template.
  - `internal/config/config_test.go` — style validation tests.
  - `internal/macos/cocoa/matrixview_darwin.{h,m}` — the animation engine to mirror.
  - `internal/macos/cocoa/window_darwin.m` — style → content-view dispatch (opaque path).
  - `internal/macos/cocoa/window_darwin.go` — `createOverlayWindowStyled` Go wrapper + docstring.
  - `internal/macos/cocoa/controller_darwin.go` — style threading + docstrings (`black|matrix|glass`).
  - `cmd/dndmode/main.go` — `--style` flag usage text + invalid-style error text.
  - `internal/macos/cocoa/matrix_smoketest_test.go` — smoke-test template.
  - `README.md` — "Overlay styles" table.
- **Related patterns found:**
  - Opaque animated style contract (from `matrixview_darwin.m`): `wantsLayer`,
    `layer.backgroundColor = black`, `isOpaque = YES`, FPS-capped `NSTimer` at
    ~30 FPS added to the **main** run loop in `NSRunLoopCommonModes`, flat
    `drawRect:` with `drawAtPoint:` (no shadow/blur), rebuild on `setFrameSize:` /
    `resizeSubviewsWithOldSize:`.
  - Leak-free lifecycle contract: start timer in `viewDidMoveToWindow` (window != nil),
    stop + release in `viewWillMoveToWindow:nil` **and** `dealloc` (guarded).
  - New style checklist (from the `matrix` grep): config constant + validator +
    template, cgo content-view branch + shared header, user-facing string updates,
    README row, smoke test.
  - cgo TU rule: each `.m` is a separate translation unit, so the `@interface` lives
    in a shared `.h` imported by both `terminalview_darwin.m` and `window_darwin.m`.
- **Dependencies identified:** Cocoa/AppKit + QuartzCore (CALayer), same as
  `matrix`. No new frameworks, no config-schema migration, no network.

## Development Approach

- **testing approach**: **Regular** (code first, then tests).
  - Obj-C `drawRect:` output cannot be asserted programmatically — the WindowServer
    owns the pixels (documented precedent: `matrix_smoketest_test.go`). So the
    Obj-C engine tasks are verified by **(a) cgo compilation** (`go build ./...`
    actually compiles the `.m`), **(b) a create/tick/close smoke test** proving the
    wiring + lifecycle do not crash, and **(c) a manual visual run** in
    Post-Completion. Only the Go `config` task has true unit-testable logic.
  - Where a task's deliverable is non-unit-testable Obj-C, its checklist ends with a
    build/vet gate instead of a unit test, and the integration coverage is the smoke
    test (Task 7). This is the `matrix` precedent, stated explicitly per task.
- complete each task fully before moving to the next; small, focused changes.
- **CRITICAL: all tests must pass before starting next task** — `go build ./...`,
  `go vet ./...`, and `go test ./...` are green before advancing.
- maintain backward compatibility: `black`/`matrix`/`glass`/`none` behavior is
  untouched; `terminal` is purely additive.

## Testing Strategy

- **unit tests**: `internal/config/config_test.go` — `ValidateOverlayStyle("terminal")`
  accepted, plus a normalize/round-trip check. Real Go logic, fully unit-tested.
- **compilation gate**: every Obj-C task must keep `go build ./...` (cgo compiles
  the `.m` files) and `go vet ./...` green — a hard, automatable check that the new
  translation unit and the `window_darwin.m` branch are well-formed.
- **integration/smoke**: `internal/macos/cocoa/terminal_smoketest_test.go`
  (`//go:build darwin && manual`) — create a `style="terminal"` window, let ≥1
  animation tick fire (~150 ms), close it; must not panic and the handle must
  round-trip. Skipped under `HEADLESS=1` / off-main.
- **e2e tests**: none — dndmode has no UI-driver e2e harness. The equivalent is the
  manual visual run recorded under Post-Completion.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update this plan file if implementation deviates from original scope

## Solution Overview

`TerminalView` is a self-contained opaque content view. It holds a small **static
corpus** of realistic source lines (a mix of Go/C so it reads like a real project;
a few dndmode-flavored lines for theme). A ring of **visible lines** is drawn top to
bottom; the **bottom line types itself out** one character at a time behind a
blinking caret. When the bottom line finishes it pauses briefly, the buffer
**jump-scrolls up by one line** (classic terminal behavior — robust and cheap), and
the next corpus line (wrapping modulo the corpus length) begins typing at the
bottom. Each line is tokenized **once** when it enters the buffer (not per frame)
into colored segments for **light syntax highlighting**.

**Key design decisions:**

1. **Opaque, not glass.** `setOpaque:YES`, layer/base fill pure `#000000` — the
   `T-gh8-03` no-bleed-through guarantee is preserved, matching `matrix`.
2. **Ambient, never reactive.** The animation is driven solely by its own timer and
   ignores all input. This is load-bearing for the security stance ("silent on wrong
   input"): a reactive flash/shake would leak the fact that keystrokes are being
   intercepted. No `NSResponder` input handling is added.
3. **Jump-scroll per line, not pixel-smooth scroll.** A per-line jump (type → pause →
   shift up by one) is the terminal-authentic, low-risk v1. Smooth pixel scrolling is
   noted as a future enhancement, not v1 scope.
4. **Tokenize on enter, not per frame.** The corpus is static, so each line is
   classified once when it scrolls into the buffer; `drawRect:` just paints
   pre-colored segments. Keeps 30 FPS cheap across all displays.
5. **Hardcoded look (no config knobs) in v1**, mirroring `matrix`. A restrained dark
   editor palette (keyword / string / comment / number / ident / caret) lives in
   `static const` tables, trivially promotable to config later.

## Technical Details

**New style value:** `terminal` (config `overlay_style: terminal`; `--style terminal`).
No parameter suffix (unlike `glass:N`).

**Data structures (in `terminalview_darwin.m`):**
- `static const char *kCorpus[]` — ~50–80 ASCII source lines (Go/C flavored). ASCII
  keeps tokenizing + monospace width trivial.
- Per-visible-line record: corpus index + tokenized segments `{start, len, class}`.
- Buffer: array of visible-line records sized to the view height (rebuilt on resize).
- Typing state: `visibleChars` on the bottom line, a fractional chars/frame speed,
  and a phase enum `{TYPING, PAUSE, SCROLL}` with a frame countdown for PAUSE.
- Caret blink: derived from a frame counter (~0.5 s on/off).
- Monospaced font via `[NSFont monospacedSystemFontOfSize:weight:]` (fallback Menlo),
  fixed cell width → `x = col * cellW`, fixed line height → `y = row * cellH`.

**Per-frame `step:` flow:**
1. `TYPING`: advance `visibleChars` by the per-frame speed; on reaching the bottom
   line's length → `PAUSE` (reset countdown).
2. `PAUSE`: decrement countdown; at 0 → `SCROLL`.
3. `SCROLL`: shift buffer up by one; pull `kCorpus[(cursor++) % N]` into the bottom
   slot, tokenize it, reset `visibleChars = 0`; → `TYPING`.
4. `setNeedsDisplay:YES`.

**`drawRect:`:**
- Fill bounds pure opaque black.
- For each buffered line top→bottom: draw its colored segments; for the **bottom**
  (typing) line draw only the first `visibleChars` characters.
- Draw caret glyph (`▊`) after the last visible char of the bottom line when the
  blink phase is "on".
- Flat `drawAtPoint:withAttributes:` per segment (no `NSShadow`), like `matrix`.
- **Long lines** (review): with `x = col * cellW`, a line wider than the screen runs
  past the right edge and is clipped by the view bounds. v1 keeps the corpus lines
  short enough to fit typical widths and relies on bounds clipping — no soft-wrap.

**Lightweight tokenizer (`class` per segment):**
- Classes: `keyword`, `string`, `comment`, `number`, `ident`, `punct`.
- Rules over ASCII: `//` → rest of line is `comment`; `"` → until closing `"` is
  `string`; leading digit → `number`; `[A-Za-z_]` run → `ident`, promoted to
  `keyword` if it is in a small Go+C keyword set; otherwise `punct`.
- Keyword set (small, static): `func return if else for range var const type struct
  import package switch case default break continue go defer chan map interface nil
  true false int char void static sizeof while`.

**Palette (`static const`, restrained dark editor theme):**
| class    | sRGB (approx)        |
| -------- | -------------------- |
| ident    | 0.80, 0.82, 0.85     |
| keyword  | 0.65, 0.55, 0.95     |
| string   | 0.45, 0.80, 0.45     |
| comment  | 0.40, 0.42, 0.45     |
| number   | 0.90, 0.65, 0.35     |
| punct    | 0.60, 0.62, 0.65     |
| caret    | 0.90, 1.00, 0.90     |

**Performance:** only the (few dozen) visible lines are drawn; tokenization is
once-per-line-on-scroll, not per frame; flat drawing at a 30 FPS cap. Comparable
cost to `matrix`, safe on all displays. (When `allow_display_sleep: true` the panel
may idle off; the timer still ticks but draws to a dark screen — acceptable, same as
`matrix`; no special-casing in v1.)

**Security/privacy:** the corpus is fully synthetic and compiled in — no real
filesystem, git, or system data is ever read or shown. No input handling is added.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and docs changes inside this repo.
- **Post-Completion** (no checkboxes): the manual visual run on a real GUI session
  (multi-display if available), which is the only way to validate the actual pixels.

## Implementation Steps

### Task 1: Add `terminal` to config (constant, validation, template)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [x] add `OverlayStyleTerminal = "terminal"` constant with a doc comment next to `OverlayStyleMatrix`
- [x] add `OverlayStyleTerminal` to the accepted `case` in `ValidateOverlayStyle` (~line 144)
- [x] update the `ValidateOverlayStyle` error text to `(valid: black, matrix, terminal, glass, none)` (~line 147)
- [x] **[review]** update the `ValidateOverlayStyle` **doc comment** enumeration (`config.go:136-137`: "accepts …, \"black\", \"matrix\", \"glass\" and \"none\"") to include `terminal`
- [x] **[review]** update the `GlassBlur` field doc (`config.go:79`: "ignored for black/matrix/none") → `black/matrix/terminal/none`
- [x] extend `defaultConfigTemplate` `overlay_style` docs with a `terminal :` line describing the scrolling-source look (cosmetic, opaque, every guarantee identical to black)
- [x] update the `OverlayStyle` struct-field doc comment (~line 71) to list `terminal` among valid values
- [x] write test: `ValidateOverlayStyle("terminal")` returns nil (add to the existing table)
- [x] **[review]** add `OverlayStyleTerminal` to the "all valid styles accepted" loop invariant in `config_test.go:430` (the `[]string{"", OverlayStyleBlack, OverlayStyleMatrix, OverlayStyleGlass, OverlayStyleNone}` slice in the `neon` case) — else `terminal` stays under-tested even with the standalone case
- [x] write test: an unknown style still errors and the message names the full valid set incl. `terminal`
- [x] run `go test ./internal/config/...` — must pass before next task

### Task 2: TerminalView skeleton + lifecycle (Obj-C)

**Files:**
- Create: `internal/macos/cocoa/terminalview_darwin.h`
- Create: `internal/macos/cocoa/terminalview_darwin.m`

- [x] create `terminalview_darwin.h` with `@interface TerminalView : NSView @end` and the shared-header rationale comment (mirrors `matrixview_darwin.h`)
- [x] create `terminalview_darwin.m`: `initWithFrame:` sets `wantsLayer = YES`, opaque black backing layer, builds the monospaced font(s)
- [x] implement `isOpaque` → `YES` and `isFlipped` → `YES` (rows grow downward)
- [x] implement the FPS-capped `NSTimer` (`kTermFrameInterval = 1.0/30.0`) `startTimer`/`stopTimer`, added to the **main** run loop in `NSRunLoopCommonModes`
- [x] implement the leak-free lifecycle: `viewDidMoveToWindow` (start when window != nil), `viewWillMoveToWindow:nil` (stop), `dealloc` (stop + free buffers, double-invalidate guarded)
- [x] add a minimal `drawRect:` that fills opaque black (placeholder until Task 4)
- [x] tests: rendering is not unit-testable (WindowServer owns pixels — `matrix` precedent); coverage is the smoke test (Task 7) + visual run (Post-Completion)
- [x] run `go build ./...` and `go vet ./...` (cgo compiles the new `.m`) — must pass before next task

### Task 3: Corpus, visible-line buffer, typing + jump-scroll (Obj-C)

**Files:**
- Modify: `internal/macos/cocoa/terminalview_darwin.m`

- [x] add the static `kCorpus[]` source lines (Go/C flavored, ASCII, a few dndmode-themed)
- [x] add the visible-line buffer + `rebuild` sizing it to the view height/width; free + realloc like `MatrixView` (`calloc`/`free`, NULL-reset)
- [x] hook `setFrameSize:` and `resizeSubviewsWithOldSize:` to rebuild
- [x] add typing state (`visibleChars`, chars/frame speed, `phase`, PAUSE countdown, caret blink counter)
- [x] implement `step:` — TYPING advances chars; PAUSE counts down; SCROLL shifts the buffer up, pulls `kCorpus[(cursor++) % N]` into the bottom slot, resets typing; then `setNeedsDisplay:YES`
- [x] tests: non-unit-testable Obj-C animation state (see Task 2 rationale); exercised by the smoke test (Task 7)
- [x] run `go build ./...` and `go vet ./...` — must pass before next task

### Task 4: Syntax tokenizer + full drawRect (Obj-C)

**Files:**
- Modify: `internal/macos/cocoa/terminalview_darwin.m`
- (optional test shim) Modify: `internal/macos/cocoa/terminalview_darwin.m`; Create: `internal/macos/cocoa/terminalview_darwin.go`, `internal/macos/cocoa/terminalview_test.go`

- [ ] add the `static const` palette table + token-class enum
- [ ] add the small static keyword set (Go + C)
- [ ] implement `tokenizeLine:` → colored segments `{start, len, class}`, called once when a line enters the buffer (not per frame)
- [ ] **[review]** wire `tokenizeLine:` into BOTH entry paths so no line is ever drawn untokenized: (a) the SCROLL branch of `step:` (Task 3) when a fresh corpus line enters the bottom slot, and (b) the initial buffer fill during `rebuild` (Task 3) — Task 3 places the RAW corpus line, Task 4 attaches the tokenized segments at both sites
- [ ] rewrite `drawRect:`: opaque-black fill, then per buffered line draw its colored segments (bottom line clipped to `visibleChars`), then the blinking caret `▊`
- [ ] tests: rendering not unit-testable (see Task 2 rationale); visual correctness verified in the Post-Completion run; smoke test proves no crash
- [ ] **[review, recommended]** `tokenizeLine:` is the ONE piece of pure, testable logic here (string → `{start,len,class}` segments, where off-by-one boundary bugs live). Follow the existing C-shim → Go-wrapper test pattern (`cocoa_first_attached_display_id` at `window_darwin.m:372` + `firstAttachedDisplayIDForTest` at `window_darwin.go:107`): export a `terminal_tokenize_for_test` shim, wrap it in `terminalview_darwin.go`, and table-test keyword/string/comment/number/ident classification in `terminalview_test.go`. Skip only if the shim proves disproportionate — correctness here is cosmetic (a wrong color is invisible harm), so it is defensible to omit, matching the `matrix` precedent
- [ ] run `go build ./...` and `go vet ./...` (and `go test ./internal/macos/cocoa/...` if the tokenizer test was added) — must pass before next task

### Task 5: Wire `terminal` into the window content-view dispatch (Obj-C)

**Files:**
- Modify: `internal/macos/cocoa/window_darwin.m`
- Modify: `internal/macos/cocoa/window_darwin.go`

- [ ] `#import "terminalview_darwin.h"` in `window_darwin.m`
- [ ] in `cocoa_create_overlay_window`, inside the opaque (else) branch, add `strcmp(style, "terminal") == 0` → install a `TerminalView` content view (mirrors the `matrix` branch; keeps `setOpaque:YES`)
- [ ] update the `cocoa_create_overlay_window` header comment to document the `terminal` style
- [ ] update the `createOverlayWindowStyled` doc comment in `window_darwin.go` to include `terminal`
- [ ] tests: wiring is exercised by the smoke test (Task 7); no new Go unit surface here
- [ ] run `go build ./...` and `go vet ./...` — must pass before next task

### Task 6: Sync user-facing style strings (flag usage, error text, docstrings)

**Files:**
- Modify: `cmd/dndmode/main.go`
- Modify: `internal/macos/cocoa/controller_darwin.go`

- [ ] update the `--style` flag usage string in `main.go` to `(black|matrix|terminal|glass|none)`
- [ ] update the invalid-`overlay_style` stderr template in `main.go` to list `terminal` in the valid set
- [ ] update the `cgoWindowFactory` (`controller_darwin.go:47`) and `NewController` (`:172`) doc comments to include `terminal`. **[review]** note `:47` currently reads only `black|matrix` (already missing `glass`) — write the FULL set `black|matrix|glass|terminal`, don't just append to the incomplete list. A literal `black|matrix|glass` grep will miss `:47`, so edit by symbol name, not grep
- [ ] verify no other hardcoded style whitelist exists in `main.go`/`acceptance_test.go` (grep `black.*matrix.*glass`); update if found (review confirmed `acceptance_test.go` has no exhaustive style list — no `terminal` acceptance test needed, matching `matrix`)
- [ ] run `go build ./...` and `go test ./cmd/...` — must pass before next task

### Task 7: Terminal smoke test (create/tick/close)

**Files:**
- Create: `internal/macos/cocoa/terminal_smoketest_test.go`

- [ ] create the test with `//go:build darwin && manual`, mirroring `matrix_smoketest_test.go` (package `cocoa`, `runtimepin` import)
- [ ] `HEADLESS=1` → `t.Skip`; `skipUnlessMainThread(t)`; skip if no display attached
- [ ] `createOverlayWindowStyled(id, "terminal", 0)` → assert non-nil handle, no error
- [ ] `time.Sleep(150ms)` to let ≥1 animation tick fire, then `closeOverlayWindow(w)` inside a `recover()` guard (must not panic)
- [ ] run the smoke test on a GUI session: `go test -tags 'darwin manual' -run TestSmoke_Terminal ./internal/macos/cocoa/` — must pass (or skip cleanly if headless) before next task

### Task 8: Verify acceptance criteria
- [ ] verify all Overview requirements: new `terminal` style, opaque, ambient, scrolling source with highlighting, guarantees identical to `black`
- [ ] verify `black`/`matrix`/`glass`/`none` behavior is unchanged (no regressions in existing config tests)
- [ ] run full suite: `go build ./... && go vet ./... && go test ./...`
- [ ] run the smoke suite on a GUI session: `go test -tags 'darwin manual' -run TestSmoke ./internal/macos/cocoa/`
- [ ] confirm no `.gitignore`-matched or restricted paths (`CLAUDE.md`, `.claude/`, `.planning/`) are touched by this change

### Task 9: [Final] Update documentation
- [ ] add a `terminal` row to the README "Overlay styles" table (Look: green-tinted scrolling source; Bleeds through: No; Input blocked: Yes) and a short paragraph
- [ ] **[review]** add `terminal` to the two OTHER style enumerations in README that will otherwise go stale: the `--style` flag values table (`README.md:197`) and the inline config.yml example comment (`README.md:232`, `# Overlay look: black (default) | matrix | glass | none`)
- [ ] **[review, optional]** in "Known limitations" (`README.md:467-468`) the opaque alternatives to glass are listed as "Use `black` or `matrix`" — add `terminal` for completeness
- [ ] update `CLAUDE.md` only if a genuinely new pattern emerged (likely not — `terminal` follows the `matrix` template) — otherwise skip
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification** (required — the only way to validate pixels):
- Build and run: set `overlay_style: terminal` (or `dndmode --style terminal`) and confirm scrolling source with a blinking caret and syntax colors on the primary display.
- Multi-display: if a second display is available, confirm every screen is covered and animating; hot-plug a display mid-session and confirm the overlay rebuilds (reconcile path) without leaking or crashing.
- Teardown: enter the unlock hotkey and confirm clean exit (timer stops, no lingering process, cursor restored) — the `viewWillMoveToWindow:nil` / `dealloc` contract.
- Performance sanity: confirm no excessive CPU/GPU/fan while idle across all displays (30 FPS flat draw should be light).
- Security sanity: confirm the animation does not react to any keystroke or trackpad input (ambient only), and that on-screen text is clearly synthetic (no real project/system data).
