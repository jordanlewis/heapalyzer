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

/*
func (a *analyzer) sizeOfSlice(curName string, v api.Variable) uintptr {
	switch v.RealType {
	case "[]uint", "[]int", "[]uint64", "[]int64", "[]float64":
		return uintptr(8 * v.Cap)
	case "[]uint32", "[]int32", "[]float32":
		return uintptr(4 * v.Cap)
	case "[]uint16", "[]int16":
		return uintptr(2 * v.Cap)
	case "[]uint8", "[]int8":
		return uintptr(1 * v.Cap)
	}

	// We have an array of structs! We need to load them all and traverse them
	// all.
	innerV, err := a.client.EvalVariable(api.EvalScope{
		GoroutineID: a.g.ID,
		Frame:       a.frameIdx,
	}, curName, api.LoadConfig{
		FollowPointers:     true,
		MaxVariableRecurse: 0,
		MaxStringLen:       0,
		MaxArrayValues:     10000,
		MaxStructFields:    -1,
	})
	if err != nil {
		fmt.Println(curName)
		panic(err)
	}

	var ret uintptr
	for i := range innerV.Children {
		fmt.Println("hi", innerV.Children[i].Name)
		ret += a.sizeOfVar(fmt.Sprintf("%s.%s", curName, innerV.Children[i].Name), innerV.Children[i])
	}
	return ret
}

func (a *analyzer) sizeOfVar(curName string, v api.Variable) uintptr {
	fmt.Println(curName)
	switch v.Kind {
	case reflect.Bool:
		return unsafe.Sizeof(true)
	case reflect.Int:
		return unsafe.Sizeof(int(3))
	case reflect.Int8:
		return unsafe.Sizeof(int8(3))
	case reflect.Int16:
		return unsafe.Sizeof(int16(3))
	case reflect.Int32:
		return unsafe.Sizeof(int32(3))
	case reflect.Int64:
		return unsafe.Sizeof(int64(3))
	case reflect.Uint:
		return unsafe.Sizeof(uint(3))
	case reflect.Uint8:
		return unsafe.Sizeof(uint8(3))
	case reflect.Uint16:
		return unsafe.Sizeof(uint16(3))
	case reflect.Uint32:
		return unsafe.Sizeof(uint32(3))
	case reflect.Uint64:
		return unsafe.Sizeof(uint64(3))
	case reflect.Uintptr:
		return unsafe.Sizeof(uintptr(3))
	case reflect.Float32:
		return unsafe.Sizeof(float32(3))
	case reflect.Float64:
		return unsafe.Sizeof(float64(3))
	case reflect.Complex64:
		return unsafe.Sizeof(complex64(3))
	case reflect.Complex128:
		return unsafe.Sizeof(complex128(3))
	case reflect.Array:
		return a.sizeOfSlice(curName, v)
	case reflect.Chan:
		return unsafe.Sizeof(make(chan int, 0))
	case reflect.Func:
		return unsafe.Sizeof(func() {})
	case reflect.Interface:
		return unsafe.Sizeof(interface{}(nil))
	case reflect.Map:
		return unsafe.Sizeof(make(map[int]int, 0))
	case reflect.Ptr:
		return unsafe.Sizeof(&struct{}{})
	case reflect.Slice:
		return unsafe.Sizeof(make([]int, 0))
	case reflect.String:
		if len(v.Children) > 0 {
			panic("oh no")
		}
		if v.Len > 1024*1024*1024*8 {
			fmt.Println("Ignoring malformed 8gb terribly broken string")
			fmt.Println(a.frame.File, a.frame.Line)
			fmt.Println("!!!!!!!!!")
			fmt.Printf("%+v\n", v)
			fmt.Println(humanize.Bytes(uint64(v.Len)))
			return 0
		}
		return uintptr(v.Len)
	case reflect.Struct:
		var ret uintptr
		// How to calculate size of struct?
		// Let's put our ear to the ground and find out :)
		fmt.Println("Hi, i'm a struct", v.Name, v.Type)
		for i := range v.Children {
			fmt.Printf("%+v\n", v.Children[i])
		}
		for i := range v.Children {
			ret += a.sizeOfVar(fmt.Sprintf("%s.%s", curName, v.Children[i].Name), v.Children[i])
		}
		return ret
	case reflect.UnsafePointer:
		return unsafe.Sizeof(unsafe.Pointer(nil))
	case reflect.Invalid:
		// ???
		return 0
	}
	panic("unhandled case " + v.Kind.String())
}
*/
