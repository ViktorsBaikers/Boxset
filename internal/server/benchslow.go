//go:build benchslow

package server

import "time"

// The slow control's three recorded delays (plan.md § Validation strategy,
// Timing protocol): a client measures the start band through the pre-listen
// delay, the key band through the per-key-verification delay and the list band
// through the list-response delay.
const (
	startDelay = 300 * time.Millisecond
	keyDelay   = 200 * time.Microsecond
	listDelay  = 25 * time.Millisecond
)

var _ benchSlowHooks = (*Server)(nil)

func (*Server) benchStartDelay() { time.Sleep(startDelay) }

func (*Server) benchKeyDelay() { time.Sleep(keyDelay) }

func (*Server) benchListDelay() { time.Sleep(listDelay) }
