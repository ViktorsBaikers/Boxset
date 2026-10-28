package server

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

const compressionThreshold = 1024

// compress buffers only the threshold prefix, so a large response streams
// without retaining a second copy. Flush commits identity for streaming/SSE.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &compressionWriter{ResponseWriter: w, acceptsGzip: acceptsGzip(r.Header.Get("Accept-Encoding")), status: http.StatusOK}
		next.ServeHTTP(cw, r)
		cw.commit(false)
		if cw.gzip != nil {
			_ = cw.gzip.Close()
		}
	})
}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		coding := strings.TrimSpace(fields[0])
		quality := 1.0
		for _, param := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(key, "q") {
				n, err := strconv.ParseFloat(value, 64)
				if err != nil || n < 0 || n > 1 {
					quality = 0
				} else {
					quality = n
				}
			}
		}
		if strings.EqualFold(coding, "gzip") {
			return quality > 0
		}
		if coding == "*" {
			wildcard = quality > 0
		}
	}
	return wildcard
}

type compressionWriter struct {
	http.ResponseWriter
	acceptsGzip            bool
	status                 int
	wroteHeader, committed bool
	prefix                 []byte
	gzip                   *gzip.Writer
}

func (w *compressionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *compressionWriter) WriteHeader(status int) {
	if w.wroteHeader || w.committed {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status, w.wroteHeader = status, true
}

func (w *compressionWriter) Write(p []byte) (int, error) {
	if w.committed {
		if w.gzip != nil {
			return w.gzip.Write(p)
		}
		return w.ResponseWriter.Write(p)
	}
	w.wroteHeader = true
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", http.DetectContentType(p))
	}
	if !w.compressible() {
		w.commit(false)
		return w.ResponseWriter.Write(p)
	}
	if len(w.prefix)+len(p) <= compressionThreshold {
		w.prefix = append(w.prefix, p...)
		return len(p), nil
	}
	w.commit(w.acceptsGzip)
	if w.gzip != nil {
		return w.gzip.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *compressionWriter) compressible() bool {
	contentType := strings.Split(w.Header().Get("Content-Type"), ";")[0]
	return w.status != http.StatusNoContent && w.status != http.StatusNotModified && w.Header().Get("Content-Encoding") == "" && (contentType == "application/json" || contentType == "application/problem+json")
}

func (w *compressionWriter) commit(compressed bool) {
	if w.committed {
		return
	}
	w.committed = true
	if w.compressible() || w.status == http.StatusNotModified {
		found := false
		for _, line := range w.Header().Values("Vary") {
			for _, value := range strings.Split(line, ",") {
				if strings.EqualFold(strings.TrimSpace(value), "Accept-Encoding") {
					found = true
				}
			}
		}
		if !found {
			w.Header().Add("Vary", "Accept-Encoding")
		}
	}
	if compressed {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.gzip, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.prefix) > 0 {
		if w.gzip != nil {
			_, _ = w.gzip.Write(w.prefix)
		} else {
			_, _ = w.ResponseWriter.Write(w.prefix)
		}
		w.prefix = nil
	}
}

func (w *compressionWriter) Flush() {
	w.commit(false)
	if w.gzip != nil {
		_ = w.gzip.Flush()
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
