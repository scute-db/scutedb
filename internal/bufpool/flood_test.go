package bufpool

import (
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

type floodResult struct {
	hotHits, hotTotal int
	stats             Stats
	hotResident       int
}

func touch(t *testing.T, p *Pool, id core.PageID) bool {
	t.Helper()
	before := p.Stats().Hits
	f := mustFetch(t, p, id)
	mustUnpin(t, p, f, false)
	return p.Stats().Hits > before
}

func runFlood(t *testing.T, pol Policy, frames, hot, cold, rounds int, interleave bool) floodResult {
	t.Helper()
	src, ids := seeded(t, hot+cold)
	hotIDs, coldIDs := ids[:hot], ids[hot:]
	p := mustPool(t, src, Options{Frames: frames, Policy: pol})
	for warm := 0; warm < 3; warm++ {
		for _, id := range hotIDs {
			touch(t, p, id)
		}
	}
	p.ResetStats()
	var r floodResult
	if interleave {
		for i := 0; i < rounds*cold; i++ {
			touch(t, p, coldIDs[i%cold])
			if touch(t, p, hotIDs[i%hot]) {
				r.hotHits++
			}
			r.hotTotal++
		}
	} else {
		for round := 0; round < rounds; round++ {
			for _, id := range coldIDs {
				touch(t, p, id)
			}
			for _, id := range hotIDs {
				if touch(t, p, id) {
					r.hotHits++
				}
				r.hotTotal++
			}
		}
	}
	r.stats = p.Stats()
	for _, id := range hotIDs {
		if p.Contains(id) {
			r.hotResident++
		}
	}
	return r
}

func TestASequentialScanWipesTheHotSetUnderLRUButNotUnderLRUK(t *testing.T) {
	const frames, hot, cold = 64, 48, 1000
	got := map[Policy]floodResult{}
	for _, pol := range allPolicies() {
		r := runFlood(t, pol, frames, hot, cold, 3, false)
		got[pol] = r
		t.Logf("%-6s hot hits %d/%d, overall hit ratio %.3f, evictions %d",
			pol, r.hotHits, r.hotTotal, r.stats.HitRatio(), r.stats.Evictions)
	}
	if r := got[LRU]; r.hotHits != 0 {
		t.Errorf("LRU kept %d of %d hot pages through a %d-page scan; the scan should flush them all", r.hotHits, r.hotTotal, cold)
	}
	if r := got[Clock]; r.hotHits != 0 {
		t.Errorf("Clock kept %d of %d hot pages through the scan", r.hotHits, r.hotTotal)
	}
	if r := got[LRUK]; r.hotHits != r.hotTotal || r.hotResident != hot {
		t.Errorf("LRU-K hit %d of %d hot accesses with %d hot pages resident; it should keep the whole hot set",
			r.hotHits, r.hotTotal, r.hotResident)
	}
}

func TestInterleavedScanAndHotAccessesKeepHotPagesOnlyUnderLRUK(t *testing.T) {
	const frames, hot, cold = 64, 48, 1000
	got := map[Policy]floodResult{}
	for _, pol := range allPolicies() {
		r := runFlood(t, pol, frames, hot, cold, 2, true)
		got[pol] = r
		t.Logf("%-6s hot hit ratio %.3f, overall hit ratio %.3f", pol,
			float64(r.hotHits)/float64(r.hotTotal), r.stats.HitRatio())
	}
	lru, lruk := got[LRU], got[LRUK]
	if lruk.hotHits != lruk.hotTotal {
		t.Errorf("LRU-K missed %d hot accesses", lruk.hotTotal-lruk.hotHits)
	}
	if lru.hotHits*20 > lru.hotTotal {
		t.Errorf("LRU hit %d of %d hot accesses; with a reuse distance of 96 pages in 64 frames it should hit almost none",
			lru.hotHits, lru.hotTotal)
	}
	if lruk.stats.HitRatio() <= lru.stats.HitRatio() {
		t.Errorf("LRU-K overall hit ratio %.3f is not above LRU's %.3f", lruk.stats.HitRatio(), lru.stats.HitRatio())
	}
}

func TestWithoutAScanEveryPolicyKeepsAHotSetThatFits(t *testing.T) {
	for _, pol := range allPolicies() {
		r := runFlood(t, pol, 64, 48, 0, 0, false)
		if r.hotResident != 48 {
			t.Errorf("%s: only %d of 48 hot pages resident with no scan at all", pol, r.hotResident)
		}
	}
}
