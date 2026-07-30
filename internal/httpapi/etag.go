package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// The optimistic-lock token is contents.current_version, carried over HTTP as
// an entity tag. A strong tag is correct here: the representation is exactly
// determined by the version number, so two responses with the same tag are
// byte-identical.

var (
	errNoIfMatch   = errors.New("missing If-Match header")
	errBadIfMatch  = errors.New("malformed If-Match header")
	errWeakIfMatch = errors.New("If-Match must be a strong validator")
)

// formatETag renders a version number as a strong entity tag.
func formatETag(version int64) string {
	return `"` + strconv.FormatInt(version, 10) + `"`
}

// setETag attaches the entity tag for the given version.
func setETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", formatETag(version))
}

// parseIfMatch extracts the version number from an If-Match header.
//
// Returns (0, true, nil) for "*", which RFC 9110 defines as matching any
// current representation -- an unconditional write against existing content.
func parseIfMatch(header string) (version int64, wildcard bool, err error) {
	raw := strings.TrimSpace(header)
	if raw == "" {
		return 0, false, errNoIfMatch
	}
	if raw == "*" {
		return 0, true, nil
	}

	// If-Match permits a comma-separated list. Accepting exactly one entry
	// keeps the semantics unambiguous: a list of candidate versions has no
	// sensible meaning for a compare-and-swap.
	if strings.Contains(raw, ",") {
		return 0, false, errBadIfMatch
	}

	// A weak validator explicitly means "semantically equivalent, possibly not
	// byte-identical", which is not a safe basis for a lost-update check.
	if strings.HasPrefix(raw, "W/") {
		return 0, false, errWeakIfMatch
	}

	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, false, errBadIfMatch
	}
	inner := raw[1 : len(raw)-1]

	v, convErr := strconv.ParseInt(inner, 10, 64)
	if convErr != nil || v < 1 {
		return 0, false, errBadIfMatch
	}
	return v, false, nil
}

// ifNoneMatchMatches reports whether an If-None-Match header matches the
// current entity tag, meaning the client's cached copy is still fresh and the
// response should be 304.
//
// Unlike If-Match this does accept a list, because a client legitimately caches
// several representations. Weak comparison is used, as RFC 9110 requires for
// If-None-Match.
func ifNoneMatchMatches(header, currentETag string) bool {
	raw := strings.TrimSpace(header)
	if raw == "" {
		return false
	}
	if raw == "*" {
		return true
	}
	for _, candidate := range strings.Split(raw, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == currentETag {
			return true
		}
	}
	return false
}
