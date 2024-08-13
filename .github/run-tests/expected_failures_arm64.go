package main

import "regexp"

func expectedFailures() (failureMatchers, error) {
	// Race detector binaries crash under QEMU. See
	// https://github.com/golang/go/issues/29948.
	return failureMatchers{
		"goid.race.test": regexp.MustCompile(
			`^FATAL: ThreadSanitizer: unsupported VMA range\nFATAL: Found 47 - Supported 48$`,
		),
	}, nil
}
