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

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/rpc2"
)

func main() {
	// Create and start a terminal - attach to running instance
	client := rpc2.NewClient("localhost:7070")
	defer func(client *rpc2.RPCClient, cont bool) {
		if err := client.Disconnect(cont); err != nil {
			panic(err)
		}
	}(client, false)

	var analyzer = analyzer{
		client: client,
	}

	if err := analyzer.analyze(); err != nil {
		panic(err)
	}
}

type Var struct {
	api.Variable
	isRoot    bool
	backedges []Var
}

type objCounts struct {
	count int
	size  uint64
}

type analyzer struct {
	client *rpc2.RPCClient

	mu struct {
		sync.RWMutex

		// Map from object pointer to whether we've seen it or not
		seen map[uint64]bool
		// Map from string "type" to counts
		objectMap map[string]*objCounts
	}

	frameIdx int
	frame    *api.Stackframe
	g        *api.Goroutine
}

func (a *analyzer) analyze() error {
	a.mu.seen = make(map[uint64]bool)
	a.mu.objectMap = make(map[string]*objCounts)

	gs, _, err := a.client.ListGoroutines(0, 100000)
	if err != nil {
		return err
	}
	for _, g := range gs {
		trace, err := a.client.Stacktrace(g.ID, 1000, 0, &api.LoadConfig{
			FollowPointers:     true,
			MaxVariableRecurse: 0,
			MaxStringLen:       0,
			MaxArrayValues:     1,
			MaxStructFields:    -1,
		})
		if err != nil {
			panic(err)
		}
		fmt.Printf("goroutine %d: %d frames from %s\n", g.ID, len(trace), g.CurrentLoc.File)
		a.g = g
		for i, frame := range trace {
			a.frameIdx = i
			a.frame = &frame
			locals := frame.Locals
			locals = append(frame.Locals, frame.Arguments...)
			for _, v := range locals {
				a.processVar(v.Name, v)
			}
		}
	}
	fmt.Println("COUNTS PER TYPE")
	fmt.Println("---------------")
	fmt.Println()

	type objectSorters struct {
		typ string
		objCounts
	}
	objects := make([]objectSorters, len(a.mu.objectMap))
	i := 0
	for t, c := range a.mu.objectMap {
		objects[i] = objectSorters{
			typ:       t,
			objCounts: *c,
		}
		i++
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].size < objects[j].size })
	for _, o := range objects {
		fmt.Printf("%5d (%5s) -> %s\n", o.count, humanize.Bytes(uint64(o.size)), o.typ)
	}
	return nil
}

// processVar recurses down a variable, processing it and any children.
func (a *analyzer) processVar(curName string, v api.Variable) {
	if strings.HasPrefix(v.RealType, "runtime.") {
		return
	}
	// We've already seen this particular var.
	//a.mu.RLock()
	if a.mu.seen[v.Addr] {
		//a.mu.RUnlock()
		return
	}
	//a.mu.RUnlock()
	//a.mu.Lock()
	counts := a.mu.objectMap[v.RealType]
	if counts == nil {
		counts = &objCounts{}
		a.mu.objectMap[v.RealType] = counts
	}
	counts.count += 1
	counts.size += v.Size
	a.mu.seen[v.Addr] = true
	//a.mu.Unlock()

	if v.Kind == reflect.Slice {
		a.processVar(curName, *v.ArrayChild)
	}

	for _, c := range v.Children {
		a.processVar(fmt.Sprintf("%s.%s", curName, c.Name), c)
	}
}
