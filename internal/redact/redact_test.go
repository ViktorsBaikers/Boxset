package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

const sentinel = "S3NT1NEL-c0ffee-9f8e7d"

func TestSentinelsNeverEscape(t *testing.T) {
	var log bytes.Buffer
	logger := slog.New(NewHandler(slog.NewJSONHandler(&log, nil)))
	logger.Info("connection failed", "diagnostics", map[string]any{"nested": map[string]any{"api_key": []any{sentinel}, "authorization": "Bearer " + sentinel}, "url": "https://h.example/?apikey=" + sentinel})
	for surface, raw := range map[string]string{
		"outbound_url":       "https://h.example/?apikey=" + sentinel,
		"url_error":          (&url.Error{Op: "Get", URL: "https://h.example/?apikey=" + sentinel, Err: errors.New("connection failed")}).Error(),
		"test_connection":    "connection failed: https://h.example/?api_key=" + sentinel,
		"stored_link":        "https://h.example/feed?token=" + sentinel,
		"userinfo":           "https://" + sentinel + "@h.example/feed",
		"reason_params":      `{"params":{"api-key":"` + sentinel + `"}}`,
		"success_serializer": `{"url":"https://h.example/?apikey=` + sentinel + `"}`,
		"error_serializer":   `{"password":"` + sentinel + `"}`,
		"fixture":            "X-Api-Key: " + sentinel,
		"bare_key":           "bxk_" + strings.Repeat("A", 43),
	} {
		clean := String(raw)
		if strings.Contains(clean, sentinel) || strings.Contains(clean, "bxk_") || !strings.Contains(clean, Marker) {
			t.Errorf("%s escaped", surface)
		}
	}
	if strings.Contains(log.String(), sentinel) || !strings.Contains(log.String(), Marker) {
		t.Fatal("nested slog value escaped")
	}
	t.Log("REDACTION surfaces=11 sentinels=0")
}

func TestStringMasksCredentials(t *testing.T) {
	for _, in := range []string{
		"http://indexer.example/api?t=search&apikey=" + sentinel + "&q=x",
		"http://indexer.example/api?API_KEY=" + sentinel,
		"GET /feed?token=" + sentinel + " failed",
		"redirect to https://h.example/cb?access_token=" + sentinel + "#frag",
		"setup_token=" + sentinel,
		"password%3D" + sentinel,
		"https://h.example/?%74oken=" + sentinel,
		"https://h.example/?api%5fkey=" + sentinel,
		`{"username":"admin","password":"` + sentinel + `"}`,
		`{"Token": "` + sentinel + `\"tail"}`,
		"Authorization: Bearer " + sentinel,
		"x-api-key: " + sentinel,
		"https://user:" + sentinel + "@nzb.example/get",
	} {
		got := String(in)
		if strings.Contains(got, sentinel) || !strings.Contains(got, Marker) {
			t.Errorf("String(%q) = %q: the sentinel escaped or nothing was masked", in, got)
		}
	}
	for _, in := range []string{"/api/v1/system/status", "username=admin", "plain text with no secret", "tokens are counted"} {
		if got := String(in); got != in {
			t.Errorf("String(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestURLErrorRedacted(t *testing.T) {
	for _, punctuation := range []string{",", ";", "'", `"`, " ", "\t", "\n", `\"`} {
		raw := "https://indexer.example/api?apikey=prefix" + punctuation + sentinel + "&q=keep"
		u := &url.URL{Scheme: "https", Host: "indexer.example", Path: "/api", RawQuery: "apikey=prefix" + punctuation + sentinel + "&q=keep"}
		ue := &url.Error{Op: "Get", URL: raw, Err: errors.New("synthetic timeout")}
		var log bytes.Buffer
		slog.New(NewHandler(slog.NewJSONHandler(&log, nil))).Info("request failed", "url", raw, "error", ue)
		for surface, got := range map[string]string{"string": String(raw), "url": URL(u), "error": String(ue.Error()), "slog": log.String()} {
			if strings.Contains(got, sentinel) || !strings.Contains(got, Marker) || !strings.Contains(got, "q=keep") {
				t.Errorf("%s punctuation %q: credential suffix escaped or nonsecret query lost", surface, punctuation)
			}
		}
		plain := "https://h.example/?q=prefix" + punctuation + "keep"
		if String(plain) != plain {
			t.Errorf("nonsecret punctuation %q was rewritten", punctuation)
		}
	}
	u, err := url.Parse("https://indexer.example/api?apikey=" + sentinel)
	if err != nil {
		t.Fatal(err)
	}
	ue := &url.Error{Op: "Get", URL: u.String(), Err: errors.New("dial tcp: connection refused")}
	if got := String(ue.Error()); strings.Contains(got, sentinel) {
		t.Errorf("url.Error text kept the sentinel: %q", got)
	}
	if got := URL(u); strings.Contains(got, sentinel) || !strings.Contains(got, "indexer.example") {
		t.Errorf("URL = %q", got)
	}
}

func TestQueryHasSecret(t *testing.T) {
	for _, q := range []string{"apikey=x", "a=1&api_key=x", "TOKEN=x", "access_token", "Password=", "%74oken=x", "q=1&password=x&z=2"} {
		if !QueryHasSecret(q) {
			t.Errorf("QueryHasSecret(%q) = false, want true", q)
		}
	}
	for _, q := range []string{"", "q=token", "tokens=1", "limit=50&sort=username", "passwords=1"} {
		if QueryHasSecret(q) {
			t.Errorf("QueryHasSecret(%q) = true, want false", q)
		}
	}
}

func TestMixedPairsRedacted(t *testing.T) {
	for name, raw := range map[string]string{
		"form prefix":       "username=admin password=" + sentinel + "&q=keep",
		"query prefix":      "https://h.example/?username=admin&password=" + sentinel + "&q=keep",
		"adjacent URLs":     "https://a.example/?q=keep https://b.example/?password=" + sentinel + "&mode=read",
		"secret then URL":   "https://a.example/?token=" + sentinel + "&q=keep https://b.example/?password=" + sentinel + "&mode=read",
		"quoted URLs":       `first="https://a.example/?q=keep" next="https://b.example/?password=prefix "` + sentinel + `&mode=read"`,
		"mixed case":        "username=admin PaSsWoRd=" + sentinel + "&q=keep",
		"encoded name":      "username=admin %74oken=" + sentinel + "&q=keep",
		"encoded separator": "username=admin api%5fkey%3D" + sentinel + "&q=keep",
		"embedded pair":     "token=password=" + sentinel + "&q=keep",
		"encoded padding":   "token=api%5fkey%3D" + sentinel + "==&q=keep",
		"malformed name":    "username=admin %zz=" + sentinel + "&q=keep",
		"multiple secrets":  "username=admin token=" + sentinel + "&password=other-" + sentinel + "&q=keep",
		"fragment boundary": "https://a.example/?q=keep#public https://b.example/?token=" + sentinel + "&mode=read",
		"quoted whitespace": "username=admin password=prefix \"\t\n" + sentinel + "&q=keep",
		"many prefixes":     strings.Repeat("username=admin ", 1024) + "password=" + sentinel + "&q=keep",
	} {
		t.Run(name, func(t *testing.T) {
			u := &url.URL{Scheme: "https", Host: "raw.example", RawQuery: raw}
			ue := &url.Error{Op: "Get", URL: raw, Err: errors.New("synthetic timeout")}
			var log bytes.Buffer
			slog.New(NewHandler(slog.NewJSONHandler(&log, nil))).With("request", raw).Info(raw, "url", u, "error", ue)
			for surface, got := range map[string]string{"string": String(raw), "url": URL(u), "error": String(ue.Error()), "slog": log.String()} {
				if strings.Contains(got, sentinel) || !strings.Contains(got, Marker) || !strings.Contains(got, "q=keep") {
					t.Errorf("%s: secret escaped or nonsecret query lost", surface)
				}
			}
			if name == "multiple secrets" && strings.Count(String(raw), Marker) != 2 {
				t.Error("both ampersand-separated credentials must be masked")
			}
			if name == "embedded pair" || name == "encoded padding" {
				if got := String(raw); got != "token="+Marker+"&q=keep" {
					t.Error("the entire embedded assignment and padding must be masked")
				}
			}
		})
	}
	for _, raw := range []string{"username=admin q=keep", "https://a.example/?q=keep https://b.example/?mode=read"} {
		if got := String(raw); got != raw {
			t.Errorf("nonsecret mixed text changed: %q", got)
		}
	}
}

func TestHandlerRedactsRecords(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&buf, nil)))
	log = log.With("token", sentinel, "peer", "127.0.0.1")
	log.Info("fetch https://h.example/?apikey="+sentinel,
		"password", sentinel,
		"setup_token", sentinel,
		"url", "https://h.example/x?token="+sentinel,
		"err", &url.Error{Op: "Get", URL: "https://h.example/?api_key=" + sentinel, Err: errors.New("timeout")},
		slog.Group("req", "authorization", "Bearer "+sentinel, "path", "/api/v1/setup"),
		"code", "setup_created",
		"n", 3,
	)
	out := buf.String()
	if strings.Contains(out, sentinel) {
		t.Fatalf("a sentinel reached the log: %s", out)
	}
	for _, want := range []string{`"code":"setup_created"`, `"n":3`, `"peer":"127.0.0.1"`, `"path":"/api/v1/setup"`, Marker} {
		if !strings.Contains(out, want) {
			t.Errorf("log %s lacks %s", out, want)
		}
	}
	if n := strings.Count(out, Marker); n < 7 {
		t.Errorf("only %d masked values in %s", n, out)
	}
}
