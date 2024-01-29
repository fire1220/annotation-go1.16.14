// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// Per-thread (in Go, per-P) cache for small objects.
// This includes a small object cache and local allocation stats.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
// 注释：每个逻辑处理P下的内存缓存，同一时刻P只能处理一个G所以不需要加锁
//
// 注释：逻辑处理P里缓存的span
//go:notinheap
type mcache struct {
	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	// 注释：以下成员在每个malloc上都可以访问，因此在这里对它们进行分组以获得更好的缓存。
	nextSample uintptr // 注释：分配这么多字节后触发堆实例 // trigger heap sample after allocating this many bytes
	scanAlloc  uintptr // 注释：已分配的可扫描堆的字节数 // bytes of scannable heap allocated

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.
	// 注释：用于不带指针的微小对象的分配器缓存。请参阅malloc.go中的“微小分配器”注释。

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	// 注释：微小点指向当前微小块的开始，如果没有当前微小块，则为零。
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	// 注释：tiny是一个堆指针。由于mcache在非GC的内存中，我们通过在标记终止期间在releaseAll中清除它来处理它。
	//
	// tinyAllocs is the number of tiny allocations performed
	// by the P that owns this mcache.
	// 注释：tinyAllocs是拥有此mcache的P执行的微小分配的数量。
	tiny       uintptr // 注释：一个微对象基地址
	tinyoffset uintptr // 注释：一个微对象的偏移量(已经分配的偏移量)（小于等于tinyoffset的表示已分配过了）
	tinyAllocs uintptr // 注释：一个微对象分配的次数

	// The rest is not accessed on every malloc.

	// 注释：这里存储的是：每种span类型只存储一个(至少含有一个空块的span)，会把分配完成是span放到中心缓存mcentral中，然后再从中心缓存mcentral中拿含有空块的span放到这里(mcache.alloc)
	alloc [numSpanClasses]*mspan // 注释：存储着所有类型的span数据链表，按class分组的mspan列表,对象注释在/src/runtime/sizeclasses.go里 // spans to allocate from, indexed by spanClass

	stackcache [_NumStackOrders]stackfreelist

	// flushGen indicates the sweepgen during which this mcache
	// was last flushed. If flushGen != mheap_.sweepgen, the spans
	// in this mcache are stale and need to the flushed so they
	// can be swept. This is done in acquirep.
	flushGen uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy mspan that contains no free objects.
var emptymspan mspan // 注释：(所有块都被分配完)不包含空闲对象的mspan。

func allocmcache() *mcache {
	var c *mcache
	systemstack(func() {
		lock(&mheap_.lock)
		c = (*mcache)(mheap_.cachealloc.alloc())
		c.flushGen = mheap_.sweepgen
		unlock(&mheap_.lock)
	})
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	c.nextSample = nextSample()
	return c
}

// freemcache releases resources associated with this
// mcache and puts the object onto a free list.
//
// In some cases there is no way to simply release
// resources, such as statistics, so donate them to
// a different mcache (the recipient).
func freemcache(c *mcache) {
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// getMCache is a convenience function which tries to obtain an mcache.
//
// Returns nil if we're not bootstrapping or we don't have a P. The caller's
// P must not change, so we must be in a non-preemptible state.
// 注释：把P中的mcache指针返回，如果P中的值是nil说明是P0，返回全局的P0对应的全局mcache0指针
// 注释：mcache是绑定到P上的
func getMCache() *mcache {
	// Grab the mcache, since that's where stats live.
	pp := getg().m.p.ptr() // 注释：获取当前G对应的M下的P地址
	var c *mcache
	if pp == nil { // 注释：如果为nil说明当前是P0，则返回P0对应的全局mcache0
		// We will be called without a P while bootstrapping,
		// in which case we use mcache0, which is set in mallocinit.
		// mcache0 is cleared when bootstrapping is complete,
		// by procresize.
		c = mcache0
	} else {
		c = pp.mcache // 注释：返回P下的mcache
	}
	return c
}

// refill acquires a new span of span class spc for c. This span will
// have at least one free object. The current span in c must be full.
// 注释：relfill为c获取一个span类spc的新span。这个span将至少有一个空闲对象。c中的当前跨度必须是满的。
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
// 注释：必须在不可抢占的上下文中运行，否则c的所有者可能会更改。
//
// 注释：(从中心缓存或堆中拿空间)从新填装空span到mcache里，确保mcache缓存中只要有一个可以使用的span里的空闲块
// 注释：函数步骤：
//		1.获取旧span
//		2.把旧span放到中心缓存mcentral的已清里队列中
//		3.从中心缓存获取一个新的span
//		4.设置span状态为【无需清理、已缓存】
//		5.重新设置mcache的span值
func (c *mcache) refill(spc spanClass) {
	// Return the current cached span to the central lists.
	s := c.alloc[spc] // 注释：获取旧span，在mcache中取出span（此时span中的块都已经分配完了）

	if uintptr(s.allocCount) != s.nelems { // 注释：校验如果全部的块没有分配完则报错
		throw("refill of span with free space remaining")
	}
	if s != &emptymspan { // 注释：如果非空，初始化的时候会赋值为&emptymspan
		// Mark this span as no longer cached.
		// 注释：将此跨度标记为不再缓存。
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		mheap_.central[spc].mcentral.uncacheSpan(s) // 注释：把已经分配完的span放到中心缓存mcentral中去
	}

	// Get a new cached span from the central lists.
	// 注释：从中心列表中获取新的缓存span。
	s = mheap_.central[spc].mcentral.cacheSpan() // 注释：从中心缓存mcental中获取span并缓存起来【ing】
	if s == nil {
		throw("out of memory")
	}

	if uintptr(s.allocCount) == s.nelems {
		throw("span has no free space")
	}

	// Indicate that this span is cached and prevent asynchronous
	// sweeping in the next sweep phase.
	// 注释：译：指示此跨度已缓存，并在下一个扫描阶段阻止异步扫描。
	s.sweepgen = mheap_.sweepgen + 3 // 注释：设置状态【无需清理、已缓存】

	// Assume all objects from this span will be allocated in the
	// mcache. If it gets uncached, we'll adjust this.
	// 注释：译：假设此跨度中的所有对象都将被分配到mcache中。如果它被解开，我们会调整它。
	stats := memstats.heapStats.acquire()
	atomic.Xadduintptr(&stats.smallAllocCount[spc.sizeclass()], uintptr(s.nelems)-uintptr(s.allocCount))
	memstats.heapStats.release()

	// Update heap_live with the same assumption.
	usedBytes := uintptr(s.allocCount) * s.elemsize
	atomic.Xadd64(&memstats.heap_live, int64(s.npages*pageSize)-int64(usedBytes))

	// Flush tinyAllocs.
	if spc == tinySpanClass {
		atomic.Xadd64(&memstats.tinyallocs, int64(c.tinyAllocs))
		c.tinyAllocs = 0
	}

	// While we're here, flush scanAlloc, since we have to call
	// revise anyway.
	atomic.Xadd64(&memstats.heap_scan, int64(c.scanAlloc))
	c.scanAlloc = 0

	if trace.enabled {
		// heap_live changed.
		traceHeapAlloc()
	}
	if gcBlackenEnabled != 0 { // 注释：如果GC正在标记
		// heap_live and heap_scan changed.
		gcController.revise() // 注释：【ing】
	}

	c.alloc[spc] = s
}

// allocLarge allocates a span for a large object.
// 注释：大对象分配，size 分配的大小, needzero 是否需要0填充内存, noscan 是否不包含指针, 返回span对象指针，span对象ID为0
// 注释：步骤
// 		1.计算需要的页数量
// 		2.
// 		3.
// 		4.
// 		5.
func (c *mcache) allocLarge(size uintptr, needzero bool, noscan bool) *mspan {
	if size+_PageSize < size { // 注释：判断内存是否超出
		throw("out of memory")
	}
	npages := size >> _PageShift // 注释：计算页的数量
	if size&_PageMask != 0 {     // 注释：如果size低位有值说明不是整数倍，则页数加一
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	// 注释：译：扣除此跨度分配的贷项，如有必要，进行扫掠。mHeap_Alloc也会扫描npage，所以这只会将债务向下支付到npage页面。
	deductSweepCredit(npages*_PageSize, npages) // 注释：减低清理积分spanBytes是一个span的大小

	spc := makeSpanClass(0, noscan)          // 注释：(组合跨度类ID)组合span对象ID和是否不扫描表示
	s := mheap_.alloc(npages, spc, needzero) // 注释：申请内存
	if s == nil {
		throw("out of memory")
	}
	stats := memstats.heapStats.acquire()
	atomic.Xadduintptr(&stats.largeAlloc, npages*pageSize)
	atomic.Xadduintptr(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Update heap_live and revise pacing if needed.
	atomic.Xadd64(&memstats.heap_live, int64(npages*pageSize))
	if trace.enabled {
		// Trace that a heap alloc occurred because heap_live changed.
		traceHeapAlloc()
	}
	if gcBlackenEnabled != 0 {
		gcController.revise()
	}

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	s.limit = s.base() + size
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}

func (c *mcache) releaseAll() {
	// Take this opportunity to flush scanAlloc.
	atomic.Xadd64(&memstats.heap_scan, int64(c.scanAlloc))
	c.scanAlloc = 0

	sg := mheap_.sweepgen
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			// Adjust nsmallalloc in case the span wasn't fully allocated.
			n := uintptr(s.nelems) - uintptr(s.allocCount)
			stats := memstats.heapStats.acquire()
			atomic.Xadduintptr(&stats.smallAllocCount[spanClass(i).sizeclass()], -n)
			memstats.heapStats.release()
			if s.sweepgen != sg+1 {
				// refill conservatively counted unallocated slots in heap_live.
				// Undo this.
				//
				// If this span was cached before sweep, then
				// heap_live was totally recomputed since
				// caching this span, so we don't do this for
				// stale spans.
				atomic.Xadd64(&memstats.heap_live, -int64(n)*int64(s.elemsize))
			}
			// Release the span to the mcentral.
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0
	atomic.Xadd64(&memstats.tinyallocs, int64(c.tinyAllocs))
	c.tinyAllocs = 0

	// Updated heap_scan and possible heap_live.
	if gcBlackenEnabled != 0 {
		gcController.revise()
	}
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	sg := mheap_.sweepgen
	if c.flushGen == sg {
		return
	} else if c.flushGen != sg-2 {
		println("bad flushGen", c.flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	atomic.Store(&c.flushGen, mheap_.sweepgen) // Synchronizes with gcStart
}
