package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
)

// Only locally identified network failures opt into the short credential
// cooldown. HTTP/Connect status errors continue to use the host's own policy.
type transportRetryError struct {
	err error
}

func (e *transportRetryError) Error() string { return e.err.Error() }
func (e *transportRetryError) Unwrap() error { return e.err }

func transportFailure(status int, message string) error {
	return &transportRetryError{err: &statusError{status: status, message: message}}
}

func retryAfterSeconds(err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0
	}
	if status := errorStatus(err); status != 502 && status != 504 {
		return 0
	}
	var retry *transportRetryError
	if errors.As(err, &retry) {
		return 2
	}
	return 0
}

func preserveRetryHint(err error, replacement error) error {
	if replacement == nil || retryAfterSeconds(replacement) > 0 || errors.Is(replacement, context.Canceled) || errors.Is(replacement, context.DeadlineExceeded) {
		return replacement
	}
	if retryAfterSeconds(err) > 0 {
		return &transportRetryError{err: replacement}
	}
	return replacement
}

// Tag errors returned by the network body reader before Connect/gzip parsing.
// This keeps malformed in-memory compressed frames out of the network policy.
type nativeReadError struct{ error }

func (e nativeReadError) Unwrap() error { return e.error }

type nativeNetworkReader struct{ io.Reader }

func (r nativeNetworkReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		err = nativeReadError{err}
	}
	return n, err
}

func nativeNetworkFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var read nativeReadError
	var operation *net.OpError
	var request *url.Error
	var timeout net.Error
	if errors.As(err, &request) {
		// A URL wrapper is not itself evidence of a transient network fault;
		// certificate verification and request validation failures use it too.
		return nativeNetworkFailure(request.Err)
	}
	return errors.As(err, &read) || errors.As(err, &operation) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		(errors.As(err, &timeout) && timeout.Timeout())
}
