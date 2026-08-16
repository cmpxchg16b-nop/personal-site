// Package log provides HTTP middleware for structured access logging.
package log

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"personal-site/pkg/session"
	"personal-site/pkg/utils"
)

// responseWriter wraps an http.ResponseWriter to capture the response status
// code and the number of bytes written, so they can be logged once the request
// completes.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// Unwrap exposes the underlying ResponseWriter so that http.ResponseController
// (used by modern handlers and by net/http internally) can still reach the
// underlying Flusher, Hijacker, etc. through the wrapper.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush delegates to the underlying Flusher when present, preserving streaming
// support for handlers that type-assert directly to http.Flusher.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteHeader records the first status code reported and forwards it. Repeated
// calls are forwarded but only the first status is recorded, matching net/http
// semantics where only the initial WriteHeader takes effect.
func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

// Write records the implicit 200 status on the first write and accumulates the
// number of bytes written.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.status = http.StatusOK
		rw.wroteHeader = true
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytes += n
	return n, err
}

// accessLog emits the structured access record shared by WithHTTPLog and
// WithSessionAwaredLog. The log level reflects the status: Error for 5xx, Warn
// for 4xx, Info otherwise. Any extra attributes supplied by the caller (e.g.
// session fields) are appended to the standard set.
func accessLog(logger *slog.Logger, r *http.Request, rw *responseWriter, extra ...slog.Attr) {
	level := slog.LevelInfo
	switch {
	case rw.status >= 500:
		level = slog.LevelError
	case rw.status >= 400:
		level = slog.LevelWarn
	}

	attrs := make([]slog.Attr, 0, 4+len(extra))
	attrs = append(attrs,
		slog.String("method", r.Method),
		slog.String("url", r.URL.RequestURI()),
		slog.String("remote", r.RemoteAddr),
		slog.String("real_ip", utils.GetRemoteAddr(r)),
	)
	attrs = append(attrs, extra...)

	if traceId, ok := r.Context().Value(utils.CtxLogTraceId).(string); ok && traceId != "" {
		attrs = append(attrs, slog.String("trace_id", traceId))
	}

	logger.LogAttrs(r.Context(), level, "http request", attrs...)
}

// WithHTTPLog wraps h with structured access logging via logger. For each
// request it emits a single record containing the method, URL, client address,
// response status, bytes written, and latency. The log level reflects the
// status: Error for 5xx, Warn for 4xx, Info otherwise. A nil logger falls back
// to slog.Default().
func WithHTTPLog(logger *slog.Logger, h http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		accessLog(logger, r, rw)
		h.ServeHTTP(rw, r)
	})
}

// WithOverallLog wraps h so that an access record is emitted after the handler
// completes. Unlike WithHTTPLog (which logs on request entry), this records the
// request's outcome, appending the response status, bytes written, and duration
// as extra attributes. Because it runs after h.ServeHTTP, the status-based log
// level reflects the actual response: Error for 5xx, Warn for 4xx, Info
// otherwise. A nil logger falls back to slog.Default().
func WithOverallLog(logger *slog.Logger, h http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rw, r)
		accessLog(logger, r, rw,
			slog.Int("status", rw.status),
			slog.Int("bytes", rw.bytes),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// WithLogTraceId wraps h so that each request is assigned a random log trace
// id, stored in the request context under utils.CtxLogTraceId. The trace id
// correlates log statements emitted across multiple locations while handling
// the same request. It should be nested outside (before) any logging
// middleware that should record the trace id, e.g.:
//
//	log.WithLogTraceId(log.WithHTTPLog(logger, mux))
func WithLogTraceId(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		cryptoRand.Read(b)
		traceId := hex.EncodeToString(b)
		r = r.WithContext(context.WithValue(r.Context(), utils.CtxLogTraceId, traceId))
		h.ServeHTTP(w, r)
	})
}

// WithSessionAwaredLog behaves like WithHTTPLog and additionally records the
// session id and expiry. It reads the session from the request context via sm,
// so it must be nested inside the middleware that attaches the session (e.g.
// session.WithSessionId):
//
//	session.WithSessionId(httplog.WithSessionAwaredLog(logger, sm, mux), sm)
//
// If no session is present in the context (or sm is nil) the session fields are
// simply omitted. A nil logger falls back to slog.Default().
func WithSessionAwaredLog(logger *slog.Logger, sm session.SessionManager, h http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		var extra []slog.Attr
		if sm != nil {
			if sess, ok := sm.GetSessionFromContext(r.Context()); ok {
				extra = append(extra,
					slog.String("session_id", sess.Id()),
					slog.String("subject_id", sess.SubjectId()),
					slog.Time("session_expiry", sess.Expiry()),
				)
			}
		}
		accessLog(logger, r, rw, extra...)
		h.ServeHTTP(rw, r)
	})
}
