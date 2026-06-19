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
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	resultSchema = 1
	zigCCPrefix  = "zig-cc-"
)

type resultStatus string

const (
	statusPending       resultStatus = "pending"
	statusSuccess       resultStatus = "success"
	statusFailure       resultStatus = "failure"
	statusNotApplicable resultStatus = "not_applicable"
)

type resultFile struct {
	Schema        int                     `json:"schema"`
	Go            string                  `json:"go"`
	RunAttempt    int                     `json:"run_attempt"`
	CrossCompiler string                  `json:"cross_compiler,omitempty"`
	Results       map[string]resultStatus `json:"results"`
}

type goVersion struct {
	gccgo bool
	minor int
}

type architecture struct {
	name         string
	goarch       string
	goarm        string
	minimumMinor int
	emulated     bool
	image        string
	platform     string
	raceMinor    int
	zigTarget    string
}

var architectures = []architecture{
	{
		name:         "armv6",
		goarch:       "arm",
		goarm:        "6",
		minimumMinor: 5,
		emulated:     true,
		image:        "balenalib/rpi-raspbian:bookworm",
	},
	{
		name:         "armv7",
		goarch:       "arm",
		goarm:        "7",
		minimumMinor: 5,
		emulated:     true,
		image:        "arm32v7/debian:bookworm",
		platform:     "linux/arm/v7",
	},
	{
		name:         "aarch64",
		goarch:       "arm64",
		minimumMinor: 5,
		emulated:     true,
		image:        "arm64v8/debian:bookworm",
		platform:     "linux/arm64",
		// Race detector support on linux/arm64 was added in Go 1.12.
		// See https://go.dev/doc/go1.12.
		raceMinor: 12,
		zigTarget: "aarch64-linux-gnu",
	},
	{
		name:   "s390x",
		goarch: "s390x",
		// Support for s390x was added in Go 1.7.
		// See https://go.dev/doc/go1.7#ports.
		minimumMinor: 7,
		emulated:     true,
		image:        "s390x/debian:bookworm",
		platform:     "linux/s390x",
		// Race detector support on linux/s390x was added in Go 1.19.
		// See https://go.dev/doc/go1.19.
		raceMinor: 19,
		zigTarget: "s390x-linux-gnu",
	},
	{
		name:         "386",
		goarch:       "386",
		minimumMinor: 5,
	},
	{
		name:         "x64",
		goarch:       "amd64",
		minimumMinor: 3,
		// Race builds with Go 1.4 and below are broken with newer C
		// compilers. Several fixes only reached go1.4-bootstrap. See
		// https://github.com/golang/go/compare/go1.4.3...release-branch.go1.4.
		raceMinor: 5,
	},
}

var (
	inlineGetPattern = regexp.MustCompile(`(?m)can inline Get$`)
	goVersionPattern = regexp.MustCompile(`\bgo(1\.[0-9]+(?:\.[0-9]+)?)\b`)
)

func main() {
	if target, ok := strings.CutPrefix(filepath.Base(os.Args[0]), zigCCPrefix); ok {
		if target == "" {
			fatal(fmt.Errorf("missing Zig target in %q", os.Args[0]))
		}
		if err := runZigCC(runCommand, target, os.Args[1:]); err != nil {
			fatal(err)
		}
		return
	}

	if len(os.Args) < 2 {
		fatal(fmt.Errorf("usage: run-tests <plan|run> -go <version> -attempt <positive integer> -output <path>"))
	}

	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	goLabel := flags.String("go", "", "matrix Go version label")
	output := flags.String("output", "", "result JSON path")
	runAttempt := flags.Int("attempt", 0, "workflow run attempt")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fatal(err)
	}
	if *goLabel == "" || *output == "" || *runAttempt <= 0 || flags.NArg() != 0 {
		fatal(fmt.Errorf("usage: run-tests %s -go <version> -attempt <positive integer> -output <path>", os.Args[1]))
	}

	switch os.Args[1] {
	case "plan":
		results, err := newPlan(*goLabel, *runAttempt)
		if err != nil {
			fatal(err)
		}
		if err := writeResults(*output, results); err != nil {
			fatal(err)
		}
	case "run":
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			fatal(fmt.Errorf("run requires linux/amd64, got %s/%s", runtime.GOOS, runtime.GOARCH))
		}

		version, err := parseGoVersion(*goLabel)
		if err != nil {
			fatal(err)
		}
		expected, err := newPlan(*goLabel, *runAttempt)
		if err != nil {
			fatal(err)
		}
		results, err := readResults(*output)
		if err != nil {
			fatal(err)
		}
		if !reflect.DeepEqual(results, expected) {
			fatal(fmt.Errorf("%s does not contain the plan for %s", *output, *goLabel))
		}

		coordinator := coordinator{
			version: version,
			results: &results,
			output:  *output,
			execute: runCommand,
		}
		failures := coordinator.run()
		for _, err := range failures {
			fmt.Fprintln(os.Stderr, err)
		}
		if len(failures) != 0 {
			os.Exit(1)
		}
	default:
		fatal(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func parseGoVersion(label string) (goVersion, error) {
	if strings.HasPrefix(label, "gccgo-") {
		if _, err := strconv.Atoi(strings.TrimPrefix(label, "gccgo-")); err != nil {
			return goVersion{}, fmt.Errorf("invalid Go version %q: %s", label, err)
		}
		return goVersion{gccgo: true}, nil
	}

	parts := strings.Split(label, ".")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "1" {
		return goVersion{}, fmt.Errorf("invalid Go version %q", label)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return goVersion{}, fmt.Errorf("invalid Go version %q: %s", label, err)
	}
	if minor < 3 {
		return goVersion{}, fmt.Errorf("unsupported Go version %q", label)
	}
	if len(parts) == 3 {
		if _, err := strconv.Atoi(parts[2]); err != nil {
			return goVersion{}, fmt.Errorf("invalid Go version %q: %s", label, err)
		}
	}
	return goVersion{minor: minor}, nil
}

func newPlan(label string, runAttempt int) (resultFile, error) {
	if runAttempt <= 0 {
		return resultFile{}, fmt.Errorf("invalid run attempt %d", runAttempt)
	}
	version, err := parseGoVersion(label)
	if err != nil {
		return resultFile{}, err
	}

	results := resultFile{
		Schema:     resultSchema,
		Go:         label,
		RunAttempt: runAttempt,
		Results:    make(map[string]resultStatus, len(architectures)),
	}
	for _, architecture := range architectures {
		status := statusNotApplicable
		if architecture.applicable(version) {
			status = statusPending
		}
		results.Results[architecture.name] = status
		if status == statusPending &&
			architecture.zigTarget != "" &&
			architecture.supportsRace(version) {
			results.CrossCompiler = "zig"
		}
	}
	return results, nil
}

func (architecture architecture) applicable(version goVersion) bool {
	if version.gccgo {
		// gccgo cross-compilation requires a target-specific GCC toolchain
		// rather than GOARCH, so only native x64 builds are included.
		return architecture.name == "x64"
	}
	// Cross-compilation became possible in Go 1.5 with the removal of C
	// code from the compiler. See https://go.dev/doc/go1.5#c.
	return version.minor >= architecture.minimumMinor
}

func (architecture architecture) supportsRace(version goVersion) bool {
	return !version.gccgo && architecture.raceMinor != 0 && version.minor >= architecture.raceMinor
}

func readResults(path string) (resultFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return resultFile{}, fmt.Errorf("open %s: %s", path, err)
	}
	defer file.Close()

	var results resultFile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&results); err != nil {
		return resultFile{}, fmt.Errorf("decode %s: %s", path, err)
	}
	return results, nil
}

func writeResults(path string, results resultFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create result directory: %s", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".run-tests-results-")
	if err != nil {
		return fmt.Errorf("create result file: %s", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(results); err != nil {
		file.Close()
		return fmt.Errorf("encode results: %s", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close result file: %s", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace %s: %s", path, err)
	}
	return nil
}

type coordinator struct {
	version goVersion
	results *resultFile
	output  string
	execute commandRunner
}

type commandRunner func([]string, string, ...string) error

func (coordinator coordinator) run() []error {
	workDirectory, err := os.MkdirTemp("", "goid-run-tests-")
	if err != nil {
		return []error{fmt.Errorf("create working directory: %s", err)}
	}
	defer os.RemoveAll(workDirectory)

	raceFailures := coordinator.prepareRace(workDirectory)
	emulationFailures := coordinator.prepareEmulation()
	type completedArchitecture struct {
		architecture architecture
		failures     []error
	}
	completed := make(chan completedArchitecture, len(architectures))
	pending := 0

	fmt.Println("::group::architectures")
	for _, target := range architectures {
		if coordinator.results.Results[target.name] == statusNotApplicable {
			continue
		}

		pending++
		go func(target architecture) {
			fmt.Printf("--- %s started ---\n", target.name)
			failures := coordinator.runArchitecture(
				workDirectory,
				target,
				raceFailures[target.name],
				emulationFailures[target.name],
			)
			fmt.Printf("--- %s finished ---\n", target.name)
			completed <- completedArchitecture{
				architecture: target,
				failures:     failures,
			}
		}(target)
	}

	completedFailures := make(map[string][]error, pending)
	var writeFailures []error
	checkpointFailed := false
	for ; pending > 0; pending-- {
		result := <-completed
		completedFailures[result.architecture.name] = result.failures
		status := statusSuccess
		if len(result.failures) != 0 {
			status = statusFailure
		}
		coordinator.results.Results[result.architecture.name] = status
		if checkpointFailed {
			continue
		}
		if err := writeResults(coordinator.output, *coordinator.results); err != nil {
			writeFailures = append(writeFailures, err)
			checkpointFailed = true
		}
	}
	fmt.Println("::endgroup::")

	var failures []error
	for _, architecture := range architectures {
		for _, err := range completedFailures[architecture.name] {
			failures = append(failures, fmt.Errorf("%s: %s", architecture.name, err))
		}
	}
	failures = append(failures, writeFailures...)
	return failures
}

func (coordinator coordinator) prepareRace(workDirectory string) map[string][]error {
	failures := make(map[string][]error)
	var targets []architecture
	for _, architecture := range architectures {
		if architecture.applicable(coordinator.version) &&
			architecture.zigTarget != "" &&
			architecture.supportsRace(coordinator.version) {
			targets = append(targets, architecture)
		}
	}
	if len(targets) == 0 {
		return failures
	}

	fmt.Println("::group::cross-race setup")
	// Go 1.17 and below keep only the first word of CC when invoking cgo.
	// Give them a single executable path that main dispatches back to Zig.
	executable, err := os.Executable()
	if err != nil {
		for _, architecture := range targets {
			failures[architecture.name] = append(
				failures[architecture.name],
				fmt.Errorf("find test runner executable: %s", err),
			)
		}
	} else {
		for _, architecture := range targets {
			if err := os.Symlink(executable, zigCCPath(workDirectory, architecture)); err != nil {
				failures[architecture.name] = append(
					failures[architecture.name],
					fmt.Errorf("create Zig compiler trampoline: %s", err),
				)
			}
		}
	}

	version, err := resolvedGoVersion()
	if err != nil {
		for _, architecture := range targets {
			failures[architecture.name] = append(failures[architecture.name], err)
		}
		fmt.Println("::endgroup::")
		return failures
	}
	goroot, err := commandOutput(nil, "go", "env", "GOROOT")
	if err != nil {
		for _, architecture := range targets {
			failures[architecture.name] = append(failures[architecture.name], err)
		}
		fmt.Println("::endgroup::")
		return failures
	}
	for _, architecture := range targets {
		// Non-host *.syso files are missing from the Go toolchains provided
		// by setup-go. See https://github.com/actions/setup-go/issues/181.
		path := filepath.Join(
			strings.TrimSpace(goroot),
			"src",
			"runtime",
			"race",
			"race_linux_"+architecture.goarch+".syso",
		)
		url := fmt.Sprintf(
			"https://github.com/golang/go/raw/refs/tags/go%s/src/runtime/race/race_linux_%s.syso",
			version,
			architecture.goarch,
		)
		if err := coordinator.execute(nil, "curl", "--fail", "--location", "--output", path, url); err != nil {
			failures[architecture.name] = append(
				failures[architecture.name],
				fmt.Errorf("download race runtime: %s", err),
			)
		} else if err := os.Link(path, path+".o"); err != nil {
			failures[architecture.name] = append(
				failures[architecture.name],
				fmt.Errorf("create Zig race object alias: %s", err),
			)
		}
	}
	fmt.Println("::endgroup::")
	return failures
}

func (coordinator coordinator) prepareEmulation() map[string][]error {
	failures := make(map[string][]error)
	// Go binaries built with Go 1.8 and below are incompatible with QEMU
	// user-level emulation. See https://github.com/golang/go/commit/2673f9e.
	if coordinator.version.gccgo || coordinator.version.minor < 9 {
		return failures
	}

	var targets []architecture
	for _, architecture := range architectures {
		if architecture.emulated && architecture.applicable(coordinator.version) {
			targets = append(targets, architecture)
		}
	}
	if len(targets) == 0 {
		return failures
	}

	fmt.Println("::group::QEMU setup")
	if err := coordinator.execute(nil, "docker", "run", "--rm", "--privileged", "tonistiigi/binfmt", "--install", "all"); err != nil {
		for _, architecture := range targets {
			failures[architecture.name] = append(
				failures[architecture.name],
				fmt.Errorf("configure QEMU: %s", err),
			)
		}
	}
	fmt.Println("::endgroup::")
	return failures
}

func (coordinator coordinator) runArchitecture(
	workDirectory string,
	architecture architecture,
	raceFailures []error,
	emulationFailures []error,
) []error {
	directory := filepath.Join(workDirectory, architecture.name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return []error{fmt.Errorf("create output directory: %s", err)}
	}
	environment := targetEnvironment(workDirectory, architecture, false)
	var failures []error

	if !coordinator.version.gccgo && coordinator.version.minor >= 12 {
		// Go 1.12 introduced the inliner that should inline Get. See
		// https://go.dev/doc/go1.12#compiler.
		if err := checkInlining(environment); err != nil {
			failures = append(failures, err)
		}
	}

	normalBinary := filepath.Join(directory, "goid.test")
	testArguments := []string{"test", "-c", "-o", normalBinary, "./..."}
	if !coordinator.version.gccgo && coordinator.version.minor == 3 {
		// The -o flag was added to go test in Go 1.4. Go 1.3 writes the
		// binary to the current directory instead.
		checkout, err := os.Getwd()
		if err != nil {
			return append(failures, fmt.Errorf("get working directory: %s", err))
		}
		normalBinary = filepath.Join(checkout, filepath.Base(checkout)+".test")
		testArguments = []string{"test", "-c", "./..."}
		defer os.Remove(normalBinary)
	}
	normalReady := true
	if err := coordinator.execute(environment, "go", "build", "-v", "./..."); err != nil {
		failures = append(failures, err)
		normalReady = false
	}
	if normalReady {
		if err := coordinator.execute(environment, "go", testArguments...); err != nil {
			failures = append(failures, err)
			normalReady = false
		}
	}

	var binaries []string
	if normalReady {
		binaries = append(binaries, normalBinary)
	}

	if architecture.supportsRace(coordinator.version) {
		if len(raceFailures) != 0 {
			failures = append(failures, raceFailures...)
		} else {
			raceEnvironment := targetEnvironment(workDirectory, architecture, true)
			raceBinary := filepath.Join(directory, "goid.race.test")
			raceBuildArguments := []string{"build", "-v", "-race"}
			raceTestArguments := []string{"test", "-c", "-race"}
			if architecture.zigTarget != "" {
				// The Go internal linker cannot resolve libc references in
				// cross-race objects linked by Zig. Use Zig for the final link.
				raceBuildArguments = append(raceBuildArguments, "-ldflags=-linkmode=external")
				raceTestArguments = append(raceTestArguments, "-ldflags=-linkmode=external")
			}
			raceBuildArguments = append(raceBuildArguments, "./...")
			raceTestArguments = append(raceTestArguments, "-o", raceBinary, "./...")
			raceReady := true
			if err := coordinator.execute(raceEnvironment, "go", raceBuildArguments...); err != nil {
				failures = append(failures, err)
				raceReady = false
			}
			if raceReady {
				if err := coordinator.execute(raceEnvironment, "go", raceTestArguments...); err != nil {
					failures = append(failures, err)
					raceReady = false
				}
			}
			if raceReady {
				binaries = append(binaries, raceBinary)
			}
		}
	}

	if !architecture.emulated {
		return append(failures, runNative(coordinator.execute, binaries)...)
	}
	if coordinator.version.minor < 9 {
		return failures
	}
	if len(emulationFailures) != 0 {
		return append(failures, emulationFailures...)
	}
	if len(binaries) == 0 {
		return failures
	}

	executor := filepath.Join(directory, "execute")
	if err := coordinator.execute(environment, "go", "build", "-o", executor, "./.github/run-tests/execute"); err != nil {
		return append(failures, err)
	}
	if err := runDocker(coordinator.execute, workDirectory, architecture, binaries); err != nil {
		failures = append(failures, err)
	}
	return failures
}

func checkInlining(environment []string) error {
	command := exec.Command("go", "build", "-gcflags=-m")
	command.Env = environment
	// Optimization diagnostics are expected output. Replay them only if the
	// command or assertion fails.
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	replayOutput := func() error {
		var failures []error
		if _, err := os.Stdout.Write(stdout.Bytes()); err != nil {
			failures = append(failures, fmt.Errorf("write go build stdout: %s", err))
		}
		if _, err := os.Stderr.Write(stderr.Bytes()); err != nil {
			failures = append(failures, fmt.Errorf("write go build stderr: %s", err))
		}
		return errors.Join(failures...)
	}
	if err := command.Run(); err != nil {
		return errors.Join(fmt.Errorf("go build -gcflags=-m: %s", err), replayOutput())
	}
	if !inlineGetPattern.Match(stderr.Bytes()) {
		return errors.Join(
			fmt.Errorf("go build -gcflags=-m did not report that Get can be inlined"),
			replayOutput(),
		)
	}
	return nil
}

func runNative(execute commandRunner, binaries []string) []error {
	var failures []error
	for _, binary := range binaries {
		if err := execute(nil, binary, "-test.v"); err != nil {
			failures = append(failures, err)
		}
		if err := execute(nil, binary, "-test.bench=.", "-test.benchmem", "-test.v"); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func runDocker(
	execute commandRunner,
	workDirectory string,
	architecture architecture,
	binaries []string,
) error {
	checkout, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %s", err)
	}
	arguments := []string{
		"run",
		"--rm",
		"--workdir",
		checkout,
		"--mount",
		fmt.Sprintf("type=bind,source=%s,target=%s", checkout, checkout),
	}
	if architecture.platform != "" {
		arguments = append(arguments, "--platform", architecture.platform)
	}
	arguments = append(
		arguments,
		"--mount",
		fmt.Sprintf("type=bind,source=%s,target=/run-tests,readonly", workDirectory),
		architecture.image,
		filepath.ToSlash(filepath.Join("/run-tests", architecture.name, "execute")),
	)
	for _, binary := range binaries {
		arguments = append(
			arguments,
			filepath.ToSlash(filepath.Join("/run-tests", architecture.name, filepath.Base(binary))),
		)
	}
	return execute(nil, "docker", arguments...)
}

func targetEnvironment(workDirectory string, architecture architecture, race bool) []string {
	removed := map[string]bool{
		"CC":            true,
		"CC_FOR_TARGET": true,
		"CGO_ENABLED":   true,
		"GOARCH":        true,
		"GOARM":         true,
		"GOOS":          true,
	}
	environment := make([]string, 0, len(os.Environ())+6)
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if !removed[key] {
			environment = append(environment, assignment)
		}
	}
	environment = append(environment, "GOOS=linux", "GOARCH="+architecture.goarch)
	if architecture.goarm != "" {
		environment = append(environment, "GOARM="+architecture.goarm)
	}
	if race && architecture.zigTarget != "" {
		environment = append(
			environment,
			"CGO_ENABLED=1",
			"CC="+zigCCPath(workDirectory, architecture),
		)
	}
	return environment
}

func zigCCPath(workDirectory string, architecture architecture) string {
	return filepath.Join(workDirectory, zigCCPrefix+architecture.zigTarget)
}

func runZigCC(execute commandRunner, target string, compilerArguments []string) error {
	arguments := []string{"cc", "-target", target}
	for _, argument := range compilerArguments {
		// Unlike GCC, Zig rejects object files with the .syso extension.
		// prepareRace creates an object alias that Zig recognizes.
		if filepath.Ext(argument) == ".syso" {
			argument += ".o"
		}
		arguments = append(arguments, argument)
	}
	return execute(nil, "zig", arguments...)
}

func resolvedGoVersion() (string, error) {
	output, err := commandOutput(nil, "go", "version")
	if err != nil {
		return "", err
	}
	match := goVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return "", fmt.Errorf("parse go version output %q", strings.TrimSpace(output))
	}
	return match[1], nil
}

func commandOutput(environment []string, name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	if environment != nil {
		command.Env = environment
	}
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s %q: %s", name, arguments, err)
	}
	return string(output), nil
}

func runCommand(environment []string, name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	if environment != nil {
		command.Env = environment
	}
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %q: %s", name, arguments, err)
	}
	return nil
}
