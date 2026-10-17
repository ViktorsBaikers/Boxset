//go:build !race

package parse

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestTimeoutOvershootReport measures how far a real, default-clock match
// overshoots the fixed 50 ms regexp2 timeout. regexp2 checks the timeout
// against a coarse clock, so a running match can overshoot; this reports the
// observed distribution and asserts only that every run failed closed. It is
// a measurement, not a hard-deadline claim, and is excluded under -race where
// timings are meaningless.
func TestTimeoutOvershootReport(t *testing.T) {
	const runs = 100
	p := newTestParser(t, nil) // default clock
	compileFormats(t, p, 1, single("evil", required(evilPattern)))

	overshoots := make([]time.Duration, 0, runs)
	for i := range runs {
		start := time.Now()
		res, err := p.Evaluate(context.Background(), evilName)
		elapsed := time.Since(start)
		if res != nil {
			t.Fatalf("run %d: a timeout returned results %+v", i, res)
		}
		wantReason(t, err, codeMatchTimeout, map[string]int64{"limit_ms": 50})
		overshoots = append(overshoots, elapsed-matchTimeout)
	}
	slices.Sort(overshoots)
	var sum time.Duration
	for _, o := range overshoots {
		sum += o
	}
	t.Logf("timeout overshoot over %d runs of the %v match timeout: min=%v median=%v p90=%v p99=%v max=%v mean=%v (approximate in-flight check; no hard deadline is claimed)",
		len(overshoots), matchTimeout, overshoots[0], overshoots[len(overshoots)/2], overshoots[len(overshoots)*9/10],
		overshoots[len(overshoots)*99/100], overshoots[len(overshoots)-1], sum/time.Duration(len(overshoots)))
}
