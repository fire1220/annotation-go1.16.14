// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Page heap.
//
// See malloc.go for overview.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// minPhysPageSize is a lower-bound on the physical page size. The
	// true physical page size may be larger than this. In contrast,
	// sys.PhysPageSize is an upper-bound on the physical page size.
	minPhysPageSize = 4096

	// maxPhysPageSize is the maximum page size the runtime supports.
	maxPhysPageSize = 512 << 10

	// maxPhysHugePageSize sets an upper-bound on the maximum huge page size
	// that the runtime supports.
	maxPhysHugePageSize = pallocChunkBytes

	// pagesPerReclaimerChunk indicates how many pages to scan from the
	// pageInUse bitmap at a time. Used by the page reclaimer.
	//
	// Higher values reduce contention on scanning indexes (such as
	// h.reclaimIndex), but increase the minimum latency of the
	// operation.
	//
	// The time required to scan this many pages can vary a lot depending
	// on how many spans are actually freed. Experimentally, it can
	// scan for pages at ~300 GB/ms on a 2.6GHz Core i7, but can only
	// free spans at ~32 MB/ms. Using 512 pages bounds this at
	// roughly 100µs.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	pagesPerReclaimerChunk = 512

	// physPageAlignedStacks indicates whether stack allocations must be
	// physical page aligned. This is a requirement for MAP_STACK on
	// OpenBSD.
	physPageAlignedStacks = GOOS == "openbsd"
)

// Main malloc heap.
// The heap itself is the "free" and "scav" treaps,
// but all the other global data is here too.
//
// mheap must not be heap-allocated because it contains mSpanLists,
// which must not be heap-allocated.
//
//go:notinheap
type mheap struct {
	// lock must only be acquired on the system stack, otherwise a g
	// could self-deadlock if its stack grows with the lock held.
	lock      mutex     // 注释：互斥锁
	pages     pageAlloc // 注释：指向spans区域，用于映射span和page的关系(页面分配数据结构) // page allocation data structure
	sweepgen  uint32    // 注释：GC清理的计数器(GC清理版本)（每次GC清理开始处自增2）// sweep generation, see comment in mspan; written during STW
	sweepdone uint32    // 注释：是否清理完成1是0否(所有的内存区块（span）都已经被清理了)(如果存在没有清理的的数据时是0) // all spans are swept
	sweepers  uint32    // 注释：活动的处理sweepdone的数量 // number of active sweepone calls

	// allspans is a slice of all mspans ever created. Each mspan
	// appears exactly once.
	//
	// The memory for allspans is manually managed and can be
	// reallocated and move as the heap grows.
	//
	// In general, allspans is protected by mheap_.lock, which
	// prevents concurrent access as well as freeing the backing
	// store. Accesses during STW might not hold the lock, but
	// must ensure that allocation cannot happen around the
	// access (since that may free the backing store).
	allspans []*mspan // 所管理的所有 mspan 包括已经分配的和空闲的 // all spans out there

	_ uint32 // align uint64 fields on 32-bit for atomics

	// Proportional sweep
	// 注释：比例扫描
	//
	// These parameters represent a linear function from heap_live
	// to page sweep count. The proportional sweep system works to
	// stay in the black by keeping the current page sweep count
	// above this line at the current heap_live.
	// 注释：译：这些参数代表了从heap_live到页面扫描计数的线性函数。比例扫描系统的工作原理是通过保持当前堆存活量下的页面扫描计数高于该线条来保持在良好状态。
	//
	// The line has slope sweepPagesPerByte and passes through a
	// basis point at (sweepHeapLiveBasis, pagesSweptBasis). At
	// any given time, the system is at (memstats.heap_live,
	// pagesSwept) in this space.
	// 注释：译：这条线的斜率为sweepPagesPerByte，并通过一个基础点 (sweepHeapLiveBasis, pagesSweptBasis)。在任何给定的时刻，系统在这个空间中处于 (memstats.heap_live, pagesSwept) 的位置。
	//
	// It's important that the line pass through a point we
	// control rather than simply starting at a (0,0) origin
	// because that lets us adjust sweep pacing at any time while
	// accounting for current progress. If we could only adjust
	// the slope, it would create a discontinuity in debt if any
	// progress has already been made.
	// 注释：译：重要的是，该线条通过我们控制的一个点而不是简单地从 (0,0) 原点开始，因为这样可以让我们在任何时候调整扫描步调，同时考虑当前的进展。
	//		如果我们只能调整斜率，那么如果已经取得了任何进展，将会产生债务上的不连续性。
	pagesInUse         uint64  // 注释：stats 结构中的 mSpanInUse 字段中跟踪的 span 页数；这些页数是以原子方式更新的。 // pages of spans in stats mSpanInUse; updated atomically
	pagesSwept         uint64  // 注释：清理的页的数量；以原子方式更新 // pages swept this cycle; updated atomically
	pagesSweptBasis    uint64  // 注释：（清扫扫描的基点）用作扫描比的原点；以原子方式更新 // pagesSwept to use as the origin of the sweep ratio; updated atomically
	sweepHeapLiveBasis uint64  // 注释：heap_live的值用作清扫比的基地址；写入带锁，读取不带锁 // value of heap_live to use as the origin of sweep ratio; written with lock, read without
	sweepPagesPerByte  float64 // 注释：GC触发清扫的比例，0表示不触发，大于0则是比例值，写入带锁，读取不带锁 // proportional sweep ratio; written with lock, read without
	// TODO(austin): pagesInUse should be a uintptr, but the 386
	// compiler can't 8-byte align fields.

	// scavengeGoal is the amount of total retained heap memory (measured by
	// heapRetained) that the runtime will try to maintain by returning memory
	// to the OS.
	// 注释：scavengeGoal 是指运行时将尝试通过将内存返回给操作系统来维持的总保留堆内存量（由 heapRetained 衡量）。
	scavengeGoal uint64

	// Page reclaimer state

	// reclaimIndex is the page index in allArenas of next page to
	// reclaim. Specifically, it refers to page (i %
	// pagesPerArena) of arena allArenas[i / pagesPerArena].
	//
	// If this is >= 1<<63, the page reclaimer is done scanning
	// the page marks.
	//
	// This is accessed atomically.
	// 注释：译：reclainIndex是下一个要回收的页面的allArenas中的页面索引。具体来说，它是指arena(竞技场)allArenas[i/pagesPerArena]的页面（i%pagesPerArena）。
	//		如果这是>=1<<63，则页面回收器将完成对页面标记的扫描。这是以原子方式访问的。
	reclaimIndex uint64 // 注释：回收的下标
	// reclaimCredit is spare credit for extra pages swept. Since
	// the page reclaimer works in large chunks, it may reclaim
	// more than requested. Any spare pages released go to this
	// credit pool.
	// 注释：译：reclaimCredit是额外页page清理的备用信用。由于页回收器工作在大块中，它可能会回收比请求的更多的内容。释放的任何备用页都将进入此信用池。
	//
	// This is accessed atomically.
	reclaimCredit uintptr // 注释：回收信用，类似受保护缓冲区，如果回收的页数大于这个值时，超出的页数则进行回收，(可用的页的数量)

	// arenas is the heap arena map. It points to the metadata for
	// the heap for every arena frame of the entire usable virtual
	// address space.
	//
	// Use arenaIndex to compute indexes into this array.
	//
	// For regions of the address space that are not backed by the
	// Go heap, the arena map contains nil.
	//
	// Modifications are protected by mheap_.lock. Reads can be
	// performed without locking; however, a given entry can
	// transition from nil to non-nil at any time when the lock
	// isn't held. (Entries never transitions back to nil.)
	//
	// In general, this is a two-level mapping consisting of an L1
	// map and possibly many L2 maps. This saves space when there
	// are a huge number of arena frames. However, on many
	// platforms (even 64-bit), arenaL1Bits is 0, making this
	// effectively a single-level map. In this case, arenas[0]
	// will never be nil.
	// 注释：译：竞技场是堆竞技场地图。它指向整个可用虚拟地址空间的每个竞技场帧的堆的元数据。使用arenIndex计算此数组中的索引。
	//		对于没有Go堆支持的地址空间区域，竞技场映射包含nil。修改受mheap_.lock保护。可以在不锁定的情况下执行读取；
	//		但是，给定的条目可以在未持有锁的任何时候从nil转换为non-nil。（条目从不转换回零。）
	//		通常，这是由L1映射和可能的许多L2映射组成的两级映射。当有大量的竞技场框架时，这样可以节省空间。
	//		然而，在许多平台上（甚至是64位），arenaL1Bits是0，这实际上是一个单级映射。在这种情况下，arenas[0]永远不会为零。
	// 注释：arenas[1]*[1<<22]*heapArena ，每个heapArena是64M，所以这里管理了 64M << 22 = 256TB（Linux AMD64架构）
	arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena // 注释：arenas矩阵数据（指向虚拟地址空间）,所有arena都是从这个矩阵中获取的

	// heapArenaAlloc is pre-reserved space for allocating heapArena
	// objects. This is only used on 32-bit, where we pre-reserve
	// this space to avoid interleaving it with the heap itself.
	heapArenaAlloc linearAlloc

	// arenaHints is a list of addresses at which to attempt to
	// add more heap arenas. This is initially populated with a
	// set of general hint addresses, and grown with the bounds of
	// actual heap arena ranges.
	arenaHints *arenaHint

	// arena is a pre-reserved space for allocating heap arenas
	// (the actual arenas). This is only used on 32-bit.
	arena linearAlloc

	// allArenas is the arenaIndex of every mapped arena. This can
	// be used to iterate through the address space.
	// 注释：译：allArenas是每个映射竞技场的竞技场索引。这可以用于在地址空间中进行迭代。
	//
	// Access is protected by mheap_.lock. However, since this is
	// append-only and old backing arrays are never freed, it is
	// safe to acquire mheap_.lock, copy the slice header, and
	// then release mheap_.lock.
	// 注释：译：访问受到mheap_lock的保护。但是，由于这是仅追加的，并且永远不会释放旧的备份数组，因此可以安全地获取mheap_lock，复制切片标头，然后释放mheap_lock。
	allArenas []arenaIdx // 注释：全部的arena

	// sweepArenas is a snapshot of allArenas taken at the
	// beginning of the sweep cycle. This can be read safely by
	// simply blocking GC (by disabling preemption).
	// 注释：译：sweepArenas是在清理开始时对allArenas的快照。这可以通过简单地阻塞GC（通过禁用抢占）来安全地读取。
	sweepArenas []arenaIdx // 注释：(需要清理的arena)清理开始时对allArenas的快照

	// markArenas is a snapshot of allArenas taken at the beginning
	// of the mark cycle. Because allArenas is append-only, neither
	// this slice nor its contents will change during the mark, so
	// it can be read safely.
	// 注释：译：markArenas是在mark开始时对Arenas的快照。因为allArenas只是附加的，所以在标记过程中，这个切片及其内容都不会改变，所以可以安全地读取。
	markArenas []arenaIdx

	// curArena is the arena that the heap is currently growing
	// into. This should always be physPageSize-aligned.
	// 注释：curArena是堆目前正在增长的arena。这应该始终与physPageSize对齐。
	curArena struct {
		base, end uintptr // 注释：指示arena区首/尾地址
	}

	_ uint32 // ensure 64-bit alignment of central

	// central free lists for small size classes.
	// 注释：译：central是空闲的小型对象列表
	// the padding makes sure that the mcentrals are
	// spaced CacheLinePadSize bytes apart, so that each mcentral.lock
	// gets its own cache line.
	// 注释：译：填充确保mcentrals的CacheLinePadSize字节间隔开，这样每个mcentral.lock都有自己的缓存行
	// central is indexed by spanClass.
	//
	// 注释：中心缓存(mcentral)，存在在堆里(mheap)，大对象（对象ID=0）也会缓存到中心缓存里
	central [numSpanClasses]struct { // 注释：是class对象表的两倍(每种class对应的两个mcentral,分别表示已经被mcache缓存了和没有被mcache缓存)
		mcentral mcentral                                                                    // 注释：中心缓存mcentral，操作时需要对mheap加锁，包含【有空闲】和【无空闲】两个结构，每个结构包含GC【已清理】和【未清理】的两个span链表
		pad      [cpu.CacheLinePadSize - unsafe.Sizeof(mcentral{})%cpu.CacheLinePadSize]byte // 注释：用来数据对其
	}

	spanalloc             fixalloc // allocator for span*
	cachealloc            fixalloc // allocator for mcache*
	specialfinalizeralloc fixalloc // allocator for specialfinalizer*
	specialprofilealloc   fixalloc // allocator for specialprofile*
	speciallock           mutex    // lock for special record allocators.
	arenaHintAlloc        fixalloc // allocator for arenaHints

	unused *specialfinalizer // never set, just here to force the specialfinalizer type into DWARF
}

var mheap_ mheap

// A heapArena stores metadata for a heap arena. heapArenas are stored
// outside of the Go heap and accessed via the mheap_.arenas index.
// 注释：译：heapArena存储堆竞技场的元数据。heapArenas存储在Go堆之外，并通过mheap_arenas索引进行访问。
//
// 注释：一个arena的结构体，是对虚拟内存单元
//go:notinheap
type heapArena struct {
	// bitmap stores the pointer/scalar bitmap for the words in
	// this arena. See mbitmap.go for a description. Use the
	// heapBits type to access this.
	// 注释：译：位图存储该领域中单词的指针/标量位图。有关描述，请参见mbitmap.go。使用heapBits类型访问此。
	bitmap [heapArenaBitmapBytes]byte // 注释：arena对应的位图

	// spans maps from virtual address page ID within this arena to *mspan.
	// For allocated spans, their pages map to the span itself.
	// For free spans, only the lowest and highest pages map to the span itself.
	// Internal pages map to an arbitrary span.
	// For pages that have never been allocated, spans entries are nil.
	//
	// Modifications are protected by mheap.lock. Reads can be
	// performed without locking, but ONLY from indexes that are
	// known to contain in-use or stack spans. This means there
	// must not be a safe-point between establishing that an
	// address is live and looking it up in the spans array.
	// 注释：译：跨越从该竞技场内的虚拟地址页ID到*mspan的映射。对于已分配的跨度，其页面将映射到跨度本身。
	//		对于空闲跨度，只有最低和最高的页面映射到跨度本身。内部页面映射到任意范围。对于从未分配过的页面，跨度条目为零。
	//		修改受mheap.lock保护。可以在不锁定的情况下执行读取，但只能从已知包含在用或堆栈跨度的索引中执行。
	//		这意味着在确定地址为活动地址和在span数组中查找地址之间不能存在安全点。
	// 注释：每个arena存储page的数量是8192， (1<<26)/(1<<13)，64MB/8KB，(也就是说一个arena可以存储8KB个页(共64MB))
	spans [pagesPerArena]*mspan // 注释：所有的跨度类都是在一个连续的地址空间里，这里表示跨度类的相对地址空间的位置

	// pageInUse is a bitmap that indicates which spans are in
	// state mSpanInUse. This bitmap is indexed by page number,
	// but only the bit corresponding to the first page in each
	// span is used.
	//
	// Reads and writes are atomic.
	// 注释：译：pageInUse是一个位图，指示哪些跨度处于mSpanInUse状态。此位图按页码进行索引，但仅使用与每个跨度中的第一页相对应的位。
	//		读取和写入是原子的。
	pageInUse [pagesPerArena / 8]uint8 // 注释：已使用的页分组，(每个数组是8页)（LinuxAMD64下最大1024个组，共8192页）

	// pageMarks is a bitmap that indicates which spans have any
	// marked objects on them. Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically during marking. Reads are
	// non-atomic and lock-free since they only occur during
	// sweeping (and hence never race with writes).
	//
	// This is used to quickly find whole spans that can be freed.
	//
	// TODO(austin): It would be nice if this was uint64 for // 注释：译：如果这是uint64就太好了
	// faster scanning, but we don't have 64-bit atomic bit
	// operations.
	// 注释：译：pageMarks是一个位图，用于指示哪些跨度上有任何标记的对象。与pageInUse一样，只使用与每个跨度中的第一页相对应的位。
	//		在标记期间以原子方式进行写入。读取是非原子的且无锁的，因为它们只发生在扫描期间（因此从不与写入竞争）。
	//		这用于快速查找可以释放的整个跨度。更快的扫描，但我们没有64位原子位操作。
	pageMarks [pagesPerArena / 8]uint8

	// pageSpecials is a bitmap that indicates which spans have
	// specials (finalizers or other). Like pageInUse, only the bit
	// corresponding to the first page in each span is used.
	//
	// Writes are done atomically whenever a special is added to
	// a span and whenever the last special is removed from a span.
	// Reads are done atomically to find spans containing specials
	// during marking.
	// 注释：译：pageSpecials是一个位图，用于指示哪些跨度具有特殊值（终结器或其他）。与pageInUse一样，只使用与每个跨度中的第一页相对应的位。
	//		每当向跨度中添加特殊项以及从跨度中删除最后一个特殊项时，都会以原子方式进行写入。在标记过程中，以原子方式进行读取以查找包含特殊值的跨度。
	pageSpecials [pagesPerArena / 8]uint8

	// checkmarks stores the debug.gccheckmark state. It is only
	// used if debug.gccheckmark > 0.
	checkmarks *checkmarksMap

	// zeroedBase marks the first byte of the first page in this
	// arena which hasn't been used yet and is therefore already
	// zero. zeroedBase is relative to the arena base.
	// Increases monotonically until it hits heapArenaBytes.
	//
	// This field is sufficient to determine if an allocation
	// needs to be zeroed because the page allocator follows an
	// address-ordered first-fit policy.
	//
	// Read atomically and written with an atomic CAS.
	zeroedBase uintptr
}

// arenaHint is a hint for where to grow the heap arenas. See
// mheap_.arenaHints.
//
//go:notinheap
type arenaHint struct {
	addr uintptr
	down bool
	next *arenaHint
}

// An mspan is a run of pages.
// 注释：译：mspan是一组页面。
//
// When a mspan is in the heap free treap, state == mSpanFree
// and heapmap(s->start) == span, heapmap(s->start+s->npages-1) == span.
// If the mspan is in the heap scav treap, then in addition to the
// above scavenged == true. scavenged == false in all other cases.
//
// When a mspan is allocated, state == mSpanInUse or mSpanManual
// and heapmap(i) == span for all s->start <= i < s->start+s->npages.

// Every mspan is in one doubly-linked list, either in the mheap's
// busy list or one of the mcentral's span lists.

// An mspan representing actual memory has state mSpanInUse,
// mSpanManual, or mSpanFree. Transitions between these states are
// constrained as follows:
//
//   - A span may transition from free to in-use or manual during any GC
//     phase.
//
//   - During sweeping (gcphase == _GCoff), a span may transition from
//     in-use to free (as a result of sweeping) or manual to free (as a
//     result of stacks being freed).
//
//   - During GC (gcphase != _GCoff), a span *must not* transition from
//     manual or in-use to free. Because concurrent GC may read a pointer
//     and then look up its span, the span state must be monotonic.
//
// Setting mspan.state to mSpanInUse or mSpanManual must be done
// atomically and only after all other span fields are valid.
// Likewise, if inspecting a span is contingent on it being
// mSpanInUse, the state should be loaded atomically and checked
// before depending on other fields. This allows the garbage collector
// to safely deal with potentially invalid pointers, since resolving
// such pointers may race with a span being allocated.
type mSpanState uint8 // 注释：span的状态（内存管理单元的状态）

// 注释：span的状态，一共有4中状态，这里第四中状态没有定义，第四中状态值为3，下面状态码映射名称的map里有定义
const (
	mSpanDead   mSpanState = iota // 注释：内存已回收，初始化时或将跨度span释放回堆中时设置
	mSpanInUse                    // 注释：【无空闲、已分配】 GC管理 // allocated for garbage collected heap
	mSpanManual                   // 注释：(手动)【无空闲、已分配】分配用于手动管理(GC不管理) // allocated for manual management (e.g., stack allocator)
)

// mSpanStateNames are the names of the span states, indexed by
// mSpanState.
// 注释：span状态名称：一共有4中状态
var mSpanStateNames = []string{
	"mSpanDead",   // 注释：内存已回收，初始化时或将跨度span释放回堆中时设置
	"mSpanInUse",  // 注释：【无空闲、已分配】
	"mSpanManual", // 注释：【无空闲、已分配】（手动）分配用于手动管理
	"mSpanFree",   // 注释：【有空闲、未分配】（在空闲堆中）
}

// mSpanStateBox holds an mSpanState and provides atomic operations on
// it. This is a separate type to disallow accidental comparison or
// assignment with mSpanState.
type mSpanStateBox struct {
	s mSpanState
}

func (b *mSpanStateBox) set(s mSpanState) {
	atomic.Store8((*uint8)(&b.s), uint8(s))
}

func (b *mSpanStateBox) get() mSpanState {
	return mSpanState(atomic.Load8((*uint8)(&b.s)))
}

// mSpanList heads a linked list of spans.
//
//go:notinheap
type mSpanList struct {
	first *mspan // first span in list, or nil if none
	last  *mspan // last span in list, or nil if none
}

// 注释：span是内存管理的基本单位,每个span用于管理特定的class对象, 跟据对象大小，span将一个或多个页拆分成多个块进行管理。
// 注释：span的结构体(双向链表结构)
//
//go:notinheap
type mspan struct {
	next *mspan     // 注释：列表中的下一个span，如果没有则为nil(用于将span链接起来) // next span in list, or nil if none
	prev *mspan     // 注释：列表中的前一个span，如果没有则为nil(用于将span链接起来) // previous span in list, or nil if none
	list *mSpanList // For debugging. TODO: Remove.

	startAddr uintptr // 注释：mpan基地址(起始地址，也即所管理页的地址) // address of first byte of span aka s.base()
	npages    uintptr // 注释：管理的页数 // number of pages in span

	manualFreeList gclinkptr // list of free objects in mSpanManual spans

	// freeindex is the slot index between 0 and nelems at which to begin scanning
	// for the next free object in this span.
	// 注释：freeindex是介于0和nelem之间的索引下标，在该下标处开始扫描此跨度中的下一个空闲对象。
	// Each allocation scans allocBits starting at freeindex until it encounters a 0
	// indicating a free object. freeindex is then adjusted so that subsequent scans begin
	// just past the newly discovered free object.
	// 注释：每个分配从freeindex开始扫描allocBits，直到它遇到一个指示空闲对象的0。然后调整freeindex，使得随后的扫描刚好在新发现的自由对象之后开始。
	//
	// If freeindex == nelem, this span has no free objects.
	// 注释：如果freeindex==nelem，则此跨度没有可用对象。
	//
	// allocBits is a bitmap of objects in this span.
	// If n >= freeindex and allocBits[n/8] & (1<<(n%8)) is 0
	// then object n is free;
	// otherwise, object n is allocated. Bits starting at nelem are
	// undefined and should never be referenced.
	// 注释：allocBits是此跨度中对象的位图。如果n>=freeindex并且allocBits[n/8] & (1<<(n%8))为0，则对象n是空闲的；否则，n已经分配对象了。从nelem开始的位是未定义的，永远不应该被引用。
	//
	// Object n starts at address n*elemsize + (start << pageShift).
	// 注释：对象n从地址 n*elemsize + (start << pageShift)
	// 注释：有效空闲下标范围是 0 <= freeindex < melems
	// 注释：当freeindex = melems时表span没有可用块（无空用内存）
	freeindex uintptr // 注释：空闲对象块的下标位置（下标范围是[0 - nelems]之间(包含nelems)）空闲内存地址是 s.freeindex * s.elemsize + s.base()
	// TODO: Look up nelems from sizeclass and remove this field if it
	// helps performance.
	// 注释：nelems对应class表(/src/runtime/sizeclasses.go)【objects】字段；【objects字段】 = 【bytes/span字段】 / 【bytes/obj字段】
	// 注释：【bytes/span字段】表示一个span的大小
	// 注释：【bytes/obj字段】表示span可以存放的对象大小
	// 注释：一共有68种span每个span有自己的块个数(classId=0除外，这个span是存放大对象的时候使用的)
	nelems uintptr // 注释：span中可容纳的总对象块数量，也即有多少个块可供分配（对应class表【objects】字段） (位置：/src/runtime/sizeclasses.go)// number of object in the span.

	// Cache of the allocBits at freeindex. allocCache is shifted
	// such that the lowest bit corresponds to the bit freeindex.
	// 注释：freeindex处allocBits的缓存。allocCache被移位，使得最低比特对应于比特空闲索引。
	// allocCache holds the complement of allocBits, thus allowing
	// ctz (count trailing zero) to use it directly.
	// 注释：allocCache保存allocBits的补码，因此允许ctz（计数尾随零）直接使用它。
	// allocCache may contain bits beyond s.nelems; the caller must ignore
	// these.
	// 注释：allocCache可能包含超出s.nelems的位；调用者必须忽略这些。
	// 注释：allocCache是allocBits的补码(是8个一组，每组前后调换位置，第一组放在allocCache的低8位上面)，为了方便ctz
	allocCache uint64 // 注释：(当前span的快速缓存)位图0已分配1空闲(默认1)(每一位控制一个span的1块，最大控制64块，每种sapn的块数量固定【objects】位置：/src/runtime/sizeclasses.go)。

	// allocBits and gcmarkBits hold pointers to a span's mark and
	// allocation bits. The pointers are 8 byte aligned.
	// There are three arenas where this data is held.
	// 注释：allocBits和gcmarkBits保存指向span's的标记和分配位的指针。指针是8字节对齐的。有三个领域可以保存这些数据。
	// free: Dirty arenas that are no longer accessed
	//       and can be reused.
	// next: Holds information to be used in the next GC cycle.
	// current: Information being used during this GC cycle.
	// previous: Information being used during the last GC cycle.
	// 注释：free：肮脏的arenas不再被访问，可以重复使用。
	// 		next：保存要在下一个GC周期中使用的信息。
	// 		current：此GC周期中使用的信息。
	// 		previous：在上一个GC周期中使用的信息。
	// A new GC cycle starts with the call to finishsweep_m.
	// finishsweep_m moves the previous arena to the free arena,
	// the current arena to the previous arena, and
	// the next arena to the current arena.
	// The next arena is populated as the spans request
	// memory to hold gcmarkBits for the next GC cycle as well
	// as allocBits for newly allocated spans.
	// 注释：一个新的GC循环从调用finishsweep_m开始。finishsweep_m将上一个arena移到free arena，将当前arena移到上一个，将下一个arena移动到当前arena。
	//		当跨度请求内存为下一个GC周期保存gcmarkBits以及为新分配的跨度保存allocBits时，会填充下一个arena。
	//
	// The pointer arithmetic is done "by hand" instead of using
	// arrays to avoid bounds checks along critical performance
	// paths.
	// The sweep will free the old allocBits and set allocBits to the
	// gcmarkBits. The gcmarkBits are replaced with a fresh zeroed
	// out memory.
	// 注释：指针运算是“手动”完成的，而不是使用数组来避免沿关键性能路径进行边界检查。扫描将释放旧的allocBits，并将allocBit设置为gcmarkBits。gcmarkBits将替换为新的清零内存。
	//
	// 注释：标记内存占用情况,8字节对齐的位图0未分配1已分配
	// 注释：会把其中64位补码放到缓存allocCache里方便ctz右尾零计算,所以allocCache里0表示已分配
	// 注释：连续数组空间首指针(对应的是一个uint8数组的首指针)(在申请时是个大的连续空间里截取出一段连续的gcBits空间)每8个字节一组，8字节对齐的位图
	// 注释：gcBits实际上是uint8类型，其中中每一位控制一个当前span的1块，，每种span的块数量固定【objects】位置：src/runtime/sizeclasses.go
	allocBits  *gcBits // 注释：块位图基地址,值是位图的前8位。当前span的所有块（每一位代表1块）
	gcmarkBits *gcBits // 注释：标记内存GC回收情况位图，和allocBits长度一致，1代表黑色（对象使用），0代表白色（空闲对象，待清理对象）

	// sweep generation:
	// if sweepgen == h->sweepgen - 2, the span needs sweeping
	// if sweepgen == h->sweepgen - 1, the span is currently being swept
	// if sweepgen == h->sweepgen, the span is swept and ready to use
	// if sweepgen == h->sweepgen + 1, the span was cached before sweep began and is still cached, and needs sweeping
	// if sweepgen == h->sweepgen + 3, the span was swept and then cached and is still cached
	// h->sweepgen is incremented by 2 after every GC
	// 注释：清理阶段
	// 如果sweepgen == h->sweepgen - 2，【需要清理、未缓存】span(跨度)需要清扫标记。(注释：理解为重新清扫一下，因为每次清扫完成后会自动加2)
	// 如果sweepgen == h->sweepgen - 1，【正在清理、未缓存】span(跨度)正在扫描标记。(注释：理解为重新清理过程中的状态，从新清理是-2,过程中会+1，所以状态为-1)
	// 如果sweepgen == h->sweepgen，    【已经清理、未缓存】span(跨度)清扫标记完成，准备使用。
	// 如果sweepgen == h->sweepgen + 1，【需要清理、已缓存】span(跨度)在扫描开始前在mcache缓存中，现在仍在mchche缓存中，需要扫描
	// 如果sweepgen == h->sweepgen + 3，【无需清理、已缓存】span(跨度)已经被扫描，并放到mcache缓存中
	// h->sweepgen在每次垃圾回收后会增加2。
	sweepgen    uint32        // 注释：向 mheap.sweepgen 标记看齐
	divMul      uint16        // for divide by elemsize - divMagic.mul
	baseMask    uint16        // 注释：译：如果不是0，elemsize是2的幂，这将获得对象分配基数 if non-0, elemsize is a power of 2, & this will get object allocation base
	allocCount  uint16        // 注释：已分配块的个数(分配的对象数) // number of allocated objects
	spanclass   spanClass     // 注释：span的ID(也叫做对象ID，对应【class】字段，位置：/src/runtime/sizeclasses.go) // size class and noscan (uint8)
	state       mSpanStateBox // 注释：span的状态 // mSpanInUse etc; accessed atomically (get/set methods)
	needzero    uint8         // 注释：需要在分配前归零(零填充)，1是0否 // needs to be zeroed before allocation
	divShift    uint8         // for divide by elemsize - divMagic.shift
	divShift2   uint8         // for divide by elemsize - divMagic.shift2
	elemsize    uintptr       // 注释：(块大小)存储的单个对象大小；(对应class表中的【bytes/obj】字段,地址:/src/runtime/sizeclasses.go) // computed from sizeclass or from npages
	limit       uintptr       // 注释：内存尾部地址(s.base() + sapn的对象大小*页数量) // end of data in span
	speciallock mutex         // guards specials list
	specials    *special      // 注释：译：按偏移量排序的特殊记录的链接列表。 // linked list of special records sorted by offset.
}

// 注释：获取span的基地址
func (s *mspan) base() uintptr {
	return s.startAddr
}

func (s *mspan) layout() (size, n, total uintptr) {
	total = s.npages << _PageShift
	size = s.elemsize
	if size > 0 {
		n = total / size
	}
	return
}

// recordspan adds a newly allocated span to h.allspans.
//
// This only happens the first time a span is allocated from
// mheap.spanalloc (it is not called when a span is reused).
//
// Write barriers are disallowed here because it can be called from
// gcWork when allocating new workbufs. However, because it's an
// indirect call from the fixalloc initializer, the compiler can't see
// this.
//
// The heap lock must be held.
//
//go:nowritebarrierrec
func recordspan(vh unsafe.Pointer, p unsafe.Pointer) {
	h := (*mheap)(vh)
	s := (*mspan)(p)

	assertLockHeld(&h.lock)

	if len(h.allspans) >= cap(h.allspans) {
		n := 64 * 1024 / sys.PtrSize
		if n < cap(h.allspans)*3/2 {
			n = cap(h.allspans) * 3 / 2
		}
		var new []*mspan
		sp := (*slice)(unsafe.Pointer(&new))
		sp.array = sysAlloc(uintptr(n)*sys.PtrSize, &memstats.other_sys)
		if sp.array == nil {
			throw("runtime: cannot allocate memory")
		}
		sp.len = len(h.allspans)
		sp.cap = n
		if len(h.allspans) > 0 {
			copy(new, h.allspans)
		}
		oldAllspans := h.allspans
		*(*notInHeapSlice)(unsafe.Pointer(&h.allspans)) = *(*notInHeapSlice)(unsafe.Pointer(&new))
		if len(oldAllspans) != 0 {
			sysFree(unsafe.Pointer(&oldAllspans[0]), uintptr(cap(oldAllspans))*unsafe.Sizeof(oldAllspans[0]), &memstats.other_sys)
		}
	}
	h.allspans = h.allspans[:len(h.allspans)+1]
	h.allspans[len(h.allspans)-1] = s
}

// A spanClass represents the size class and noscan-ness of a span.
// 注释：spanClass表示跨度的大小等级和范围。
//
// Each size class has a noscan spanClass and a scan spanClass. The
// noscan spanClass contains only noscan objects, which do not contain
// pointers and thus do not need to be scanned by the garbage
// collector.
// 注释：每个尺寸类都有一个noscan spanClass和一个scan spanClass。noscan spanClass只包含noscan对象，这些对象不包含指针，因此不需要垃圾收集器扫描。
// 注释：spanClass是class对象的ID目前是0到67，一共有68个span个对象分类，spanClass代表每个分类的ID（classId）
type spanClass uint8

const (
	numSpanClasses = _NumSizeClasses << 1 // 注释：是class对象列表的两倍
	tinySpanClass  = spanClass(tinySizeClass<<1 | 1)
)

func makeSpanClass(sizeclass uint8, noscan bool) spanClass {
	return spanClass(sizeclass<<1) | spanClass(bool2int(noscan))
}

func (sc spanClass) sizeclass() int8 {
	return int8(sc >> 1)
}

func (sc spanClass) noscan() bool {
	return sc&1 != 0
}

// arenaIndex returns the index into mheap_.arenas of the arena
// containing metadata for p. This index combines of an index into the
// L1 map and an index into the L2 map and should be used as
// mheap_.arenas[ai.l1()][ai.l2()].
//
// If p is outside the range of valid heap addresses, either l1() or
// l2() will be out of bounds.
//
// It is nosplit because it's called by spanOf and several other
// nosplit functions.
// 注释：译：arenaIndex将索引返回到包含p元数据的竞技场的mheap_arenas中。该索引结合了L1地图的索引和L2地图的索引，应用作mheap_.renas[ai.l1()][ai.L2()]。
//		如果p在有效堆地址的范围之外，则l1（）或l2（）将越界。它是nosplit，因为它是由spanOf和其他几个nosplit函数调用的。
//
// 注释：arena二维矩阵的组合下标（返回arena的正数倍数）
// 注释：arenaIdx的高位（L1），低位（L2），用来表示：mheap_.arenas[L1][L2]，用一个整型表示二维矩阵的组合下标
// 注释：低位(L1)是低22位,高位(L2)是arenaIdx>>22，每个架构不同这里是Linux AMD64架构
// 注释：入参span.base()基地址，返回arena的下标
//go:nosplit
func arenaIndex(p uintptr) arenaIdx {
	return arenaIdx((p - arenaBaseOffset) / heapArenaBytes) // 注释：返回arena的正数倍数
}

// arenaBase returns the low address of the region covered by heap
// arena i.
func arenaBase(i arenaIdx) uintptr {
	return uintptr(i)*heapArenaBytes + arenaBaseOffset
}

type arenaIdx uint // 注释：arena二维矩阵的组合下标，arenaIdx的高位（L1），低位（L2）用来表示：mheap_.arenas[L1][L2]，用一个整型表示二维矩阵的组合下标

// 注释：l1是高位，返回：arenaIdx >> 22位(Linux AMD64架构)
// 注释：mheap_.arenas[l1][l2]
func (i arenaIdx) l1() uint {
	if arenaL1Bits == 0 {
		// Let the compiler optimize this away if there's no
		// L1 map.
		return 0
	} else {
		return uint(i) >> arenaL1Shift
	}
}

// 注释：l2是低位，返回：arenaIdx的低22位(Linux AMD64架构)
// 注释：mheap_.arenas[l1][l2]
func (i arenaIdx) l2() uint {
	if arenaL1Bits == 0 {
		return uint(i)
	} else {
		return uint(i) & (1<<arenaL2Bits - 1)
	}
}

// inheap reports whether b is a pointer into a (potentially dead) heap object.
// It returns false for pointers into mSpanManual spans.
// Non-preemptible because it is used by write barriers.
//
//go:nowritebarrier
//go:nosplit
func inheap(b uintptr) bool {
	return spanOfHeap(b) != nil
}

// inHeapOrStack is a variant of inheap that returns true for pointers
// into any allocated heap span.
//
//go:nowritebarrier
//go:nosplit
func inHeapOrStack(b uintptr) bool {
	s := spanOf(b)
	if s == nil || b < s.base() {
		return false
	}
	switch s.state.get() {
	case mSpanInUse, mSpanManual:
		return b < s.limit
	default:
		return false
	}
}

// spanOf returns the span of p. If p does not point into the heap
// arena or no span has ever contained p, spanOf returns nil.
//
// If p does not point to allocated memory, this may return a non-nil
// span that does *not* contain p. If this is a possibility, the
// caller should either call spanOfHeap or check the span bounds
// explicitly.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOf(p uintptr) *mspan {
	// This function looks big, but we use a lot of constant
	// folding around arenaL1Bits to get it under the inlining
	// budget. Also, many of the checks here are safety checks
	// that Go needs to do anyway, so the generated code is quite
	// short.
	ri := arenaIndex(p)
	if arenaL1Bits == 0 {
		// If there's no L1, then ri.l1() can't be out of bounds but ri.l2() can.
		if ri.l2() >= uint(len(mheap_.arenas[0])) {
			return nil
		}
	} else {
		// If there's an L1, then ri.l1() can be out of bounds but ri.l2() can't.
		if ri.l1() >= uint(len(mheap_.arenas)) {
			return nil
		}
	}
	l2 := mheap_.arenas[ri.l1()]
	if arenaL1Bits != 0 && l2 == nil { // Should never happen if there's no L1.
		return nil
	}
	ha := l2[ri.l2()]
	if ha == nil {
		return nil
	}
	return ha.spans[(p/pageSize)%pagesPerArena]
}

// spanOfUnchecked is equivalent to spanOf, but the caller must ensure
// that p points into an allocated heap arena.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfUnchecked(p uintptr) *mspan {
	ai := arenaIndex(p)
	return mheap_.arenas[ai.l1()][ai.l2()].spans[(p/pageSize)%pagesPerArena]
}

// spanOfHeap is like spanOf, but returns nil if p does not point to a
// heap object.
//
// Must be nosplit because it has callers that are nosplit.
//
//go:nosplit
func spanOfHeap(p uintptr) *mspan {
	s := spanOf(p)
	// s is nil if it's never been allocated. Otherwise, we check
	// its state first because we don't trust this pointer, so we
	// have to synchronize with span initialization. Then, it's
	// still possible we picked up a stale span pointer, so we
	// have to check the span's bounds.
	if s == nil || s.state.get() != mSpanInUse || p < s.base() || p >= s.limit {
		return nil
	}
	return s
}

// pageIndexOf returns the arena, page index, and page mask for pointer p.
// The caller must ensure p is in the heap.
// 注释：入参span基地址，返回：arena *heapArena, pageIdx:页的偏移量, pageMask:页的位图标记
func pageIndexOf(p uintptr) (arena *heapArena, pageIdx uintptr, pageMask uint8) {
	ai := arenaIndex(p)                                            // 注释：获取arena下标
	arena = mheap_.arenas[ai.l1()][ai.l2()]                        // 注释：获取arena(通过arena下标获取arena)
	pageIdx = ((p / pageSize) / 8) % uintptr(len(arena.pageInUse)) // 注释：页组下标（页组偏移量）
	pageMask = byte(1 << ((p / pageSize) % 8))                     // 注释：页每组下的位图标记(后期会atomic.Or8(&arena.pageMarks[pageIdx], pageMask))
	return
}

// Initialize the heap.
func (h *mheap) init() {
	lockInit(&h.lock, lockRankMheap)
	lockInit(&h.speciallock, lockRankMheapSpecial)

	h.spanalloc.init(unsafe.Sizeof(mspan{}), recordspan, unsafe.Pointer(h), &memstats.mspan_sys)
	h.cachealloc.init(unsafe.Sizeof(mcache{}), nil, nil, &memstats.mcache_sys)
	h.specialfinalizeralloc.init(unsafe.Sizeof(specialfinalizer{}), nil, nil, &memstats.other_sys)
	h.specialprofilealloc.init(unsafe.Sizeof(specialprofile{}), nil, nil, &memstats.other_sys)
	h.arenaHintAlloc.init(unsafe.Sizeof(arenaHint{}), nil, nil, &memstats.other_sys)

	// Don't zero mspan allocations. Background sweeping can
	// inspect a span concurrently with allocating it, so it's
	// important that the span's sweepgen survive across freeing
	// and re-allocating a span to prevent background sweeping
	// from improperly cas'ing it from 0.
	//
	// This is safe because mspan contains no heap pointers.
	h.spanalloc.zero = false

	// h->mapcache needs no init

	for i := range h.central {
		h.central[i].mcentral.init(spanClass(i))
	}

	h.pages.init(&h.lock, &memstats.gcMiscSys)
}

// reclaim sweeps and reclaims at least npage pages into the heap.
// It is called before allocating npage pages to keep growth in check.
// 注释：译：reclaim将至少npage页扫掠并回收到堆中。它是在分配npage页面之前调用的，以控制增长。
//
// reclaim implements the page-reclaimer half of the sweeper.
// 注释：译：reclaim实现了页面回收器的一半。
//
// h.lock must NOT be held.
//
// 注释：回收内存
// 注释：参数npage需要回收页的数量
// 注释：步骤
// 		1.加锁禁止抢占
// 		2.获取需要清理的arena
// 		3.遍历页数量，逐个执行回收
// 			(1).跳过信用页数（受保护的页数量）
// 			(2).
// 			(3).
// 			(4).
// 注释：【ing】
func (h *mheap) reclaim(npage uintptr) {
	// TODO(austin): Half of the time spent freeing spans is in
	// locking/unlocking the heap (even with low contention). We
	// could make the slow path here several times faster by
	// batching heap frees.

	// Bail early if there's no more reclaim work.
	if atomic.Load64(&h.reclaimIndex) >= 1<<63 {
		return
	}

	// Disable preemption so the GC can't start while we're
	// sweeping, so we can read h.sweepArenas, and so
	// traceGCSweepStart/Done pair on the P.
	mp := acquirem() // 注释：获取M加锁禁止抢占

	if trace.enabled { // 注释：如果链路追踪开启
		traceGCSweepStart() // 注释：启动GC清理是链路追踪
	}

	arenas := h.sweepArenas // 注释：获取需要回收的arena
	locked := false
	for npage > 0 { // 注释：遍历页数量，逐个执行
		// Pull from accumulated credit first.
		// 注释：译：先从累积的信贷中提取
		// 注释：回收信用，类似受保护缓冲区，如果回收的页数大于这个值时，超出的页数则进行回收，(可用的页的数量)
		if credit := atomic.Loaduintptr(&h.reclaimCredit); credit > 0 { // 注释：获取信用缓冲区页数，回收页数超出这个时则超出的进行回收
			take := credit    // 注释：待回收的页数
			if take > npage { // 注释：如果待回收页数量大于当前页总数，则拿走当前页大小的数量
				// Take only what we need.
				take = npage // 注释：重置拿出的页数量
			}
			if atomic.Casuintptr(&h.reclaimCredit, credit, credit-take) { // 注释：修改待回收页数数量
				npage -= take // 注释：跳过信用页数量(受保护的页数量)，超出的才可以回收
			}
			continue
		}

		// Claim a chunk of work.
		// 注释：译：索赔一大块工作。
		// 注释：(idx是&h.reclaimIndex的旧值)idx = &h.reclaimIndex; &h.reclaimIndex += pagesPerReclaimerChunk
		idx := uintptr(atomic.Xadd64(&h.reclaimIndex, pagesPerReclaimerChunk) - pagesPerReclaimerChunk) // 注释：获取回收下标后，重置下标，原子操作
		if idx/pagesPerArena >= uintptr(len(arenas)) {
			// Page reclaiming is done.
			atomic.Store64(&h.reclaimIndex, 1<<63)
			break
		}

		if !locked {
			// Lock the heap for reclaimChunk.
			lock(&h.lock)
			locked = true
		}

		// Scan this chunk.
		nfound := h.reclaimChunk(arenas, idx, pagesPerReclaimerChunk)
		if nfound <= npage {
			npage -= nfound
		} else {
			// Put spare pages toward global credit.
			atomic.Xadduintptr(&h.reclaimCredit, nfound-npage)
			npage = 0
		}
	}
	if locked {
		unlock(&h.lock)
	}

	if trace.enabled {
		traceGCSweepDone()
	}
	releasem(mp)
}

// reclaimChunk sweeps unmarked spans that start at page indexes [pageIdx, pageIdx+n).
// It returns the number of pages returned to the heap.
//
// h.lock must be held and the caller must be non-preemptible. Note: h.lock may be
// temporarily unlocked and re-locked in order to do sweeping or if tracing is
// enabled.
func (h *mheap) reclaimChunk(arenas []arenaIdx, pageIdx, n uintptr) uintptr {
	// The heap lock must be held because this accesses the
	// heapArena.spans arrays using potentially non-live pointers.
	// In particular, if a span were freed and merged concurrently
	// with this probing heapArena.spans, it would be possible to
	// observe arbitrary, stale span pointers.
	assertLockHeld(&h.lock)

	n0 := n
	var nFreed uintptr
	sg := h.sweepgen
	for n > 0 {
		ai := arenas[pageIdx/pagesPerArena]
		ha := h.arenas[ai.l1()][ai.l2()]

		// Get a chunk of the bitmap to work on.
		arenaPage := uint(pageIdx % pagesPerArena)
		inUse := ha.pageInUse[arenaPage/8:]
		marked := ha.pageMarks[arenaPage/8:]
		if uintptr(len(inUse)) > n/8 {
			inUse = inUse[:n/8]
			marked = marked[:n/8]
		}

		// Scan this bitmap chunk for spans that are in-use
		// but have no marked objects on them.
		for i := range inUse {
			inUseUnmarked := atomic.Load8(&inUse[i]) &^ marked[i]
			if inUseUnmarked == 0 {
				continue
			}

			for j := uint(0); j < 8; j++ {
				if inUseUnmarked&(1<<j) != 0 {
					s := ha.spans[arenaPage+uint(i)*8+j]
					if atomic.Load(&s.sweepgen) == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) {
						npages := s.npages
						unlock(&h.lock)
						if s.sweep(false) {
							nFreed += npages
						}
						lock(&h.lock)
						// Reload inUse. It's possible nearby
						// spans were freed when we dropped the
						// lock and we don't want to get stale
						// pointers from the spans array.
						inUseUnmarked = atomic.Load8(&inUse[i]) &^ marked[i]
					}
				}
			}
		}

		// Advance.
		pageIdx += uintptr(len(inUse) * 8)
		n -= uintptr(len(inUse) * 8)
	}
	if trace.enabled {
		unlock(&h.lock)
		// Account for pages scanned but not reclaimed.
		traceGCSweepSpan((n0 - nFreed) * pageSize)
		lock(&h.lock)
	}

	assertLockHeld(&h.lock) // Must be locked on return.
	return nFreed
}

// spanAllocType represents the type of allocation to make, or
// the type of allocation to be freed.
type spanAllocType uint8

const (
	spanAllocHeap          spanAllocType = iota // heap span
	spanAllocStack                              // stack span
	spanAllocPtrScalarBits                      // unrolled GC prog bitmap span
	spanAllocWorkBuf                            // work buf span
)

// manual returns true if the span allocation is manually managed.
func (s spanAllocType) manual() bool {
	return s != spanAllocHeap
}

// alloc allocates a new span of npage pages from the GC'd heap.
//
// spanclass indicates the span's size class and scannability.
//
// If needzero is true, the memory for the returned span will be zeroed.
// 注释：译：alloc从GC的堆中分配一个新的npage页面跨度。panclass指示跨度的大小类和可扫描性。如果needzero为true，则返回跨度的内存将为零。
// 注释：申请内存，npages 页数, spanclass 对象ID（包含是否不需要扫描标识）, needzero 是否0填充，返回新的span
// 注释：步骤
// 		1.系统栈执行
// 		2.如果存在没有清理的数据时，执行回收动作
// 		3.分配内存
// 		4.如果需要0填充，则0填充初始化内存
func (h *mheap) alloc(npages uintptr, spanclass spanClass, needzero bool) *mspan {
	// Don't do any operations that lock the heap on the G stack.
	// It might trigger stack growth, and the stack growth code needs
	// to be able to allocate heap.
	var s *mspan
	systemstack(func() { // 注释：切换到系统栈执行
		// To prevent excessive heap growth, before allocating n pages
		// we need to sweep and reclaim at least n pages.
		if h.sweepdone == 0 { // 注释：如果存在没有清理的的数据
			h.reclaim(npages) // 注释：回收内存(如果存在没有清理的数据时需要回收内存，重新分配)
		}
		s = h.allocSpan(npages, spanAllocHeap, spanclass) // 注释：申请（分配）内存
	})

	if s != nil {
		if needzero && s.needzero != 0 { // 注释：如果需要0填充，并且span.needzero同时也需要0填充时，执行0填充
			memclrNoHeapPointers(unsafe.Pointer(s.base()), s.npages<<_PageShift) // 注释：初始化内存，0填充，汇编执行
		}
		s.needzero = 0 // 注释：需要在分配前归零(零填充)，1是0否
	}
	return s
}

// allocManual allocates a manually-managed span of npage pages.
// allocManual returns nil if allocation fails.
//
// allocManual adds the bytes used to *stat, which should be a
// memstats in-use field. Unlike allocations in the GC'd heap, the
// allocation does *not* count toward heap_inuse or heap_sys.
//
// The memory backing the returned span may not be zeroed if
// span.needzero is set.
//
// allocManual must be called on the system stack because it may
// acquire the heap lock via allocSpan. See mheap for details.
//
// If new code is written to call allocManual, do NOT use an
// existing spanAllocType value and instead declare a new one.
//
//go:systemstack
func (h *mheap) allocManual(npages uintptr, typ spanAllocType) *mspan {
	if !typ.manual() {
		throw("manual span allocation called with non-manually-managed type")
	}
	return h.allocSpan(npages, typ, 0)
}

// setSpans modifies the span map so [spanOf(base), spanOf(base+npage*pageSize))
// is s.
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
	p := base / pageSize
	ai := arenaIndex(base)
	ha := h.arenas[ai.l1()][ai.l2()]
	for n := uintptr(0); n < npage; n++ {
		i := (p + n) % pagesPerArena
		if i == 0 {
			ai = arenaIndex(base + n*pageSize)
			ha = h.arenas[ai.l1()][ai.l2()]
		}
		ha.spans[i] = s
	}
}

// allocNeedsZero checks if the region of address space [base, base+npage*pageSize),
// assumed to be allocated, needs to be zeroed, updating heap arena metadata for
// future allocations.
//
// This must be called each time pages are allocated from the heap, even if the page
// allocator can otherwise prove the memory it's allocating is already zero because
// they're fresh from the operating system. It updates heapArena metadata that is
// critical for future page allocations.
//
// There are no locking constraints on this method.
func (h *mheap) allocNeedsZero(base, npage uintptr) (needZero bool) {
	for npage > 0 {
		ai := arenaIndex(base)
		ha := h.arenas[ai.l1()][ai.l2()]

		zeroedBase := atomic.Loaduintptr(&ha.zeroedBase)
		arenaBase := base % heapArenaBytes
		if arenaBase < zeroedBase {
			// We extended into the non-zeroed part of the
			// arena, so this region needs to be zeroed before use.
			//
			// zeroedBase is monotonically increasing, so if we see this now then
			// we can be sure we need to zero this memory region.
			//
			// We still need to update zeroedBase for this arena, and
			// potentially more arenas.
			needZero = true
		}
		// We may observe arenaBase > zeroedBase if we're racing with one or more
		// allocations which are acquiring memory directly before us in the address
		// space. But, because we know no one else is acquiring *this* memory, it's
		// still safe to not zero.

		// Compute how far into the arena we extend into, capped
		// at heapArenaBytes.
		arenaLimit := arenaBase + npage*pageSize
		if arenaLimit > heapArenaBytes {
			arenaLimit = heapArenaBytes
		}
		// Increase ha.zeroedBase so it's >= arenaLimit.
		// We may be racing with other updates.
		for arenaLimit > zeroedBase {
			if atomic.Casuintptr(&ha.zeroedBase, zeroedBase, arenaLimit) {
				break
			}
			zeroedBase = atomic.Loaduintptr(&ha.zeroedBase)
			// Sanity check zeroedBase.
			if zeroedBase <= arenaLimit && zeroedBase > arenaBase {
				// The zeroedBase moved into the space we were trying to
				// claim. That's very bad, and indicates someone allocated
				// the same region we did.
				throw("potentially overlapping in-use allocations detected")
			}
		}

		// Move base forward and subtract from npage to move into
		// the next arena, or finish.
		base += arenaLimit - arenaBase
		npage -= (arenaLimit - arenaBase) / pageSize
	}
	return
}

// tryAllocMSpan attempts to allocate an mspan object from
// the P-local cache, but may fail.
//
// h.lock need not be held.
//
// This caller must ensure that its P won't change underneath
// it during this function. Currently to ensure that we enforce
// that the function is run on the system stack, because that's
// the only place it is used now. In the future, this requirement
// may be relaxed if its use is necessary elsewhere.
//
//go:systemstack
func (h *mheap) tryAllocMSpan() *mspan {
	pp := getg().m.p.ptr()
	// If we don't have a p or the cache is empty, we can't do
	// anything here.
	// 注释：如果缓存里没有空间则返回nil，后面会根据nil来装在缓存
	if pp == nil || pp.mspancache.len == 0 {
		return nil
	}
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// allocMSpanLocked allocates an mspan object.
//
// h.lock must be held.
//
// allocMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
//go:systemstack
func (h *mheap) allocMSpanLocked() *mspan {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	if pp == nil {
		// We don't have a p so just do the normal thing.
		return (*mspan)(h.spanalloc.alloc()) // 注释：如果没有p则直接分配内存
	}
	// 注释：下面是有p的情况，会缓存内存到p里，拿出缓存最后一个并返回
	// Refill the cache if necessary.
	if pp.mspancache.len == 0 {
		const refillCount = len(pp.mspancache.buf) / 2
		for i := 0; i < refillCount; i++ {
			pp.mspancache.buf[i] = (*mspan)(h.spanalloc.alloc())
		}
		pp.mspancache.len = refillCount
	}
	// Pull off the last entry in the cache.
	s := pp.mspancache.buf[pp.mspancache.len-1]
	pp.mspancache.len--
	return s
}

// freeMSpanLocked free an mspan object.
//
// h.lock must be held.
//
// freeMSpanLocked must be called on the system stack because
// its caller holds the heap lock. See mheap for details.
// Running on the system stack also ensures that we won't
// switch Ps during this function. See tryAllocMSpan for details.
//
//go:systemstack
func (h *mheap) freeMSpanLocked(s *mspan) {
	assertLockHeld(&h.lock)

	pp := getg().m.p.ptr()
	// First try to free the mspan directly to the cache.
	if pp != nil && pp.mspancache.len < len(pp.mspancache.buf) {
		pp.mspancache.buf[pp.mspancache.len] = s
		pp.mspancache.len++
		return
	}
	// Failing that (or if we don't have a p), just free it to
	// the heap.
	h.spanalloc.free(unsafe.Pointer(s))
}

// allocSpan allocates an mspan which owns npages worth of memory.
//
// If typ.manual() == false, allocSpan allocates a heap span of class spanclass
// and updates heap accounting. If manual == true, allocSpan allocates a
// manually-managed span (spanclass is ignored), and the caller is
// responsible for any accounting related to its use of the span. Either
// way, allocSpan will atomically add the bytes in the newly allocated
// span to *sysStat.
//
// The returned span is fully initialized.
//
// h.lock must not be held.
//
// allocSpan must be called on the system stack both because it acquires
// the heap lock and because it must block GC transitions.
//
// 注释：【ing】
// 注释：申请内存，首先到p的pageCache里获取，再去mheap的pageData里获取
// 注释：步骤：
// 		1.如果申请小内存，先去p.pcache缓存里找（就是pageCache）（64*8/6）
// 			a.如果缓存为空则从新装填缓存后获取(装填是从全局的mheap里的获取，所以需要加锁)
// 		3.
// 		4.
// 		5.
//go:systemstack
func (h *mheap) allocSpan(npages uintptr, typ spanAllocType, spanclass spanClass) (s *mspan) {
	// Function-global state.
	gp := getg()                         // 注释：获取当前运行的g
	base, scav := uintptr(0), uintptr(0) // 注释：初始化基地址和已清理的位图

	// On some platforms we need to provide physical page aligned stack
	// allocations. Where the page size is less than the physical page
	// size, we already manage to do this by default.
	needPhysPageAlign := physPageAlignedStacks && typ == spanAllocStack && pageSize < physPageSize

	// If the allocation is small enough, try the page cache!
	// The page cache does not support aligned allocations, so we cannot use
	// it if we need to provide a physical page aligned stack allocation.
	pp := gp.m.p.ptr() // 注释：获取P
	// 注释：尝试从p的缓存中获取内存
	if !needPhysPageAlign && pp != nil && npages < pageCachePages/4 {
		c := &pp.pcache

		// If the cache is empty, refill it.
		if c.empty() { // 注释：如果缓存为空则从新装填(装填是从全局的mheap里的获取，所以需要加锁)
			lock(&h.lock)               // 注释：加锁
			*c = h.pages.allocToCache() // 注释：装填缓存（从全局的mheap里的获取装填到p.pcache里）
			unlock(&h.lock)             // 注释：解锁
		}

		// Try to allocate from the cache.
		base, scav = c.alloc(npages) // 注释：从p.pcache里获取内存,返回基地址偏移的实际内存地址和已经清理的地址
		if base != 0 {               // 注释：如果有值说明：缓存区里有可用的内存
			// 注释：如果缓存里没有空间则返回nil，后面会根据nil来装在缓存
			s = h.tryAllocMSpan() // 注释：尝试在缓存里获取内存(s是最终返回的变量)
			if s != nil {         // 注释：如果有值说明已经找分配的内存地址(s是最终返回的变量)
				goto HaveSpan // 注释：已经获取内存地址，前去处理其他操作
			}
			// We have a base but no mspan, so we need
			// to lock the heap.
		}
	}

	// 注释：下面是从全局的mheap里获取内存
	// For one reason or another, we couldn't get the
	// whole job done without the heap lock.
	lock(&h.lock) // 注释：从全局mheap里获取内存，需要加锁

	if needPhysPageAlign {
		// Overallocate by a physical page to allow for later alignment.
		npages += physPageSize / pageSize
	}

	// 注释：从全局的mheap里获取基地址的偏移量
	// 注释：在mheap里的定位要获取的内存对应的地址
	if base == 0 {
		// Try to acquire a base address.
		base, scav = h.pages.alloc(npages) // 注释：尝试从mheap里获取基地址的偏移量，和已经清理的地址
		if base == 0 {                     // 注释：如果没有获取到，说明需要扩容
			if !h.grow(npages) { // 注释：扩容mheap内存
				unlock(&h.lock) // 注释：如果扩容失败，则解锁并返回nil
				return nil
			}
			base, scav = h.pages.alloc(npages) // 注释：扩容完成后重新从mheap里获取基地址的偏移量，和已经清理的地址
			if base == 0 {                     // 注释：如果依然没有获取到内存则立刻报错
				throw("grew heap, but no adequate free space found")
			}
		}
	}
	// 注释：如果是nil说明没有开始获取内存存或者缓存为空，则从mheap里获取内存，并且装到缓存里
	if s == nil {
		// We failed to get an mspan earlier, so grab
		// one now that we have the heap lock.
		s = h.allocMSpanLocked()
	}

	if needPhysPageAlign {
		allocBase, allocPages := base, npages
		base = alignUp(allocBase, physPageSize)
		npages -= physPageSize / pageSize

		// Return memory around the aligned allocation.
		spaceBefore := base - allocBase
		if spaceBefore > 0 {
			h.pages.free(allocBase, spaceBefore/pageSize)
		}
		spaceAfter := (allocPages-npages)*pageSize - spaceBefore
		if spaceAfter > 0 {
			h.pages.free(base+npages*pageSize, spaceAfter/pageSize)
		}
	}

	unlock(&h.lock)

	// 注释：已经获得要分配的内存地址和已经清理的地址
	// 注释：此时准备开始分配内存
HaveSpan:
	// At this point, both s != nil and base != 0, and the heap
	// lock is no longer held. Initialize the span.
	s.init(base, npages)
	if h.allocNeedsZero(base, npages) {
		s.needzero = 1
	}
	nbytes := npages * pageSize
	if typ.manual() {
		s.manualFreeList = 0
		s.nelems = 0
		s.limit = s.base() + s.npages*pageSize
		s.state.set(mSpanManual)
	} else {
		// We must set span properties before the span is published anywhere
		// since we're not holding the heap lock.
		s.spanclass = spanclass
		if sizeclass := spanclass.sizeclass(); sizeclass == 0 {
			s.elemsize = nbytes
			s.nelems = 1

			s.divShift = 0
			s.divMul = 0
			s.divShift2 = 0
			s.baseMask = 0
		} else {
			s.elemsize = uintptr(class_to_size[sizeclass])
			s.nelems = nbytes / s.elemsize

			m := &class_to_divmagic[sizeclass]
			s.divShift = m.shift
			s.divMul = m.mul
			s.divShift2 = m.shift2
			s.baseMask = m.baseMask
		}

		// Initialize mark and allocation structures.
		s.freeindex = 0
		s.allocCache = ^uint64(0) // all 1s indicating all free.
		s.gcmarkBits = newMarkBits(s.nelems)
		s.allocBits = newAllocBits(s.nelems)

		// It's safe to access h.sweepgen without the heap lock because it's
		// only ever updated with the world stopped and we run on the
		// systemstack which blocks a STW transition.
		atomic.Store(&s.sweepgen, h.sweepgen)

		// Now that the span is filled in, set its state. This
		// is a publication barrier for the other fields in
		// the span. While valid pointers into this span
		// should never be visible until the span is returned,
		// if the garbage collector finds an invalid pointer,
		// access to the span may race with initialization of
		// the span. We resolve this race by atomically
		// setting the state after the span is fully
		// initialized, and atomically checking the state in
		// any situation where a pointer is suspect.
		s.state.set(mSpanInUse)
	}

	// Commit and account for any scavenged memory that the span now owns.
	if scav != 0 {
		// sysUsed all the pages that are actually available
		// in the span since some of them might be scavenged.
		sysUsed(unsafe.Pointer(base), nbytes)
		atomic.Xadd64(&memstats.heap_released, -int64(scav))
	}
	// Update stats.
	if typ == spanAllocHeap {
		atomic.Xadd64(&memstats.heap_inuse, int64(nbytes))
	}
	if typ.manual() {
		// Manually managed memory doesn't count toward heap_sys.
		memstats.heap_sys.add(-int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	atomic.Xaddint64(&stats.committed, int64(scav))
	atomic.Xaddint64(&stats.released, -int64(scav))
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, int64(nbytes))
	}
	memstats.heapStats.release()

	// Publish the span in various locations.

	// This is safe to call without the lock held because the slots
	// related to this span will only ever be read or modified by
	// this thread until pointers into the span are published (and
	// we execute a publication barrier at the end of this function
	// before that happens) or pageInUse is updated.
	h.setSpans(s.base(), npages, s)

	if !typ.manual() {
		// Mark in-use span in arena page bitmap.
		//
		// This publishes the span to the page sweeper, so
		// it's imperative that the span be completely initialized
		// prior to this line.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.Or8(&arena.pageInUse[pageIdx], pageMask)

		// Update related page sweeper stats.
		atomic.Xadd64(&h.pagesInUse, int64(npages))
	}

	// Make sure the newly allocated span will be observed
	// by the GC before pointers into the span are published.
	publicationBarrier()

	return s
}

// Try to add at least npage pages of memory to the heap,
// returning whether it worked.
//
// h.lock must be held.
func (h *mheap) grow(npage uintptr) bool {
	assertLockHeld(&h.lock)

	// We must grow the heap in whole palloc chunks.
	ask := alignUp(npage, pallocChunkPages) * pageSize

	totalGrowth := uintptr(0)
	// This may overflow because ask could be very large
	// and is otherwise unrelated to h.curArena.base.
	end := h.curArena.base + ask
	nBase := alignUp(end, physPageSize)
	if nBase > h.curArena.end || /* overflow */ end < h.curArena.base {
		// Not enough room in the current arena. Allocate more
		// arena space. This may not be contiguous with the
		// current arena, so we have to request the full ask.
		av, asize := h.sysAlloc(ask)
		if av == nil {
			print("runtime: out of memory: cannot allocate ", ask, "-byte block (", memstats.heap_sys, " in use)\n")
			return false
		}

		if uintptr(av) == h.curArena.end {
			// The new space is contiguous with the old
			// space, so just extend the current space.
			h.curArena.end = uintptr(av) + asize
		} else {
			// The new space is discontiguous. Track what
			// remains of the current space and switch to
			// the new space. This should be rare.
			if size := h.curArena.end - h.curArena.base; size != 0 {
				h.pages.grow(h.curArena.base, size)
				totalGrowth += size
			}
			// Switch to the new space.
			h.curArena.base = uintptr(av)
			h.curArena.end = uintptr(av) + asize
		}

		// The memory just allocated counts as both released
		// and idle, even though it's not yet backed by spans.
		//
		// The allocation is always aligned to the heap arena
		// size which is always > physPageSize, so its safe to
		// just add directly to heap_released.
		atomic.Xadd64(&memstats.heap_released, int64(asize))
		stats := memstats.heapStats.acquire()
		atomic.Xaddint64(&stats.released, int64(asize))
		memstats.heapStats.release()

		// Recalculate nBase.
		// We know this won't overflow, because sysAlloc returned
		// a valid region starting at h.curArena.base which is at
		// least ask bytes in size.
		nBase = alignUp(h.curArena.base+ask, physPageSize)
	}

	// Grow into the current arena.
	v := h.curArena.base
	h.curArena.base = nBase
	h.pages.grow(v, nBase-v)
	totalGrowth += nBase - v

	// We just caused a heap growth, so scavenge down what will soon be used.
	// By scavenging inline we deal with the failure to allocate out of
	// memory fragments by scavenging the memory fragments that are least
	// likely to be re-used.
	if retained := heapRetained(); retained+uint64(totalGrowth) > h.scavengeGoal {
		todo := totalGrowth
		if overage := uintptr(retained + uint64(totalGrowth) - h.scavengeGoal); todo > overage {
			todo = overage
		}
		h.pages.scavenge(todo, false)
	}
	return true
}

// Free the span back into the heap.
func (h *mheap) freeSpan(s *mspan) {
	systemstack(func() {
		lock(&h.lock)
		if msanenabled {
			// Tell msan that this entire span is no longer in use.
			base := unsafe.Pointer(s.base())
			bytes := s.npages << _PageShift
			msanfree(base, bytes)
		}
		h.freeSpanLocked(s, spanAllocHeap)
		unlock(&h.lock)
	})
}

// freeManual frees a manually-managed span returned by allocManual.
// typ must be the same as the spanAllocType passed to the allocManual that
// allocated s.
//
// This must only be called when gcphase == _GCoff. See mSpanState for
// an explanation.
//
// freeManual must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack
func (h *mheap) freeManual(s *mspan, typ spanAllocType) {
	s.needzero = 1
	lock(&h.lock)
	h.freeSpanLocked(s, typ)
	unlock(&h.lock)
}

func (h *mheap) freeSpanLocked(s *mspan, typ spanAllocType) {
	assertLockHeld(&h.lock)

	switch s.state.get() {
	case mSpanManual:
		if s.allocCount != 0 {
			throw("mheap.freeSpanLocked - invalid stack free")
		}
	case mSpanInUse:
		if s.allocCount != 0 || s.sweepgen != h.sweepgen {
			print("mheap.freeSpanLocked - span ", s, " ptr ", hex(s.base()), " allocCount ", s.allocCount, " sweepgen ", s.sweepgen, "/", h.sweepgen, "\n")
			throw("mheap.freeSpanLocked - invalid free")
		}
		atomic.Xadd64(&h.pagesInUse, -int64(s.npages))

		// Clear in-use bit in arena page bitmap.
		arena, pageIdx, pageMask := pageIndexOf(s.base())
		atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	default:
		throw("mheap.freeSpanLocked - invalid span state")
	}

	// Update stats.
	//
	// Mirrors the code in allocSpan.
	nbytes := s.npages * pageSize
	if typ == spanAllocHeap {
		atomic.Xadd64(&memstats.heap_inuse, -int64(nbytes))
	}
	if typ.manual() {
		// Manually managed memory doesn't count toward heap_sys, so add it back.
		memstats.heap_sys.add(int64(nbytes))
	}
	// Update consistent stats.
	stats := memstats.heapStats.acquire()
	switch typ {
	case spanAllocHeap:
		atomic.Xaddint64(&stats.inHeap, -int64(nbytes))
	case spanAllocStack:
		atomic.Xaddint64(&stats.inStacks, -int64(nbytes))
	case spanAllocPtrScalarBits:
		atomic.Xaddint64(&stats.inPtrScalarBits, -int64(nbytes))
	case spanAllocWorkBuf:
		atomic.Xaddint64(&stats.inWorkBufs, -int64(nbytes))
	}
	memstats.heapStats.release()

	// Mark the space as free.
	h.pages.free(s.base(), s.npages)

	// Free the span structure. We no longer have a use for it.
	s.state.set(mSpanDead)
	h.freeMSpanLocked(s)
}

// scavengeAll acquires the heap lock (blocking any additional
// manipulation of the page allocator) and iterates over the whole
// heap, scavenging every free page available.
func (h *mheap) scavengeAll() {
	// Disallow malloc or panic while holding the heap lock. We do
	// this here because this is a non-mallocgc entry-point to
	// the mheap API.
	gp := getg()
	gp.m.mallocing++
	lock(&h.lock)
	// Start a new scavenge generation so we have a chance to walk
	// over the whole heap.
	h.pages.scavengeStartGen()
	released := h.pages.scavenge(^uintptr(0), false)
	gen := h.pages.scav.gen
	unlock(&h.lock)
	gp.m.mallocing--

	if debug.scavtrace > 0 {
		printScavTrace(gen, released, true)
	}
}

//go:linkname runtime_debug_freeOSMemory runtime/debug.freeOSMemory
func runtime_debug_freeOSMemory() {
	GC()
	systemstack(func() { mheap_.scavengeAll() })
}

// Initialize a new span with the given start and npages.
func (span *mspan) init(base uintptr, npages uintptr) {
	// span is *not* zeroed.
	span.next = nil
	span.prev = nil
	span.list = nil
	span.startAddr = base
	span.npages = npages
	span.allocCount = 0
	span.spanclass = 0
	span.elemsize = 0
	span.speciallock.key = 0
	span.specials = nil
	span.needzero = 0
	span.freeindex = 0
	span.allocBits = nil
	span.gcmarkBits = nil
	span.state.set(mSpanDead)
	lockInit(&span.speciallock, lockRankMspanSpecial)
}

func (span *mspan) inList() bool {
	return span.list != nil
}

// Initialize an empty doubly-linked list.
func (list *mSpanList) init() {
	list.first = nil
	list.last = nil
}

func (list *mSpanList) remove(span *mspan) {
	if span.list != list {
		print("runtime: failed mSpanList.remove span.npages=", span.npages,
			" span=", span, " prev=", span.prev, " span.list=", span.list, " list=", list, "\n")
		throw("mSpanList.remove")
	}
	if list.first == span {
		list.first = span.next
	} else {
		span.prev.next = span.next
	}
	if list.last == span {
		list.last = span.prev
	} else {
		span.next.prev = span.prev
	}
	span.next = nil
	span.prev = nil
	span.list = nil
}

func (list *mSpanList) isEmpty() bool {
	return list.first == nil
}

func (list *mSpanList) insert(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insert", span, span.next, span.prev, span.list)
		throw("mSpanList.insert")
	}
	span.next = list.first
	if list.first != nil {
		// The list contains at least one span; link it in.
		// The last span in the list doesn't change.
		list.first.prev = span
	} else {
		// The list contains no spans, so this is also the last span.
		list.last = span
	}
	list.first = span
	span.list = list
}

func (list *mSpanList) insertBack(span *mspan) {
	if span.next != nil || span.prev != nil || span.list != nil {
		println("runtime: failed mSpanList.insertBack", span, span.next, span.prev, span.list)
		throw("mSpanList.insertBack")
	}
	span.prev = list.last
	if list.last != nil {
		// The list contains at least one span.
		list.last.next = span
	} else {
		// The list contains no spans, so this is also the first span.
		list.first = span
	}
	list.last = span
	span.list = list
}

// takeAll removes all spans from other and inserts them at the front
// of list.
func (list *mSpanList) takeAll(other *mSpanList) {
	if other.isEmpty() {
		return
	}

	// Reparent everything in other to list.
	for s := other.first; s != nil; s = s.next {
		s.list = list
	}

	// Concatenate the lists.
	if list.isEmpty() {
		*list = *other
	} else {
		// Neither list is empty. Put other before list.
		other.last.next = list.first
		list.first.prev = other.last
		list.first = other.first
	}

	other.first, other.last = nil, nil
}

const (
	_KindSpecialFinalizer = 1 // 注释：special.kind 结束标识
	_KindSpecialProfile   = 2 // 注释：
	// Note: The finalizer special must be first because if we're freeing
	// an object, a finalizer special will cause the freeing operation
	// to abort, and we want to keep the other special records around
	// if that happens.
)

//go:notinheap
type special struct {
	next   *special // linked list in span
	offset uint16   // 注释：对象指针偏移量（值为该对象的首指针），uintptr(p) - span.base()// span offset of object
	kind   byte     // 注释：类型 // kind of special
}

// spanHasSpecials marks a span as having specials in the arena bitmap.
func spanHasSpecials(s *mspan) {
	arenaPage := (s.base() / pageSize) % pagesPerArena
	ai := arenaIndex(s.base())
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.Or8(&ha.pageSpecials[arenaPage/8], uint8(1)<<(arenaPage%8))
}

// spanHasNoSpecials marks a span as having no specials in the arena bitmap.
// 注释：译：spanHasNoSpecials将span标记为在竞技场位图中没有特殊功能。
func spanHasNoSpecials(s *mspan) {
	// 注释：(s.base() / pageSize)是span存的页数量，pagesPerArena是每个arena可以容纳的页的数量
	arenaPage := (s.base() / pageSize) % pagesPerArena // 注释：一个arena下的page下标位置，下面可以定位具体按个arena，然后通过这个定位具体的页
	ai := arenaIndex(s.base())                         // 注释：获取arena矩阵的信息
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	atomic.And8(&ha.pageSpecials[arenaPage/8], ^(uint8(1) << (arenaPage % 8)))
}

// Adds the special record s to the list of special records for
// the object p. All fields of s should be filled in except for
// offset & next, which this routine will fill in.
// Returns true if the special was successfully added, false otherwise.
// (The add will fail only if a record with the same p and s->kind
//
//	already exists.)
func addspecial(p unsafe.Pointer, s *special) bool {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("addspecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()
	kind := s.kind

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	t := &span.specials
	for {
		x := *t
		if x == nil {
			break
		}
		if offset == uintptr(x.offset) && kind == x.kind {
			unlock(&span.speciallock)
			releasem(mp)
			return false // already exists
		}
		if offset < uintptr(x.offset) || (offset == uintptr(x.offset) && kind < x.kind) {
			break
		}
		t = &x.next
	}

	// Splice in record, fill in offset.
	s.offset = uint16(offset)
	s.next = *t
	*t = s
	spanHasSpecials(span)
	unlock(&span.speciallock)
	releasem(mp)

	return true
}

// Removes the Special record of the given kind for the object p.
// Returns the record if the record existed, nil otherwise.
// The caller must FixAlloc_Free the result.
func removespecial(p unsafe.Pointer, kind uint8) *special {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		throw("removespecial on invalid pointer")
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()

	var result *special
	lock(&span.speciallock)
	t := &span.specials
	for {
		s := *t
		if s == nil {
			break
		}
		// This function is used for finalizers only, so we don't check for
		// "interior" specials (p must be exactly equal to s->offset).
		if offset == uintptr(s.offset) && kind == s.kind {
			*t = s.next
			result = s
			break
		}
		t = &s.next
	}
	if span.specials == nil {
		spanHasNoSpecials(span)
	}
	unlock(&span.speciallock)
	releasem(mp)
	return result
}

// The described object has a finalizer set for it.
//
// specialfinalizer is allocated from non-GC'd memory, so any heap
// pointers must be specially handled.
//
//go:notinheap
type specialfinalizer struct {
	special special
	fn      *funcval // May be a heap pointer.
	nret    uintptr
	fint    *_type   // May be a heap pointer, but always live.
	ot      *ptrtype // May be a heap pointer, but always live.
}

// Adds a finalizer to the object p. Returns true if it succeeded.
func addfinalizer(p unsafe.Pointer, f *funcval, nret uintptr, fint *_type, ot *ptrtype) bool {
	lock(&mheap_.speciallock)
	s := (*specialfinalizer)(mheap_.specialfinalizeralloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialFinalizer
	s.fn = f
	s.nret = nret
	s.fint = fint
	s.ot = ot
	if addspecial(p, &s.special) {
		// This is responsible for maintaining the same
		// GC-related invariants as markrootSpans in any
		// situation where it's possible that markrootSpans
		// has already run but mark termination hasn't yet.
		if gcphase != _GCoff {
			base, _, _ := findObject(uintptr(p), 0, 0)
			mp := acquirem()
			gcw := &mp.p.ptr().gcw
			// Mark everything reachable from the object
			// so it's retained for the finalizer.
			scanobject(base, gcw)
			// Mark the finalizer itself, since the
			// special isn't part of the GC'd heap.
			scanblock(uintptr(unsafe.Pointer(&s.fn)), sys.PtrSize, &oneptrmask[0], gcw, nil)
			releasem(mp)
		}
		return true
	}

	// There was an old finalizer
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
	return false
}

// Removes the finalizer (if any) from the object p.
func removefinalizer(p unsafe.Pointer) {
	s := (*specialfinalizer)(unsafe.Pointer(removespecial(p, _KindSpecialFinalizer)))
	if s == nil {
		return // there wasn't a finalizer to remove
	}
	lock(&mheap_.speciallock)
	mheap_.specialfinalizeralloc.free(unsafe.Pointer(s))
	unlock(&mheap_.speciallock)
}

// The described object is being heap profiled.
//
//go:notinheap
type specialprofile struct {
	special special
	b       *bucket
}

// Set the heap profile bucket associated with addr to b.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	lock(&mheap_.speciallock)
	s := (*specialprofile)(mheap_.specialprofilealloc.alloc())
	unlock(&mheap_.speciallock)
	s.special.kind = _KindSpecialProfile
	s.b = b
	if !addspecial(p, &s.special) {
		throw("setprofilebucket: profile already set")
	}
}

// Do whatever cleanup needs to be done to deallocate s. It has
// already been unlinked from the mspan specials list.
// 注释：译：做任何需要做的清理以解除分配s。它已经从mspan特价商品列表中取消了链接。
// 注释：释放special【ing】
func freespecial(s *special, p unsafe.Pointer, size uintptr) {
	switch s.kind {
	case _KindSpecialFinalizer:
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		queuefinalizer(p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
	case _KindSpecialProfile:
		sp := (*specialprofile)(unsafe.Pointer(s))
		mProf_Free(sp.b, size)
		lock(&mheap_.speciallock)
		mheap_.specialprofilealloc.free(unsafe.Pointer(sp))
		unlock(&mheap_.speciallock)
	default:
		throw("bad special kind")
		panic("not reached")
	}
}

// gcBits is an alloc/mark bitmap. This is always used as *gcBits.
// 注释：gcBits是一个分配/标记位图。这总是被用作*gcBits。
//
//go:notinheap
type gcBits uint8

// bytep returns a pointer to the n'th byte of b.
// 注释：bytep返回指向b的第n个字节的指针。
// 注释：指针b偏移n个
// 注释：可以理解为返回b[n],(返回n下标对应的地址)
func (b *gcBits) bytep(n uintptr) *uint8 {
	return addb((*uint8)(b), n) // 注释：返回 b + n
}

// bitp returns a pointer to the byte containing bit n and a mask for
// selecting that bit from *bytep.
// 注释：返回8的整数倍，和余数对应的位图
// 注释：标记位图，8个一组，返回第几组，当前组的第几位来控制对象分配情况
func (b *gcBits) bitp(n uintptr) (bytep *uint8, mask uint8) {
	return b.bytep(n / 8), 1 << (n % 8) // 注释： b.bytep(n / 8)是第几组的指针，1 << (n % 8)是对应组里的span位图。
}

const gcBitsChunkBytes = uintptr(64 << 10)
const gcBitsHeaderBytes = unsafe.Sizeof(gcBitsHeader{})

type gcBitsHeader struct {
	free uintptr // free is the index into bits of the next free byte.
	next uintptr // *gcBits triggers recursive type bug. (issue 14620)
}

//go:notinheap
type gcBitsArena struct {
	// gcBitsHeader // side step recursive type bug (issue 14620) by including fields by hand.
	free uintptr // free is the index into bits of the next free byte; read/write atomically
	next *gcBitsArena
	bits [gcBitsChunkBytes - gcBitsHeaderBytes]gcBits
}

var gcBitsArenas struct {
	lock     mutex
	free     *gcBitsArena
	next     *gcBitsArena // Read atomically. Write atomically under lock.
	current  *gcBitsArena
	previous *gcBitsArena
}

// tryAlloc allocates from b or returns nil if b does not have enough room.
// This is safe to call concurrently.
func (b *gcBitsArena) tryAlloc(bytes uintptr) *gcBits {
	if b == nil || atomic.Loaduintptr(&b.free)+bytes > uintptr(len(b.bits)) {
		return nil
	}
	// Try to allocate from this block.
	end := atomic.Xadduintptr(&b.free, bytes)
	if end > uintptr(len(b.bits)) {
		return nil
	}
	// There was enough room.
	start := end - bytes
	return &b.bits[start]
}

// newMarkBits returns a pointer to 8 byte aligned bytes
// to be used for a span's mark bits.
func newMarkBits(nelems uintptr) *gcBits {
	blocksNeeded := uintptr((nelems + 63) / 64)
	bytesNeeded := blocksNeeded * 8

	// Try directly allocating from the current head arena.
	head := (*gcBitsArena)(atomic.Loadp(unsafe.Pointer(&gcBitsArenas.next)))
	if p := head.tryAlloc(bytesNeeded); p != nil {
		return p
	}

	// There's not enough room in the head arena. We may need to
	// allocate a new arena.
	lock(&gcBitsArenas.lock)
	// Try the head arena again, since it may have changed. Now
	// that we hold the lock, the list head can't change, but its
	// free position still can.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate a new arena. This may temporarily drop the lock.
	fresh := newArenaMayUnlock()
	// If newArenaMayUnlock dropped the lock, another thread may
	// have put a fresh arena on the "next" list. Try allocating
	// from next again.
	if p := gcBitsArenas.next.tryAlloc(bytesNeeded); p != nil {
		// Put fresh back on the free list.
		// TODO: Mark it "already zeroed"
		fresh.next = gcBitsArenas.free
		gcBitsArenas.free = fresh
		unlock(&gcBitsArenas.lock)
		return p
	}

	// Allocate from the fresh arena. We haven't linked it in yet, so
	// this cannot race and is guaranteed to succeed.
	p := fresh.tryAlloc(bytesNeeded)
	if p == nil {
		throw("markBits overflow")
	}

	// Add the fresh arena to the "next" list.
	fresh.next = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), unsafe.Pointer(fresh))

	unlock(&gcBitsArenas.lock)
	return p
}

// newAllocBits returns a pointer to 8 byte aligned bytes
// to be used for this span's alloc bits.
// newAllocBits is used to provide newly initialized spans
// allocation bits. For spans not being initialized the
// mark bits are repurposed as allocation bits when
// the span is swept.
func newAllocBits(nelems uintptr) *gcBits {
	return newMarkBits(nelems)
}

// nextMarkBitArenaEpoch establishes a new epoch for the arenas
// holding the mark bits. The arenas are named relative to the
// current GC cycle which is demarcated by the call to finishweep_m.
//
// All current spans have been swept.
// During that sweep each span allocated room for its gcmarkBits in
// gcBitsArenas.next block. gcBitsArenas.next becomes the gcBitsArenas.current
// where the GC will mark objects and after each span is swept these bits
// will be used to allocate objects.
// gcBitsArenas.current becomes gcBitsArenas.previous where the span's
// gcAllocBits live until all the spans have been swept during this GC cycle.
// The span's sweep extinguishes all the references to gcBitsArenas.previous
// by pointing gcAllocBits into the gcBitsArenas.current.
// The gcBitsArenas.previous is released to the gcBitsArenas.free list.
func nextMarkBitArenaEpoch() {
	lock(&gcBitsArenas.lock)
	if gcBitsArenas.previous != nil {
		if gcBitsArenas.free == nil {
			gcBitsArenas.free = gcBitsArenas.previous
		} else {
			// Find end of previous arenas.
			last := gcBitsArenas.previous
			for last = gcBitsArenas.previous; last.next != nil; last = last.next {
			}
			last.next = gcBitsArenas.free
			gcBitsArenas.free = gcBitsArenas.previous
		}
	}
	gcBitsArenas.previous = gcBitsArenas.current
	gcBitsArenas.current = gcBitsArenas.next
	atomic.StorepNoWB(unsafe.Pointer(&gcBitsArenas.next), nil) // newMarkBits calls newArena when needed
	unlock(&gcBitsArenas.lock)
}

// newArenaMayUnlock allocates and zeroes a gcBits arena.
// The caller must hold gcBitsArena.lock. This may temporarily release it.
func newArenaMayUnlock() *gcBitsArena {
	var result *gcBitsArena
	if gcBitsArenas.free == nil {
		unlock(&gcBitsArenas.lock)
		result = (*gcBitsArena)(sysAlloc(gcBitsChunkBytes, &memstats.gcMiscSys))
		if result == nil {
			throw("runtime: cannot allocate memory")
		}
		lock(&gcBitsArenas.lock)
	} else {
		result = gcBitsArenas.free
		gcBitsArenas.free = gcBitsArenas.free.next
		memclrNoHeapPointers(unsafe.Pointer(result), gcBitsChunkBytes)
	}
	result.next = nil
	// If result.bits is not 8 byte aligned adjust index so
	// that &result.bits[result.free] is 8 byte aligned.
	if uintptr(unsafe.Offsetof(gcBitsArena{}.bits))&7 == 0 {
		result.free = 0
	} else {
		result.free = 8 - (uintptr(unsafe.Pointer(&result.bits[0])) & 7)
	}
	return result
}
