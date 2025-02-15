// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"fmt"
	"internal/buildcfg"
	"os"
	"sort"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
)

// cmpstackvarlt reports whether the stack variable a sorts before b.
//
// Sort the list of stack variables. Autos after anything else,
// within autos, unused after used, within used, things with
// pointers first, zeroed things first, and then decreasing size.
// Because autos are laid out in decreasing addresses
// on the stack, pointers first, zeroed things first and decreasing size
// really means, in memory, things with pointers needing zeroing at
// the top of the stack and increasing in size.
// Non-autos sort on offset.
func cmpstackvarlt(a, b *ir.Name) bool {
	if needAlloc(a) != needAlloc(b) {
		return needAlloc(b)
	}

	if !needAlloc(a) {
		return a.FrameOffset() < b.FrameOffset()
	}

	if a.Used() != b.Used() {
		return a.Used()
	}

	ap := a.Type().HasPointers()
	bp := b.Type().HasPointers()
	if ap != bp {
		return ap
	}

	ap = a.Needzero()
	bp = b.Needzero()
	if ap != bp {
		return ap
	}

	if a.Type().Size() != b.Type().Size() {
		return a.Type().Size() > b.Type().Size()
	}

	return a.Sym().Name < b.Sym().Name
}

// byStackVar implements sort.Interface for []*Node using cmpstackvarlt.
type byStackVar []*ir.Name

func (s byStackVar) Len() int           { return len(s) }
func (s byStackVar) Less(i, j int) bool { return cmpstackvarlt(s[i], s[j]) }
func (s byStackVar) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// needAlloc reports whether n is within the current frame, for which we need to
// allocate space. In particular, it excludes arguments and results, which are in
// the callers frame.
func needAlloc(n *ir.Name) bool {
	if n.Op() != ir.ONAME {
		base.FatalfAt(n.Pos(), "%v has unexpected Op %v", n, n.Op())
	}

	switch n.Class {
	case ir.PAUTO:
		return true
	case ir.PPARAM:
		return false
	case ir.PPARAMOUT:
		return n.IsOutputParamInRegisters()

	default:
		base.FatalfAt(n.Pos(), "%v has unexpected Class %v", n, n.Class)
		return false
	}
}

func (s *ssafn) AllocFrame(f *ssa.Func) {
	s.stksize = 0
	s.stkptrsize = 0
	s.stkalign = int64(types.RegSize)
	fn := s.curfn

	// Mark the PAUTO's unused.
	for _, ln := range fn.Dcl {
		if needAlloc(ln) {
			ln.SetUsed(false)
		}
	}

	for _, l := range f.RegAlloc {
		if ls, ok := l.(ssa.LocalSlot); ok {
			ls.N.SetUsed(true)
		}
	}

	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if n, ok := v.Aux.(*ir.Name); ok {
				switch n.Class {
				case ir.PPARAMOUT:
					if n.IsOutputParamInRegisters() && v.Op == ssa.OpVarDef {
						// ignore VarDef, look for "real" uses.
						// TODO: maybe do this for PAUTO as well?
						continue
					}
					fallthrough
				case ir.PPARAM, ir.PAUTO:
					n.SetUsed(true)
				}
			}
		}
	}

	// Use sort.Stable instead of sort.Sort so stack layout (and thus
	// compiler output) is less sensitive to frontend changes that
	// introduce or remove unused variables.
	sort.Stable(byStackVar(fn.Dcl))

	// Reassign stack offsets of the locals that are used.
	lastHasPtr := false
	for i, n := range fn.Dcl {
		if n.Op() != ir.ONAME || n.Class != ir.PAUTO && !(n.Class == ir.PPARAMOUT && n.IsOutputParamInRegisters()) {
			// i.e., stack assign if AUTO, or if PARAMOUT in registers (which has no predefined spill locations)
			continue
		}
		if !n.Used() {
			fn.DebugInfo.(*ssa.FuncDebug).OptDcl = fn.Dcl[i:]
			fn.Dcl = fn.Dcl[:i]
			break
		}

		types.CalcSize(n.Type())
		w := n.Type().Size()
		if w >= types.MaxWidth || w < 0 {
			base.Fatalf("bad width")
		}
		if w == 0 && lastHasPtr {
			// Pad between a pointer-containing object and a zero-sized object.
			// This prevents a pointer to the zero-sized object from being interpreted
			// as a pointer to the pointer-containing object (and causing it
			// to be scanned when it shouldn't be). See issue 24993.
			w = 1
		}
		s.stksize += w
		s.stksize = types.RoundUp(s.stksize, n.Type().Alignment())
		if n.Type().Alignment() > int64(types.RegSize) {
			s.stkalign = n.Type().Alignment()
		}
		if n.Type().HasPointers() {
			s.stkptrsize = s.stksize
			lastHasPtr = true
		} else {
			lastHasPtr = false
		}
		n.SetFrameOffset(-s.stksize)
	}

	s.stksize = types.RoundUp(s.stksize, s.stkalign)
	s.stkptrsize = types.RoundUp(s.stkptrsize, s.stkalign)
}

const maxStackSize = 1 << 30

// Compile builds an SSA backend function,
// uses it to generate a plist,
// and flushes that plist to machine code.
// worker indicates which of the backend workers is doing the processing.
func Compile(fn *ir.Func, worker int) {
	f := buildssa(fn, worker)
	// Note: check arg size to fix issue 25507.
	if f.Frontend().(*ssafn).stksize >= maxStackSize || f.OwnAux.ArgWidth() >= maxStackSize {
		largeStackFramesMu.Lock()
		largeStackFrames = append(largeStackFrames, largeStack{locals: f.Frontend().(*ssafn).stksize, args: f.OwnAux.ArgWidth(), pos: fn.Pos()})
		largeStackFramesMu.Unlock()
		return
	}
	pp := objw.NewProgs(fn, worker)
	defer pp.Free()
	genssa(f, pp)
	// Check frame size again.
	// The check above included only the space needed for local variables.
	// After genssa, the space needed includes local variables and the callee arg region.
	// We must do this check prior to calling pp.Flush.
	// If there are any oversized stack frames,
	// the assembler may emit inscrutable complaints about invalid instructions.
	if pp.Text.To.Offset >= maxStackSize {
		largeStackFramesMu.Lock()
		locals := f.Frontend().(*ssafn).stksize
		largeStackFrames = append(largeStackFrames, largeStack{locals: locals, args: f.OwnAux.ArgWidth(), callee: pp.Text.To.Offset - locals, pos: fn.Pos()})
		largeStackFramesMu.Unlock()
		return
	}

	pp.Flush() // assemble, fill in boilerplate, etc.

	// If we're compiling the package init function, search for any
	// relocations that target global map init outline functions and
	// turn them into weak relocs.
	if fn.IsPackageInit() && base.Debug.WrapGlobalMapCtl != 1 {
		weakenGlobalMapInitRelocs(fn)
	}

	// fieldtrack must be called after pp.Flush. See issue 20014.
	fieldtrack(pp.Text.From.Sym, fn.FieldTrack)
}

// globalMapInitLsyms records the LSym of each map.init.NNN outlined
// map initializer function created by the compiler.
var globalMapInitLsyms map[*obj.LSym]struct{}

// RegisterMapInitLsym records "s" in the set of outlined map initializer
// functions.
func RegisterMapInitLsym(s *obj.LSym) {
	if globalMapInitLsyms == nil {
		globalMapInitLsyms = make(map[*obj.LSym]struct{})
	}
	globalMapInitLsyms[s] = struct{}{}
}

// weakenGlobalMapInitRelocs walks through all of the relocations on a
// given a package init function "fn" and looks for relocs that target
// outlined global map initializer functions; if it finds any such
// relocs, it flags them as R_WEAK.
func weakenGlobalMapInitRelocs(fn *ir.Func) {
	if globalMapInitLsyms == nil {
		return
	}
	for i := range fn.LSym.R {
		tgt := fn.LSym.R[i].Sym
		if tgt == nil {
			continue
		}
		if _, ok := globalMapInitLsyms[tgt]; !ok {
			continue
		}
		if base.Debug.WrapGlobalMapDbg > 1 {
			fmt.Fprintf(os.Stderr, "=-= weakify fn %v reloc %d %+v\n", fn, i,
				fn.LSym.R[i])
		}
		// set the R_WEAK bit, leave rest of reloc type intact
		fn.LSym.R[i].Type |= objabi.R_WEAK
	}
}

// StackOffset returns the stack location of a LocalSlot relative to the
// stack pointer, suitable for use in a DWARF location entry. This has nothing
// to do with its offset in the user variable.
func StackOffset(slot ssa.LocalSlot) int32 {
	n := slot.N
	var off int64
	switch n.Class {
	case ir.PPARAM, ir.PPARAMOUT:
		if !n.IsOutputParamInRegisters() {
			off = n.FrameOffset() + base.Ctxt.Arch.FixedFrameSize
			break
		}
		fallthrough // PPARAMOUT in registers allocates like an AUTO
	case ir.PAUTO:
		off = n.FrameOffset()
		if base.Ctxt.Arch.FixedFrameSize == 0 {
			off -= int64(types.PtrSize)
		}
		if buildcfg.FramePointerEnabled {
			off -= int64(types.PtrSize)
		}
	}
	return int32(off + slot.Off)
}

// fieldtrack adds R_USEFIELD relocations to fnsym to record any
// struct fields that it used.
func fieldtrack(fnsym *obj.LSym, tracked map[*obj.LSym]struct{}) {
	if fnsym == nil {
		return
	}
	if !buildcfg.Experiment.FieldTrack || len(tracked) == 0 {
		return
	}

	trackSyms := make([]*obj.LSym, 0, len(tracked))
	for sym := range tracked {
		trackSyms = append(trackSyms, sym)
	}
	sort.Slice(trackSyms, func(i, j int) bool { return trackSyms[i].Name < trackSyms[j].Name })
	for _, sym := range trackSyms {
		r := obj.Addrel(fnsym)
		r.Sym = sym
		r.Type = objabi.R_USEFIELD
	}
}

// largeStack is info about a function whose stack frame is too large (rare).
type largeStack struct {
	locals int64
	args   int64
	callee int64
	pos    src.XPos
}

var (
	largeStackFramesMu sync.Mutex // protects largeStackFrames
	largeStackFrames   []largeStack
)

func CheckLargeStacks() {
	// Check whether any of the functions we have compiled have gigantic stack frames.
	sort.Slice(largeStackFrames, func(i, j int) bool {
		return largeStackFrames[i].pos.Before(largeStackFrames[j].pos)
	})
	for _, large := range largeStackFrames {
		if large.callee != 0 {
			base.ErrorfAt(large.pos, "stack frame too large (>1GB): %d MB locals + %d MB args + %d MB callee", large.locals>>20, large.args>>20, large.callee>>20)
		} else {
			base.ErrorfAt(large.pos, "stack frame too large (>1GB): %d MB locals + %d MB args", large.locals>>20, large.args>>20)
		}
	}
}
