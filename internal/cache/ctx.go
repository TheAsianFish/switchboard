package cache

import (
	"context"
	"net/http"
	"strconv"
	"sync"
)

// Response headers that report what the cache did.
const (
	// StatusHeader carries the lookup's Status.
	StatusHeader = "X-Switchboard-Cache-Status"
	// SimilarityHeader carries a semantic hit's similarity.
	SimilarityHeader = "X-Switchboard-Cache-Similarity"
)

// Report is filled by the pipeline and read by the HTTP layer, which turns
// it into response headers. The front ends never see it.
type Report struct {
	mu         sync.Mutex
	directive  string
	status     Status
	similarity float64
}

type reportKey struct{}

// WithRequest returns a context carrying the client's cache directive and
// an empty report.
func WithRequest(ctx context.Context, directive string) (context.Context, *Report) {
	r := &Report{directive: directive}
	return context.WithValue(ctx, reportKey{}, r), r
}

// ReportFrom returns the context's report, or nil.
func ReportFrom(ctx context.Context) *Report {
	r, _ := ctx.Value(reportKey{}).(*Report)
	return r
}

// Directive is the client's cache header for this request, or "".
func (r *Report) Directive() string {
	if r == nil {
		return ""
	}
	return r.directive
}

// Set records a lookup's result.
func (r *Report) Set(l *Lookup) {
	if r == nil || l == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.similarity = l.Status, l.Similarity
}

// WriteHeaders adds the report's headers to h, if a lookup was recorded.
func (r *Report) WriteHeaders(h http.Header) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == "" {
		return
	}
	h.Set(StatusHeader, string(r.status))
	if r.status == StatusSemantic {
		h.Set(SimilarityHeader, strconv.FormatFloat(r.similarity, 'f', 4, 64))
	}
}
