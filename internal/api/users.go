package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ViktorsBaikers/Boxset/internal/auth"
)

type UserQuery struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Cursor string `query:"cursor" maxLength:"1024"`
	Sort   string `query:"sort" enum:"username,createdAt" default:"username"`
	Order  string `query:"order" enum:"asc,desc" default:"asc"`
	Q      string `query:"q" maxLength:"64"`
	Role   string `query:"role" enum:"admin,member,viewer"`
}

type UserListInput struct {
	UserQuery
	IfNoneMatch string `header:"If-None-Match"`
}

type UserPage struct {
	Items              []User `json:"items" maxItems:"500" nullable:"false"`
	NextCursor         string `json:"nextCursor,omitempty" maxLength:"1024"`
	CollectionRevision int64  `json:"collectionRevision" minimum:"1"`
}

type UserListOutput struct {
	Status       int
	ETag         string `header:"ETag"`
	Vary         string `header:"Vary"`
	CacheControl string `header:"Cache-Control"`
	Body         *UserPage
}

type UserPatch struct {
	ExpectedRevision int64   `json:"expectedRevision" minimum:"1"`
	Role             *string `json:"role,omitempty" enum:"admin,member,viewer"`
	Disabled         *bool   `json:"disabled,omitempty"`
}

type UserPatchInput struct {
	ID   int64 `path:"id" minimum:"1"`
	Body UserPatch
}
type UserPatchOutput struct{ Body User }

type userCursor struct {
	V                  int                `json:"v"`
	InstallID          string             `json:"installId"`
	RestoreEpoch       int64              `json:"restoreEpoch"`
	SchemaVersion      int64              `json:"schemaVersion"`
	Query              string             `json:"query"`
	Scope              string             `json:"scope"`
	CollectionRevision int64              `json:"collectionRevision"`
	After              [2]json.RawMessage `json:"after"`
}

func encodeUserCursor(cursor userCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeUserCursor(encoded string) (userCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(b) == 0 || b[0] != '{' {
		return userCursor{}, NewProblem(400, "cursor_invalid")
	}
	var version struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(b, &version); err != nil {
		return userCursor{}, NewProblem(400, "cursor_invalid")
	}
	if version.V != 1 {
		return userCursor{}, staleCursor("cursor_version")
	}
	var cursor userCursor
	if err := json.Unmarshal(b, &cursor); err != nil {
		return userCursor{}, NewProblem(400, "cursor_invalid")
	}
	return cursor, nil
}

func staleCursor(reason string) *Problem {
	p := NewProblem(409, "cursor_stale")
	p.Params = ProblemParams{"reason": reason}
	return p
}

func (h *handlers) listUsers(ctx context.Context, in *UserListInput) (*UserListOutput, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, NewProblem(401, "unauthenticated")
	}
	if !p.Has(auth.RightUsersRead) {
		return nil, NewProblem(403, "forbidden")
	}
	search := ""
	if in.Q != "" {
		_, key, err := auth.UsernameIdentity(in.Q)
		if err != nil {
			return nil, NewProblem(422, "validation_failed")
		}
		search = key
	}
	query := auth.UserQuery{Limit: in.Limit, Sort: in.Sort, Order: in.Order, Search: search, Role: in.Role}
	normalized, _ := json.Marshal(struct {
		Limit                     int
		Sort, Order, Search, Role string
	}{query.Limit, query.Sort, query.Order, query.Search, query.Role})
	querySum := sha256.Sum256(normalized)
	queryHash := hex.EncodeToString(querySum[:])
	rights := slices.Clone(p.Rights)
	slices.Sort(rights)
	scope := strings.Join(slices.Compact(rights), " ")
	credential := "cookie"
	if session, ok := auth.SessionFrom(ctx); ok {
		credential = session.Credential
	}
	var cursor userCursor
	if in.Cursor != "" {
		var err error
		cursor, err = decodeUserCursor(in.Cursor)
		if err != nil {
			return nil, err
		}
		// Identity/revision priority is checked against the snapshot below. The
		// bounded anchor is only a SQL parameter and grants no authority.
		if err := json.Unmarshal(cursor.After[1], &query.AfterID); err != nil || query.AfterID < 1 {
			return nil, NewProblem(400, "cursor_invalid")
		}
		if query.Sort == "username" {
			if err := json.Unmarshal(cursor.After[0], &query.AfterName); err != nil || !utf8.ValidString(query.AfterName) || utf8.RuneCountInString(query.AfterName) > auth.MaxUsernameRunes {
				return nil, NewProblem(400, "cursor_invalid")
			}
		} else if err := json.Unmarshal(cursor.After[0], &query.AfterCreated); err != nil {
			return nil, NewProblem(400, "cursor_invalid")
		}
	}
	var result *UserListOutput
	err := h.svc.ListUsers(ctx, query, func(collection auth.UserCollection) error {
		if in.Cursor != "" {
			if cursor.InstallID != collection.InstallID || cursor.Query != queryHash || cursor.Scope != scope {
				return NewProblem(400, "cursor_invalid")
			}
			if cursor.RestoreEpoch != collection.RestoreEpoch {
				return staleCursor("restore_epoch")
			}
			if cursor.SchemaVersion != collection.SchemaVersion {
				return staleCursor("schema_version")
			}
			if cursor.CollectionRevision != collection.Revision {
				return staleCursor("collection_revision")
			}
		}
		identity, _ := json.Marshal([]any{collection.InstallID, collection.RestoreEpoch, collection.SchemaVersion, collection.Revision, string(normalized), in.Cursor, credential, scope})
		sum := sha256.Sum256(identity)
		tag := `W/"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
		result = &UserListOutput{Status: 200, ETag: tag, Vary: "Cookie, X-Api-Key, Accept-Encoding", CacheControl: "private, no-cache"}
		if weakETagMatch(in.IfNoneMatch, tag) {
			result.Status = 304
			return nil
		}
		page := &UserPage{Items: make([]User, 0, min(query.Limit, len(collection.Items))), CollectionRevision: collection.Revision}
		for _, row := range collection.Items[:min(query.Limit, len(collection.Items))] {
			page.Items = append(page.Items, userOf(row))
		}
		if len(collection.Items) > query.Limit {
			last := page.Items[len(page.Items)-1]
			var anchor any = last.Username
			if query.Sort == "createdAt" {
				anchor = last.CreatedAt.Unix()
			}
			key, _ := json.Marshal(anchor)
			id, _ := json.Marshal(last.ID)
			page.NextCursor = encodeUserCursor(userCursor{V: 1, InstallID: collection.InstallID, RestoreEpoch: collection.RestoreEpoch, SchemaVersion: collection.SchemaVersion, Query: queryHash, Scope: scope, CollectionRevision: collection.Revision, After: [2]json.RawMessage{key, id}})
		}
		result.Body = page
		return nil
	})
	if err != nil {
		var problem *Problem
		if errors.As(err, &problem) {
			return nil, problem
		}
		return nil, NewProblem(500, "internal_error")
	}
	return result, nil
}

func weakETagMatch(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(tag, "W/") {
			return true
		}
	}
	return false
}

func (h *handlers) patchUser(ctx context.Context, in *UserPatchInput) (*UserPatchOutput, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, NewProblem(401, "unauthenticated")
	}
	if !p.Has(auth.RightUsersWrite) {
		return nil, NewProblem(403, "forbidden")
	}
	if in.Body.Role == nil && in.Body.Disabled == nil {
		return nil, NewProblem(422, "patch_empty")
	}
	actor, ok := auth.ActorFrom(ctx)
	if !ok {
		return nil, NewProblem(http.StatusUnauthorized, "credential_invalid")
	}
	u, err := h.svc.UpdateUser(ctx, actor, in.ID, auth.UserPatch{ExpectedRevision: in.Body.ExpectedRevision, Role: in.Body.Role, Disabled: in.Body.Disabled})
	if err == nil {
		return &UserPatchOutput{Body: userOf(u)}, nil
	}
	var conflict *auth.RevisionConflict
	switch {
	case errors.As(err, &conflict):
		problem := NewProblem(409, "revision_conflict")
		problem.CurrentRevision = conflict.CurrentRevision
		return nil, problem
	case errors.Is(err, auth.ErrLastAdmin):
		return nil, NewProblem(409, "last_admin")
	case errors.Is(err, auth.ErrUserNotFound):
		return nil, NewProblem(404, "user_not_found")
	case errors.Is(err, auth.ErrForbidden):
		return nil, NewProblem(http.StatusForbidden, "forbidden")
	case errors.Is(err, auth.ErrSessionInvalid):
		return nil, NewProblem(http.StatusUnauthorized, "credential_invalid")
	}
	return nil, NewProblem(http.StatusInternalServerError, "internal_error")
}
