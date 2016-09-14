// Assembly to mimic runtime.getg.
// This should work on arm64 as well, but it hasn't been tested.

// +build arm
// +build go1.5

#include "textflag.h"

// func getg() uintptr
TEXT ·getg(SB),NOSPLIT,$0-8
	MOVW g, ret+0(FP)
	RET
