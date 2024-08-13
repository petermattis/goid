package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type failureMatchers map[string]*regexp.Regexp

func main() {
	matchers, err := expectedFailures()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	tests := os.Args[1:]
	testArguments := [][]string{
		{"-test.v"},
		{"-test.bench=.", "-test.benchmem", "-test.v"},
	}

	var failures []error
	if len(tests) == 0 {
		failures = append(failures, fmt.Errorf("no test binaries specified"))
	}
	for _, test := range tests {
		if expectedFailure, ok := matchers[filepath.Base(test)]; ok {
			if err := run(test, expectedFailure, "-test.v"); err != nil {
				failures = append(failures, err)
			}
			continue
		}

		for _, args := range testArguments {
			if err := run(test, nil, args...); err != nil {
				failures = append(failures, err)
			}
		}
	}

	for _, err := range failures {
		fmt.Fprintln(os.Stderr, err)
	}
	if len(failures) != 0 {
		os.Exit(1)
	}
}

func run(binary string, expectedFailure *regexp.Regexp, args ...string) error {
	command := exec.Command(binary, args...)
	if expectedFailure == nil {
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("%s with arguments %q: %s", binary, args, err)
		}
		return nil
	}

	var stderr bytes.Buffer
	command.Stdout = os.Stdout
	command.Stderr = io.MultiWriter(os.Stderr, &stderr)
	err := command.Run()
	if err == nil {
		return fmt.Errorf("%s unexpectedly succeeded", binary)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		return fmt.Errorf("%s: %s", binary, err)
	}
	if !expectedFailure.MatchString(strings.TrimRight(stderr.String(), "\n")) {
		return fmt.Errorf("%s failed differently than expected", binary)
	}
	return nil
}
