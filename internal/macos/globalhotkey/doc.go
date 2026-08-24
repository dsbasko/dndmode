//go:build darwin

// Package globalhotkey registers ONE system-wide activation combination via
// Carbon's RegisterEventHotKey and reports every press on a channel.
//
// # Why Carbon and not a second CGEventTap
//
// The watch mode (`dndmode --watch`) sits idle for hours waiting for the
// user to ask for a shield. A resident CGEventTap would put dndmode in the
// hot path of every keystroke on the machine — including the user's
// passwords — and would hold the Accessibility grant the whole time, for a
// process that is doing nothing. RegisterEventHotKey inverts that: the OS
// holds the combination and matches it, this process observes nothing until
// its own combination fires, and no TCC grant is required to wait. The
// session that follows a press still needs Accessibility, but the waiting
// does not.
//
// The cost is expressiveness. Carbon matches "modifiers + exactly one key"
// and nothing else, so an activation combination cannot be a multi-step
// sequence the way an unlock code can (hotkey.ParseSequence). That is an
// acceptable trade for an activation trigger — Ctrl+Cmd+Q, the system's own
// lock-screen shortcut, has the same shape — and it is why Register takes a
// single hotkey.Spec rather than a slice.
//
// # Activation is not a secret
//
// Unlike the unlock code, the activation combination is public: it is
// printed at startup, documented in the README, and anyone standing nearby
// can watch it being typed. A bystander pressing it can raise the shield on
// an unattended machine — the same exposure Ctrl+Cmd+Q already has — but
// they cannot lower it, because lowering still requires the unlock code.
// Nothing in this package needs to be silent, and diagnostics here may name
// the combination freely. That is the opposite of the rule that governs
// internal/config/hotkey, whose errors must never echo their input.
//
// # Threading
//
// Registration and delivery have different requirements, and the gap between
// them is the sharpest edge in this package: registration succeeding proves
// nothing about delivery.
//
// REGISTRATION needs neither NSApp nor the main goroutine. The smoke tests
// register and release from ordinary test goroutines in a process that never
// called sharedApplication.
//
// DELIVERY needs two further things, both established by measurement:
//
//   - A spinning main run loop. The Carbon handler is dispatched by [NSApp
//     run], so presses arrive only while cocoa.RunApp is inside it. This is
//     why delivery cannot be exercised from `go test` at all.
//   - An activation policy of at least Accessory. Under Prohibited — which is
//     what cocoa.Init establishes — RegisterEventHotKey returns noErr and the
//     combination is genuinely claimed away from other applications, yet no
//     press is ever delivered. A Prohibited process is never eligible to be
//     the active application, and the hot-key event follows that eligibility.
//     cmd/dndmode therefore calls cocoa.SetAtRestPolicyAccessory before
//     registering.
//
// That second one is worth restating because of how it fails: silently, with
// every observable signal reporting success. A caller that registers under
// Prohibited gets a valid *Registration, a channel that never fires, and no
// error to explain it.
//
// In practice watch mode raises the policy, registers between cocoa.Init and
// cocoa.RunApp, and releases through the LIFO cleanup stack after RunApp
// returns. Presses landing outside that window are simply never delivered.
//
// The handler makes exactly one Go call: a non-blocking send on a buffered
// channel. The strict zero-Go-calls rule that governs the CGEventTap
// callback (internal/macos/eventtap) deliberately does NOT carry over here,
// and the distinction is worth stating because the two look similar. That
// callback runs on a foreign worker thread in the hot path of every
// keystroke in the system; this handler runs on the thread Go itself
// started, at a rate bounded by how fast a human can press a chord.
//
// # Presses during an active session
//
// While a session holds the shield, its kCGHIDEventTap suppresses input
// before WindowServer ever sees it, and Carbon matches after WindowServer.
// So the activation combination simply cannot fire while the shield is up —
// no de-duplication logic is needed for that case. The channel's capacity
// of 1 covers the remaining narrow window: presses landing between the
// trigger and the tap going in.
package globalhotkey
