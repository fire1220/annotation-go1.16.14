// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Runtime type representation.

package runtime

import "unsafe"

// tflag is documented in reflect/type.go.
//
// tflag values must be kept in sync with copies in:
//
//		cmd/compile/internal/gc/reflect.go
//		cmd/link/internal/ld/decodesym.go
//		reflect/type.go
//	     internal/reflectlite/type.go
type tflag uint8

const (
	tflagUncommon      tflag = 1 << 0
	tflagExtraStar     tflag = 1 << 1
	tflagNamed         tflag = 1 << 2
	tflagRegularMemory tflag = 1 << 3 // equal and hash can treat values of this type as a single region of t.size bytes
)

// Needs to be in sync with ../cmd/link/internal/ld/decodesym.go:/^func.commonsize,
// ../cmd/compile/internal/gc/reflect.go:/^func.dcommontype and
// ../reflect/type.go:/^type.rtype.
// ../internal/reflectlite/type.go:/^type.rtype.
// 注释：go语言的所有类型都是在这个类型的基础通过加上额为的字段来管理的
// 注释：_type包含了类型的大小、哈希、标志、偏移量等元数据。
type _type struct {
	size       uintptr // 注释：类型大小
	ptrdata    uintptr // 注释：对象里最后一个包含指针的偏移量，数据对象是以指针大小对齐的，偏移量值是数据对象基地址到最后一个包含指针的字节数（0表示不包含指针） // size of memory prefix holding all pointers
	hash       uint32  // 注释：哈希是动态类型的唯一标识
	tflag      tflag   // 注释：标志
	align      uint8   // 注释：类型的内存对齐
	fieldAlign uint8   // 注释：字段内存对齐
	kind       uint8   // 注释：基础类型（包含GC程序类型）
	// function for comparing objects of this type
	// (ptr to object A, ptr to object B) -> ==?
	equal func(unsafe.Pointer, unsafe.Pointer) bool // 注释：比较函数(map中有使用这个对比key是否相等)
	// gcdata stores the GC type data for the garbage collector.
	// If the KindGCProg bit is set in kind, gcdata is a GC program.
	// Otherwise it is a ptrmask bitmap. See mbitmap.go for details.
	gcdata    *byte // 注释：位图1有指针0无指针；如果ptrdata是0则这里也是0
	str       nameOff
	ptrToThis typeOff
}

func (t *_type) string() string {
	s := t.nameOff(t.str).name()
	if t.tflag&tflagExtraStar != 0 {
		return s[1:]
	}
	return s
}

func (t *_type) uncommon() *uncommontype {
	if t.tflag&tflagUncommon == 0 {
		return nil
	}
	switch t.kind & kindMask {
	case kindStruct:
		type u struct {
			structtype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindPtr:
		type u struct {
			ptrtype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindFunc:
		type u struct {
			functype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindSlice:
		type u struct {
			slicetype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindArray:
		type u struct {
			arraytype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindChan:
		type u struct {
			chantype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindMap:
		type u struct {
			maptype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	case kindInterface:
		type u struct {
			interfacetype
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	default:
		type u struct {
			_type
			u uncommontype
		}
		return &(*u)(unsafe.Pointer(t)).u
	}
}

func (t *_type) name() string {
	if t.tflag&tflagNamed == 0 {
		return ""
	}
	s := t.string()
	i := len(s) - 1
	for i >= 0 && s[i] != '.' {
		i--
	}
	return s[i+1:]
}

// pkgpath returns the path of the package where t was defined, if
// available. This is not the same as the reflect package's PkgPath
// method, in that it returns the package path for struct and interface
// types, not just named types.
func (t *_type) pkgpath() string {
	if u := t.uncommon(); u != nil {
		return t.nameOff(u.pkgpath).name()
	}
	switch t.kind & kindMask {
	case kindStruct:
		st := (*structtype)(unsafe.Pointer(t))
		return st.pkgPath.name()
	case kindInterface:
		it := (*interfacetype)(unsafe.Pointer(t))
		return it.pkgpath.name()
	}
	return ""
}

// reflectOffs holds type offsets defined at run time by the reflect package.
//
// When a type is defined at run time, its *rtype data lives on the heap.
// There are a wide range of possible addresses the heap may use, that
// may not be representable as a 32-bit offset. Moreover the GC may
// one day start moving heap memory, in which case there is no stable
// offset that can be defined.
//
// To provide stable offsets, we add pin *rtype objects in a global map
// and treat the offset as an identifier. We use negative offsets that
// do not overlap with any compile-time module offsets.
//
// Entries are created by reflect.addReflectOff.
var reflectOffs struct {
	lock mutex
	next int32
	m    map[int32]unsafe.Pointer
	minv map[unsafe.Pointer]int32
}

func reflectOffsLock() {
	lock(&reflectOffs.lock)
	if raceenabled {
		raceacquire(unsafe.Pointer(&reflectOffs.lock))
	}
}

func reflectOffsUnlock() {
	if raceenabled {
		racerelease(unsafe.Pointer(&reflectOffs.lock))
	}
	unlock(&reflectOffs.lock)
}

// 注释：通过imethod.ityp和偏移量快速查找方法名称
func resolveNameOff(ptrInModule unsafe.Pointer, off nameOff) name {
	if off == 0 {
		return name{}
	}
	base := uintptr(ptrInModule)
	for md := &firstmoduledata; md != nil; md = md.next {
		if base >= md.types && base < md.etypes {
			res := md.types + uintptr(off)
			if res > md.etypes {
				println("runtime: nameOff", hex(off), "out of range", hex(md.types), "-", hex(md.etypes))
				throw("runtime: name offset out of range")
			}
			return name{(*byte)(unsafe.Pointer(res))}
		}
	}

	// No module found. see if it is a run time name.
	reflectOffsLock()
	res, found := reflectOffs.m[int32(off)]
	reflectOffsUnlock()
	if !found {
		println("runtime: nameOff", hex(off), "base", hex(base), "not in ranges:")
		for next := &firstmoduledata; next != nil; next = next.next {
			println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
		}
		throw("runtime: name offset base pointer out of range")
	}
	return name{(*byte)(res)}
}

func (t *_type) nameOff(off nameOff) name {
	return resolveNameOff(unsafe.Pointer(t), off)
}

// 注释：通过imethod.ityp偏移量快速查找方法类型
func resolveTypeOff(ptrInModule unsafe.Pointer, off typeOff) *_type {
	if off == 0 || off == -1 {
		// -1 is the sentinel value for unreachable code.
		// See cmd/link/internal/ld/data.go:relocsym.
		return nil
	}
	base := uintptr(ptrInModule)
	var md *moduledata
	for next := &firstmoduledata; next != nil; next = next.next {
		if base >= next.types && base < next.etypes {
			md = next
			break
		}
	}
	if md == nil {
		reflectOffsLock()
		res := reflectOffs.m[int32(off)]
		reflectOffsUnlock()
		if res == nil {
			println("runtime: typeOff", hex(off), "base", hex(base), "not in ranges:")
			for next := &firstmoduledata; next != nil; next = next.next {
				println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
			}
			throw("runtime: type offset base pointer out of range")
		}
		return (*_type)(res)
	}
	if t := md.typemap[off]; t != nil {
		return t
	}
	res := md.types + uintptr(off)
	if res > md.etypes {
		println("runtime: typeOff", hex(off), "out of range", hex(md.types), "-", hex(md.etypes))
		throw("runtime: type offset out of range")
	}
	return (*_type)(unsafe.Pointer(res))
}

func (t *_type) typeOff(off typeOff) *_type {
	return resolveTypeOff(unsafe.Pointer(t), off)
}

func (t *_type) textOff(off textOff) unsafe.Pointer {
	if off == -1 {
		// -1 is the sentinel value for unreachable code.
		// See cmd/link/internal/ld/data.go:relocsym.
		return unsafe.Pointer(^uintptr(0))
	}
	base := uintptr(unsafe.Pointer(t))
	var md *moduledata
	for next := &firstmoduledata; next != nil; next = next.next {
		if base >= next.types && base < next.etypes {
			md = next
			break
		}
	}
	if md == nil {
		reflectOffsLock()
		res := reflectOffs.m[int32(off)]
		reflectOffsUnlock()
		if res == nil {
			println("runtime: textOff", hex(off), "base", hex(base), "not in ranges:")
			for next := &firstmoduledata; next != nil; next = next.next {
				println("\ttypes", hex(next.types), "etypes", hex(next.etypes))
			}
			throw("runtime: text offset base pointer out of range")
		}
		return res
	}
	res := uintptr(0)

	// The text, or instruction stream is generated as one large buffer.  The off (offset) for a method is
	// its offset within this buffer.  If the total text size gets too large, there can be issues on platforms like ppc64 if
	// the target of calls are too far for the call instruction.  To resolve the large text issue, the text is split
	// into multiple text sections to allow the linker to generate long calls when necessary.  When this happens, the vaddr
	// for each text section is set to its offset within the text.  Each method's offset is compared against the section
	// vaddrs and sizes to determine the containing section.  Then the section relative offset is added to the section's
	// relocated baseaddr to compute the method addess.

	if len(md.textsectmap) > 1 {
		for i := range md.textsectmap {
			sectaddr := md.textsectmap[i].vaddr
			sectlen := md.textsectmap[i].length
			if uintptr(off) >= sectaddr && uintptr(off) < sectaddr+sectlen {
				res = md.textsectmap[i].baseaddr + uintptr(off) - uintptr(md.textsectmap[i].vaddr)
				break
			}
		}
	} else {
		// single text section
		res = md.text + uintptr(off)
	}

	if res > md.etext && GOARCH != "wasm" { // on wasm, functions do not live in the same address space as the linear memory
		println("runtime: textOff", hex(off), "out of range", hex(md.text), "-", hex(md.etext))
		throw("runtime: text offset out of range")
	}
	return unsafe.Pointer(res)
}

func (t *functype) in() []*_type {
	// See funcType in reflect/type.go for details on data layout.
	uadd := uintptr(unsafe.Sizeof(functype{}))
	if t.typ.tflag&tflagUncommon != 0 {
		uadd += unsafe.Sizeof(uncommontype{})
	}
	return (*[1 << 20]*_type)(add(unsafe.Pointer(t), uadd))[:t.inCount]
}

func (t *functype) out() []*_type {
	// See funcType in reflect/type.go for details on data layout.
	uadd := uintptr(unsafe.Sizeof(functype{}))
	if t.typ.tflag&tflagUncommon != 0 {
		uadd += unsafe.Sizeof(uncommontype{})
	}
	outCount := t.outCount & (1<<15 - 1)
	return (*[1 << 20]*_type)(add(unsafe.Pointer(t), uadd))[t.inCount : t.inCount+outCount]
}

func (t *functype) dotdotdot() bool {
	return t.outCount&(1<<15) != 0
}

type nameOff int32
type typeOff int32
type textOff int32

type method struct {
	name nameOff
	mtyp typeOff
	ifn  textOff
	tfn  textOff
}

type uncommontype struct {
	pkgpath nameOff
	mcount  uint16 // number of methods
	xcount  uint16 // number of exported methods
	moff    uint32 // offset from this uncommontype to [mcount]method
	_       uint32 // unused
}

// 注释：接口中暴露的方法在最终可执行文件中的名字和类型的偏移量
type imethod struct {
	name nameOff // 注释：名字
	ityp typeOff // 注释：类型的偏移量
}

// 注释：接口本身的类型
type interfacetype struct {
	typ     _type     // 注释：基础类型
	pkgpath name      // 注释：接口所在的路径包名
	mhdr    []imethod // 注释：接口中暴露的方法在最终可执行文件中的名字和类型的偏移量
}

// 注释：maptype对象记键值对和桶的大小等必要信息
// 注释：map的类型
type maptype struct {
	typ  _type  // 注释：map的基础类型
	key  *_type // 注释：key的类型
	elem *_type // 注释：value值的类型
	// 注释：桶的类型，一般是创建新的桶时用到（newobject），比如发生扩容时会创建新的桶，是根据这个类型创建桶的数组
	bucket *_type // internal type representing a hash bucket
	// function for hashing keys (ptr to key, seed) -> hash
	hasher     func(unsafe.Pointer, uintptr) uintptr // 注释：存放随机粽子的方法
	keysize    uint8                                 // 注释：key的大小 // size of key slot
	elemsize   uint8                                 // 注释：元素(值）的大小 // size of elem slot
	bucketsize uint16                                // 注释：单个桶的大小 // size of bucket
	flags      uint32                                // 注释：用来标记，例如”间接key、间接的value、反射key、离散可能会死机”等信息
}

// Note: flag values must match those used in the TMAP case
// in ../cmd/compile/internal/gc/reflect.go:dtypesym.
// 注释：判断是否是间接的key(如果int可以存下key的话，那么key就是其本身，如果int存不下，则是通过指针指向key)
func (mt *maptype) indirectkey() bool { // store ptr to key instead of key itself
	return mt.flags&1 != 0 // 注释：indirect key(间接的key)
}
func (mt *maptype) indirectelem() bool { // store ptr to elem instead of elem itself
	return mt.flags&2 != 0 // 注释：indirect value(间接的value)
}
func (mt *maptype) reflexivekey() bool { // true if k==k for all keys
	return mt.flags&4 != 0 // 注释：reflexive key(反射的key)
}
func (mt *maptype) needkeyupdate() bool { // true if we need to update key on an overwrite
	return mt.flags&8 != 0 // 注释：等量扩容标识是8，判断是否是等量扩容，通过key更新时，如果是等量扩容需要特殊处理 // need key update
}
func (mt *maptype) hashMightPanic() bool { // true if hash function might panic
	return mt.flags&16 != 0 // 注释：hash might panic(散列可能会死机)
}

type arraytype struct {
	typ   _type
	elem  *_type
	slice *_type
	len   uintptr
}

// 注释：管道类型
type chantype struct {
	typ  _type   // 注释：管道的基础类型
	elem *_type  // 注释：管道元素的类型
	dir  uintptr // 注释：
}

// 注释：切片类型
type slicetype struct {
	typ  _type  // 注释：切片的基础类型
	elem *_type // 注释：切片元素的首指针
}

type functype struct {
	typ      _type
	inCount  uint16
	outCount uint16
}

type ptrtype struct {
	typ  _type
	elem *_type
}

// 注释：结构体字段的结构体
type structfield struct {
	name       name    // 注释：结构体里字段名
	typ        *_type  // 注释：结构体字段的类型
	offsetAnon uintptr // 注释：结构体字段对应的地址（地址是int类型，用uintptr左对齐装地址的话，实际地址的值是：offsetAnon >> 1）
}

// 注释：获取结构体每个字段的开始内存位置（内存地址），结束地址是：memEnd = f.offset() + f.typ.size
func (f *structfield) offset() uintptr {
	return f.offsetAnon >> 1
}

// 注释：结构体类型
type structtype struct {
	typ     _type         // 注释：结构体数据的基础类型
	pkgPath name          // 注释：结构体的包路径
	fields  []structfield // 注释：结构体的字段
}

// name is an encoded type name with optional extra data.
// See reflect/type.go for details.
type name struct {
	bytes *byte
}

func (n name) data(off int) *byte {
	return (*byte)(add(unsafe.Pointer(n.bytes), uintptr(off)))
}

func (n name) isExported() bool {
	return (*n.bytes)&(1<<0) != 0
}

func (n name) nameLen() int {
	return int(uint16(*n.data(1))<<8 | uint16(*n.data(2)))
}

func (n name) tagLen() int {
	if *n.data(0)&(1<<1) == 0 {
		return 0
	}
	off := 3 + n.nameLen()
	return int(uint16(*n.data(off))<<8 | uint16(*n.data(off + 1)))
}

func (n name) name() (s string) {
	if n.bytes == nil {
		return ""
	}
	nl := n.nameLen()
	if nl == 0 {
		return ""
	}
	hdr := (*stringStruct)(unsafe.Pointer(&s))
	hdr.str = unsafe.Pointer(n.data(3))
	hdr.len = nl
	return s
}

func (n name) tag() (s string) {
	tl := n.tagLen()
	if tl == 0 {
		return ""
	}
	nl := n.nameLen()
	hdr := (*stringStruct)(unsafe.Pointer(&s))
	hdr.str = unsafe.Pointer(n.data(3 + nl + 2))
	hdr.len = tl
	return s
}

func (n name) pkgPath() string {
	if n.bytes == nil || *n.data(0)&(1<<2) == 0 {
		return ""
	}
	off := 3 + n.nameLen()
	if tl := n.tagLen(); tl > 0 {
		off += 2 + tl
	}
	var nameOff nameOff
	copy((*[4]byte)(unsafe.Pointer(&nameOff))[:], (*[4]byte)(unsafe.Pointer(n.data(off)))[:])
	pkgPathName := resolveNameOff(unsafe.Pointer(n.bytes), nameOff)
	return pkgPathName.name()
}

func (n name) isBlank() bool {
	if n.bytes == nil {
		return false
	}
	if n.nameLen() != 1 {
		return false
	}
	return *n.data(3) == '_'
}

// typelinksinit scans the types from extra modules and builds the
// moduledata typemap used to de-duplicate type pointers.
func typelinksinit() {
	if firstmoduledata.next == nil {
		return
	}
	typehash := make(map[uint32][]*_type, len(firstmoduledata.typelinks))

	modules := activeModules()
	prev := modules[0]
	for _, md := range modules[1:] {
		// Collect types from the previous module into typehash.
	collect:
		for _, tl := range prev.typelinks {
			var t *_type
			if prev.typemap == nil {
				t = (*_type)(unsafe.Pointer(prev.types + uintptr(tl)))
			} else {
				t = prev.typemap[typeOff(tl)]
			}
			// Add to typehash if not seen before.
			tlist := typehash[t.hash]
			for _, tcur := range tlist {
				if tcur == t {
					continue collect
				}
			}
			typehash[t.hash] = append(tlist, t)
		}

		if md.typemap == nil {
			// If any of this module's typelinks match a type from a
			// prior module, prefer that prior type by adding the offset
			// to this module's typemap.
			tm := make(map[typeOff]*_type, len(md.typelinks))
			pinnedTypemaps = append(pinnedTypemaps, tm)
			md.typemap = tm
			for _, tl := range md.typelinks {
				t := (*_type)(unsafe.Pointer(md.types + uintptr(tl)))
				for _, candidate := range typehash[t.hash] {
					seen := map[_typePair]struct{}{}
					if typesEqual(t, candidate, seen) {
						t = candidate
						break
					}
				}
				md.typemap[typeOff(tl)] = t
			}
		}

		prev = md
	}
}

type _typePair struct {
	t1 *_type
	t2 *_type
}

// typesEqual reports whether two types are equal.
//
// Everywhere in the runtime and reflect packages, it is assumed that
// there is exactly one *_type per Go type, so that pointer equality
// can be used to test if types are equal. There is one place that
// breaks this assumption: buildmode=shared. In this case a type can
// appear as two different pieces of memory. This is hidden from the
// runtime and reflect package by the per-module typemap built in
// typelinksinit. It uses typesEqual to map types from later modules
// back into earlier ones.
//
// Only typelinksinit needs this function.
func typesEqual(t, v *_type, seen map[_typePair]struct{}) bool {
	tp := _typePair{t, v}
	if _, ok := seen[tp]; ok {
		return true
	}

	// mark these types as seen, and thus equivalent which prevents an infinite loop if
	// the two types are identical, but recursively defined and loaded from
	// different modules
	seen[tp] = struct{}{}

	if t == v {
		return true
	}
	kind := t.kind & kindMask
	if kind != v.kind&kindMask {
		return false
	}
	if t.string() != v.string() {
		return false
	}
	ut := t.uncommon()
	uv := v.uncommon()
	if ut != nil || uv != nil {
		if ut == nil || uv == nil {
			return false
		}
		pkgpatht := t.nameOff(ut.pkgpath).name()
		pkgpathv := v.nameOff(uv.pkgpath).name()
		if pkgpatht != pkgpathv {
			return false
		}
	}
	if kindBool <= kind && kind <= kindComplex128 {
		return true
	}
	switch kind {
	case kindString, kindUnsafePointer:
		return true
	case kindArray:
		at := (*arraytype)(unsafe.Pointer(t))
		av := (*arraytype)(unsafe.Pointer(v))
		return typesEqual(at.elem, av.elem, seen) && at.len == av.len
	case kindChan:
		ct := (*chantype)(unsafe.Pointer(t))
		cv := (*chantype)(unsafe.Pointer(v))
		return ct.dir == cv.dir && typesEqual(ct.elem, cv.elem, seen)
	case kindFunc:
		ft := (*functype)(unsafe.Pointer(t))
		fv := (*functype)(unsafe.Pointer(v))
		if ft.outCount != fv.outCount || ft.inCount != fv.inCount {
			return false
		}
		tin, vin := ft.in(), fv.in()
		for i := 0; i < len(tin); i++ {
			if !typesEqual(tin[i], vin[i], seen) {
				return false
			}
		}
		tout, vout := ft.out(), fv.out()
		for i := 0; i < len(tout); i++ {
			if !typesEqual(tout[i], vout[i], seen) {
				return false
			}
		}
		return true
	case kindInterface:
		it := (*interfacetype)(unsafe.Pointer(t))
		iv := (*interfacetype)(unsafe.Pointer(v))
		if it.pkgpath.name() != iv.pkgpath.name() {
			return false
		}
		if len(it.mhdr) != len(iv.mhdr) {
			return false
		}
		for i := range it.mhdr {
			tm := &it.mhdr[i]
			vm := &iv.mhdr[i]
			// Note the mhdr array can be relocated from
			// another module. See #17724.
			tname := resolveNameOff(unsafe.Pointer(tm), tm.name)
			vname := resolveNameOff(unsafe.Pointer(vm), vm.name)
			if tname.name() != vname.name() {
				return false
			}
			if tname.pkgPath() != vname.pkgPath() {
				return false
			}
			tityp := resolveTypeOff(unsafe.Pointer(tm), tm.ityp)
			vityp := resolveTypeOff(unsafe.Pointer(vm), vm.ityp)
			if !typesEqual(tityp, vityp, seen) {
				return false
			}
		}
		return true
	case kindMap:
		mt := (*maptype)(unsafe.Pointer(t))
		mv := (*maptype)(unsafe.Pointer(v))
		return typesEqual(mt.key, mv.key, seen) && typesEqual(mt.elem, mv.elem, seen)
	case kindPtr:
		pt := (*ptrtype)(unsafe.Pointer(t))
		pv := (*ptrtype)(unsafe.Pointer(v))
		return typesEqual(pt.elem, pv.elem, seen)
	case kindSlice:
		st := (*slicetype)(unsafe.Pointer(t))
		sv := (*slicetype)(unsafe.Pointer(v))
		return typesEqual(st.elem, sv.elem, seen)
	case kindStruct:
		st := (*structtype)(unsafe.Pointer(t))
		sv := (*structtype)(unsafe.Pointer(v))
		if len(st.fields) != len(sv.fields) {
			return false
		}
		if st.pkgPath.name() != sv.pkgPath.name() {
			return false
		}
		for i := range st.fields {
			tf := &st.fields[i]
			vf := &sv.fields[i]
			if tf.name.name() != vf.name.name() {
				return false
			}
			if !typesEqual(tf.typ, vf.typ, seen) {
				return false
			}
			if tf.name.tag() != vf.name.tag() {
				return false
			}
			if tf.offsetAnon != vf.offsetAnon {
				return false
			}
		}
		return true
	default:
		println("runtime: impossible type kind", kind)
		throw("runtime: impossible type kind")
		return false
	}
}
