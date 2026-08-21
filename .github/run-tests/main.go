package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type status string

const (
	failure       status = "failure"
	notApplicable status = "not_applicable"
	success       status = "success"
)

type architecture struct {
	name, goarch, goarm string
	minimum, qemu, race int
	zigTarget           string
}

var architectures = []architecture{
	// Cross-compilation became possible in Go 1.5. Go 1.8 and older
	// binaries cannot run under QEMU user emulation.
	// https://go.dev/doc/go1.5#c
	// https://github.com/golang/go/commit/2673f9e
	{name: "armv6", goarch: "arm", goarm: "6", minimum: 5, qemu: 9},
	{name: "armv7", goarch: "arm", goarm: "7", minimum: 5, qemu: 9},
	// Go 1.12 added linux/arm64 race support.
	// https://go.dev/doc/go1.12
	{name: "aarch64", goarch: "arm64", minimum: 5, qemu: 9, race: 12, zigTarget: "aarch64-linux-gnu"},
	// Go 1.7 added s390x; Go 1.19 added its race detector.
	// https://go.dev/doc/go1.7#ports
	// https://go.dev/doc/go1.19
	{name: "s390x", goarch: "s390x", minimum: 7, qemu: 9, race: 19, zigTarget: "s390x-linux-gnu"},
	{name: "386", goarch: "386", minimum: 5},
	{name: "x64", goarch: "amd64", minimum: 5, race: 5},
}

var inlineGet = regexp.MustCompile("(?m)can inline Get$")

func main() {
	if err := run(); err != nil {
		if _, err := fmt.Fprintln(os.Stderr, err); err != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func run() (err error) {
	output := flag.String("output", "", "result JSON path")
	label := flag.String("version", "", "Go minor version")
	goCommand := flag.String("go", "", "Go command")
	release := flag.String("release", "", "resolved Go release")
	flag.Parse()
	if *output == "" || *label == "" || *goCommand == "" || *release == "" || flag.NArg() != 0 {
		return errors.New("usage: run-tests --output FILE --version 1.N --go FILE --release 1.N.PATCH")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("run-tests requires linux/amd64, got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if !filepath.IsAbs(*goCommand) {
		return fmt.Errorf("Go command is not absolute: %q", *goCommand)
	}
	minor, err := parseVersion(*label)
	if err != nil {
		return err
	}

	results, err := runVersion(*goCommand, *release, minor)
	return errors.Join(err, func() error {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("encode results: %w", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(*output, data, 0o644); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
		return nil
	}())
}

func parseVersion(label string) (int, error) {
	minorText, ok := strings.CutPrefix(label, "1.")
	if !ok {
		return 0, fmt.Errorf("invalid Go version %q", label)
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || minor < 5 || label != fmt.Sprintf("1.%d", minor) {
		return 0, fmt.Errorf("invalid Go version %q", label)
	}
	return minor, nil
}

func runVersion(goCommand, release string, minor int) (results map[string]status, err error) {
	results = make(map[string]status, len(architectures))
	for _, target := range architectures {
		results[target.name] = notApplicable
		if minor >= target.minimum {
			results[target.name] = failure
		}
	}

	goroot, err := func() (string, error) {
		command := exec.Command(goCommand, "env", "GOROOT")
		command.Stderr = os.Stderr
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("%s env GOROOT: %w", goCommand, err)
		}
		value := strings.TrimSpace(string(output))
		if value == "" {
			return "", errors.New("go env GOROOT returned an empty path")
		}
		return value, nil
	}()
	if err != nil {
		return results, err
	}
	if minor >= 12 {
		if err := checkInlining(goCommand, environment(goroot, architecture{goarch: "amd64"})); err != nil {
			return results, err
		}
	}

	work, err := os.MkdirTemp("", "goid-run-tests-")
	if err != nil {
		return results, fmt.Errorf("create working directory: %w", err)
	}
	defer func() {
		err = errors.Join(err, func() error {
			if err := os.RemoveAll(work); err != nil {
				return fmt.Errorf("remove working directory: %w", err)
			}
			return nil
		}())
	}()

	outcomes := make([]error, len(architectures))
	var wait sync.WaitGroup
	for index, target := range architectures {
		if minor < target.minimum {
			continue
		}
		wait.Go(func() {
			outcomes[index] = runArchitecture(goCommand, release, goroot, work, minor, target)
		})
	}
	wait.Wait()

	for index, target := range architectures {
		if minor < target.minimum {
			continue
		}
		err = errors.Join(err, func() error {
			if err := outcomes[index]; err != nil {
				return fmt.Errorf("%s: %w", target.name, err)
			}
			results[target.name] = success
			return nil
		}())
	}
	return results, err
}

func runArchitecture(goCommand, release, goroot, work string, minor int, target architecture) (err error) {
	directory := filepath.Join(work, target.name)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create working directory: %w", err)
	}
	env := environment(goroot, target)
	var binaries []string

	normal := filepath.Join(directory, "goid.test")
	err = errors.Join(err, func() error {
		if err := errors.Join(
			func() error {
				command := exec.Command(goCommand, "build", "-v", "./...")
				command.Env, command.Stdout, command.Stderr = env, os.Stdout, os.Stderr
				if err := command.Run(); err != nil {
					return fmt.Errorf("%s build: %w", goCommand, err)
				}
				return nil
			}(),
			func() error {
				command := exec.Command(goCommand, "test", "-c", "-o", normal, "./...")
				command.Env, command.Stdout, command.Stderr = env, os.Stdout, os.Stderr
				if err := command.Run(); err != nil {
					return fmt.Errorf("%s test -c: %w", goCommand, err)
				}
				return nil
			}(),
		); err != nil {
			return err
		}
		binaries = append(binaries, normal)
		return nil
	}())

	if target.race != 0 && minor >= target.race {
		err = errors.Join(err, func() error {
			raceEnv := env
			if target.zigTarget != "" {
				wrapper, err := filepath.Abs(filepath.Join(".github", "zig-cc"))
				if err != nil {
					return fmt.Errorf("resolve zig-cc: %w", err)
				}
				raceEnv = environment(goroot, target,
					"CGO_ENABLED=1",
					"CC="+wrapper,
					"ZIG_TARGET="+target.zigTarget,
				)
				// setup-go omits non-host race objects. Zig classifies linker
				// inputs by suffix, so zig-cc rewrites this .syso path to the
				// hard-linked .o alias.
				// https://github.com/actions/setup-go/issues/181
				raceObject := filepath.Join(goroot, "src", "runtime", "race", "race_linux_"+target.goarch+".syso")
				command := exec.Command(
					"curl", "--fail", "--location", "--output", raceObject, "--",
					"https://github.com/golang/go/raw/refs/tags/go"+release+"/src/runtime/race/"+filepath.Base(raceObject),
				)
				command.Stdout, command.Stderr = os.Stdout, os.Stderr
				if err := command.Run(); err != nil {
					return fmt.Errorf("download race object: %w", err)
				}
				if err := os.Link(raceObject, raceObject+".o"); err != nil {
					return fmt.Errorf("link race object alias: %w", err)
				}
			}
			race := filepath.Join(directory, "goid.race.test")
			build := []string{"build", "-v", "-race"}
			test := []string{"test", "-c", "-race", "-o", race}
			if target.zigTarget != "" {
				build = append(build, "-ldflags=-linkmode=external")
				test = append(test, "-ldflags=-linkmode=external")
			}
			build = append(build, "./...")
			test = append(test, "./...")
			if err := errors.Join(
				func() error {
					command := exec.Command(goCommand, build...)
					command.Env, command.Stdout, command.Stderr = raceEnv, os.Stdout, os.Stderr
					if err := command.Run(); err != nil {
						return fmt.Errorf("%s build -race: %w", goCommand, err)
					}
					return nil
				}(),
				func() error {
					command := exec.Command(goCommand, test...)
					command.Env, command.Stdout, command.Stderr = raceEnv, os.Stdout, os.Stderr
					if err := command.Run(); err != nil {
						return fmt.Errorf("%s test -c -race: %w", goCommand, err)
					}
					return nil
				}(),
			); err != nil {
				return err
			}
			// Cross-race binaries are known to crash under QEMU, so current CI
			// only verifies that they build.
			// https://github.com/golang/go/issues/29948
			// https://github.com/golang/go/issues/67881
			if target.zigTarget == "" {
				binaries = append(binaries, race)
			}
			return nil
		}())
	}

	if target.qemu != 0 && minor < target.qemu {
		return err
	}
	for _, binary := range binaries {
		for _, arguments := range [][]string{{"-test.v"}, {"-test.bench=.", "-test.benchmem", "-test.v"}} {
			err = errors.Join(err, func() error {
				command := exec.Command(binary, arguments...)
				command.Stdout, command.Stderr = os.Stdout, os.Stderr
				if err := command.Run(); err != nil {
					return fmt.Errorf("%s %q: %w", binary, arguments, err)
				}
				return nil
			}())
		}
	}
	return err
}

func environment(goroot string, target architecture, add ...string) []string {
	env := append(
		os.Environ(),
		"GOTOOLCHAIN=local",
		"GOROOT="+goroot,
		"GOOS=linux",
		"GOARCH="+target.goarch,
	)
	if target.goarm != "" {
		env = append(env, "GOARM="+target.goarm)
	}
	return append(env, add...)
}

func checkInlining(goCommand string, env []string) error {
	var stdout, stderr bytes.Buffer
	command := exec.Command(goCommand, "build", "-gcflags=-m")
	command.Env, command.Stdout, command.Stderr = env, &stdout, &stderr
	err := command.Run()
	if err == nil && inlineGet.Match(stderr.Bytes()) {
		return nil
	}
	return errors.Join(
		func() error {
			if err != nil {
				return fmt.Errorf("%s build -gcflags=-m: %w", goCommand, err)
			}
			return errors.New("go build -gcflags=-m did not report that Get can be inlined")
		}(),
		func() error {
			if _, err := os.Stdout.Write(stdout.Bytes()); err != nil {
				return fmt.Errorf("write build stdout: %w", err)
			}
			return nil
		}(),
		func() error {
			if _, err := os.Stderr.Write(stderr.Bytes()); err != nil {
				return fmt.Errorf("write build stderr: %w", err)
			}
			return nil
		}(),
	)
}
