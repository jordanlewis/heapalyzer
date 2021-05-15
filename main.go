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
	"flag"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/rpc2"
)

var debug = flag.Bool("debug", false, "debug")

func main() {
	flag.Parse()
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

	varDepth int

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

var primitiveTypes = map[string]struct{}{}

func init() {
	for _, s := range []string{
		"int", "int64", "int32", "int16", "int8",
		"uint", "uint64", "uint32", "uint16", "uint8",
		"float64", "float32",
	} {
		primitiveTypes[s] = struct{}{}
	}
}

// processVar recurses down a variable, processing it and any children.
func (a *analyzer) processVar(curName string, v api.Variable) {
	a.varDepth++
	defer func() { a.varDepth-- }()
	if *debug {
		fmt.Printf("%sprocessvar %s %s %s %s\n", strings.Repeat("  ", a.varDepth), curName, v.Kind, v.Name, v.Type)
	}
	if strings.HasPrefix(v.RealType, "runtime.") {
		return
	}
	if strings.HasPrefix(v.RealType, "*sudog<") || strings.HasPrefix(v.RealType, "*waitq<") {
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

	switch v.Kind {
	case reflect.Slice:
		if v.ArrayChild != nil {
			a.processVar(curName, *v.ArrayChild)
		}
	case reflect.Ptr:
		//fmt.Println("following pointer!", v.Name, v.Type)
		innerV, err := a.client.EvalVariable(api.EvalScope{
			GoroutineID: a.g.ID,
			Frame:       a.frameIdx,
		}, curName, api.LoadConfig{FollowPointers: true, MaxStructFields: -1})
		if err != nil {
			fmt.Println("ptr error", curName, err)
			return
		}
		if len(innerV.Children) > 0 {
			a.processVar(curName, innerV.Children[0])
		}
	case reflect.Struct:
		if len(v.Children) != int(v.Len) {
			// We need to Eval the struct.
			v2, err := a.client.EvalVariable(api.EvalScope{
				GoroutineID: a.g.ID,
				Frame:       a.frameIdx,
			}, curName, api.LoadConfig{FollowPointers: true, MaxStructFields: -1})
			if err != nil {
				fmt.Println("struct error", curName, err)
				return
			}
			v = *v2
		}
		//fmt.Println("Following ", v.Kind.String(), v.Name, v.Type, len(v.Children))
		for _, c := range v.Children {
			//fmt.Println("Child", c.Name, c.Type)
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", c.RealType, c.Addr), c)
		}
	case reflect.Array:
		//fmt.Printf("Evaluating array of type %+v\n", v)
		if _, ok := primitiveTypes[v.ElemType]; ok {
			// No need to recurse into primitive type arrays
			return
		}
		v2, err := a.client.EvalVariable(api.EvalScope{
			GoroutineID: a.g.ID,
			Frame:       a.frameIdx,
		}, curName, api.LoadConfig{FollowPointers: true, MaxStructFields: -1, MaxArrayValues: 100000})
		if err != nil {
			fmt.Println("error dereferencing array", curName, err)
			return
		}
		for _, c := range v2.Children {
			//fmt.Println("Child", c.Name, c.Type)

			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", v.ElemType, c.Addr), c)
		}
	case reflect.Map:
		v2, err := a.client.EvalVariable(api.EvalScope{
			GoroutineID: a.g.ID,
			Frame:       a.frameIdx,
		}, curName, api.LoadConfig{FollowPointers: true, MaxStructFields: -1, MaxArrayValues: 100000})
		if err != nil {
			fmt.Println("error dereffing map", curName, err)
			return
		}
		for i := 0; i < len(v2.Children)/2; i++ {
			key := v2.Children[i*2]
			val := v2.Children[i*2+1]
			//fmt.Println("processing parent")
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", v.KeyType, key.Addr), key)
			//fmt.Println("processing child")
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", v.ElemType, val.Addr), val)
		}

	case reflect.Chan:
		//fmt.Printf("Evaluating chan of type %+v\n", v)
		v2, err := a.client.EvalVariable(api.EvalScope{
			GoroutineID: a.g.ID,
			Frame:       a.frameIdx,
		}, curName, api.LoadConfig{FollowPointers: true, MaxStructFields: -1, MaxArrayValues: 100000})
		if err != nil {
			fmt.Println("chan error", curName, err)
			return
		}
		for _, c := range v2.Children {
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", v.ElemType, c.Addr), c)
		}
	default:
		for _, c := range v.Children {
			a.processVar(fmt.Sprintf("%s.%s", curName, c.Name), c)
		}
	}
}
