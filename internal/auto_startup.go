package internal

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ccOctoWatchdogInterval is how often the machine-level watchdog re-checks the
	// gateway. The first check fires immediately on startup (machine reboot →
	// bring cc-channel-octo back up without waiting a full interval).
	ccOctoWatchdogInterval = 30 * time.Second
	// ccOctoStartMaxAttempts is the number of `cc-channel-octo start` tries per
	// detection before backing off until the next interval.
	ccOctoStartMaxAttempts = 3
	// ccOctoStartVerifyDelay gives a freshly forked gateway time to come up before
	// we re-check status to decide whether the attempt succeeded.
	ccOctoStartVerifyDelay = 3 * time.Second
	// ccOctoStartTimeout bounds a single `cc-channel-octo start` attempt so a
	// hung start can never hold the gateway lock for the whole daemon lifetime
	// (which would stall every provision/upgrade on the host). start is normally
	// sub-second; 10s is a generous ceiling.
	ccOctoStartTimeout = 10 * time.Second
	// ccOctoConfigDir is the per-host cc-channel-octo root under $HOME. Mirrors the
	// adapter's claudeChannelDir (kept local to avoid exporting it just for this).
	ccOctoConfigDir = ".cc-channel-octo"
)

// ccOctoState classifies the gateway for the auto-start watchdog.
type ccOctoState int

const (
	// ccOctoUnavailable: not installed or not yet configured — nothing to start.
	ccOctoUnavailable ccOctoState = iota
	// ccOctoRunning: gateway is up — nothing to do.
	ccOctoRunning
	// ccOctoStopped: installed + configured + not running — start it.
	ccOctoStopped
	// ccOctoStatusError: binary present but `status` errored unexpectedly (should
	// essentially never happen) — skip with a loud log.
	ccOctoStatusError
)

// runCcOctoWatchdog is the machine-level auto-start loop. It keeps cc-channel-octo
// running across reboots without pm2 (whose supervision would fight the daemon's
// own self-fork `restart` during provision/upgrade). It checks immediately, then
// every ccOctoWatchdogInterval, and coordinates with the daemon's lifecycle calls
// through s.gwLock so it never races a restart. Returns on ctx cancellation.
func (s *Supervisor) runCcOctoWatchdog(ctx context.Context) {
	s.ccOctoAutoStartTick(ctx)

	ticker := time.NewTicker(ccOctoWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ccOctoAutoStartTick(ctx)
		}
	}
}

// ccOctoAutoStartTick runs one detect-and-maybe-start cycle.
func (s *Supervisor) ccOctoAutoStartTick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	state, statusErr := ccOctoAutoStartState(ctx)
	switch state {
	case ccOctoUnavailable, ccOctoRunning:
		return
	case ccOctoStatusError:
		log.Printf("[ERROR] cc-channel-octo is installed but `status` errored unexpectedly — skipping auto-start this tick: %v", statusErr)
		return
	}

	// Gateway is stopped. Coordinate with daemon-initiated restart/upgrade: if a
	// lifecycle op holds the lock, skip this tick rather than race a concurrent
	// start (the conflict that rules out pm2 supervision).
	if !s.gwLock.TryLock() {
		log.Printf("[INFO] cc-channel-octo stopped, but a daemon gateway op is in progress — skipping auto-start this tick")
		return
	}
	defer s.gwLock.Unlock()

	for attempt := 1; attempt <= ccOctoStartMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[INFO] cc-channel-octo stopped, auto-start attempt %d/%d", attempt, ccOctoStartMaxAttempts)
		startCtx, cancel := context.WithTimeout(ctx, ccOctoStartTimeout)
		out, err := exec.CommandContext(startCtx, "cc-channel-octo", "start").CombinedOutput()
		cancel()
		if err != nil {
			log.Printf("[WARN] cc-channel-octo auto-start attempt %d/%d failed: %v\noutput: %s",
				attempt, ccOctoStartMaxAttempts, err, truncateOutput(string(out), 400))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(ccOctoStartVerifyDelay):
		}
		if isCcChannelOctoRunning() {
			log.Printf("[INFO] cc-channel-octo auto-started successfully (attempt %d/%d)", attempt, ccOctoStartMaxAttempts)
			return
		}
	}
	log.Printf("[ERROR] cc-channel-octo auto-start failed after %d attempts, retrying in %s", ccOctoStartMaxAttempts, ccOctoWatchdogInterval)
}

// ccOctoAutoStartState resolves the watchdog precondition: cc-channel-octo must be
// installed (in PATH), configured (~/.cc-channel-octo/config.json exists), and its
// status determined. A status command error is reported distinctly so the caller
// can log it loudly instead of silently treating it as stopped.
func ccOctoAutoStartState(ctx context.Context) (ccOctoState, error) {
	if _, err := exec.LookPath("cc-channel-octo"); err != nil {
		return ccOctoUnavailable, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ccOctoUnavailable, nil
	}
	if _, err := os.Stat(filepath.Join(home, ccOctoConfigDir, "config.json")); err != nil {
		return ccOctoUnavailable, nil
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "cc-channel-octo", "status").CombinedOutput()
	if err != nil {
		return ccOctoStatusError, err
	}
	if ccOctoStatusRunning(string(out)) {
		return ccOctoRunning, nil
	}
	return ccOctoStopped, nil
}

// ccOctoStatusRunning reports whether `cc-channel-octo status` output indicates a
// running gateway. Mirrors isCcChannelOctoRunning's contract (": running" substring;
// "stopped"/"not running" lack it). Pure, for unit testing.
func ccOctoStatusRunning(statusOutput string) bool {
	return strings.Contains(stripAnsi(statusOutput), ": running")
}
