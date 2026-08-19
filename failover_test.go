package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsCatastrophic(t *testing.T) {
	require.False(t, isCatastrophic(nil, http.StatusOK))
	require.False(t, isCatastrophic(nil, http.StatusBadRequest))
	require.False(t, isCatastrophic(nil, http.StatusTooManyRequests))
	require.False(t, isCatastrophic(nil, http.StatusInternalServerError))
	require.True(t, isCatastrophic(nil, http.StatusBadGateway))
	require.True(t, isCatastrophic(nil, http.StatusServiceUnavailable))
	require.True(t, isCatastrophic(nil, http.StatusGatewayTimeout))

	require.False(t, isCatastrophic(context.Canceled, 0))
	require.False(t, isCatastrophic(&url.Error{Err: context.Canceled}, 0))
	require.False(t, isCatastrophic(context.DeadlineExceeded, 0))

	dialErr := &url.Error{Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}
	require.True(t, isCatastrophic(dialErr, 0))

	dnsErr := &url.Error{Err: &net.DNSError{Err: "no such host", IsNotFound: true}}
	require.True(t, isCatastrophic(dnsErr, 0))

	timeoutErr := &url.Error{Err: &timeoutError{}}
	require.False(t, isCatastrophic(timeoutErr, 0))

	require.True(t, isCatastrophic(io.ErrUnexpectedEOF, 0))
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsCatastrophicTimeoutError(t *testing.T) {
	require.True(t, errors.As(&timeoutError{}, new(net.Error)))
}
