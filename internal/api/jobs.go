package api

// The jobs read model. A job row is the durable intent of accepted work, so the
// route that lists it is a protected read under the right the caller already
// holds: it pages by keyset and shows each row's state and the reason word that
// put it there. Nothing here admits, mutates or decides a right.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
	"github.com/ViktorsBaikers/Boxset/internal/jobs"
)

// Job is one durable job as an authorized reader sees it. Its identity keys,
// params, lease and creator are not part of the wire contract.
type Job struct {
	ID           int64         `json:"id" minimum:"1"`
	Kind         string        `json:"kind" pattern:"^[a-z0-9._-]{1,64}$"`
	Class        string        `json:"class" enum:"required,optional"`
	Lane         string        `json:"lane" enum:"interactive,background"`
	State        string        `json:"state" enum:"queued,running,reconciling,succeeded,failed,review,expired"`
	ReasonCode   string        `json:"reasonCode,omitempty" pattern:"^[a-z_]{1,64}$"`
	ReasonParams ProblemParams `json:"reasonParams,omitempty"`
	Attempts     int           `json:"attempts" minimum:"0"`
	MaxAttempts  int           `json:"maxAttempts" minimum:"1" maximum:"10"`
	DueAt        time.Time     `json:"dueAt"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`
	Revision     int64         `json:"revision" minimum:"1"`
}

// JobQuery is the list's filter and page bound.
type JobQuery struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Cursor string `query:"cursor" maxLength:"1024"`
	State  string `query:"state" enum:"queued,running,reconciling,succeeded,failed,review,expired"`
}

type JobListInput struct{ JobQuery }

// JobPage is one page of jobs, newest first.
type JobPage struct {
	Items      []Job  `json:"items" maxItems:"500" nullable:"false"`
	NextCursor string `json:"nextCursor,omitempty" maxLength:"1024"`
}

type JobListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JobPage
}

// jobCursor is one page's bound anchor. It carries the snapshot it was produced
// under, so a page from another installation, epoch or filter is refused instead
// of silently answered with rows the caller never saw.
type jobCursor struct {
	V            int    `json:"v"`
	InstallID    string `json:"installId"`
	RestoreEpoch int64  `json:"restoreEpoch"`
	State        string `json:"state"`
	AfterID      int64  `json:"afterId"`
}

func encodeJobCursor(cursor jobCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeJobCursor(encoded string) (jobCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(b) == 0 || b[0] != '{' {
		return jobCursor{}, NewProblem(400, "cursor_invalid")
	}
	var version struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(b, &version); err != nil {
		return jobCursor{}, NewProblem(400, "cursor_invalid")
	}
	if version.V != 1 {
		return jobCursor{}, staleCursor("cursor_version")
	}
	var cursor jobCursor
	if err := json.Unmarshal(b, &cursor); err != nil {
		return jobCursor{}, NewProblem(400, "cursor_invalid")
	}
	return cursor, nil
}

func (h *handlers) listJobs(ctx context.Context, in *JobListInput) (*JobListOutput, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, NewProblem(401, "unauthenticated")
	}
	if !p.Has(auth.RightStatusRead) {
		return nil, NewProblem(403, "forbidden")
	}
	query := jobs.Query{Limit: in.Limit, State: in.State}
	var cursor jobCursor
	if in.Cursor != "" {
		var err error
		cursor, err = decodeJobCursor(in.Cursor)
		if err != nil {
			return nil, err
		}
		if cursor.State != in.State || cursor.AfterID < 1 {
			return nil, NewProblem(400, "cursor_invalid")
		}
		query.AfterID = cursor.AfterID
	}
	page, err := h.svc.ListJobs(ctx, query)
	if err != nil {
		var problem *Problem
		if errors.As(err, &problem) {
			return nil, problem
		}
		return nil, NewProblem(http.StatusInternalServerError, "internal_error")
	}
	if in.Cursor != "" {
		// The identity and epoch are read in the same snapshot as the rows, so a
		// page never mixes two installations or a restored one with its former
		// epochs.
		if cursor.InstallID != page.InstallID {
			return nil, NewProblem(400, "cursor_invalid")
		}
		if cursor.RestoreEpoch != page.RestoreEpoch {
			return nil, staleCursor("restore_epoch")
		}
	}
	out := &JobListOutput{CacheControl: "private, no-cache", Body: JobPage{Items: make([]Job, 0, len(page.Items))}}
	for _, row := range page.Items {
		out.Body.Items = append(out.Body.Items, jobOf(row))
	}
	if page.More && len(out.Body.Items) > 0 {
		last := out.Body.Items[len(out.Body.Items)-1]
		out.Body.NextCursor = encodeJobCursor(jobCursor{V: 1, InstallID: page.InstallID, RestoreEpoch: page.RestoreEpoch, State: in.State, AfterID: last.ID})
	}
	return out, nil
}

// jobOf is the wire form of one read-model row.
func jobOf(row jobs.Job) Job {
	job := Job{
		ID:          row.ID,
		Kind:        row.Kind,
		Class:       row.Class,
		Lane:        row.Lane,
		State:       row.State,
		ReasonCode:  row.ReasonCode,
		Attempts:    row.Attempts,
		MaxAttempts: row.MaxAttempts,
		DueAt:       time.Unix(row.DueAt, 0).UTC(),
		CreatedAt:   time.Unix(row.CreatedAt, 0).UTC(),
		UpdatedAt:   time.Unix(row.UpdatedAt, 0).UTC(),
		Revision:    row.Revision,
	}
	if len(row.ReasonParams) > 0 {
		job.ReasonParams = make(ProblemParams, len(row.ReasonParams))
		for name, value := range row.ReasonParams {
			job.ReasonParams[name] = value
		}
	}
	return job
}
