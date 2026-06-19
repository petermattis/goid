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

const (
	goReleasesURL = "https://go.dev/dl/?mode=json&include=all"
	resultSchema  = 1
	zigCCPrefix   = "zig-cc-"
)

type resultStatus string

const (
	statusPending       resultStatus = "pending"
	statusSuccess       resultStatus = "success"
	statusFailure       resultStatus = "failure"
	statusNotApplicable resultStatus = "not_applicable"
)

type resultFile struct {
	Schema     int                     `json:"schema"`
	Go         string                  `json:"go"`
	RunAttempt int                     `json:"run_attempt"`
	Results    map[string]resultStatus `json:"results"`
}

type goVersion struct {
	gccgo bool
	minor int
}

type toolchain struct {
	label   string
	command string
}

type plannedToolchain struct {
	toolchain
	version goVersion
	results resultFile
	output  string
}

type goRelease struct {
	Version string          `json:"version"`
	Stable  bool            `json:"stable"`
	Files   []goReleaseFile `json:"files"`
}

type goReleaseFile struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kind     string `json:"kind"`
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
	inlineGetPattern      = regexp.MustCompile(`(?m)can inline Get$`)
	goLabelPattern        = regexp.MustCompile(`^1\.([0-9]+)(?:\.([0-9]+))?$`)
	gccgoLabelPattern     = regexp.MustCompile(`^gccgo-([0-9]+)$`)
	goVersionPattern      = regexp.MustCompile(`\bgo(1\.[0-9]+(?:\.[0-9]+)?)\b`)
	releaseVersionPattern = regexp.MustCompile(`^go1\.([0-9]+)\.([0-9]+)$`)
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
		fatal(fmt.Errorf("usage: run-tests <plan|download|installed> [arguments]"))
	}

	switch os.Args[1] {
	case "plan":
		flags := flag.NewFlagSet("plan", flag.ContinueOnError)
		goLabel := flags.String("go", "", "matrix Go version label")
		output := flags.String("output", "", "result JSON path")
		runAttempt := flags.Int("attempt", 0, "workflow run attempt")
		if err := flags.Parse(os.Args[2:]); err != nil {
			fatal(err)
		}
		if *goLabel == "" || *output == "" || *runAttempt <= 0 || flags.NArg() != 0 {
			fatal(fmt.Errorf("usage: run-tests plan -go <version> -attempt <positive integer> -output <path>"))
		}
		results, err := newPlan(*goLabel, *runAttempt)
		if err != nil {
			fatal(err)
		}
		if err := writeResults(*output, results); err != nil {
			fatal(err)
		}
	case "download":
		flags := flag.NewFlagSet("download", flag.ContinueOnError)
		bootstrapGo := flags.String("bootstrap-go", "", "bootstrap Go command")
		output := flags.String("output", "", "result JSON directory")
		runAttempt := flags.Int("attempt", 0, "workflow run attempt")
		if err := flags.Parse(os.Args[2:]); err != nil {
			fatal(err)
		}
		if *bootstrapGo == "" || *output == "" || *runAttempt <= 0 || flags.NArg() == 0 {
			fatal(fmt.Errorf("usage: run-tests download -bootstrap-go <command> -attempt <positive integer> -output <directory> <minor>..."))
		}
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			fatal(fmt.Errorf("download requires linux/amd64, got %s/%s", runtime.GOOS, runtime.GOARCH))
		}
		if err := runDownload(
			*bootstrapGo,
			*runAttempt,
			*output,
			flags.Args(),
			fetchGoReleases,
			runCommand,
			runPlannedToolchain,
		); err != nil {
			fatal(err)
		}
	case "installed":
		flags := flag.NewFlagSet("installed", flag.ContinueOnError)
		output := flags.String("output", "", "result JSON directory")
		runAttempt := flags.Int("attempt", 0, "workflow run attempt")
		if err := flags.Parse(os.Args[2:]); err != nil {
			fatal(err)
		}
		if *output == "" || *runAttempt <= 0 || flags.NArg() == 0 {
			fatal(fmt.Errorf("usage: run-tests installed -attempt <positive integer> -output <directory> <label=command>..."))
		}
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			fatal(fmt.Errorf("installed requires linux/amd64, got %s/%s", runtime.GOOS, runtime.GOARCH))
		}
		if err := runInstalled(*runAttempt, *output, flags.Args(), runPlannedToolchain); err != nil {
			fatal(err)
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
	if match := gccgoLabelPattern.FindStringSubmatch(label); match != nil {
		major, err := strconv.Atoi(match[1])
		if err != nil || major == 0 || strconv.Itoa(major) != match[1] {
			return goVersion{}, fmt.Errorf("invalid Go version %q", label)
		}
		return goVersion{gccgo: true}, nil
	}

	match := goLabelPattern.FindStringSubmatch(label)
	if match == nil {
		return goVersion{}, fmt.Errorf("invalid Go version %q", label)
	}
	minor, err := strconv.Atoi(match[1])
	if err != nil {
		return goVersion{}, fmt.Errorf("invalid Go version %q: %s", label, err)
	}
	if minor < 3 || strconv.Itoa(minor) != match[1] {
		return goVersion{}, fmt.Errorf("unsupported Go version %q", label)
	}
	if match[2] != "" {
		patch, err := strconv.Atoi(match[2])
		if err != nil || strconv.Itoa(patch) != match[2] {
			return goVersion{}, fmt.Errorf("invalid Go version %q", label)
		}
	}
	return goVersion{minor: minor}, nil
}

type releaseFetcher func() ([]goRelease, error)
type toolchainRunner func(*plannedToolchain) []error

func parseDownloadLabels(labels []string) ([]toolchain, error) {
	if len(labels) == 0 {
		return nil, fmt.Errorf("no Go versions specified")
	}

	seen := make(map[string]bool, len(labels))
	toolchains := make([]toolchain, 0, len(labels))
	for _, label := range labels {
		version, err := parseGoVersion(label)
		if err != nil {
			return nil, err
		}
		if version.gccgo || label != fmt.Sprintf("1.%d", version.minor) {
			return nil, fmt.Errorf("download requires a Go minor label, got %q", label)
		}
		if version.minor < 5 {
			return nil, fmt.Errorf("download does not support Go version %q; use installed", label)
		}
		if seen[label] {
			return nil, fmt.Errorf("duplicate Go version %q", label)
		}
		seen[label] = true
		toolchains = append(toolchains, toolchain{label: label})
	}
	return toolchains, nil
}

func parseInstalledSpecs(specs []string) ([]toolchain, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("no Go toolchains specified")
	}

	seen := make(map[string]bool, len(specs))
	toolchains := make([]toolchain, 0, len(specs))
	for _, spec := range specs {
		label, command, ok := strings.Cut(spec, "=")
		if !ok || command == "" {
			return nil, fmt.Errorf("invalid installed toolchain %q; want label=command", spec)
		}
		if _, err := parseGoVersion(label); err != nil {
			return nil, err
		}
		if seen[label] {
			return nil, fmt.Errorf("duplicate Go version %q", label)
		}
		seen[label] = true
		toolchains = append(toolchains, toolchain{label: label, command: command})
	}
	return toolchains, nil
}

func planToolchains(toolchains []toolchain, runAttempt int, outputDirectory string) ([]*plannedToolchain, error) {
	plans := make([]*plannedToolchain, 0, len(toolchains))
	var failures []error
	for _, toolchain := range toolchains {
		version, err := parseGoVersion(toolchain.label)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		results, err := newPlan(toolchain.label, runAttempt)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		output := filepath.Join(outputDirectory, "build-status-"+toolchain.label+".json")
		if err := writeResults(output, results); err != nil {
			failures = append(failures, fmt.Errorf("plan %s: %s", toolchain.label, err))
			continue
		}
		plans = append(plans, &plannedToolchain{
			toolchain: toolchain,
			version:   version,
			results:   results,
			output:    output,
		})
	}
	return plans, errors.Join(failures...)
}

func runToolchains(plans []*plannedToolchain, runner toolchainRunner) error {
	var failures []error
	for _, plan := range plans {
		for _, err := range runner(plan) {
			failures = append(failures, fmt.Errorf("%s: %s", plan.label, err))
		}
	}
	return errors.Join(failures...)
}

func runPlannedToolchain(plan *plannedToolchain) []error {
	coordinator := coordinator{
		version:   plan.version,
		goCommand: plan.command,
		results:   &plan.results,
		output:    plan.output,
		execute:   runCommand,
	}
	return coordinator.run()
}

func runInstalled(runAttempt int, outputDirectory string, specs []string, runner toolchainRunner) error {
	toolchains, err := parseInstalledSpecs(specs)
	if err != nil {
		return err
	}
	plans, err := planToolchains(toolchains, runAttempt, outputDirectory)
	var failures []error
	if err != nil {
		failures = append(failures, err)
	}
	if err := runToolchains(plans, runner); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func runDownload(
	bootstrapGo string,
	runAttempt int,
	outputDirectory string,
	labels []string,
	fetch releaseFetcher,
	execute commandRunner,
	runner toolchainRunner,
) error {
	toolchains, err := parseDownloadLabels(labels)
	if err != nil {
		return err
	}
	plans, err := planToolchains(toolchains, runAttempt, outputDirectory)
	var failures []error
	if err != nil {
		failures = append(failures, err)
	}
	if len(plans) == 0 {
		return errors.Join(failures...)
	}

	releases, err := fetch()
	if err != nil {
		failures = append(failures, fmt.Errorf("fetch Go releases: %s", err))
		return errors.Join(failures...)
	}
	plannedLabels := make([]string, 0, len(plans))
	for _, plan := range plans {
		plannedLabels = append(plannedLabels, plan.label)
	}
	selected, err := selectGoReleases(releases, plannedLabels)
	if err != nil {
		failures = append(failures, err)
	}

	workDirectory, err := os.MkdirTemp("", "goid-download-")
	if err != nil {
		failures = append(failures, fmt.Errorf("create download directory: %s", err))
		return errors.Join(failures...)
	}
	defer os.RemoveAll(workDirectory)
	binDirectory := filepath.Join(workDirectory, "bin")
	if err := os.Mkdir(binDirectory, 0o755); err != nil {
		failures = append(failures, fmt.Errorf("create wrapper directory: %s", err))
		return errors.Join(failures...)
	}

	var ready []*plannedToolchain
	installArguments := []string{"install"}
	for _, plan := range plans {
		exact, ok := selected[plan.label]
		if !ok {
			continue
		}
		plan.command = filepath.Join(binDirectory, exact)
		installArguments = append(installArguments, "golang.org/dl/"+exact+"@latest")
		ready = append(ready, plan)
	}
	if len(ready) == 0 {
		return errors.Join(failures...)
	}
	if err := execute(toolchainEnvironment("GOBIN="+binDirectory), bootstrapGo, installArguments...); err != nil {
		failures = append(failures, fmt.Errorf("install Go download wrappers: %s", err))
		return errors.Join(failures...)
	}

	downloadFailures := make([]error, len(ready))
	var downloads sync.WaitGroup
	for index, plan := range ready {
		downloads.Add(1)
		go func(index int, plan *plannedToolchain) {
			defer downloads.Done()
			if err := execute(toolchainEnvironment(), plan.command, "download"); err != nil {
				downloadFailures[index] = fmt.Errorf("%s: download toolchain: %s", plan.label, err)
			}
		}(index, plan)
	}
	downloads.Wait()

	downloaded := ready[:0]
	for index, plan := range ready {
		if downloadFailures[index] != nil {
			failures = append(failures, downloadFailures[index])
			continue
		}
		downloaded = append(downloaded, plan)
	}
	if err := runToolchains(downloaded, runner); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func fetchGoReleases() ([]goRelease, error) {
	output, err := commandOutput(
		nil,
		"curl",
		"--fail",
		"--location",
		"--max-time", "30",
		"--show-error",
		"--silent",
		goReleasesURL,
	)
	if err != nil {
		return nil, err
	}

	var releases []goRelease
	if err := json.NewDecoder(strings.NewReader(output)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode %s: %s", goReleasesURL, err)
	}
	return releases, nil
}

func selectGoReleases(releases []goRelease, labels []string) (map[string]string, error) {
	requested := make(map[string]bool, len(labels))
	for _, label := range labels {
		requested[label] = true
	}
	type selection struct {
		version string
		patch   int
	}
	selections := make(map[string]selection, len(labels))
	for _, release := range releases {
		if !release.Stable {
			continue
		}
		match := releaseVersionPattern.FindStringSubmatch(release.Version)
		if match == nil {
			continue
		}
		minor, err := strconv.Atoi(match[1])
		if err != nil || strconv.Itoa(minor) != match[1] {
			continue
		}
		label := fmt.Sprintf("1.%d", minor)
		if !requested[label] {
			continue
		}
		patch, err := strconv.Atoi(match[2])
		if err != nil || strconv.Itoa(patch) != match[2] {
			continue
		}
		archive := release.Version + ".linux-amd64.tar.gz"
		available := false
		for _, file := range release.Files {
			if file.Filename == archive && file.OS == "linux" && file.Arch == "amd64" && file.Kind == "archive" {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		selected, ok := selections[label]
		if !ok || patch > selected.patch {
			selections[label] = selection{version: release.Version, patch: patch}
		}
	}

	selected := make(map[string]string, len(selections))
	var failures []error
	for _, label := range labels {
		selection, ok := selections[label]
		if !ok {
			failures = append(failures, fmt.Errorf("no stable linux/amd64 release found for Go %s", label))
			continue
		}
		selected[label] = selection.version
	}
	return selected, errors.Join(failures...)
}

func toolchainEnvironment(assignments ...string) []string {
	assignments = append([]string{"GOTOOLCHAIN=local"}, assignments...)
	overridden := make(map[string]bool, len(assignments))
	for _, assignment := range assignments {
		key, _, _ := strings.Cut(assignment, "=")
		overridden[key] = true
	}
	environment := make([]string, 0, len(os.Environ())+len(assignments))
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if key != "GOROOT" && !overridden[key] {
			environment = append(environment, assignment)
		}
	}
	return append(environment, assignments...)
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
	version   goVersion
	goCommand string
	results   *resultFile
	output    string
	execute   commandRunner
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

	version, err := resolvedGoVersion(coordinator.goCommand)
	if err != nil {
		for _, architecture := range targets {
			failures[architecture.name] = append(failures[architecture.name], err)
		}
		fmt.Println("::endgroup::")
		return failures
	}
	goroot, err := commandOutput(toolchainEnvironment(), coordinator.goCommand, "env", "GOROOT")
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
		if err := checkInlining(coordinator.goCommand, environment); err != nil {
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
	if err := coordinator.execute(environment, coordinator.goCommand, "build", "-v", "./..."); err != nil {
		failures = append(failures, err)
		normalReady = false
	}
	if normalReady {
		if err := coordinator.execute(environment, coordinator.goCommand, testArguments...); err != nil {
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
			if err := coordinator.execute(raceEnvironment, coordinator.goCommand, raceBuildArguments...); err != nil {
				failures = append(failures, err)
				raceReady = false
			}
			if raceReady {
				if err := coordinator.execute(raceEnvironment, coordinator.goCommand, raceTestArguments...); err != nil {
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
	if err := coordinator.execute(environment, coordinator.goCommand, "build", "-o", executor, "./.github/run-tests/execute"); err != nil {
		return append(failures, err)
	}
	if err := runDocker(coordinator.execute, workDirectory, architecture, binaries); err != nil {
		failures = append(failures, err)
	}
	return failures
}

func checkInlining(goCommand string, environment []string) error {
	command := exec.Command(goCommand, "build", "-gcflags=-m")
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
		return errors.Join(fmt.Errorf("%s build -gcflags=-m: %s", goCommand, err), replayOutput())
	}
	if !inlineGetPattern.Match(stderr.Bytes()) {
		return errors.Join(
			fmt.Errorf("%s build -gcflags=-m did not report that Get can be inlined", goCommand),
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
		"GOROOT":        true,
		"GOOS":          true,
		"GOTOOLCHAIN":   true,
	}
	environment := make([]string, 0, len(os.Environ())+6)
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if !removed[key] {
			environment = append(environment, assignment)
		}
	}
	environment = append(environment, "GOTOOLCHAIN=local", "GOOS=linux", "GOARCH="+architecture.goarch)
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

func resolvedGoVersion(goCommand string) (string, error) {
	output, err := commandOutput(toolchainEnvironment(), goCommand, "version")
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
