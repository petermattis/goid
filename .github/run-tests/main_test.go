package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestPlanCrossCompiler(t *testing.T) {
	for _, test := range []struct {
		version string
		want    string
	}{
		{version: "1.11"},
		{version: "1.12", want: "zig"},
		{version: "gccgo-14"},
	} {
		t.Run(test.version, func(t *testing.T) {
			plan, err := newPlan(test.version, 1)
			if err != nil {
				t.Fatal(err)
			}
			if plan.CrossCompiler != test.want {
				t.Errorf("got cross compiler %q, want %q", plan.CrossCompiler, test.want)
			}
		})
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
	coordinator := coordinator{
		version: goVersion{minor: 3},
		execute: func(environment []string, name string, arguments ...string) error {
			if name == "go" && len(arguments) != 0 && arguments[0] == "test" {
				testArguments = append([]string(nil), arguments...)
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

	checkpointObserved := false
	raceBuildObserved := false
	raceBinaryObserved := false
	normalBinaryObserved := false
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
		normalBuild := name == "go" && reflect.DeepEqual(arguments, []string{"build", "-v", "./..."})
		raceBuild := name == "go" && reflect.DeepEqual(arguments, []string{"build", "-v", "-race", "./..."})

		if normalBuild && goarch == "arm" && goarm == "6" {
			return fmt.Errorf("armv6 build failed")
		}
		if normalBuild && goarch == "arm" && goarm == "7" {
			checkpoint, err := readResults(output)
			if err != nil {
				t.Fatal(err)
			}
			if got := checkpoint.Results["armv6"]; got != statusFailure {
				t.Fatalf("armv6 checkpoint: got %q, want %q", got, statusFailure)
			}
			checkpointObserved = true
		}
		if normalBuild && goarch == "amd64" {
			return fmt.Errorf("x64 build failed")
		}
		if raceBuild && goarch == "amd64" {
			raceBuildObserved = true
		}
		if filepath.Base(name) == "goid.race.test" {
			raceBinaryObserved = true
		}
		if filepath.Base(name) == "goid.test" && filepath.Base(filepath.Dir(name)) == "x64" {
			normalBinaryObserved = true
		}
		return nil
	}

	coordinator := coordinator{
		version: goVersion{minor: 5},
		results: &plan,
		output:  output,
		execute: execute,
	}
	failures := coordinator.run()
	if len(failures) != 2 {
		t.Fatalf("got %d failures, want 2", len(failures))
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
	if !checkpointObserved {
		t.Error("armv6 result was not checkpointed before armv7 started")
	}
	if !raceBuildObserved || !raceBinaryObserved {
		t.Error("race build did not complete after the normal x64 build failed")
	}
	if normalBinaryObserved {
		t.Error("normal x64 binary ran after its build failed")
	}
}
