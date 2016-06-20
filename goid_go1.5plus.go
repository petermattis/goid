// +build go1.5

package goid

import (
	"runtime"
	"strings"
	"unsafe"
)

// Just enough of the structs from runtime2.go to get the offset to goid.

type stack struct {
	lo uintptr
	hi uintptr
}

type gobuf struct {
	sp   uintptr
	pc   uintptr
	g    uintptr
	ctxt uintptr
	ret  uint64
	lr   uintptr
	bp   uintptr
}

type g15 struct {
	stack       stack
	stackguard0 uintptr
	stackguard1 uintptr

	_panic       uintptr
	_defer       uintptr
	m            uintptr
	stackAlloc   uintptr
	sched        gobuf
	syscallsp    uintptr
	syscallpc    uintptr
	stkbar       []uintptr
	stkbarPos    uintptr
	param        unsafe.Pointer
	atomicstatus uint32
	stackLock    uint32
	goid         int64 // Here it is!
}

type g16plus struct {
	stack       stack
	stackguard0 uintptr
	stackguard1 uintptr

	_panic       uintptr
	_defer       uintptr
	m            uintptr
	stackAlloc   uintptr
	sched        gobuf
	syscallsp    uintptr
	syscallpc    uintptr
	stkbar       []uintptr
	stkbarPos    uintptr
	stktopsp     uintptr
	param        unsafe.Pointer
	atomicstatus uint32
	stackLock    uint32
	goid         int64 // Here it is!
}

// Backdoor access to runtime·getg().
func getg() uintptr // in goid.s

// The goid is in the G struct, which varies from version to
// version. See runtime.h from the Go sources.

func get15() int64 {
	var gg *g15
	gg = (*g15)(unsafe.Pointer(getg()))
	return gg.goid
}

func get16plus() int64 {
	var gg *g16plus
	gg = (*g16plus)(unsafe.Pointer(getg()))
	return gg.goid
}

// Get returns the id of the current goroutine.
var Get func() int64

func init() {
	Get = func() int64 { return 0 }

	v := runtime.Version()
	if strings.HasPrefix(v, "go1.") {
		switch v[4] {
		case '5':
			Get = get15
		case '6', '7':
			// Tested on go1.6.2, go1.7beta2
			Get = get16plus
		}
	}
}
