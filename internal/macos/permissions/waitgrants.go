//go:build darwin

package permissions

import (
	"context"
	"log/slog"
	"time"
)

//go:generate mockgen -source=waitgrants.go -destination=mocks/waitgrants.go -package=mocks

// Checker abstracts the AX / IM permission probes that the polling loop
// invokes once per cycle (both checks run in parallel — independent
// state). Production implementation (NewCgoChecker) wraps the cgo bridges
// IsAccessibilityTrusted / CheckInputMonitoring from. Tests
// inject a fake to drive cycle-by-cycle state transitions without leaving
// pure Go.
//
// Methods MUST be safe to call from any goroutine and MUST NOT trigger
// the AX system prompt as a side effect (that is reserved for the
// `prompt` closure handed to WaitForGrants).
type Checker interface {
	// IsAXTrusted returns true iff AXIsProcessTrusted() reports the
	// process as trusted. No UI side effect.
	IsAXTrusted() bool

	// IsIMGranted returns true iff IOHIDCheckAccess(kIOHIDRequestTypeListenEvent)
	// returns kIOHIDAccessTypeGranted. No UI side effect.
	IsIMGranted() bool
}

// cgoChecker is the production Checker, wired to the cgo bridges from
// ax_darwin.go and inputmonitoring_darwin.go.
type cgoChecker struct{}

func (cgoChecker) IsAXTrusted() bool {
	return IsAccessibilityTrusted()
}

func (cgoChecker) IsIMGranted() bool {
	return CheckInputMonitoring().IsGranted()
}

// NewCgoChecker returns the production Checker backed by the
// IsAccessibilityTrusted / CheckInputMonitoring cgo probes. cmd/dndmode/main.go
// (Step 8) instantiates this via NewCgoChecker() and hands the
// result to WaitForGrants alongside NewDeepLinker() and
// NewStatusWriter(os.Stdout).
func NewCgoChecker() Checker {
	return cgoChecker{}
}

// WaitForGrants is the polling-loop orchestrator. It blocks until
// both Accessibility and Input Monitoring are granted, or until ctx is
// cancelled (SIGINT/SIGTERM/SIGHUP via signal.NotifyContext per
//), and returns the corresponding error.
//
// Contract honoured:
//
// -: grant-edge events ("permission granted" with kind=ax|im) are
//     logged via slog.Info exactly once per permission as it flips
//     false→true.
// -: AX and IM are probed each cycle in parallel (independent
//     state). Both must be true to exit successfully.
// -: polling is indefinite; only ctx.Done() (SIGINT/SIGTERM/SIGHUP
//     in main.go) prunes the wait. Returning ctx.Err() maps to exit code
// 3 (exitPermissionDenied per).
// -: on entry, if AX is missing, prompt() is invoked exactly once
//     and link.OpenAX() exactly once. Same for IM. Subsequent polling
//     cycles use plain IsAXTrusted/IsIMGranted (no further prompts, no
// further deep-links). Linker errors are logged warn and the
//     polling continues.
//
// Nil-safety:
//
//   - prompt == nil is treated as a no-op (some tests do not need to drive
//     the prompt invariant; production main.go always passes a real func).
//   - log == nil falls back to slog.Default() (mirrors Phase 2 controller).
//
// Status rendering is delegated to status (StatusWriter):
// status.EntryBanner() prints the once-per-WaitForGrants entry message
// (TTY: "dndmode: waiting for grants…\n"; pipe: no-op — startup state is
// encoded in pipeWriter.Update's first call). The cold-start state is
// rendered once before any prompts/deep-links, and each polling cycle
// renders again. status.Final() is called exactly once when both grants
// land. tick is the cycle interval — fixes 500ms in production;
// tests pass much shorter values to keep wall-clock low.
//
// contract: EntryBanner is invoked unconditionally (even when both
// grants are already present), so the TTY user always sees the "we tried
// to wait" line — followed immediately by Final's "grants received."
// when the cold-start check finds both trusted. This is intentional
// observability, not a performance regression.
func WaitForGrants(
	ctx context.Context,
	chk Checker,
	link DeepLinker,
	status StatusWriter,
	prompt func(),
	log *slog.Logger,
	tick time.Duration,
) error {
	if log == nil {
		log = slog.Default()
	}

	// print the "waiting for grants…" entry banner BEFORE any
	// probe / prompt / deep-link so the TTY user sees the intent before
	// the \r-cycle Update repaints start. pipeWriter.EntryBanner is a
	// no-op — its startup line (from the first Update) already carries
	// state. See.
	status.EntryBanner()

	ax := chk.IsAXTrusted()
	im := chk.IsIMGranted()
	status.Update(ax, im)
	if ax && im {
		status.Final()
		return nil
	}

	// one-shot prompt + deep-link per missing permission, at entry only.
	if !ax {
		if prompt != nil {
			prompt()
		}
		if err := link.OpenAX(); err != nil {
			log.Warn("open AX settings", slog.Any("err", err))
		}
	}
	if !im {
		if err := link.OpenIM(); err != nil {
			log.Warn("open IM settings", slog.Any("err", err))
		}
	}

	// indefinite polling. Only ctx.Done() prunes the loop.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
			newAX := chk.IsAXTrusted()
			newIM := chk.IsIMGranted()
			// grant-edge events. Logged exactly once per permission
			// as it flips false→true.
			if newAX && !ax {
				log.Info("permission granted", slog.String("kind", "ax"))
			}
			if newIM && !im {
				log.Info("permission granted", slog.String("kind", "im"))
			}
			ax, im = newAX, newIM
			status.Update(ax, im)
			if ax && im {
				status.Final()
				return nil
			}
		}
	}
}
