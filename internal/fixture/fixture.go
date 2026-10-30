// Package fixture records outbound wire exchanges against throwaway services and
// replays them offline through the real client. A recording is published only
// after its secrets are scrubbed out and a re-read of the bytes finds none left,
// so a saved set is safe to commit and a test always has something to replay.
package fixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

const (
	// Schema is the one document version this harness writes and reads.
	Schema = 1
	// FileName is the one file a fixture set holds.
	FileName = "exchanges.json"
	// Root is the runner's write root of one set, relative to the repository root.
	Root = "internal/outbound/testdata/fixtures"
)

// Where a set's exchanges came from, and what may be done with them. The recorder
// derives both from the targets it actually drove, so a saved sample states its own
// limits: two recordings of the same set are the same bytes and the disposition is
// part of the artifact, not of a reader's assumption.
const (
	// SourceSynthetic marks a set the recorder generated in the repository: no
	// provider account, credential or live capture reproduces it.
	SourceSynthetic = "synthetic"
	// SourceLiveGet marks a set captured from the owner's live services: the reads
	// it holds are the owner's data, never a write and never a rerunnable record.
	SourceLiveGet = "live-get"
	// RedistributionRepository marks a set the repository ships with its tests.
	RedistributionRepository = "repository-tests-only"
	// RedistributionOwnerOnly marks a set that stays with the owner.
	RedistributionOwnerOnly = "owner-only"
)

// A fault names a wire exchange that produced no usable answer, in the class the
// transport observed: the answer never came, the peer dropped the connection after
// the write, or the body ended before its declared length.
const (
	FaultTimeoutBeforeResponse = "timeout_before_response"
	FaultResetAfterWrite       = "reset_after_write"
	FaultPartial               = "partial"
)

// setPattern is the one set name the runner and this package accept: a
// lower-case word, never a path.
var setPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidSet reports whether name is a fixture set name.
func ValidSet(name string) bool { return setPattern.MatchString(name) }

// Dir is the project-relative directory of one set.
func Dir(set string) string { return Root + "/" + set }

// Request is what a replay has to match: the method and the absolute URL the
// client sent. Request headers and bodies stay out of the file, so a credential
// never travels into a fixture through them.
type Request struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

// Response is the recorded answer of one exchange. Header holds the peer's
// stable headers; a volatile one (Date) is not recorded. Body is the peer's bytes,
// base64 in the file, so a replay serves exactly what arrived whatever the bytes
// are.
type Response struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

// Exchange is one recorded wire exchange. Fault is set when the exchange ended
// without a usable answer; a partial answer keeps its bytes in Response.
type Exchange struct {
	Request  Request   `json:"request"`
	Response *Response `json:"response,omitempty"`
	Fault    string    `json:"fault,omitempty"`
}

// Provenance is where a set came from and what may be done with it. Both halves are
// facts about the recording: a set driven only through services the recorder
// started is the repository's synthetic one, and a read of the owner's live service
// is a capture the repository does not redistribute.
type Provenance struct {
	Source         string `json:"source"`
	Redistribution string `json:"redistribution"`
}

// Valid reports a provenance this harness writes: a source it knows, carrying the
// redistribution disposition that source allows.
func (p Provenance) Valid() error {
	switch p.Source {
	case SourceSynthetic:
		if p.Redistribution != RedistributionRepository {
			return fmt.Errorf("fixture: a synthetic set is not %q", p.Redistribution)
		}
	case SourceLiveGet:
		if p.Redistribution != RedistributionOwnerOnly {
			return fmt.Errorf("fixture: a live capture is not %q", p.Redistribution)
		}
	case "":
		return errors.New("fixture: the document names no provenance")
	default:
		return fmt.Errorf("fixture: the document names the unknown source %q", p.Source)
	}
	return nil
}

// Document is one recorded set: its provenance and every exchange the replay
// serves, in the order they were recorded.
type Document struct {
	Schema     int        `json:"schema"`
	Set        string     `json:"set"`
	Provenance Provenance `json:"provenance"`
	Exchanges  []Exchange `json:"exchanges"`
}

// Valid reports a document this harness can write and read back: one set, a
// provenance, and exchanges that either hold a response or name a fault.
func (d Document) Valid() error {
	switch {
	case d.Schema != Schema:
		return fmt.Errorf("fixture: schema %d is not %d", d.Schema, Schema)
	case !ValidSet(d.Set):
		return fmt.Errorf("fixture: %q is not a set name", d.Set)
	case len(d.Exchanges) == 0:
		return errors.New("fixture: the document holds no exchange")
	}
	if err := d.Provenance.Valid(); err != nil {
		return err
	}
	for i, exchange := range d.Exchanges {
		if exchange.Request.Method == "" {
			return fmt.Errorf("fixture: exchange %d names no method", i)
		}
		if parsed, err := url.Parse(exchange.Request.URL); err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return fmt.Errorf("fixture: exchange %d names no absolute URL", i)
		}
		switch exchange.Fault {
		case "":
			if exchange.Response == nil {
				return fmt.Errorf("fixture: exchange %d holds neither a response nor a fault", i)
			}
		case FaultTimeoutBeforeResponse, FaultResetAfterWrite:
			// These are the peer's silence, so there is nothing to serve with them.
			if exchange.Response != nil {
				return fmt.Errorf("fixture: exchange %d holds a response and a %s fault", i, exchange.Fault)
			}
		case FaultPartial:
			// The bytes that did arrive are the whole record of a cut answer, and a
			// replay without them has nothing to hand the client.
			if exchange.Response == nil {
				return fmt.Errorf("fixture: exchange %d holds a %s fault and no response", i, exchange.Fault)
			}
		default:
			return fmt.Errorf("fixture: exchange %d names the unknown fault %q", i, exchange.Fault)
		}
	}
	return nil
}

// Encode is the file's bytes: indented JSON and a newline, so two recordings of
// the same set compare equal and a change is reviewable. A body is written base64
// and reads back byte for byte; the round trip is proved where a body reaches the
// replay, not by a second parse here that cannot fail.
func Encode(doc Document) ([]byte, error) {
	if err := doc.Valid(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Decode strictly reads one document: no unknown field, nothing after it, and
// every exchange whole.
func Decode(data []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("fixture: data after the document")
	}
	if err := doc.Valid(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Load reads the set file at path through Decode.
func Load(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	return Decode(data)
}

// Publish writes doc's bytes as path's FileName inside dir. The bytes are
// composed before anything is opened, part of a file is never visible as the
// set, and a document that fails its own rules writes nothing at all.
func Publish(dir string, doc Document) error {
	data, err := Encode(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	part, err := os.CreateTemp(dir, "."+FileName+".part.*")
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return errors.Join(err, part.Close(), os.Remove(part.Name()))
	}
	if err := part.Chmod(0o644); err != nil {
		return errors.Join(err, part.Close(), os.Remove(part.Name()))
	}
	if err := part.Close(); err != nil {
		return errors.Join(err, os.Remove(part.Name()))
	}
	if err := os.Rename(part.Name(), filepath.Join(dir, FileName)); err != nil {
		return errors.Join(err, os.Remove(part.Name()))
	}
	return nil
}
