// Copyright 2015 Peter Mattis.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.

package goid

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"testing"
)

// Parse the goid from runtime.Stack() output. Slow, but it works.
var goroutineRE = regexp.MustCompile(`^goroutine\s+(\d+)\s+.*`)

func getSlow() int64 {
	var buf [1024]byte
	s := buf[0:runtime.Stack(buf[:], false)]
	m := goroutineRE.FindSubmatch(s)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseInt(string(m[1]), 10, 64)
	return v
}

func TestGet(t *testing.T) {
	ch := make(chan *string, 100)
	for i := 0; i < cap(ch); i++ {
		go func(i int) {
			goid := Get()
			expected := getSlow()
			if goid == expected {
				ch <- nil
				return
			}
			s := fmt.Sprintf("Expected %d, but got %d", expected, goid)
			ch <- &s
		}(i)
	}

	for i := 0; i < cap(ch); i++ {
		val := <-ch
		if val != nil {
			t.Fatal(*val)
		}
	}
}
