package api

import (
	"context"
	"net/http"
	"runtime"
	"time"
)

// Status is the lifecycle snapshot an authorized principal may read.
type Status struct {
	State           string    `json:"state" enum:"starting,recovering,ready,draining"`
	Reason          string    `json:"reason,omitempty" enum:"key_missing,key_mismatch,restore_in_progress" doc:"Only while recovering."`
	Version         string    `json:"version"`
	SchemaVersion   int64     `json:"schemaVersion" minimum:"1"`
	RestoreEpoch    int64     `json:"restoreEpoch" minimum:"1"`
	StartedAt       time.Time `json:"startedAt"`
	ReadyAt         time.Time `json:"readyAt,omitzero"`
	Timings         Timings   `json:"timings"`
	GoMemLimitBytes int64     `json:"goMemLimitBytes" minimum:"1"`
	Goroutines      int       `json:"goroutines" minimum:"1" doc:"Live goroutine count of this process, sampled when the snapshot is taken."`
}

// Timings are the startup phases, in milliseconds, reported apart.
type Timings struct {
	MigrationMs int64 `json:"migrationMs" minimum:"0"`
	RecoveryMs  int64 `json:"recoveryMs" minimum:"0"`
	SetupMs     int64 `json:"setupMs,omitempty" minimum:"0" doc:"Setup-token preparation; only while setup is open."`
}

// StatusOutput is the 200 response.
type StatusOutput struct {
	Body Status
}

func (h *handlers) status(ctx context.Context, _ *struct{}) (*StatusOutput, error) {
	st, err := h.svc.Status(ctx)
	if err != nil {
		return nil, NewProblem(http.StatusInternalServerError, "internal_error")
	}
	// The service snapshot is assembled elsewhere; this process reads its own
	// goroutine count at the moment the response is built.
	st.Goroutines = runtime.NumGoroutine()
	return &StatusOutput{Body: st}, nil
}
