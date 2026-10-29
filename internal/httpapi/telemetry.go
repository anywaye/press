package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/anywaye/press/internal/telemetry"
)

var requestSpanName = mustRequestSpanName()

func mustRequestSpanName() string {
	return telemetry.MustSpanName("request")
}

// telemetryMiddleware opens the server span for a request and records how it ended.
//
// # WHY NOT `otelhttp.NewHandler`
//
// It is the obvious choice and it does the propagation and the span for you. It
// also records `url.path`, `url.full`, `url.query`, `user_agent.original`,
// `server.address` and the request headers as span attributes.
//
// Every one of those is dropped by kit's collector, so shipping them would not be a
// leak today. It would mean this service EXPORTS a span carrying a caller's
// concrete path and query string and relies on the collector to take them back out,
// and the fleet's rule is the opposite: the collector's allowlist is a backstop and
// the service is the design. One control that needs a misconfiguration elsewhere to
// fail is not two controls.
//
// So this records three attributes, all bounded, all on core's trace allowlist, and
// all through `telemetry.Record` — the one path to a span, and the one that asks the
// GENERATED contract whether each value may be recorded:
//
//	http.request.method         the method
//	http.route                  the matched route TEMPLATE
//	http.response.status_code   the status
//
// plus `error.type` and a failed status on a 5xx, which `recordRequestFailure`
// owns.
//
// # IT IS MOUNTED OUTERMOST, IN BOTH PLACEMENTS, AND THAT IS THE POINT
//
// `everyRequest` builds one chain and `New` mounts it in two places — chi's `Use`
// when there is no pass-through, and `compose` around the pass-through when there
// is. First in that list is therefore first in both, so a forwarded request is
// spanned too: the upstream call gateway makes on somebody's behalf is the most
// expensive thing this process does, and a service that traces its own routes and
// not its own forwarding has traced the cheap half.
func TelemetryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := telemetry.StartServer(r.Context(), r.Header, requestSpanName)

		telemetry.Record(span, map[string]any{"http.request.method": r.Method})

		tracked := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()

		defer func() {
			telemetry.Record(span, map[string]any{
				"http.response.status_code": tracked.status,
				"http.route":                routeTemplate(r),
			})
			recordRequestFailure(span, tracked.status)
			telemetry.RecordRequest(r.Context(), r.Method, routeTemplate(r), tracked.status, time.Since(start))
			span.End()
		}()

		next.ServeHTTP(tracked, r.WithContext(ctx))
	})
}

func routeTemplate(r *http.Request) string {
	routeContext := chi.RouteContext(r.Context())
	if routeContext == nil {
		return ""
	}
	return routeContext.RoutePattern()
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func recordRequestFailure(span trace.Span, status int) {
	if status < 500 {
		return
	}
	_ = telemetry.RecordFailure(span, "internal_error", "the handler returned a server error")
}
