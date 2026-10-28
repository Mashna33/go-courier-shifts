package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder запоминает код ответа для логирования.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverer не даёт панике в обработчике уронить весь процесс.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "panic", p, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal server error", Code: "internal"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
