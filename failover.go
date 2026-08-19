package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
)

func isCatastrophic(err error, status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	if err == nil {
		return false
	}

	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}

	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	return true
}
