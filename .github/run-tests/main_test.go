package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanApplicability(t *testing.T) {
	tests := []struct {
		version    string
		applicable map[string]bool
	}{
		{
			version:    "1.3",
			applicable: map[string]bool{"x64": true},
		},
		{
			version: "1.5",
			applicable: map[string]bool{
				"armv6":   true,
				"armv7":   true,
				"aarch64": true,
				"386":     true,
				"x64":     true,
			},
		},
		{
			version: "1.7",
			applicable: map[string]bool{
				"armv6":   true,
				"armv7":   true,
				"aarch64": true,
				"s390x":   true,
				"386":     true,
				"x64":     true,
			},
		},
		{
			version:    "gccgo-14",
			applicable: map[string]bool{"x64": true},
		},
	}

	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			plan, err := newPlan(test.version, 2)
			if err != nil {
				t.Fatal(err)
			}
			if plan.RunAttempt != 2 {
				t.Fatalf("got run attempt %d, want 2", plan.RunAttempt)
			}
			for _, architecture := range architectures {
				want := statusNotApplicable
				if test.applicable[architecture.name] {
					want = statusPending
				}
				if got := plan.Results[architecture.name]; got != want {
					t.Errorf("%s: got %q, want %q", architecture.name, got, want)
				}
			}
			for architecture := range test.applicable {
				if _, ok := plan.Results[architecture]; !ok {
					t.Errorf("missing %s result", architecture)
				}
			}
		})
	}
}

func TestPlanRejectsInvalidVersions(t *testing.T) {
	for _, version := range []string{"1.2", "2.0", "gccgo-x"} {
		t.Run(version, func(t *testing.T) {
			if _, err := newPlan(version, 1); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPlanRejectsInvalidRunAttempt(t *testing.T) {
	if _, err := newPlan("1.26", 0); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSelectGoReleases(t *testing.T) {
	archive := func(version string) goReleaseFile {
		return goReleaseFile{
			Filename: version + ".linux-amd64.tar.gz",
			OS:       "linux",
			Arch:     "amd64",
			Kind:     "archive",
		}
	}
	releases := []goRelease{
		{Version: "go1.25.10", Stable: true, Files: []goReleaseFile{archive("go1.25.10")}},
		{Version: "go1.25.9", Stable: true, Files: []goReleaseFile{archive("go1.25.9")}},
		{Version: "go1.25.11", Stable: false, Files: []goReleaseFile{archive("go1.25.11")}},
		{Version: "go1.26.4", Stable: true, Files: []goReleaseFile{{
			Filename: "go1.26.4.darwin-amd64.tar.gz",
			OS:       "darwin",
			Arch:     "amd64",
			Kind:     "archive",
		}}},
		{Version: "go1.26.3", Stable: true, Files: []goReleaseFile{archive("go1.26.3")}},
		{Version: "go1.24", Stable: true, Files: []goReleaseFile{archive("go1.24")}},
	}

	selected, err := selectGoReleases(releases, []string{"1.25", "1.26", "1.24"})
	if err == nil {
		t.Fatal("expected a missing Go 1.24 error")
	}
	if !strings.Contains(err.Error(), "Go 1.24") {
		t.Fatalf("got error %s, want missing Go 1.24", err)
	}
	want := map[string]string{"1.25": "go1.25.10", "1.26": "go1.26.3"}
	if !reflect.DeepEqual(selected, want) {
		t.Errorf("got selections %q, want %q", selected, want)
	}
}

func TestParseInstalledSpecs(t *testing.T) {
	got, err := parseInstalledSpecs([]string{
		"1.3=/opt/go 1.3/bin/go",
		"gccgo-14=/usr/bin/go-14=debug",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []toolchain{
		{label: "1.3", command: "/opt/go 1.3/bin/go"},
		{label: "gccgo-14", command: "/usr/bin/go-14=debug"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got toolchains %q, want %q", got, want)
	}

	for _, specs := range [][]string{
		{"1.25"},
		{"1.25="},
		{"../1.25=go"},
		{"1.25=go", "1.25=other-go"},
		{"gccgo-0=go"},
	} {
		if _, err := parseInstalledSpecs(specs); err == nil {
			t.Errorf("specs %q unexpectedly succeeded", specs)
		}
	}
}

func TestRunInstalledPreplansAndContinues(t *testing.T) {
	output := t.TempDir()
	var ran []string
	err := runInstalled(
		3,
		output,
		[]string{"1.3=/sdk/go1.3", "1.4=/sdk/go1.4"},
		func(plan *plannedToolchain) []error {
			for _, label := range []string{"1.3", "1.4"} {
				results, err := readResults(filepath.Join(output, "build-status-"+label+".json"))
				if err != nil {
					t.Error(err)
					continue
				}
				if results.RunAttempt != 3 || results.Results["x64"] != statusPending {
					t.Errorf("%s was not preplanned: %#v", label, results)
				}
			}
			ran = append(ran, plan.label)
			if plan.label == "1.3" {
				return []error{fmt.Errorf("broken toolchain")}
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected a Go 1.3 failure")
	}
	if !strings.Contains(err.Error(), "1.3: broken toolchain") {
		t.Fatalf("got error %s, want Go 1.3 failure", err)
	}
	if want := []string{"1.3", "1.4"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
}

func TestRunInstalledContinuesAfterPlanFailure(t *testing.T) {
	output := t.TempDir()
	if err := os.Mkdir(filepath.Join(output, "build-status-1.3.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	var ran []string
	err := runInstalled(
		1,
		output,
		[]string{"1.3=/sdk/go1.3", "1.4=/sdk/go1.4"},
		func(plan *plannedToolchain) []error {
			ran = append(ran, plan.label)
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected a Go 1.3 plan failure")
	}
	if !strings.Contains(err.Error(), "plan 1.3") {
		t.Fatalf("got error %s, want Go 1.3 plan failure", err)
	}
	if want := []string{"1.4"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
}

func TestRunDownloadPreplansAndContinues(t *testing.T) {
	t.Setenv("GOROOT", "/ambient/go")
	t.Setenv("GOTOOLCHAIN", "auto")
	output := t.TempDir()
	archive := func(version string) goReleaseFile {
		return goReleaseFile{
			Filename: version + ".linux-amd64.tar.gz",
			OS:       "linux",
			Arch:     "amd64",
			Kind:     "archive",
		}
	}
	fetch := func() ([]goRelease, error) {
		for _, label := range []string{"1.5", "1.6"} {
			results, err := readResults(filepath.Join(output, "build-status-"+label+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if results.Results["x64"] != statusPending {
				t.Fatalf("%s was not preplanned: %#v", label, results)
			}
		}
		return []goRelease{
			{Version: "go1.5.4", Stable: true, Files: []goReleaseFile{archive("go1.5.4")}},
			{Version: "go1.6.4", Stable: true, Files: []goReleaseFile{archive("go1.6.4")}},
		}, nil
	}

	var observed struct {
		sync.Mutex
		bootstrapEnvironment []string
		installArguments     []string
		downloads            []string
		ran                  []toolchain
	}
	downloadsStarted := make(chan struct{}, 2)
	downloadsReady := make(chan struct{})
	go func() {
		<-downloadsStarted
		<-downloadsStarted
		close(downloadsReady)
	}()
	execute := func(environment []string, name string, arguments ...string) error {
		observed.Lock()
		if name == "/bootstrap/go" {
			observed.bootstrapEnvironment = append([]string(nil), environment...)
			observed.installArguments = append([]string(nil), arguments...)
			observed.Unlock()
			return nil
		}
		exact := filepath.Base(name)
		observed.downloads = append(observed.downloads, exact)
		observed.Unlock()
		downloadsStarted <- struct{}{}
		select {
		case <-downloadsReady:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("toolchain downloads did not overlap")
		}
		if exact == "go1.5.4" {
			return fmt.Errorf("download failed")
		}
		return nil
	}
	err := runDownload(
		"/bootstrap/go",
		4,
		output,
		[]string{"1.5", "1.6"},
		fetch,
		execute,
		func(plan *plannedToolchain) []error {
			observed.Lock()
			observed.ran = append(observed.ran, plan.toolchain)
			observed.Unlock()
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected a Go 1.5 download failure")
	}
	if !strings.Contains(err.Error(), "1.5: download toolchain: download failed") {
		t.Fatalf("got error %s, want Go 1.5 download failure", err)
	}

	observed.Lock()
	defer observed.Unlock()
	wantInstall := []string{
		"install",
		"golang.org/dl/go1.5.4@latest",
		"golang.org/dl/go1.6.4@latest",
	}
	if !reflect.DeepEqual(observed.installArguments, wantInstall) {
		t.Errorf("got install arguments %q, want %q", observed.installArguments, wantInstall)
	}
	values := make(map[string]string)
	for _, assignment := range observed.bootstrapEnvironment {
		key, value, _ := strings.Cut(assignment, "=")
		values[key] = value
	}
	if values["GOTOOLCHAIN"] != "local" || values["GOBIN"] == "" {
		t.Errorf("bootstrap environment does not select local toolchains and a private GOBIN: %q", values)
	}
	if _, ok := values["GOROOT"]; ok {
		t.Errorf("bootstrap environment retains GOROOT: %q", values["GOROOT"])
	}
	if len(observed.downloads) != 2 {
		t.Errorf("got downloads %q, want both toolchains", observed.downloads)
	}
	if len(observed.ran) != 1 || observed.ran[0].label != "1.6" || filepath.Base(observed.ran[0].command) != "go1.6.4" {
		t.Errorf("ran toolchains %q, want only Go 1.6.4", observed.ran)
	}
}

func TestGoHelpersUseExplicitCommand(t *testing.T) {
	t.Setenv("GOROOT", "/ambient/go")
	t.Setenv("GOTOOLCHAIN", "auto")
	command := filepath.Join(t.TempDir(), "explicit go")
	script := `#!/bin/sh
test "$GOTOOLCHAIN" = local || exit 10
test -z "$GOROOT" || exit 11
case "$1" in
version)
  echo 'go version go1.26.4 linux/amd64'
  ;;
env)
  echo '/explicit/root'
  ;;
build)
  echo 'goid.go:20:6: can inline Get' >&2
  ;;
*)
  exit 12
  ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := resolvedGoVersion(command)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.26.4" {
		t.Errorf("got Go version %q, want 1.26.4", version)
	}
	root, err := commandOutput(toolchainEnvironment(), command, "env", "GOROOT")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(root) != "/explicit/root" {
		t.Errorf("got GOROOT %q, want /explicit/root", strings.TrimSpace(root))
	}
	if err := checkInlining(command, toolchainEnvironment()); err != nil {
		t.Fatal(err)
	}
}

func TestZigCC(t *testing.T) {
	var name string
	var arguments []string
	err := runZigCC(
		func(environment []string, command string, commandArguments ...string) error {
			name = command
			arguments = append([]string(nil), commandArguments...)
			return nil
		},
		"aarch64-linux-gnu",
		[]string{"-o", "output", "race_linux_arm64.syso", "other.o"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if name != "zig" {
		t.Errorf("got command %q, want %q", name, "zig")
	}
	want := []string{
		"cc",
		"-target", "aarch64-linux-gnu",
		"-o", "output",
		"race_linux_arm64.syso.o",
		"other.o",
	}
	if !reflect.DeepEqual(arguments, want) {
		t.Errorf("got arguments %q, want %q", arguments, want)
	}
}

func TestRunCommandForwardsStdin(t *testing.T) {
	const helperEnvironment = "GOID_TEST_RUN_COMMAND_STDIN"
	if os.Getenv(helperEnvironment) == "1" {
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		if string(input) != "input" {
			t.Errorf("got stdin %q, want %q", input, "input")
		}
		return
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = stdin
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := io.WriteString(writer, "input"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv(helperEnvironment, "1")
	if err := runCommand(os.Environ(), os.Args[0], "-test.run=^TestRunCommandForwardsStdin$"); err != nil {
		t.Fatal(err)
	}
}

func TestArchitectureBoundaries(t *testing.T) {
	tests := []struct {
		version      string
		architecture string
		applicable   bool
		race         bool
	}{
		{version: "1.4", architecture: "armv6"},
		{version: "1.5", architecture: "armv6", applicable: true},
		{version: "1.6", architecture: "s390x"},
		{version: "1.7", architecture: "s390x", applicable: true},
		{version: "1.4", architecture: "x64", applicable: true},
		{version: "1.5", architecture: "x64", applicable: true, race: true},
		{version: "1.11", architecture: "aarch64", applicable: true},
		{version: "1.12", architecture: "aarch64", applicable: true, race: true},
		{version: "1.18", architecture: "s390x", applicable: true},
		{version: "1.19", architecture: "s390x", applicable: true, race: true},
		{version: "gccgo-14", architecture: "aarch64"},
		{version: "gccgo-14", architecture: "x64", applicable: true},
	}

	architectureByName := make(map[string]architecture, len(architectures))
	for _, architecture := range architectures {
		architectureByName[architecture.name] = architecture
	}
	for _, test := range tests {
		name := test.version + "/" + test.architecture
		t.Run(name, func(t *testing.T) {
			version, err := parseGoVersion(test.version)
			if err != nil {
				t.Fatal(err)
			}
			architecture, ok := architectureByName[test.architecture]
			if !ok {
				t.Fatalf("unknown architecture %q", test.architecture)
			}
			if got := architecture.applicable(version); got != test.applicable {
				t.Errorf("applicable: got %t, want %t", got, test.applicable)
			}
			if got := architecture.supportsRace(version); got != test.race {
				t.Errorf("race: got %t, want %t", got, test.race)
			}
		})
	}
}

func TestGo13TestBinary(t *testing.T) {
	checkout := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})

	var testArguments []string
	const goCommand = "/sdk/go1.3/bin/go"
	t.Setenv("GOROOT", "/ambient/go")
	t.Setenv("GOTOOLCHAIN", "auto")
	coordinator := coordinator{
		version:   goVersion{minor: 3},
		goCommand: goCommand,
		execute: func(environment []string, name string, arguments ...string) error {
			if name == goCommand && len(arguments) != 0 && arguments[0] == "test" {
				testArguments = append([]string(nil), arguments...)
			}
			if name == goCommand {
				values := make(map[string]string)
				for _, assignment := range environment {
					key, value, _ := strings.Cut(assignment, "=")
					values[key] = value
				}
				if values["GOTOOLCHAIN"] != "local" {
					t.Errorf("got GOTOOLCHAIN=%q, want local", values["GOTOOLCHAIN"])
				}
				if _, ok := values["GOROOT"]; ok {
					t.Errorf("explicit Go command retained GOROOT=%q", values["GOROOT"])
				}
			}
			return nil
		},
	}
	failures := coordinator.runArchitecture(t.TempDir(), architectures[len(architectures)-1], nil, nil)
	if len(failures) != 0 {
		t.Fatalf("got %d failures, want 0", len(failures))
	}
	if want := []string{"test", "-c", "./..."}; !reflect.DeepEqual(testArguments, want) {
		t.Errorf("go test arguments: got %q, want %q", testArguments, want)
	}
}

func TestCoordinatorContinuesAndCheckpoints(t *testing.T) {
	plan, err := newPlan("1.5", 1)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "results.json")
	if err := writeResults(output, plan); err != nil {
		t.Fatal(err)
	}

	type observations struct {
		sync.Mutex
		overlapObserved      bool
		checkpointObserved   bool
		raceBuildObserved    bool
		raceBinaryObserved   bool
		normalBinaryObserved bool
	}
	var observed observations
	armv7Started := make(chan struct{})
	var armv7StartedOnce sync.Once
	execute := func(environment []string, name string, arguments ...string) error {
		var goarch, goarm string
		for _, assignment := range environment {
			if value, ok := strings.CutPrefix(assignment, "GOARCH="); ok {
				goarch = value
			}
			if value, ok := strings.CutPrefix(assignment, "GOARM="); ok {
				goarm = value
			}
		}
		normalBuild := name == "test-go" && reflect.DeepEqual(arguments, []string{"build", "-v", "./..."})
		raceBuild := name == "test-go" && reflect.DeepEqual(arguments, []string{"build", "-v", "-race", "./..."})

		if normalBuild && goarch == "arm" && goarm == "6" {
			select {
			case <-armv7Started:
				observed.Lock()
				observed.overlapObserved = true
				observed.Unlock()
			case <-time.After(5 * time.Second):
				return fmt.Errorf("armv7 build did not start")
			}
			return fmt.Errorf("armv6 build failed")
		}
		if normalBuild && goarch == "arm" && goarm == "7" {
			armv7StartedOnce.Do(func() {
				close(armv7Started)
			})
			deadline := time.Now().Add(5 * time.Second)
			for {
				checkpoint, err := readResults(output)
				if err != nil {
					return fmt.Errorf("read checkpoint: %s", err)
				}
				if checkpoint.Results["armv6"] == statusFailure {
					observed.Lock()
					observed.checkpointObserved = true
					observed.Unlock()
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("armv6 result was not checkpointed")
				}
				time.Sleep(time.Millisecond)
			}
		}
		if normalBuild && goarch == "amd64" {
			return fmt.Errorf("x64 build failed")
		}
		if raceBuild && goarch == "amd64" {
			observed.Lock()
			observed.raceBuildObserved = true
			observed.Unlock()
		}
		if filepath.Base(name) == "goid.race.test" {
			observed.Lock()
			observed.raceBinaryObserved = true
			observed.Unlock()
		}
		if filepath.Base(name) == "goid.test" && filepath.Base(filepath.Dir(name)) == "x64" {
			observed.Lock()
			observed.normalBinaryObserved = true
			observed.Unlock()
		}
		return nil
	}

	coordinator := coordinator{
		version:   goVersion{minor: 5},
		goCommand: "test-go",
		results:   &plan,
		output:    output,
		execute:   execute,
	}
	failures := coordinator.run()
	if len(failures) != 2 {
		t.Fatalf("got %d failures, want 2", len(failures))
	}
	for index, want := range []string{
		"armv6: armv6 build failed",
		"x64: x64 build failed",
	} {
		if got := failures[index].Error(); got != want {
			t.Errorf("failure %d: got %q, want %q", index, got, want)
		}
	}
	for architecture, want := range map[string]resultStatus{
		"armv6":   statusFailure,
		"armv7":   statusSuccess,
		"aarch64": statusSuccess,
		"s390x":   statusNotApplicable,
		"386":     statusSuccess,
		"x64":     statusFailure,
	} {
		if got := plan.Results[architecture]; got != want {
			t.Errorf("%s: got %q, want %q", architecture, got, want)
		}
	}
	observed.Lock()
	defer observed.Unlock()
	if !observed.overlapObserved {
		t.Error("armv6 and armv7 builds did not overlap")
	}
	if !observed.checkpointObserved {
		t.Error("armv6 result was not checkpointed while armv7 was running")
	}
	if !observed.raceBuildObserved || !observed.raceBinaryObserved {
		t.Error("race build did not complete after the normal x64 build failed")
	}
	if observed.normalBinaryObserved {
		t.Error("normal x64 binary ran after its build failed")
	}
}
