package orchestrator

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v92/github"
)

// The sentinels classifyGitHubErr maps every GitHubClient REST failure onto
// (PKG9-PLAN.md section 10.3), so a caller matches the cause with errors.Is
// instead of inspecting go-github's own types.
var (
	ErrGitHubAuth        = errors.New("orchestrator: github refused the token")
	ErrGitHubNotFound    = errors.New("orchestrator: github object not found")
	ErrGitHubUnavailable = errors.New("orchestrator: github unavailable")
	ErrMergeRefused      = errors.New("orchestrator: github refused the merge")
)

// RateLimitedError is classifyGitHubErr's mapping of go-github's
// *RateLimitError, *AbuseRateLimitError, or a bare 429 response (PKG9-PLAN.md
// section 10.3). ResetAt is when the caller may retry; zero when GitHub's
// response carried no reset time.
type RateLimitedError struct{ ResetAt time.Time }

func (e RateLimitedError) Error() string {
	return "orchestrator: github rate limited until " + e.ResetAt.Format(time.RFC3339)
}

// classifyGitHubErr maps err, a failure from a GitHubClient REST call, onto
// the sentinels above, once, so every method wraps a failure the same way
// (PKG9-PLAN.md section 10.3):
//
//	RateLimitedError:     go-github's *RateLimitError or *AbuseRateLimitError, or a bare 429
//	ErrGitHubAuth:        401; 403 without rate-limit headers
//	ErrGitHubNotFound:    404
//	ErrGitHubUnavailable: 5xx, a network error, or a timeout
//
// Any other *github.ErrorResponse (405, 409, 422, and the like) is returned
// unclassified: those are each caller's own to interpret, as Merge does for
// a refused merge. err == nil returns nil.
func classifyGitHubErr(err error) error {
	if err == nil {
		return nil
	}

	if rle, ok := errors.AsType[*github.RateLimitError](err); ok {
		return RateLimitedError{ResetAt: rateLimitResetAt(rle.Response)}
	}
	if arle, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
		return RateLimitedError{ResetAt: rateLimitResetAt(arle.Response)}
	}

	if ere, ok := errors.AsType[*github.ErrorResponse](err); ok && ere.Response != nil {
		switch ere.Response.StatusCode {
		case http.StatusTooManyRequests:
			return RateLimitedError{ResetAt: rateLimitResetAt(ere.Response)}
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %w", ErrGitHubAuth, err)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", ErrGitHubNotFound, err)
		default:
			if ere.Response.StatusCode >= http.StatusInternalServerError {
				return fmt.Errorf("%w: %w", ErrGitHubUnavailable, err)
			}
			return err
		}
	}

	// No typed GitHub error response reached us: a network failure, a
	// timeout, or anything else go-github did not wrap, none of which
	// carries a status code to classify by.
	return fmt.Errorf("%w: %w", ErrGitHubUnavailable, err)
}

// rateLimitResetAt reads when a rate-limited caller may retry from resp's
// own headers: X-RateLimit-Reset (the primary rate limit, a Unix timestamp)
// first, then Retry-After (seconds; the secondary/abuse limit and a bare
// 429 carry this instead). Zero when resp is nil or carries neither.
func rateLimitResetAt(resp *http.Response) time.Time {
	if resp == nil {
		return time.Time{}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0)
		}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil {
			return time.Now().Add(time.Duration(sec) * time.Second)
		}
	}
	return time.Time{}
}
