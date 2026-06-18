package main

import (
	"fmt"
	"regexp"
	"syscall"
	"unsafe"
)

func expectedFailures() (failureMatchers, error) {
	canMap, err := canMapS390xTSANMeta()
	if err != nil {
		return nil, err
	}
	if canMap {
		return nil, nil
	}

	// Race detector binaries crash under QEMU on hosts that cannot map the
	// ThreadSanitizer metadata address. See
	// https://github.com/golang/go/issues/67881.
	return failureMatchers{
		"goid.race.test": regexp.MustCompile(
			`^==[0-9]+==ERROR: ThreadSanitizer failed to allocate 0x[0-9a-f]+ \([0-9]+\) bytes at address [0-9a-f]+ \(errno: 12\)$`,
		),
	}, nil
}

func canMapS390xTSANMeta() (bool, error) {
	// QEMU linux-user maps fixed guest addresses into the host address space.
	// Probe Go's current s390x metadata base to distinguish hosts with 47-bit
	// user address spaces.
	const (
		metaShadowAddress = 0x900000000000
		mappingSize       = 64 << 10
		// MAP_FIXED_NOREPLACE was added after the oldest Go version tested here.
		mapFixedNoReplace = 0x100000
	)
	mmapArguments := [6]uintptr{
		metaShadowAddress,
		mappingSize,
		syscall.PROT_READ | syscall.PROT_WRITE,
		syscall.MAP_PRIVATE | syscall.MAP_ANON | syscall.MAP_NORESERVE | mapFixedNoReplace,
		^uintptr(0),
		0,
	}
	address, _, err := syscall.Syscall(
		syscall.SYS_MMAP,
		uintptr(unsafe.Pointer(&mmapArguments[0])),
		0,
		0,
	)
	if err == syscall.ENOMEM {
		return false, nil
	}
	if err != 0 {
		return false, fmt.Errorf("probe ThreadSanitizer metadata mapping: %s", err)
	}

	_, _, err = syscall.Syscall(syscall.SYS_MUNMAP, address, mappingSize, 0)
	if err != 0 {
		return false, fmt.Errorf("unmap ThreadSanitizer metadata probe: %s", err)
	}
	if address != metaShadowAddress {
		return false, fmt.Errorf(
			"probe ThreadSanitizer metadata mapping: got address %#x, want %#x",
			address,
			metaShadowAddress,
		)
	}
	return true, nil
}
