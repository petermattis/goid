package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const zigPrefix = "zig-cc-"

const pending = "pending"

type resultFile struct {
	RunAttempt int                          `json:"run_attempt"`
	Results    map[string]map[string]string `json:"results"`
}

type version struct {
	gccgo bool
	minor int
}
type toolchain struct {
	label, command, goroot, release string
	version                         version
	installed                       bool
}
type architecture struct {
	name, goarch, goarm, image, platform, zigTarget string
	minimumMinor, raceMinor                         int
}

var architectures = []architecture{
	// Cross-compilation became possible in Go 1.5: https://go.dev/doc/go1.5#c.
	{name: "armv6", goarch: "arm", goarm: "6", minimumMinor: 5, image: "balenalib/rpi-raspbian:bookworm"},
	{name: "armv7", goarch: "arm", goarm: "7", minimumMinor: 5, image: "arm32v7/debian:bookworm", platform: "linux/arm/v7"},
	// Go 1.12 added linux/arm64 race support: https://go.dev/doc/go1.12.
	{name: "aarch64", goarch: "arm64", minimumMinor: 5, image: "arm64v8/debian:bookworm", platform: "linux/arm64", raceMinor: 12, zigTarget: "aarch64-linux-gnu"},
	// Go 1.7 added s390x; Go 1.19 added its race detector.
	{name: "s390x", goarch: "s390x", minimumMinor: 7, image: "s390x/debian:bookworm", platform: "linux/s390x", raceMinor: 19, zigTarget: "s390x-linux-gnu"},
	{name: "386", goarch: "386", minimumMinor: 5},
	// Go 1.4 race fixes only reached go1.4-bootstrap, not a release.
	{name: "x64", goarch: "amd64", minimumMinor: 3, raceMinor: 5},
}

var (
	goLabel    = regexp.MustCompile(`^1\.([0-9]+)$`)
	gccgoLabel = regexp.MustCompile(`^gccgo-[1-9][0-9]*$`)
)

func main() {
	if target, ok := strings.CutPrefix(filepath.Base(os.Args[0]), zigPrefix); ok {
		// Go 1.17 and older cannot use "zig cc" as CC. See
		// https://github.com/golang/go/issues/43078.
		arguments := []string{"cc", "-target", target}
		for _, argument := range os.Args[1:] {
			// Zig classifies linker inputs by suffix; cross-race setup creates
			// a hard-linked .o alias for each .syso object.
			if filepath.Ext(argument) == ".syso" {
				argument += ".o"
			}
			arguments = append(arguments, argument)
		}
		fatal(runCommand(nil, "zig", arguments...))
		return
	}
	if len(os.Args) < 2 || (os.Args[1] != "plan" && os.Args[1] != "run") {
		fatal(fmt.Errorf("usage: run-tests <plan|run> --attempt N --output FILE -- ARG..."))
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	attempt := flags.Int("attempt", 0, "workflow run attempt")
	output := flags.String("output", "", "result JSON path")
	var bootstrap *string
	if os.Args[1] == "run" {
		bootstrap = flags.String("bootstrap-go", "", "bootstrap Go command")
	}
	if err := flags.Parse(os.Args[2:]); err != nil {
		fatal(err)
	}
	if *attempt <= 0 || *output == "" || flags.NArg() == 0 {
		fatal(fmt.Errorf("usage: run-tests %s --attempt N --output FILE -- ARG...", os.Args[1]))
	}
	if os.Args[1] == "plan" {
		results, err := newPlan(*attempt, flags.Args())
		if err == nil {
			err = writeResults(*output, results)
		}
		fatal(err)
		return
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		fatal(fmt.Errorf("run requires linux/amd64, got %s/%s", runtime.GOOS, runtime.GOARCH))
	}
	toolchains, err := parseSpecs(flags.Args(), *bootstrap)
	if err != nil {
		fatal(err)
	}
	results, err := newPlan(*attempt, flags.Args())
	if err == nil {
		err = writeResults(*output, results)
	}
	if err == nil {
		err = run(toolchains, *bootstrap, *output, &results)
	}
	fatal(err)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseVersion(label string) (version, error) {
	if gccgoLabel.MatchString(label) {
		return version{gccgo: true}, nil
	}
	match := goLabel.FindStringSubmatch(label)
	if match == nil {
		return version{}, fmt.Errorf("invalid Go version %q", label)
	}
	minor, err := strconv.Atoi(match[1])
	if err != nil || minor < 3 {
		return version{}, fmt.Errorf("unsupported Go version %q", label)
	}
	return version{minor: minor}, nil
}

func parseSpecs(specs []string, bootstrap string) ([]toolchain, error) {
	if bootstrap != "" && !filepath.IsAbs(bootstrap) {
		return nil, fmt.Errorf("bootstrap Go command is not absolute: %q", bootstrap)
	}
	toolchains := make([]toolchain, 0, len(specs))
	for index, spec := range specs {
		label, command, installed := strings.Cut(spec, "=")
		parsed, err := parseVersion(label)
		if err != nil {
			return nil, err
		}
		if installed && (command == "" || !filepath.IsAbs(command)) {
			return nil, fmt.Errorf("Go command for %q is not absolute", label)
		}
		if installed && !parsed.gccgo && parsed.minor > 4 {
			return nil, fmt.Errorf("installed Go version %q is not supported", label)
		}
		if !installed && (parsed.gccgo || parsed.minor < 5 || label != fmt.Sprintf("1.%d", parsed.minor)) {
			return nil, fmt.Errorf("download requires a Go minor label at least 1.5, got %q", label)
		}
		if index != 0 && installed != toolchains[0].installed {
			return nil, fmt.Errorf("downloaded and installed toolchains cannot be mixed")
		}
		toolchains = append(toolchains, toolchain{label: label, command: command, version: parsed, installed: installed})
	}
	if !toolchains[0].installed && bootstrap == "" {
		return nil, fmt.Errorf("--bootstrap-go is required for downloaded toolchains")
	}
	if toolchains[0].installed && bootstrap != "" {
		return nil, fmt.Errorf("--bootstrap-go requires downloaded toolchains")
	}
	return toolchains, nil
}

func newPlan(attempt int, arguments []string) (resultFile, error) {
	results := resultFile{RunAttempt: attempt, Results: make(map[string]map[string]string, len(arguments))}
	for _, argument := range arguments {
		label, _, _ := strings.Cut(argument, "=")
		if _, ok := results.Results[label]; ok {
			return resultFile{}, fmt.Errorf("duplicate Go version %q", label)
		}
		parsed, err := parseVersion(label)
		if err != nil {
			return resultFile{}, err
		}
		results.Results[label] = make(map[string]string, len(architectures))
		for _, target := range architectures {
			value := "not_applicable"
			if target.applicable(parsed) {
				value = pending
			}
			results.Results[label][target.name] = value
		}
	}
	return results, nil
}

func (target architecture) applicable(parsed version) bool {
	if parsed.gccgo {
		// gccgo cross-compilation requires a target-specific GCC toolchain
		// rather than GOARCH, so only native x64 builds are included.
		return target.name == "x64"
	}
	return parsed.minor >= target.minimumMinor
}
func writeResults(path string, results resultFile) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("encode results: %s", err)
	}
	temporary := path + ".tmp"
	defer os.Remove(temporary)
	data = append(data, '\n')
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return fmt.Errorf("write results: %s", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace results: %s", err)
	}
	return nil
}

func latestReleaseWrapper(directory, label string) (string, error) {
	prefix := "go" + label + "."
	matches, err := filepath.Glob(filepath.Join(directory, prefix+"*"))
	if err != nil {
		return "", fmt.Errorf("find Go %s wrappers: %s", label, err)
	}
	selected, patch := "", -1
	for _, match := range matches {
		value := strings.TrimPrefix(filepath.Base(match), prefix)
		candidate, err := strconv.Atoi(value)
		if err == nil && strconv.Itoa(candidate) == value && candidate > patch {
			selected, patch = filepath.Base(match), candidate
		}
	}
	if selected == "" {
		return "", fmt.Errorf("no wrapper found for Go %s", label)
	}
	return selected, nil
}

func run(toolchains []toolchain, bootstrap, output string, results *resultFile) error {
	work, err := os.MkdirTemp("", "goid-run-tests-")
	if err != nil {
		return fmt.Errorf("create working directory: %s", err)
	}
	defer os.RemoveAll(work)

	var failures []error
	if !toolchains[0].installed {
		var module struct{ Version, Dir string }
		value, err := commandOutput(environment(), bootstrap, "mod", "download", "-json", "golang.org/dl@latest")
		if err == nil {
			err = json.Unmarshal([]byte(value), &module)
		}
		if err == nil && (module.Version == "" || module.Dir == "") {
			err = fmt.Errorf("invalid golang.org/dl module metadata")
		}
		if err != nil {
			return err
		}
		bin := filepath.Join(work, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			return fmt.Errorf("create wrapper directory: %s", err)
		}
		arguments := []string{"install"}
		var downloads []int
		for index := range toolchains {
			exact, err := latestReleaseWrapper(module.Dir, toolchains[index].label)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			toolchains[index].command = filepath.Join(bin, exact)
			toolchains[index].release = strings.TrimPrefix(exact, "go")
			arguments = append(arguments, "golang.org/dl/"+exact+"@"+module.Version)
			downloads = append(downloads, index)
		}
		downloadErrors := make([]error, len(toolchains))
		if len(downloads) != 0 {
			if err := runCommand(environment("GOBIN="+bin), bootstrap, arguments...); err != nil {
				return errors.Join(errors.Join(failures...), fmt.Errorf("install Go wrappers: %s", err))
			} else {
				var wait sync.WaitGroup
				for _, index := range downloads {
					wait.Add(1)
					go func(index int) {
						defer wait.Done()
						downloadErrors[index] = runCommand(environment("GOROOT="), toolchains[index].command, "download")
					}(index)
				}
				wait.Wait()
			}
		}
		for index, err := range downloadErrors {
			if err != nil {
				toolchains[index].command = ""
				failures = append(failures, fmt.Errorf("%s: download toolchain: %s", toolchains[index].label, err))
			}
		}
	}

	var qemuFailures []error
	for _, toolchain := range toolchains {
		if toolchain.command != "" && !toolchain.version.gccgo && toolchain.version.minor >= 9 {
			if err := runCommand(nil, "docker", "run", "--rm", "--privileged", "tonistiigi/binfmt", "--install", "all"); err != nil {
				qemuFailures = append(qemuFailures, fmt.Errorf("configure QEMU: %s", err))
			}
			break
		}
	}
	for index := range toolchains {
		toolchain := &toolchains[index]
		if toolchain.command == "" {
			continue
		}
		toolchainEnvironment := environment("GOROOT=")
		if toolchain.installed && !toolchain.version.gccgo {
			toolchainEnvironment = environment()
		}
		value, err := commandOutput(toolchainEnvironment, toolchain.command, "env", "GOROOT")
		if err == nil && strings.TrimSpace(value) == "" {
			err = fmt.Errorf("empty GOROOT")
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: resolve GOROOT: %s", toolchain.label, err))
			continue
		}
		toolchain.goroot = strings.TrimSpace(value)
		if err := runVersion(*toolchain, work, output, results, qemuFailures); err != nil {
			failures = append(failures, fmt.Errorf("%s: %s", toolchain.label, err))
		}
	}
	return errors.Join(failures...)
}

func runVersion(toolchain toolchain, work, output string, results *resultFile, qemuFailures []error) error {
	directory := filepath.Join(work, toolchain.label)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create version directory: %s", err)
	}
	type completed struct {
		name string
		err  error
	}
	done := make(chan completed, len(architectures))
	count := 0
	for _, target := range architectures {
		if !target.applicable(toolchain.version) {
			continue
		}
		count++
		go func(target architecture) {
			done <- completed{target.name, runArchitecture(toolchain, target, directory, qemuFailures)}
		}(target)
	}
	byName := make(map[string]error, count)
	var checkpointFailures []error
	for ; count != 0; count-- {
		completed := <-done
		byName[completed.name] = completed.err
		if len(checkpointFailures) != 0 {
			continue
		}
		value := "success"
		if completed.err != nil {
			value = "failure"
		}
		previous := results.Results[toolchain.label][completed.name]
		results.Results[toolchain.label][completed.name] = value
		if err := writeResults(output, *results); err != nil {
			results.Results[toolchain.label][completed.name] = previous
			checkpointFailures = append(checkpointFailures, err)
		}
	}
	var failures []error
	for _, target := range architectures {
		if err := byName[target.name]; err != nil {
			failures = append(failures, fmt.Errorf("%s: %s", target.name, err))
		}
	}
	return errors.Join(append(failures, checkpointFailures...)...)
}
