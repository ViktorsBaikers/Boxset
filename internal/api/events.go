package api

// The event stream operation and the frames it carries. The route is a thin
// protected read: the guard has already resolved the principal and the credential
// ref it authenticated, the hub decides admission, and this handler passes both
// to the stream. No SQL runs and no right is decided here.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/events"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
)

// EventJob is one committed job transition's frame payload: the same job the
// jobs read model serves, so a client decodes a frame with the generated types
// and compares it field for field with GET /api/v1/system/jobs.
type EventJob struct {
	Job Job `json:"job"`
}

// EventUsers is one committed users-collection change: the revision a client
// invalidates against.
type EventUsers struct {
	CollectionRevision int64 `json:"collectionRevision" minimum:"1"`
}

// EventSystem is one lifecycle transition the stream announces.
type EventSystem struct {
	State string `json:"state" enum:"starting,recovering,ready,draining"`
}

// EventsInput carries the client's cursor, the last frame it received. The value
// is unbounded here: the server's own header bound already limits it, and a cursor
// the hub cannot parse is the recorded resync{invalid_id} of the stream contract,
// never a validation refusal before the handshake.
type EventsInput struct {
	LastEventID string `header:"Last-Event-ID"`
}

// JobEvent is the frame payload of one committed job transition.
func JobEvent(ev jobs.Event) EventJob {
	job := Job{
		ID:          ev.JobID,
		Kind:        ev.Kind,
		Class:       ev.Class,
		Lane:        ev.Lane,
		State:       ev.State,
		ReasonCode:  ev.Reason,
		Attempts:    ev.Attempts,
		MaxAttempts: ev.MaxAttempts,
		DueAt:       time.Unix(ev.DueAt, 0).UTC(),
		CreatedAt:   time.Unix(ev.CreatedAt, 0).UTC(),
		UpdatedAt:   time.Unix(ev.UpdatedAt, 0).UTC(),
		Revision:    ev.Revision,
	}
	if len(ev.Params) > 0 {
		job.ReasonParams = make(ProblemParams, len(ev.Params))
		for name, value := range ev.Params {
			job.ReasonParams[name] = value
		}
	}
	return EventJob{Job: job}
}

// streamEvents admits one stream before the response head is written, so a
// refusal keeps its status: 429 stream_cap or 503 draining. The frames follow on
// the response writer the stream owns.
func (h *handlers) streamEvents(ctx context.Context, in *EventsInput) (*huma.StreamResponse, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, NewProblem(http.StatusUnauthorized, "unauthenticated")
	}
	ref, ok := auth.CredentialRefFrom(ctx)
	if !ok || !ref.Valid() {
		return nil, NewProblem(http.StatusUnauthorized, "credential_invalid")
	}
	hub := h.svc.Events()
	if hub == nil {
		return nil, NewProblem(http.StatusInternalServerError, "internal_error")
	}
	stream, err := hub.Admit(p, ref, in.LastEventID)
	if err != nil {
		var refusal *events.Error
		if errors.As(err, &refusal) {
			problem, retry := StreamRefusal(refusal)
			if retry > 0 {
				return nil, WithRetryAfter(problem, retry)
			}
			return nil, problem
		}
		return nil, NewProblem(http.StatusUnauthorized, "credential_invalid")
	}
	return &huma.StreamResponse{Body: stream.Serve}, nil
}

// StreamRefusal is the answer a stream route gives the hub's refusal, and the
// Retry-After a stream_cap carries: the GET answers it as its status, and the
// HEAD that aliases the GET advertises it instead of a stream it would not get.
func StreamRefusal(refusal *events.Error) (*Problem, int) {
	switch refusal.Reason {
	case events.ReasonStreamCap:
		return NewProblem(http.StatusTooManyRequests, events.ReasonStreamCap), int(refusal.RetryAfter / time.Second)
	case events.ReasonDraining:
		return NewProblem(http.StatusServiceUnavailable, events.ReasonDraining), 0
	}
	return NewProblem(http.StatusUnauthorized, "credential_invalid"), 0
}
