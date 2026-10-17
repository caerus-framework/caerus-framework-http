package problem

import (
	"net/http"

	cf_http "github.com/caerus-framework/caerus-framework-http"
)

// ErrorWriter is a cf_http.ErrorWriter that serializes Failure as RFC 9457
// application/problem+json. Pass it to Recover, CSRF, or MaxBodyBytes:
//
//	cf_http.Recover(getLogger, problem.ErrorWriter)
//	cf_http.MaxBodyBytes(1<<20, problem.ErrorWriter)
//
// Title is Failure.Message (or the status text if Message is empty). The
// machine code is the JSON extension "code". GraphQL and OAuth handlers
// should not use this writer — they keep their native envelopes.
func ErrorWriter(w http.ResponseWriter, r *http.Request, f cf_http.Failure) {
	title := f.Message
	if title == "" {
		title = http.StatusText(f.Status)
	}
	p := New(f.Status, title)
	if f.Code != "" {
		p = p.WithExtension("code", f.Code)
	}
	if f.RequestID != "" {
		p = p.WithRequestID(f.RequestID)
	}
	_ = Write(w, r, p)
}
