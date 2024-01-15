// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Central free lists.
//
// See malloc.go for an overview.
//
// The mcentral doesn't actually contain the list of free objects; the mspan does.
// Each mcentral is two lists of mspans: those with free objects (c->nonempty)
// and those that are completely allocated (c->empty).

package runtime

import "runtime/internal/atomic"

// Central list of free objects of a given size.
//
// 注释：中心缓存(属于mheap堆内存结构图的mcentral属性，由mheap锁控制)
// 注释：包含有空闲和无空闲两个结构，每个结构包含GC已扫描和未扫描的两个span链表
//go:notinheap
type mcentral struct {
	spanclass spanClass // 注释：spanId,每个mcentral管理着一组有相同class的span列表(span class ID)

	// partial and full contain two mspan sets: one of swept in-use
	// spans, and one of unswept in-use spans. These two trade
	// roles on each GC cycle. The unswept set is drained either by
	// allocation or by the background sweeper in every GC cycle,
	// so only two roles are necessary.
	// 注释：partial和full包含两个mspan集：一个是使用中扫描的spans，另一个是未使用中未扫描的spans。每个GC周期中的这两个交易角色。
	// 		在每个GC周期中，未被清除的集合要么通过分配，要么通过后台清理器来耗尽，因此只需要两个角色。
	//
	// sweepgen is increased by 2 on each GC cycle, so the swept
	// spans are in partial[sweepgen/2%2] and the unswept spans are in
	// partial[1-sweepgen/2%2]. Sweeping pops spans from the
	// unswept set and pushes spans that are still in-use on the
	// swept set. Likewise, allocating an in-use span pushes it
	// on the swept set.
	// 注释：在每个GC循环中，扫频增加2，因此扫频spans为部分[Swepgen/2%2]，而未扫频spans则为部分[1-swepgen-2%2]。
	//		扫掠从未扫掠集弹出跨距，并推送扫掠集上仍在使用的跨距。同样，分配一个正在使用的span会将其推到扫描集上。
	//
	// Some parts of the sweeper can sweep arbitrary spans, and hence
	// can't remove them from the unswept set, but will add the span
	// to the appropriate swept list. As a result, the parts of the
	// sweeper and mcentral that do consume from the unswept list may
	// encounter swept spans, and these should be ignored.
	// 注释：清扫器的某些部分可以清扫任意spans，因此无法将其从未清扫集中删除，但会将span添加到相应的清扫列表中。
	//		因此，清除器和mcentral中从未清除列表中消耗的部分可能会遇到已清除的spans，应忽略这些跨度。
	//
	// 注释：数组两个元素表示GC未扫描和已扫描的span链表，通过GC计数器进行数据含义切换(未扫描变成已扫码，已扫码变成为扫描)
	partial [2]spanSet // 注释：【有空闲】的span链表(两个元素分别表示GC【已清理】和【未清理】两类数组，通过清理计数器来区分，清理开始的时候计数器自增2) // list of spans with a free object
	full    [2]spanSet // 注释：【无空闲】的span链表(两个元素分别表示GC【已清理】和【未清理】两类数组，通过清理计数器来区分，清理开始的时候计数器自增2) // list of spans with no free objects
}

// Initialize a single central free list.
func (c *mcentral) init(spc spanClass) {
	c.spanclass = spc
	lockInit(&c.partial[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.partial[1].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[0].spineLock, lockRankSpanSetSpine)
	lockInit(&c.full[1].spineLock, lockRankSpanSetSpine)
}

// partialUnswept returns the spanSet which holds partially-filled
// unswept spans for this sweepgen.
// 注释：每两个为一组（舍去一位然后取模）
// 注释：sweepgena为GC清理计数器，partial是有空闲的数组数据，数组有两个元素，分别是已清理和未清理数据，会根据清理计数器实现位置调换
// 注释：未清理是根据清理计数器（清理版本）计算出来的，把partial分为两组，每次清理开始处自增2，调换已清理和为清理的位置进行清理动作，执行未清理数据的清理工作
// 注释：【有空闲、未清理】部分未清扫
func (c *mcentral) partialUnswept(sweepgen uint32) *spanSet {
	return &c.partial[1-sweepgen/2%2] // 注释：代码贡献者大意了！安装之前的惯例写法应该是  return &c.partial[sweepgen>>1&1 ^ 1]
}

// partialSwept returns the spanSet which holds partially-filled
// swept spans for this sweepgen.
// 注释：sweepgena为GC扫描的计数器，每次GC扫描完成后自增2，实现已扫描和未扫描的数组下标切换
// 注释：【有空闲、已清理】部分清扫
func (c *mcentral) partialSwept(sweepgen uint32) *spanSet {
	return &c.partial[sweepgen/2%2] // 注释：代码贡献者大意了！安装之前的惯例写法应该是  return &c.partial[sweepgen>>1&1]
}

// fullUnswept returns the spanSet which holds unswept spans without any
// free slots for this sweepgen.
// 注释：sweepgena为GC扫描的计数器，每次GC扫描完成后自增2，实现已扫描和未扫描的数组下标切换
// 注释：【无空闲、已清理】全部未清扫
func (c *mcentral) fullUnswept(sweepgen uint32) *spanSet {
	return &c.full[1-sweepgen/2%2] // 注释：代码贡献者大意了！安装之前的惯例写法应该是  return &c.full[sweepgen>>1&1 ^ 1]
}

// fullSwept returns the spanSet which holds swept spans without any
// free slots for this sweepgen.
// 注释：无空闲并且被GC扫描的span
// 注释：每两个为一组（舍去一位然后取模）
// 注释：sweepgena为GC扫描的计数器，每次GC扫描完成后自增2，实现已扫描和未扫描的数组下标切换
// 注释：【无空闲、已扫描】全部清扫
func (c *mcentral) fullSwept(sweepgen uint32) *spanSet {
	return &c.full[sweepgen/2%2] // 注释：代码贡献者大意了！安装之前的惯例写法应该是  return &c.full[sweepgen>>1&1]
}

// Allocate a span to use in an mcache.
// 注释：分配一个span到mcache中
// 注释：线程缓存（mcache）到中心缓存(mcentral)中获取包含空闲的跨度(span)步骤
//       1.向【有空闲、已清理】中查找，如果找到直接返回。
//       2.向【有空闲、未清理】中查找，如果找到直接返回。
//       3.向【无空闲、未清理】中查找，如果找到，执行清理工作并返回。
//       4.
//       5.
func (c *mcentral) cacheSpan() *mspan {
	// Deduct credit for this span allocation and sweep if necessary.
	// 注释：扣除此span分配的贷项，如有必要，进行扫掠。
	spanBytes := uintptr(class_to_allocnpages[c.spanclass.sizeclass()]) * _PageSize // 注释：获取span的大小，是个配置
	deductSweepCredit(spanBytes, 0)                                                 // 注释：减低清理积分spanBytes是一个span的大小

	sg := mheap_.sweepgen // 注释：获取GC清理计数器

	traceDone := false // 注释：链路最终完成标识
	if trace.enabled { // 注释：如果开启链路追踪
		traceGCSweepStart() // 注释：标记GC清扫的链路最终开始
	}

	// If we sweep spanBudget spans without finding any free
	// space, just allocate a fresh span. This limits the amount
	// of time we can spend trying to find free space and
	// amortizes the cost of small object sweeping over the
	// benefit of having a full free span to allocate from. By
	// setting this to 100, we limit the space overhead to 1%.
	// 注释：译：如果我们扫描spanBudget跨度而没有找到任何可用空间，只需分配一个新的跨度。
	//		这限制了我们试图寻找自由空间所花费的时间，并将小物体清扫的成本分摊到有一个完整的自由跨度可供分配的好处上。通过将其设置为100，我们将空间开销限制在1%
	//
	// TODO(austin,mknyszek): This still has bad worst-case
	// throughput. For example, this could find just one free slot
	// on the 100th swept span. That limits allocation latency, but
	// still has very poor throughput. We could instead keep a
	// running free-to-used budget and switch to fresh span
	// allocation if the budget runs low.
	// 注释：译：吞吐量例如，这可能在第100个扫掠跨度上只找到一个空闲插槽。这限制了分配延迟，但吞吐量仍然很低。相反，我们可以保持免费使用的预算，如果预算不足，则切换到新的跨度分配。
	spanBudget := 100 // 注释：考虑性能设置一个边界值，只在【未清理】中查找100次

	var s *mspan

	// Try partial swept spans first.
	// 注释：译：先尝试部分清扫跨度。
	if s = c.partialSwept(sg).pop(); s != nil { // 注释：从部分清扫【有空闲、已清理】链表出栈span，如果有则直接返回
		goto havespan
	}

	// Now try partial unswept spans.
	// 注释：译：现在尝试部分未清扫的跨度
	// 注释：下面是从部分未清扫【有空闲、未清理】中找
	for ; spanBudget >= 0; spanBudget-- {
		s = c.partialUnswept(sg).pop() // 注释：到部分未清扫【有空闲、未清理】链表出栈span
		if s == nil {                  // 注释：入股没有则跳过
			break
		}
		if atomic.Load(&s.sweepgen) == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) { // 注释：如果需要清理，则更改为正在清理
			// We got ownership of the span, so let's sweep it and use it.
			s.sweep(true) // 注释：执行清理动作 【ing】
			goto havespan
		}
		// We failed to get ownership of the span, which means it's being or
		// has been swept by an asynchronous sweeper that just couldn't remove it
		// from the unswept list. That sweeper took ownership of the span and
		// responsibility for either freeing it to the heap or putting it on the
		// right swept list. Either way, we should just ignore it (and it's unsafe
		// for us to do anything else).
	}
	// Now try full unswept spans, sweeping them and putting them into the
	// right list if we fail to get a span.
	for ; spanBudget >= 0; spanBudget-- { // 注释：如果【有空间、未清理】里没有找到并且还没有达到阀值时
		s = c.fullUnswept(sg).pop() // 注释：到【无空闲、为清理】队列中出栈一个span
		if s == nil {               // 注释：如果没有找到跳过
			break
		}
		if atomic.Load(&s.sweepgen) == sg-2 && atomic.Cas(&s.sweepgen, sg-2, sg-1) { // 注释：如果状态是【需要清理、未缓存】时设置状态为【正在清理、未缓存】
			// We got ownership of the span, so let's sweep it.
			s.sweep(true) // 注释：执行清理动作，并且把清理后的位图放到 mheap.allocBits 里
			// Check if there's any free space.
			freeIndex := s.nextFreeIndex() // 注释：重新到mheap.allocBits中获取，并缓存到mheap.allocCache快速缓存中
			if freeIndex != s.nelems {
				s.freeindex = freeIndex
				goto havespan
			}
			// Add it to the swept list, because sweeping didn't give us any free space.
			c.fullSwept(sg).push(s)
		}
		// See comment for partial unswept spans.
	}
	if trace.enabled {
		traceGCSweepDone()
		traceDone = true
	}

	// We failed to get a span from the mcentral so get one from mheap.
	s = c.grow()
	if s == nil {
		return nil
	}

	// At this point s is a span that should have free slots.
	// 注释：译：此时，s是一个应具有空闲插槽的跨度。
havespan:
	if trace.enabled && !traceDone { // 注释：开启链路追踪并且没有结束
		traceGCSweepDone() // 注释：(关闭GC清理时链路追踪表示)标记链路追踪结束
	}
	n := int(s.nelems) - int(s.allocCount) // 注释：未分配的块数量
	if n == 0 || s.freeindex == s.nelems || uintptr(s.allocCount) == s.nelems {
		throw("span has no free objects") // 注释：span没有可用对象
	}
	freeByteBase := s.freeindex &^ (64 - 1) // 注释：清空后5位
	whichByte := freeByteBase / 8
	// Init alloc bits cache.
	// 注释：译：初始分配的位图缓存
	s.refillAllocCache(whichByte) // 注释：重新缓存64个空的块到快速缓冲区里

	// Adjust the allocCache so that s.freeindex corresponds to the low bit in
	// s.allocCache.
	s.allocCache >>= s.freeindex % 64 // 注释：移除要被只用的空块，(s.freeindex是下一个空块的下标)

	return s
}

// Return span from an mcache.
//
// s must have a span class corresponding to this
// mcentral and it must not be empty.
//
// 注释：把已经分配完的span放到中心缓存mcentral中去
func (c *mcentral) uncacheSpan(s *mspan) {
	if s.allocCount == 0 {
		throw("uncaching span but s.allocCount == 0")
	}

	sg := mheap_.sweepgen       // 注释：获取mheap.sweepgen
	stale := s.sweepgen == sg+1 // 注释：是否需要清理(清扫)【需要清理、已缓存】

	// Fix up sweepgen.
	// 注释：根据清扫状态修改清扫标识
	if stale { // 注释：需要清理
		// Span was cached before sweep began. It's our
		// responsibility to sweep it.
		//
		// Set sweepgen to indicate it's not cached but needs
		// sweeping and can't be allocated from. sweep will
		// set s.sweepgen to indicate s is swept.
		atomic.Store(&s.sweepgen, sg-1) // 注释：设置标识【正在清理、未缓存】
	} else { // 注释：不需要清理
		// Indicate that s is no longer cached.
		atomic.Store(&s.sweepgen, sg) // 注释：设置标识【已经清理、未缓存】
	}

	// Put the span in the appropriate place.
	// 注释：如果需要清理则执行清理动作，否则把span放到已经清理的有空闲或无空闲链表上
	if stale { // 注释：需要清理
		// It's stale, so just sweep it. Sweeping will put it on
		// the right list.
		s.sweep(false) // 注释：执行清理动作工作
	} else { // 注释：不需要清理
		if int(s.nelems)-int(s.allocCount) > 0 { // 注释：存在未分配的块(如果span(跨度)里的块 - 已经分配的块 > 0)
			// Put it back on the partial swept list.
			c.partialSwept(sg).push(s) // 注释：入栈到【已经清理、有空闲】链表中
		} else {
			// There's no free space and it's not stale, so put it on the
			// full swept list.
			c.fullSwept(sg).push(s)
		}
	}
}

// grow allocates a new empty span from the heap and initializes it for c's size class.
func (c *mcentral) grow() *mspan {
	npages := uintptr(class_to_allocnpages[c.spanclass.sizeclass()])
	size := uintptr(class_to_size[c.spanclass.sizeclass()])

	s := mheap_.alloc(npages, c.spanclass, true)
	if s == nil {
		return nil
	}

	// Use division by multiplication and shifts to quickly compute:
	// n := (npages << _PageShift) / size
	n := (npages << _PageShift) >> s.divShift * uintptr(s.divMul) >> s.divShift2
	s.limit = s.base() + size*n
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}
