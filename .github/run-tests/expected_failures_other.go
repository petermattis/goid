//go:build !arm64 && !s390x
// +build !arm64,!s390x

package main

func expectedFailures() (failureMatchers, error) {
	return nil, nil
}
