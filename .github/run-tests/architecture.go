package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type binary struct {
	path     string
	expected *regexp.Regexp
}

var (
	inlineGet  = regexp.MustCompile(`(?m)can inline Get$`)
	arm64Crash = regexp.MustCompile(`^FATAL: ThreadSanitizer: unsupported VMA range\nFATAL: Found 47 - Supported 48$`)
	s390xCrash = regexp.MustCompile(`^==[0-9]+==ERROR: ThreadSanitizer failed to allocate 0x[0-9a-f]+ \([0-9]+\) bytes at address [0-9a-f]+ \(errno: 12\)$`)
)

func runArchitecture(toolchain toolchain, target architecture, work string, qemuFailures []error) error {
	directory := filepath.Join(work, target.name)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return fmt.Errorf("create architecture directory: %s", err)
	}
	environment := targetEnvironment(toolchain.goroot, work, target, false)
	var failures []error
	if !toolchain.version.gccgo && toolchain.version.minor >= 12 {
		addFailure(&failures, checkInlining(toolchain.command, environment))
	}
	normal := filepath.Join(directory, "goid.test")
	arguments := []string{"test", "-c", "-o", normal, "./..."}
	if !toolchain.version.gccgo && toolchain.version.minor == 3 {
		checkout, err := os.Getwd()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		normal = filepath.Join(checkout, filepath.Base(checkout)+".test")
		arguments = []string{"test", "-c", "./..."}
		defer os.Remove(normal)
	}
	var binaries []binary
	if err := runCommands(environment, toolchain.command, []string{"build", "-v", "./..."}, arguments); err != nil {
		failures = append(failures, err)
	} else {
		binaries = append(binaries, binary{path: normal})
	}
	if !toolchain.version.gccgo && target.raceMinor != 0 && toolchain.version.minor >= target.raceMinor {
		var raceFailures []error
		var expected *regexp.Regexp
		if target.name == "aarch64" {
			expected = arm64Crash
		} else if target.name == "s390x" {
			canMap, err := canMapS390xTSANMeta()
			if err != nil {
				raceFailures = append(raceFailures, err)
			} else if !canMap {
				expected = s390xCrash
			}
		}
		if target.zigTarget != "" && len(raceFailures) == 0 {
			executable, err := os.Executable()
			if err == nil {
				err = os.Symlink(executable, filepath.Join(work, zigPrefix+target.zigTarget))
			}
			path := filepath.Join(toolchain.goroot, "src", "runtime", "race", "race_linux_"+target.goarch+".syso")
			if err == nil {
				url := "https://github.com/golang/go/raw/refs/tags/go" + toolchain.release + "/src/runtime/race/" + filepath.Base(path)
				err = runCommand(nil, "curl", "--fail", "--location", "--output", path, url)
			}
			if err == nil {
				os.Remove(path + ".o")
				err = os.Link(path, path+".o")
			}
			if err != nil {
				raceFailures = append(raceFailures, fmt.Errorf("prepare cross-race: %s", err))
			}
		}
		if len(raceFailures) == 0 {
			race := filepath.Join(directory, "goid.race.test")
			build := []string{"build", "-v", "-race"}
			test := []string{"test", "-c", "-race"}
			if target.zigTarget != "" {
				build = append(build, "-ldflags=-linkmode=external")
				test = append(test, "-ldflags=-linkmode=external")
			}
			build = append(build, "./...")
			test = append(test, "-o", race, "./...")
			raceEnvironment := targetEnvironment(toolchain.goroot, work, target, true)
			if err := runCommands(raceEnvironment, toolchain.command, build, test); err != nil {
				failures = append(failures, err)
			} else {
				binaries = append(binaries, binary{race, expected})
			}
		} else {
			failures = append(failures, raceFailures...)
		}
	}
	if target.image != "" && toolchain.version.minor < 9 {
		return errors.Join(failures...)
	}
	if target.image != "" && len(qemuFailures) != 0 {
		return errors.Join(append(failures, qemuFailures...)...)
	}
	for _, binary := range binaries {
		command := binary.path
		var prefix []string
		if target.image != "" {
			prefix = []string{"run", "--rm", "--workdir", "/test", "--mount", fmt.Sprintf("type=bind,source=%s,target=/test,readonly", directory)}
			if target.platform != "" {
				prefix = append(prefix, "--platform", target.platform)
			}
			prefix = append(prefix, target.image, "/test/"+filepath.Base(binary.path))
			command = "docker"
		}
		runs := [][]string{{"-test.v"}, {"-test.bench=.", "-test.benchmem", "-test.v"}}
		if binary.expected != nil {
			runs = runs[:1]
		}
		for _, arguments := range runs {
			arguments = append(append([]string(nil), prefix...), arguments...)
			if binary.expected != nil {
				addFailure(&failures, runExpected(command, arguments, binary.expected))
			} else {
				addFailure(&failures, runCommand(nil, command, arguments...))
			}
		}
	}
	return errors.Join(failures...)
}

func targetEnvironment(goroot, work string, target architecture, race bool) []string {
	add := []string{"GOROOT=" + goroot, "GOOS=linux", "GOARCH=" + target.goarch}
	if target.goarm != "" {
		add = append(add, "GOARM="+target.goarm)
	}
	if race && target.zigTarget != "" {
		add = append(add, "CGO_ENABLED=1", "CC="+filepath.Join(work, zigPrefix+target.zigTarget))
	}
	return environment(add...)
}

func environment(add ...string) []string {
	keys := map[string]bool{"CC": true, "CC_FOR_TARGET": true, "CGO_ENABLED": true, "GOARCH": true, "GOARM": true, "GOOS": true, "GOTOOLCHAIN": true}
	for _, value := range add {
		key, _, _ := strings.Cut(value, "=")
		keys[key] = true
	}
	values := make([]string, 0, len(os.Environ())+len(add)+1)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !keys[key] {
			values = append(values, value)
		}
	}
	return append(values, append([]string{"GOTOOLCHAIN=local"}, add...)...)
}

func checkInlining(goCommand string, environment []string) error {
	stdout, stderr, err := capture(environment, goCommand, "build", "-gcflags=-m")
	if err == nil && inlineGet.Match(stderr.Bytes()) {
		return nil
	}
	if err != nil {
		return errors.Join(fmt.Errorf("%s build -gcflags=-m: %s", goCommand, err), replay(stdout, stderr))
	}
	return errors.Join(fmt.Errorf("%s build -gcflags=-m did not report that Get can be inlined", goCommand), replay(stdout, stderr))
}

func runExpected(name string, arguments []string, expected *regexp.Regexp) error {
	stdout, stderr, err := capture(nil, name, arguments...)
	if _, ok := err.(*exec.ExitError); ok && stdout.Len() == 0 && expected.MatchString(strings.TrimRight(stderr.String(), "\n")) {
		return nil
	}
	if err == nil {
		return errors.Join(fmt.Errorf("%s %q unexpectedly succeeded", name, arguments), replay(stdout, stderr))
	}
	return errors.Join(fmt.Errorf("%s %q failed differently than expected: %s", name, arguments, err), replay(stdout, stderr))
}

func replay(stdout, stderr *bytes.Buffer) error {
	var failures []error
	_, err := os.Stdout.Write(stdout.Bytes())
	addFailure(&failures, err)
	_, err = os.Stderr.Write(stderr.Bytes())
	addFailure(&failures, err)
	return errors.Join(failures...)
}

func addFailure(failures *[]error, err error) {
	if err != nil {
		*failures = append(*failures, err)
	}
}

func canMapS390xTSANMeta() (bool, error) {
	address64 := uint64(0x900000000000)
	address, size, fixedNoReplace := uintptr(address64), uintptr(64<<10), uintptr(0x100000)
	mapped, _, err := syscall.Syscall6(syscall.SYS_MMAP, address, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE|fixedNoReplace, ^uintptr(0), 0)
	if err == syscall.ENOMEM {
		return false, nil
	}
	if err != 0 {
		return false, fmt.Errorf("probe ThreadSanitizer metadata mapping: %s", err)
	}
	_, _, err = syscall.Syscall(syscall.SYS_MUNMAP, mapped, size, 0)
	if err != 0 {
		return false, fmt.Errorf("unmap ThreadSanitizer metadata probe: %s", err)
	}
	if mapped != address {
		return false, fmt.Errorf("probe ThreadSanitizer metadata mapping: got address %#x, want %#x", mapped, address)
	}
	return true, nil
}

func commandOutput(environment []string, name string, arguments ...string) (string, error) {
	command := exec.Command(name, arguments...)
	command.Env, command.Stderr = environment, os.Stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%s %q: %s", name, arguments, err)
	}
	return string(output), nil
}

func capture(environment []string, name string, arguments ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	command := exec.Command(name, arguments...)
	command.Env, command.Stdout, command.Stderr = environment, stdout, stderr
	return stdout, stderr, command.Run()
}

func runCommands(environment []string, name string, commands ...[]string) error {
	var failures []error
	for _, arguments := range commands {
		addFailure(&failures, runCommand(environment, name, arguments...))
	}
	return errors.Join(failures...)
}

func runCommand(environment []string, name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Env, command.Stdin, command.Stdout, command.Stderr = environment, os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %q: %s", name, arguments, err)
	}
	return nil
}
