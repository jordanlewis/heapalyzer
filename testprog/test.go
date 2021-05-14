// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import "fmt"

type p struct {
	blah int
}

type t struct {
	a       int
	b       int
	p       *p
	p2      p
	ints    []int
	ints2   []int
	intsarr [100]int
	ps      []p
	pmap    map[int]p
	ptrmap  map[int]*p
}

func main() {
	t := t{
		a:       0,
		b:       0,
		p:       &p{},
		p2:      p{},
		ints:    []int{1, 2, 3, 4},
		intsarr: [100]int{},
		ps:      []p{{}, {}},
		pmap:    nil,
		ptrmap:  nil,
	}
	t.ints2 = t.ints[:]

	panic("ohno")
	fmt.Println(t)
}
