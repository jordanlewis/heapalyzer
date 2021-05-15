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

var testVar = t{
	p:  &p{},
	ps: []p{p{}, p{}},
}

type ll struct {
	val  int
	next *ll
}

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
	smap    map[p]ll
	ll      ll
	llPtr   *ll
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
		pmap:    map[int]p{1: {}, 2: {}},
		ptrmap:  map[int]*p{1: {blah: 3}, 2: {blah: 4}},
		ll:      ll{val: 0, next: &ll{val: 1, next: &ll{val: 2, next: &ll{val: 3}}}},
		llPtr:   &ll{val: 0, next: &ll{val: 1, next: &ll{val: 2, next: &ll{val: 3}}}},
		smap:    map[p]ll{{blah: 0}: {}, {blah: 10}: {}},
	}
	t.ints2 = t.ints[:]

	panic("ohno")
	fmt.Println(t)
}
