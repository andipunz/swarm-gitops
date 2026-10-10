package controller

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// waitForCI reports whether t's commit must not be deployed yet because check
// runs of its CI (anything but swarm-gitops' own) are still queued or running
// - typically the workflow building the image the new configuration needs.
// Deploying before it finishes rolls the new configuration out with the
// previous commit's image under the same tag, which fails (and gets rolled
// back) as soon as the configuration depends on the new code.
//
// The decision is made per scan, never by blocking: a waiting commit is
// simply looked at again on the next one. After CI_WAIT_TIMEOUT the commit is
// deployed anyway. note, if set, says why the deploy went ahead without a
// clean CI result and ends up in the check run's summary.
func (c *Controller) waitForCI(t Target, log *slog.Logger) (wait bool, note string) {
	// The Checks API only answers GitHub App auth (see newRun).
	if c.cfg.CIWait == 0 || c.cfg.Token != "" {
		return false, ""
	}
	key := t.Stack + "@" + t.Branch.Commit
	now := time.Now()
	c.cacheMu.Lock()
	since, seen := c.ciSince[key]
	if !seen {
		since = now
		for k := range c.ciSince { // an older commit of the stack that was superseded while waiting
			if strings.HasPrefix(k, t.Stack+"@") {
				delete(c.ciSince, k)
			}
		}
		c.ciSince[key] = now
	}
	c.cacheMu.Unlock()
	waited := now.Sub(since).Round(time.Second)
	timedOut := now.Sub(since) >= c.cfg.CIWait
	finish := func() {
		c.cacheMu.Lock()
		delete(c.ciSince, key)
		c.cacheMu.Unlock()
	}

	runs, err := c.git.CommitCheckRuns(t.Repo, t.Branch.Commit)
	if err != nil {
		if !timedOut {
			log.Warn("reading CI status failed, retrying on the next scan", "err", err)
			return true, ""
		}
		finish()
		return false, fmt.Sprintf("Deployed without knowing whether CI had finished: its status could not be read for %s (%v).", waited, err)
	}
	var pending, failed []string
	others := 0
	for _, cr := range runs {
		if strings.HasPrefix(cr.Name, "swarm / ") || (c.cfg.AppID != 0 && cr.AppID == c.cfg.AppID) {
			continue
		}
		others++
		switch {
		case cr.Status != "completed":
			pending = append(pending, cr.Name)
		case !contains([]string{"success", "neutral", "skipped"}, cr.Conclusion):
			failed = append(failed, fmt.Sprintf("%s (%s)", cr.Name, cr.Conclusion))
		}
	}
	// GitHub creates a workflow's check runs a few seconds after the push. A
	// commit seen for the first time without any gets one more scan for them
	// to show up; one without CI at all is deployed on that next scan.
	if others == 0 && !seen {
		return true, ""
	}
	if len(pending) > 0 {
		if !timedOut {
			if !seen {
				log.Info("waiting for CI before deploying", "checks", strings.Join(pending, ", "))
			}
			return true, ""
		}
		finish()
		return false, fmt.Sprintf("Deployed after waiting %s for CI, which was still running: %s.", waited, strings.Join(pending, ", "))
	}
	finish()
	if seen && others > 0 {
		log.Info("CI finished, deploying", "waited", waited)
	}
	if len(failed) > 0 {
		return false, fmt.Sprintf("CI did not succeed (%s) - deployed anyway, with whatever images the configuration references.", strings.Join(failed, ", "))
	}
	return false, ""
}
