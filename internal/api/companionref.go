package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const companionPathPrefix = "/api/companions/"

// resolveCompanionRef rewrites an "<id>" or "<id>-<slug>" companion segment to that companion's
// current name, so a link survives a rename. Every /api/companions/{name} handler resolves the name
// itself, so doing this once here covers all of them.
//
// A segment that is not an id reference is left alone, and so is one naming an id that no longer
// exists: name-keyed URLs predate this and must keep working for bookmarks and installed PWAs.
func (s *Server) resolveCompanionRef(r *http.Request) *http.Request {
	// Split and rebuild on the escaped path throughout. A "%2F" is data inside a segment, and
	// treating the decoded path as the source would let one pass for a separator and move the
	// request onto a route its escaped path never addressed.
	escRest, found := strings.CutPrefix(r.URL.EscapedPath(), companionPathPrefix)
	if !found {
		return r
	}
	escSeg, escTail, hasTail := strings.Cut(escRest, "/")
	seg, err := url.PathUnescape(escSeg)
	if err != nil {
		return r
	}
	id, isRef := parseCompanionRef(seg)
	if !isRef {
		return r
	}
	// A companion really named "7" keeps its own URL even when another companion has id 7.
	if _, err := s.store.Companions.IDByName(r.Context(), seg); err == nil {
		return r
	}
	c, err := s.store.Companions.Get(r.Context(), id)
	if err != nil || c == nil {
		return r
	}

	out := r.Clone(r.Context())
	// Nothing constrains a companion name, so PathEscape is what keeps one containing "/" inside
	// its own segment rather than steering the request somewhere else.
	out.URL.Path = companionPathPrefix + c.Name
	out.URL.RawPath = companionPathPrefix + url.PathEscape(c.Name)
	if hasTail {
		tail, err := url.PathUnescape(escTail)
		if err != nil {
			return r
		}
		out.URL.Path += "/" + tail
		out.URL.RawPath += "/" + escTail
	}
	return out
}

// parseCompanionRef reads the id out of "12" or "12-akl". The slug is decoration and is not checked
// against the current name: a stale slug in an old link must still resolve.
func parseCompanionRef(seg string) (int64, bool) {
	digits, _, _ := strings.Cut(seg, "-")
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
