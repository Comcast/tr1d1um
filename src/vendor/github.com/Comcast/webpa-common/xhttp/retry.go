package xhttp

import (
	"net/http"

	"github.com/Comcast/webpa-common/logging"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/go-kit/kit/metrics"
)

// temporaryError is the expected interface for a (possibly) temporary error.
// Several of the error types in the net package implicitely implement this interface,
// for example net.DNSError.
type temporaryError interface {
	Temporary() bool
}

// ShouldRetryFunc is a predicate for determining if the error returned from an HTTP transaction
// should be retried.
type ShouldRetryFunc func(error) bool

// DefaultShouldRetry is the default retry predicate.  It returns true if and only if err exposes a Temporary() bool
// method and that method returns true.  That means, for example, that for a net.DNSError with the temporary flag set to true
// this predicate also returns true.
func DefaultShouldRetry(err error) bool {
	if temp, ok := err.(temporaryError); ok {
		return temp.Temporary()
	}

	return false
}

// RetryOptions are the configuration options for a retry transactor
type RetryOptions struct {
	// Logger is the go-kit logger to use.  Defaults to logging.DefaultLogger() if unset.
	Logger log.Logger

	// Retries is the count of retries.  If not positive, then no transactor decoration is performed.
	Retries int

	// ShouldRetry is the retry predicate.  Defaults to DefaultShouldRetry if unset.
	ShouldRetry ShouldRetryFunc

	// Counter is the counter for total retries.  If unset, no metrics are collected on retries.
	Counter metrics.Counter
}

// RetryTransactor returns an HTTP transactor function, of the same signature as http.Client.Do, that
// retries a certain number of times.  Note that net/http.RoundTripper.RoundTrip also is of this signature,
// so this decorator can be used with a RoundTripper or an http.Client equally well.
//
// If o.Retries is nonpositive, next is returned undecorated.
func RetryTransactor(o RetryOptions, next func(*http.Request) (*http.Response, error)) func(*http.Request) (*http.Response, error) {
	if o.Retries < 1 {
		return next
	}

	if o.Logger == nil {
		o.Logger = logging.DefaultLogger()
	}

	if o.ShouldRetry == nil {
		o.ShouldRetry = DefaultShouldRetry
	}

	attempts := o.Retries + 1
	return func(request *http.Request) (*http.Response, error) {
		var (
			response *http.Response
			err      error
		)

		for i := 0; i < attempts; i++ {
			response, err = next(request)
			if err != nil && o.ShouldRetry(err) {
				o.Logger.Log(level.Key(), level.ErrorValue(), logging.MessageKey(), "retrying HTTP transaction", "url", request.URL.String(), "error", err, "attempt", i+1)

				if o.Counter != nil {
					o.Counter.Add(1.0)
				}

				continue
			}

			break
		}

		if err != nil {
			o.Logger.Log(level.Key(), level.ErrorValue(), logging.MessageKey(), "All HTTP transaction retries failed", "url", request.URL.String(), "error", err, "attempts", attempts)
		}

		return response, err
	}
}
