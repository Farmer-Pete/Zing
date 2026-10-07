package orchestrator

// BaseFallback is one ticket's newest base fetch that fell back to the
// last fetched base because the fetch itself failed (#68 follow-up).
type BaseFallback struct {
	Branch string // the project's default branch, as o.proj.DefaultBranch
	SHA    string // the commit refs/zing/base/DEFAULT held after the fallback
	Reason string // one of no_origin, remote_ref_missing, auth, network, fetch_failed
}

// recordFallback remembers f as ticketID's newest fallback, for
// TakeBaseFallback. The map is made on first use, so New needs no change.
func (o *Orchestrator) recordFallback(ticketID int64, f BaseFallback) {
	o.fallbackGuard.Lock()
	defer o.fallbackGuard.Unlock()
	if o.fallbacks == nil {
		o.fallbacks = make(map[int64]BaseFallback)
	}
	o.fallbacks[ticketID] = f
}

// TakeBaseFallback returns the newest fallback fetchBase recorded for
// ticketID since the last call, and forgets it. ok is false when no
// fetch for the ticket fell back since then.
func (o *Orchestrator) TakeBaseFallback(ticketID int64) (f BaseFallback, ok bool) {
	o.fallbackGuard.Lock()
	defer o.fallbackGuard.Unlock()
	f, ok = o.fallbacks[ticketID]
	delete(o.fallbacks, ticketID)
	return f, ok
}
