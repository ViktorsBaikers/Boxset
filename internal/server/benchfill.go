//go:build benchfill

package server

import (
	"fmt"

	"github.com/ViktorsBaikers/Boxset/internal/bounded"
)

var _ benchFillHooks = (*Server)(nil)

// benchFill fills three collections of its own to the recorded caps —
// bounded.LogRingCap, bounded.ReleaseCacheCap and bounded.ReplayRingCap — and
// hands them back, so the hook site holds them for the process lifetime and the
// RSS series measures a full-buffer idle process. It fills its own collections,
// not the server's real log ring, whose byte ceiling is the same cap. Each fill
// is one write of exactly the cap that collection enforces: the ring keeps an
// entry of the whole cap, the cache a key plus value that sum to the cap.
//
// A fill that retained less would measure an empty process and pass the budget
// without proving anything, and the cache drops an entry one byte over its cap
// without an error, so each fill is checked here, at fill time, while nothing
// has aged out. The retained collections are never read again: reading one drops
// what has aged out, so the replay ring would report 0 B after its five minutes
// while its whole cap is still held.
func (*Server) benchFill() (*bounded.Ring, *bounded.Cache, *bounded.Ring) {
	logRing := bounded.NewRing(bounded.LogRingCap, bounded.LogRingAge)
	logRing.Append(make([]byte, bounded.LogRingCap))
	if got := logRing.Bytes(); got < bounded.LogRingCap {
		panic(fmt.Sprintf("benchfill: log ring retained %d B of %d", got, bounded.LogRingCap))
	}

	releaseCache := bounded.NewCache(bounded.ReleaseCacheCap, bounded.ReleaseTTL)
	key := "releases"
	releaseCache.Put(key, make([]byte, bounded.ReleaseCacheCap-len(key)))
	if got := releaseCache.Bytes(); got < bounded.ReleaseCacheCap {
		panic(fmt.Sprintf("benchfill: release cache retained %d B of %d", got, bounded.ReleaseCacheCap))
	}

	replayRing := bounded.NewRing(bounded.ReplayRingCap, bounded.ReplayRingAge)
	replayRing.Append(make([]byte, bounded.ReplayRingCap))
	if got := replayRing.Bytes(); got < bounded.ReplayRingCap {
		panic(fmt.Sprintf("benchfill: replay ring retained %d B of %d", got, bounded.ReplayRingCap))
	}

	return logRing, releaseCache, replayRing
}
