package log_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkglog "personal-site/pkg/log"
	"personal-site/pkg/session"
	pkgutils "personal-site/pkg/utils"
)

// newRecordLogger returns a logger that writes JSON records to the returned
// buffer, so tests can decode and assert on the emitted access records.
func newRecordLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// decodeRecord decodes the single JSON log record in buf into a map.
func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode log record %q: %v", buf.String(), err)
	}
	return rec
}

// assertStandardAttrs checks the attrs every access record carries, whatever
// middleware emitted it.
func assertStandardAttrs(t *testing.T, rec map[string]any, wantLevel, wantMethod, wantURL string) {
	t.Helper()
	if got := rec["level"]; got != wantLevel {
		t.Errorf("level = %v, want %s", got, wantLevel)
	}
	if got := rec["msg"]; got != "http request" {
		t.Errorf("msg = %v, want %q", got, "http request")
	}
	if got := rec["method"]; got != wantMethod {
		t.Errorf("method = %v, want %s", got, wantMethod)
	}
	if got := rec["url"]; got != wantURL {
		t.Errorf("url = %v, want %s", got, wantURL)
	}
	if rec["remote"] == "" {
		t.Errorf("remote is missing or empty in record %v", rec)
	}
	if rec["real_ip"] == "" {
		t.Errorf("real_ip is missing or empty in record %v", rec)
	}
}

// assertKeysAbsent checks that none of the keys appear in the record.
func assertKeysAbsent(t *testing.T, rec map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := rec[k]; ok {
			t.Errorf("key %q unexpectedly present in record %v", k, rec)
		}
	}
}

func TestWithHTTPLog(t *testing.T) {
	t.Run("logs a single record on entry, before the handler runs", func(t *testing.T) {
		logger, buf := newRecordLogger()

		var loggedBeforeHandler bool
		h := pkglog.WithHTTPLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// WithHTTPLog logs on request entry: the record must already be
			// in the buffer while the handler is still running.
			loggedBeforeHandler = buf.Len() > 0
			w.WriteHeader(http.StatusTeapot)
		}))

		r := httptest.NewRequest(http.MethodGet, "/hello?q=1", nil)
		h.ServeHTTP(httptest.NewRecorder(), r)

		if !loggedBeforeHandler {
			t.Error("record was not written before the handler ran; want logging on request entry")
		}
		rec := decodeRecord(t, buf)
		assertStandardAttrs(t, rec, "INFO", http.MethodGet, "/hello?q=1")
		// Entry logging cannot know the outcome: no status/bytes/duration keys.
		assertKeysAbsent(t, rec, "status", "bytes", "duration")
	})

	t.Run("level stays INFO even when the handler later writes a 500", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithHTTPLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

		// The record is emitted on entry, when the status is still the initial
		// 200, so the 500 cannot affect the level.
		rec := decodeRecord(t, buf)
		assertStandardAttrs(t, rec, "INFO", http.MethodGet, "/boom")
	})

	t.Run("real_ip prefers the X-Forwarded-For header over RemoteAddr", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithHTTPLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		h.ServeHTTP(httptest.NewRecorder(), r)

		rec := decodeRecord(t, buf)
		if got := rec["real_ip"]; got != "203.0.113.9" {
			t.Errorf("real_ip = %v, want 203.0.113.9", got)
		}
		if got := rec["remote"]; got == "203.0.113.9" {
			t.Errorf("remote = %v, want the raw RemoteAddr, not the forwarded header", got)
		}
	})

	t.Run("no trace_id without the trace middleware", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithHTTPLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

		assertKeysAbsent(t, decodeRecord(t, buf), "trace_id")
	})
}

func TestWithOverallLog(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantLevel  string
		wantStatus int
		wantBytes  int
	}{
		{
			name: "explicit 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			wantLevel:  "INFO",
			wantStatus: http.StatusOK,
		},
		{
			name: "implicit 200 on first Write and bytes counted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hello"))
			},
			wantLevel:  "INFO",
			wantStatus: http.StatusOK,
			wantBytes:  5,
		},
		{
			name: "first WriteHeader wins over a later one",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantLevel:  "INFO",
			wantStatus: http.StatusCreated,
		},
		{
			name: "4xx logs at Warn",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantLevel:  "WARN",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "5xx logs at Error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantLevel:  "ERROR",
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newRecordLogger()
			h := pkglog.WithOverallLog(logger, tc.handler)

			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/submit", nil))

			rec := decodeRecord(t, buf)
			assertStandardAttrs(t, rec, tc.wantLevel, http.MethodPost, "/submit")
			if got := rec["status"]; got != float64(tc.wantStatus) {
				t.Errorf("status = %v, want %d", got, tc.wantStatus)
			}
			if got := rec["bytes"]; got != float64(tc.wantBytes) {
				t.Errorf("bytes = %v, want %d", got, tc.wantBytes)
			}
			if _, ok := rec["duration"]; !ok {
				t.Errorf("duration is missing in record %v", rec)
			}
		})
	}

	t.Run("referer is recorded when the header is present", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithOverallLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		r := httptest.NewRequest(http.MethodGet, "/landing", nil)
		r.Header.Set("Referer", "https://example.com/source")
		h.ServeHTTP(httptest.NewRecorder(), r)

		rec := decodeRecord(t, buf)
		if got := rec["referer"]; got != "https://example.com/source" {
			t.Errorf("referer = %v, want https://example.com/source", got)
		}
	})

	t.Run("referer is an empty string when the header is absent", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithOverallLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/direct", nil))

		rec := decodeRecord(t, buf)
		if got := rec["referer"]; got != "" {
			t.Errorf("referer = %v, want the empty string (key always present)", got)
		}
	})

	t.Run("record is written only after the handler completes", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithOverallLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if buf.Len() != 0 {
				t.Error("record written before the handler finished; want logging on completion")
			}
		}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if buf.Len() == 0 {
			t.Error("no record written after the handler completed")
		}
	})

	t.Run("handlers can still Flush through the wrapped writer", func(t *testing.T) {
		logger, _ := newRecordLogger()
		h := pkglog.WithOverallLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f, ok := w.(http.Flusher)
			if !ok {
				t.Error("wrapped writer does not implement http.Flusher")
				return
			}
			f.Flush()
		}))

		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if !rr.Flushed {
			t.Error("Flush was not delegated to the underlying ResponseWriter")
		}
	})
}

func TestWithLogTraceId(t *testing.T) {
	t.Run("assigns a random 32-char hex trace id to the request context", func(t *testing.T) {
		var seen []string
		h := pkglog.WithLogTraceId(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := r.Context().Value(pkgutils.CtxLogTraceId).(string)
			if !ok || id == "" {
				t.Error("no trace id in the request context")
				return
			}
			if len(id) != 32 {
				t.Errorf("trace id %q has %d chars, want 32 (16 random bytes hex-encoded)", id, len(id))
			}
			if _, err := hex.DecodeString(id); err != nil {
				t.Errorf("trace id %q is not valid hex: %v", id, err)
			}
			seen = append(seen, id)
		}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

		if len(seen) == 2 && seen[0] == seen[1] {
			t.Errorf("two requests got the same trace id %q; ids should be random", seen[0])
		}
	})

	t.Run("nested outside a logging middleware, the trace id lands in the record", func(t *testing.T) {
		logger, buf := newRecordLogger()

		var handlerTraceId string
		h := pkglog.WithLogTraceId(pkglog.WithOverallLog(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerTraceId, _ = r.Context().Value(pkgutils.CtxLogTraceId).(string)
		})))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

		rec := decodeRecord(t, buf)
		if handlerTraceId == "" {
			t.Fatal("handler saw no trace id")
		}
		if got := rec["trace_id"]; got != handlerTraceId {
			t.Errorf("trace_id = %v, want the id the handler saw (%q)", got, handlerTraceId)
		}
	})
}

func TestWithSessionAwaredLog(t *testing.T) {
	// serve runs one request through the production-shaped chain: the subject
	// id and friends are seeded in the context as the JWT middleware would,
	// then session.WithSessionId builds the Session and the logging middleware
	// reads it back out. When withSession is false the bare logging middleware
	// is used, so GetSessionFromContext misses.
	serve := func(t *testing.T, logger *slog.Logger, sm *session.OnMemorySessionManager, withSession bool) {
		t.Helper()
		logging := pkglog.WithSessionAwaredLog(logger, sm, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		r := httptest.NewRequest(http.MethodGet, "/me", nil)
		h := http.Handler(logging)
		if withSession {
			ctx := r.Context()
			ctx = context.WithValue(ctx, pkgutils.CtxKeySessionId, "sess-1")
			ctx = context.WithValue(ctx, pkgutils.CtxKeySubjectId, "subject-1")
			ctx = context.WithValue(ctx, pkgutils.CtxKeySessionTTLSecs, int64(1700000000))
			r = r.WithContext(ctx)
			h = session.WithSessionId(logging, sm)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	t.Run("records the session fields when a session is present", func(t *testing.T) {
		logger, buf := newRecordLogger()
		sm := session.NewOnMemorySessionManager()

		serve(t, logger, sm, true)

		rec := decodeRecord(t, buf)
		assertStandardAttrs(t, rec, "INFO", http.MethodGet, "/me")
		if got := rec["session_id"]; got != "sess-1" {
			t.Errorf("session_id = %v, want sess-1", got)
		}
		if got := rec["subject_id"]; got != "subject-1" {
			t.Errorf("subject_id = %v, want subject-1", got)
		}
		if got := rec["session_expiry"]; got == nil || got == "" {
			t.Errorf("session_expiry is missing or empty in record %v", rec)
		}
	})

	t.Run("omits the session fields when no session is attached", func(t *testing.T) {
		logger, buf := newRecordLogger()
		sm := session.NewOnMemorySessionManager()

		serve(t, logger, sm, false)

		rec := decodeRecord(t, buf)
		assertStandardAttrs(t, rec, "INFO", http.MethodGet, "/me")
		assertKeysAbsent(t, rec, "session_id", "subject_id", "session_expiry")
	})

	t.Run("a nil session manager simply omits the session fields", func(t *testing.T) {
		logger, buf := newRecordLogger()
		h := pkglog.WithSessionAwaredLog(logger, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/me", nil))

		rec := decodeRecord(t, buf)
		assertStandardAttrs(t, rec, "INFO", http.MethodGet, "/me")
		assertKeysAbsent(t, rec, "session_id", "subject_id", "session_expiry")
	})
}

// TestNilLoggerFallsBackToDefault swaps the process default logger and checks
// that a nil logger argument selects it rather than panicking.
func TestNilLoggerFallsBackToDefault(t *testing.T) {
	logger, buf := newRecordLogger()
	original := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(original) })

	h := pkglog.WithHTTPLog(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/default", nil))

	rec := decodeRecord(t, buf)
	if got := rec["url"]; got != "/default" {
		t.Errorf("url = %v, want /default; record %v", got, rec)
	}
	if !strings.Contains(buf.String(), "http request") {
		t.Errorf("no access record written to the default logger, got %q", buf.String())
	}
}

// hijackableResponseWriter is a ResponseRecorder that also implements
// http.Hijacker, standing in for the server's real connection-backed
// writer.
type hijackableResponseWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

// TestHijackDelegatesThroughWrapper checks that the logging wrapper passes
// http.Hijacker through to the underlying writer: websocket upgrades (e.g.
// github.com/gorilla/websocket, which type-asserts Hijacker directly) must
// survive the middleware chain.
func TestHijackDelegatesThroughWrapper(t *testing.T) {
	h := pkglog.WithHTTPLog(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("wrapped writer does not implement http.Hijacker")
			return
		}
		if _, _, err := hj.Hijack(); err != nil {
			t.Errorf("Hijack: %v", err)
		}
	}))
	inner := &hijackableResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(inner, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if !inner.hijacked {
		t.Error("Hijack did not reach the underlying writer")
	}
}
