package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlan(t *testing.T) {
	tests := map[string][]string{
		"1.3":      {"x64"},
		"1.5":      {"armv6", "armv7", "aarch64", "386", "x64"},
		"1.7":      {"armv6", "armv7", "aarch64", "s390x", "386", "x64"},
		"gccgo-14": {"x64"},
	}
	for label, expected := range tests {
		plan, err := newPlan(1, []string{label})
		if err != nil {
			t.Fatal(err)
		}
		statuses := plan.Results[label]
		if len(statuses) != len(architectures) {
			t.Fatalf("%s: got %d architectures, want %d", label, len(statuses), len(architectures))
		}
		var actual []string
		for _, architecture := range architectures {
			if statuses[architecture.name] == pending {
				actual = append(actual, architecture.name)
			}
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s: got %v, want %v", label, actual, expected)
		}
	}
}

func TestSelectWrapper(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"go1.20.9", "go1.20.12", "go1.20.13rc1"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := selectWrapper(directory, "1.20")
	if err != nil {
		t.Fatal(err)
	}
	if actual != "go1.20.12" {
		t.Errorf("got %q, want go1.20.12", actual)
	}
}
