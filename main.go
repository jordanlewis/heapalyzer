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
	"math"
	"os"
	"reflect"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"

	"github.com/dustin/go-humanize"
	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
	"github.com/go-delve/delve/service/rpc2"
)

var debug = flag.Bool("debug", false, "debug")
var cpuprofile = flag.String("cpuprofile", "", "write cpu prof")

func main() {
	flag.Parse()
	bin := flag.Arg(0)
	core := flag.Arg(1)
	d, err := debugger.New(&debugger.Config{
		CoreFile: core,
	}, []string{bin})
	if err != nil {
		panic(err)
	}

	var analyzer = analyzer{
		debugger: d,
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			panic(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			panic(err)
		}
	}
	if err := analyzer.analyze(); err != nil {
		panic(err)
	}
	if *cpuprofile != "" {
		pprof.StopCPUProfile()
	}
}

type Var struct {
	api.Variable
	isRoot    bool
	backedges []Var
}

type objCounts struct {
	count int
	size  int64
}

type analyzer struct {
	client *rpc2.RPCClient

	mu struct {
		sync.RWMutex

		// Map from object pointer to whether we've seen it or not
		seen map[uint64]bool
		// Map from string "type" to counts
		objectMap map[godwarf.Type]*objCounts
	}

	varDepth int

	frameIdx int
	frame    *proc.Stackframe
	g        *proc.G
	debugger *debugger.Debugger
}

func (a *analyzer) analyze() error {
	a.mu.seen = make(map[uint64]bool)
	a.mu.objectMap = make(map[godwarf.Type]*objCounts)

	gs, _, err := a.debugger.Goroutines(0, 100000)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.ID != 3676 {
			continue
		}
		trace, err := a.debugger.Stacktrace(g.ID, 1000, 0)
		if err != nil {
			panic(err)
		}
		fmt.Printf("goroutine %d: %d frames from %s\n", g.ID, len(trace), g.CurrentLoc.File)
		a.g = g
		for i, frame := range trace {
			scope := proc.FrameToScope(a.debugger.Target().BinInfo(), a.debugger.Target().Memory(), g, frame)
			locals, err := scope.Locals()
			if err != nil {
				fmt.Println("error loading locals", err)
				continue
			}
			a.frameIdx = i
			a.frame = &frame
			for _, v := range locals {
				a.processVar(v.Name, v)
			}
		}
	}
	fmt.Println("COUNTS PER TYPE")
	fmt.Println("---------------")
	fmt.Println()

	type objectSorters struct {
		typ godwarf.Type
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
func (a *analyzer) processVar(curName string, v *proc.Variable) {
	a.varDepth++
	defer func() { a.varDepth-- }()
	if *debug {
		fmt.Printf("%sprocessvar %s %s %s %s\n", strings.Repeat("  ", a.varDepth), curName, v.Kind, v.Name, v.RealType)
	}
	if strings.HasPrefix(v.DwarfType.String(), "runtime.") {
		return
	}
	if strings.HasPrefix(v.DwarfType.String(), "*sudog<") || strings.HasPrefix(v.DwarfType.String(), "*waitq<") {
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
	counts.size += v.RealType.Size()
	a.mu.seen[v.Addr] = true

	// Scalars.
	switch v.Kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return
	case reflect.Float64, reflect.Float32, reflect.Bool, reflect.Complex128, reflect.Complex64:
		return
	}

	//a.mu.Unlock()

	v.Load(proc.LoadConfig{
		FollowPointers:     false,
		MaxVariableRecurse: 0,
		MaxStringLen:       0,
		MaxArrayValues:     1000,
		MaxStructFields:    math.MaxInt64,
		MaxMapBuckets:      0,
	})

	if v.Unreadable != nil {
		if *debug {
			fmt.Printf("unreadable var %s %s: %+v\n", curName, v.Unreadable.Error(), v)
		}
		return
	}

	switch v.Kind {
	case reflect.Slice, reflect.Array:
		// Check if we've seen this array memory already. If not, recurse.
		if a.mu.seen[v.Base] {
			return
		}
		a.mu.seen[v.Base] = true
		var elemType godwarf.Type
		switch t := v.RealType.(type) {
		case *godwarf.SliceType:
			elemType = t.ElemType
		case *godwarf.ArrayType:
			elemType = t.Type
		}
		if _, ok := primitiveTypes[elemType.String()]; ok {
			counts.size += elemType.Size() * v.Cap
			// No need to recurse into primitive type arrays
			return
		}
		for _, c := range v.Children {
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", v.RealType.String(), c.Addr), &c)
		}
	case reflect.Ptr, reflect.UnsafePointer, reflect.Interface:
		a.processVar(curName, &v.Children[0])
	case reflect.Struct:
		if len(v.Children) != int(v.Len) {
			panic(":(")
		}
		for _, c := range v.Children {
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", c.RealType, c.Addr), &c)
		}
	case reflect.Map:
		mtyp := v.DwarfType.(*godwarf.MapType)
		for i := 0; i < len(v.Children)/2; i++ {
			key := v.Children[i*2]
			val := v.Children[i*2+1]
			//fmt.Println("processing parent")
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", mtyp.KeyType, key.Addr), &key)
			//fmt.Println("processing child")
			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", mtyp.ElemType, val.Addr), &val)
		}

	case reflect.Chan:
		mtyp := v.DwarfType.(*godwarf.ChanType)
		for _, c := range v.Children {

			a.processVar(fmt.Sprintf("*(*\"%s\")(%d)", mtyp.ElemType, c.Addr), &c)
		}
	default:
		if len(v.Children) > 0 {
			fmt.Println("hmmmmmmmm", v.Children, v.Name, v.TypeString(), v.Kind)
		}
	}
}
