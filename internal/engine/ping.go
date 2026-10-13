package engine

import (
	"sort"
	"time"

	"easbridge/internal/upstream"
)

// Ping keeps new mail flowing (DESIGN section 4, "Push"): one long poll over all
// synced mail folders. A status 2 answer only marks the changed folders dirty;
// the worker syncs them, and the loop waits for that before polling again
// (otherwise Exchange would answer "changes" again at once).
//
// Heartbeat: start at PingHeartbeat (480 s). If a held request dies early with a
// transport error, a proxy in front of Exchange is cutting long requests, so the
// heartbeat steps down towards PingMinHeartbeat (120 s).

// pingSet returns the EAS ids and local ids of the folders to watch: mail
// folders that have been bootstrapped (a non-zero Sync key).
func (a *Account) pingSet() (sids []string, locals map[string]int64) {
	folders, err := a.st.Folders()
	if err != nil {
		return nil, nil
	}
	locals = map[string]int64{}
	// Most important folders first, so that a server limit on the number of
	// folders cuts off the least important ones.
	sort.SliceStable(folders, func(i, j int) bool {
		return folderPrio(folders[i].EASType) < folderPrio(folders[j].EASType)
	})
	for _, f := range folders {
		if f.SyncKey == "0" || f.SyncKey == "" {
			continue
		}
		sids = append(sids, f.ServerID)
		locals[f.ServerID] = f.ID
	}
	return sids, locals
}

func (a *Account) sleepCtx(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-a.ctx.Done():
		return false
	}
}

// pingDegradeAfter is the number of consecutive failed Pings (not counting
// those that only shorten the heartbeat) after which push is given up on and
// every folder is synced periodically instead.
const pingDegradeAfter = 3

// pingFailing records one more failed Ping. A proxy that cuts every held
// request even at the shortest heartbeat would otherwise leave the mailbox
// without any way to learn about new mail.
func (a *Account) pingFailing(consecutive int) {
	if consecutive >= pingDegradeAfter && !a.pingDegraded.Swap(true) {
		a.log.Warn("Ping keeps failing; syncing every folder periodically until it works again", "every", a.cfg.SweepInterval.String())
	}
}

// pingWorked records a Ping answer that proves push works.
func (a *Account) pingWorked() {
	if a.pingDegraded.Swap(false) {
		a.log.Info("Ping works again; push resumed")
	}
}

func (a *Account) pingLoop() {
	hb := a.cfg.PingHeartbeat
	failures := 0
	hard := 0  // consecutive failures that did not just shorten the heartbeat
	hier := 0  // consecutive answers "folder hierarchy changed" (status 7)
	limit := 0 // maximum number of folders the server accepts (0 = no limit known)
	for {
		if a.ctx.Err() != nil {
			return
		}
		if a.authFailed.Load() || a.wiped.Load() {
			if !a.sleepCtx(time.Second) {
				return
			}
			continue
		}
		if !a.pauseWait() { // the breaker is open: Ping waits like everything else
			return
		}
		sids, locals := a.pingSet()
		if limit > 0 && len(sids) > limit {
			sids = sids[:limit]
		}
		if len(sids) == 0 {
			if !a.sleepCtx(200 * time.Millisecond) {
				return
			}
			continue
		}
		a.pingHB.Store(int64(hb))
		start := time.Now()
		res, err := a.upstream().Ping(a.ctx, hb, sids)
		elapsed := time.Since(start)

		if res == nil {
			if err == nil {
				err = upstream.ErrUnavailable
			}
			if a.ctx.Err() != nil {
				return
			}
			if isPaused(err) {
				continue // the breaker opened just now; nothing was sent and Ping did not fail
			}
			if a.handleUpstreamError(err) {
				continue
			}
			failures++
			// A request that was held for a while and then died before the
			// heartbeat ended was cut by something in between (a proxy with a
			// shorter idle timeout). A connection that fails at once is just an
			// outage and says nothing about the right heartbeat.
			minHold := a.cfg.PingMinHeartbeat / 2
			if minHold > 5*time.Second {
				minHold = 5 * time.Second
			}
			if elapsed >= minHold && elapsed < hb && hb > a.cfg.PingMinHeartbeat {
				next := hb * 5 / 8
				if next < a.cfg.PingMinHeartbeat {
					next = a.cfg.PingMinHeartbeat
				}
				a.log.Info("Ping was cut short; using a shorter heartbeat", "was", hb.String(), "now", next.String(), "err", err)
				hb = next
			} else {
				hard++
				a.pingFailing(hard)
				a.log.Warn("Ping failed", "err", err)
			}
			if !a.sleepCtx(a.cfg.Backoff(failures)) {
				return
			}
			continue
		}

		if res.Status != 7 {
			hier = 0
		}
		switch res.Status {
		case 1:
			failures, hard = 0, 0
			a.pingWorked()
		case 2:
			failures, hard = 0, 0
			a.pingWorked()
			var ids []int64
			for _, sid := range res.Changed {
				if id, ok := locals[sid]; ok {
					ids = append(ids, id)
				}
			}
			a.markDirtyServerIDs(res.Changed)
			a.waitSynced(ids, 10*time.Minute)
		case 5:
			// Heartbeat outside the server's range: adopt its value.
			if res.Heartbeat > 0 {
				a.log.Info("server asked for another Ping heartbeat", "was", hb.String(), "now", res.Heartbeat.String())
				hb = res.Heartbeat
				if hb < 10*time.Second && a.cfg.PingMinHeartbeat >= 10*time.Second {
					hb = a.cfg.PingMinHeartbeat
				}
			} else if hb > a.cfg.PingMinHeartbeat {
				hb = a.cfg.PingMinHeartbeat
			}
		case 6:
			// Too many folders: watch fewer; the engine still syncs the rest when
			// it syncs everything after a restart or a hierarchy change.
			limit = len(sids) / 2
			if limit < 1 {
				limit = 1
			}
			a.pingLimited.Store(true)
			a.log.Info("server limits the number of Ping folders", "now", limit)
		case 7:
			// The folder hierarchy changed. A Ping is only meaningful once the
			// hierarchy is refreshed, so wait for the FolderSync (it may be failing,
			// or held back by the circuit breaker) instead of asking again at once,
			// and if Exchange keeps saying 7 wait longer and longer.
			hier++
			a.requestFolderSync()
			a.waitFolderSync(10 * time.Minute)
			if !a.sleepCtx(a.hierarchyWait(hier)) {
				return
			}
		default: // 3, 4, 8 and anything unknown
			failures++
			hard++
			a.pingFailing(hard)
			a.log.Warn("Ping answered with an error status", "status", res.Status)
			if !a.sleepCtx(a.cfg.Backoff(failures)) {
				return
			}
		}
	}
}

// hierarchyWait is the pause after the nth consecutive Ping status 7: the base,
// doubling, capped at 60 times the base.
func (a *Account) hierarchyWait(n int) time.Duration {
	base := a.cfg.HierarchyBackoff
	d := base
	for i := 1; i < n && d < 60*base; i++ {
		d *= 2
	}
	if d > 60*base {
		d = 60 * base
	}
	return d
}

// waitFolderSync blocks until the pending hierarchy sync has run, the timeout
// passes or the account stops.
func (a *Account) waitFolderSync(timeout time.Duration) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		a.wmu.Lock()
		want, clean := a.wantFolderSync, a.cleanCh
		a.wmu.Unlock()
		if !want || a.authFailed.Load() || a.wiped.Load() {
			return
		}
		select {
		case <-clean:
		case <-tick.C:
		case <-deadline.C:
			return
		case <-a.ctx.Done():
			return
		}
	}
}

// PingHeartbeat returns the heartbeat the Ping loop currently uses.
func (a *Account) PingHeartbeat() time.Duration { return time.Duration(a.pingHB.Load()) }
