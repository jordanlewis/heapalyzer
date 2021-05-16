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
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/pkg/proc/core"
	"github.com/go-delve/delve/service/api"
	"github.com/go-delve/delve/service/debugger"
	"github.com/go-delve/delve/service/rpc2"
)

var debug = flag.Bool("debug", false, "debug")
var cpuprofile = flag.String("cpuprofile", "", "write cpu prof")
var goroutineid = flag.Int("goroutine", -1, "only analyze this goroutine id")

func main() {
	flag.Parse()
	bin := flag.Arg(0)
	coreFile := flag.Arg(1)
	d, err := debugger.New(&debugger.Config{
		CoreFile: coreFile,
	}, []string{bin})
	if err != nil {
		panic(err)
	}

	core, err := core.OpenCore(coreFile, bin, nil)
	if err != nil {
		panic(err)
	}
	scope, err := proc.ThreadScope(core.CurrentThread())
	if err != nil {
		panic(err)
	}
	scope.BinInfo.
	pkgvars := make([]packageVar, len(scope.BinInfo.packageVars))
	pv, err := scope.PackageVariables(proc.LoadConfig{})

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
	count        int
	size         int64
	retainedSize int64
}

type analyzer struct {
	client *rpc2.RPCClient

	mu struct {
		sync.RWMutex

		// Map from object pointer to whether we've seen it or not
		seen map[uint64]bool
		// Map from string "type" to counts
		objectMap map[string]*objCounts

		hitCount int
		varCount int
	}

	varDepth int

	frameIdx int
	frame    *proc.Stackframe
	g        *proc.G
	debugger *debugger.Debugger
}

func (a *analyzer) analyze() error {
	a.mu.seen = make(map[uint64]bool)
	a.mu.objectMap = make(map[string]*objCounts)

	start := time.Now()
	packageVars, err := a.debugger.PackageVariables("", proc.LoadConfig{})
	if err != nil {
		return err
	}
	for _, v := range packageVars {
		a.processVar(v)
	}
	fmt.Println("processed package vars in", time.Since(start))
	start = time.Now()

	gs, _, err := a.debugger.Goroutines(0, 100000)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if *goroutineid != -1 && g.ID != *goroutineid {
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
				a.processVar(v)
			}
		}
	}
	fmt.Println("processed goroutines in", time.Since(start))
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
	sort.Slice(objects, func(i, j int) bool { return objects[i].retainedSize < objects[j].retainedSize })
	var total int64
	for _, o := range objects {
		fmt.Printf("%5d (%5s) (%5s) -> %s\n", o.count,
			humanize.Bytes(uint64(o.size)),
			humanize.Bytes(uint64(o.retainedSize)),
			o.typ)
		total += o.size
	}

	fmt.Println("Total size", humanize.Bytes(uint64(total)))
	fmt.Printf("processed %d objects, %d dupes\n", a.mu.varCount, a.mu.hitCount)
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
// It returns the retained size of the variable, which is the sum of its own
// size plus its children's size, recursively.
func (a *analyzer) processVar(v *proc.Variable) int64 {
	a.mu.varCount++
	a.varDepth++
	defer func() { a.varDepth-- }()
	if *debug {
		fmt.Printf("%sprocessvar %s %s %s\n", strings.Repeat(" ", a.varDepth), v.Kind, v.Name, v.RealType)
	}
	s := v.DwarfType.String()
	if strings.HasPrefix(s, "runtime.") || strings.HasPrefix(s, "*runtime.") {
		return 0
	}
	if strings.HasPrefix(s, "*sudog<") || strings.HasPrefix(s, "*waitq<") {
		return 0
	}
	// We've already seen this particular var.
	//a.mu.RLock()
	if a.mu.seen[v.Addr] {
		a.mu.hitCount++
		//a.mu.RUnlock()
		return 0
	}
	//a.mu.RUnlock()
	//a.mu.Lock()
	counts := a.mu.objectMap[v.TypeString()]
	if counts == nil {
		counts = &objCounts{}
		a.mu.objectMap[v.TypeString()] = counts
	}
	counts.count += 1
	size := v.RealType.Size()
	if *debug {
		fmt.Printf("%ssize: %d\n", strings.Repeat(" ", a.varDepth), size)
	}
	if size != math.MaxInt64 {
		// Not sure why this happens sometimes?
		counts.size += size
	}
	a.mu.seen[v.Addr] = true

	// Scalars.
	switch v.Kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return size
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return size
	case reflect.Float64, reflect.Float32, reflect.Bool, reflect.Complex128, reflect.Complex64:
		return size
	}

	retainedSize := a.getRetainedSize(v)
	counts.retainedSize = retainedSize
	return size + retainedSize
}

func (a *analyzer) getRetainedSize(v *proc.Variable) int64 {
	//a.mu.Unlock()
	var size int64

	v.Load(proc.LoadConfig{
		FollowPointers:     false,
		MaxVariableRecurse: 0,
		MaxStringLen:       0,
		// We'll load array values if we need to recurse into them down below.
		MaxArrayValues:  0,
		MaxStructFields: math.MaxInt64,
		MaxMapBuckets:   0,
	})

	if v.Unreadable != nil {
		if *debug {
			fmt.Printf("unreadable var %s: %+v\n", v.Unreadable.Error(), v)
		}
		return 0
	}

	switch v.Kind {
	case reflect.Array:
		t := v.RealType.(*godwarf.ArrayType)
		elemType := t.Type
		// Arrays already have their elements counted, so all we have to do
		// is recurse inwards.
		if _, ok := primitiveTypes[elemType.String()]; !ok {
			v.Load(proc.LoadConfig{
				MaxArrayValues: math.MaxInt64,
			})
			for _, c := range v.Children {
				size += a.processVar(&c)
			}
		}
	case reflect.Slice:
		// Check if we've seen this array memory already. If not, recurse.
		if a.mu.seen[v.Base] {
			a.mu.hitCount++
			return 0
		}
		a.mu.seen[v.Base] = true
		styp := v.RealType.(*godwarf.SliceType)
		elemType := styp.ElemType
		elemCount := v.Cap
		if _, ok := primitiveTypes[elemType.String()]; ok {
			elemSize := elemType.Size()
			size += elemSize * elemCount
			if *debug {
				fmt.Printf("slice has size %s (%d * %s)\n",
					humanize.Bytes(uint64(elemSize*elemCount)),
					elemCount, humanize.Bytes(uint64(elemSize)))
			}
			// No need to recurse into primitive type arrays
		} else {
			v.Load(proc.LoadConfig{
				MaxArrayValues: math.MaxInt64,
			})
			for _, c := range v.Children {
				size += a.processVar(&c)
			}
		}
	case reflect.Ptr, reflect.UnsafePointer, reflect.Interface:
		size += a.processVar(&v.Children[0])
	case reflect.Struct:
		if len(v.Children) != int(v.Len) {
			panic(":(")
		}
		for _, c := range v.Children {
			size += a.processVar(&c)
		}
		return size
	case reflect.Map:
		v.Load(proc.LoadConfig{
			MaxArrayValues: math.MaxInt64,
		})
		for i := 0; i < len(v.Children)/2; i++ {
			key := v.Children[i*2]
			val := v.Children[i*2+1]
			//fmt.Println("processing parent")
			size += a.processVar(&key)
			//fmt.Println("processing child")
			size += a.processVar(&val)
		}

	case reflect.Chan:
		for _, c := range v.Children {
			size += a.processVar(&c)
		}
	default:
		if len(v.Children) > 0 {
			fmt.Println("hmmmmmmmm", v.Children, v.Name, v.TypeString(), v.Kind)
		}
	}
	return size
}
