// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1)) // 注释：64位系统：96字节（96+0）
	debugChan = false                                                                       // 注释：管道调试debug开关，默认：false
)

// 注释：通道的结构体(64位操作系统：总大小是96字节)
type hchan struct {
	qcount   uint           // 注释：通道队列中的数据个数（有效数据个数） // total data in the queue
	dataqsiz uint           // 注释：通道队列的尺寸（可以容纳的总个数） // size of the circular queue
	buf      unsafe.Pointer // 注释：存放实际数据的指针 // points to an array of dataqsiz elements
	elemsize uint16         // 注释：元素类型大小
	closed   uint32         // 注释：通道是否关闭
	elemtype *_type         // 注释：元素类型（写入通道的时候用到） // element type
	sendx    uint           // 注释：记录发送者（写入）在buf中的序号（数组下标） // send index
	recvx    uint           // 注释：记录接收者（读取）在buf中的序号（数组下标） // receive index
	recvq    waitq          // 注释：读取的阻塞协程队列（双向链表） // list of recv waiters
	sendq    waitq          // 注释：写入的阻塞协程队列（双向链表） // list of send waiters

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	lock mutex // 注释：锁，并发保护
}

// 注释：阻塞队列（双向链表）结构体
type waitq struct {
	first *sudog // 注释：队列（双向链表）头部
	last  *sudog // 注释：队列（双向链表）尾部
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

// 注释：64位make管道；主要是判断是否超出int类型大小，一个管道的大小最大是int类型大小
func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

// 注释：make管道的主函数；make(chan类型, chan缓冲区大小), 如果make没有设置缓冲区大小时，则size为0
func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	// 注释：基础类型是int类型，最大值时10的32次方
	if elem.size >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	mem, overflow := math.MulUintptr(elem.size, uintptr(size)) // 注释：两数相乘（单个元素大小*缓冲区大小=需要开辟的内存大小）
	// 注释：内存有溢出或者超出可分配的最大值或者第二个参数size小于0是报错
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	var c *hchan
	switch {
	case mem == 0: // 注释：无缓冲区，开辟channel结构体的大小
		// Queue or element size is zero.
		c = (*hchan)(mallocgc(hchanSize, nil, true)) // 注释：分配管道地址
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0: // 注释：元素不包含指针，不是指针数据，开辟连续的空间
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true)) // 注释：分配管道地址
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default: // 注释：元素包含指针的情况下（默认情况），实例化channel类型指针，然后单独申请地址给c.buf
		// Elements contain pointers.
		c = new(hchan) // 注释：分配管道地址
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size)   // 注释：每个元素的类型大小
	c.elemtype = elem                // 注释：元素类型
	c.dataqsiz = uint(size)          // 注释：整个channel的容量大小
	lockInit(&c.lock, lockRankHchan) // 注释：初始化并发锁

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
// 注释：定位buf的指针；返回要插入/查找管道的位置指针（根据管道和要插入/查找位置的坐标c.sendx/c.recvx和c.elemsize每个元素的大小得出要插入的位置的指针）
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize)) // 注释：c.buf是数据头指针，i是要插入的位置(是c.sendx的值),c.elemsize是单个元素的大小
}

// full reports whether a send on c would block (that is, the channel is full).
// It uses a single word-sized read of mutable state, so although
// the answer is instantaneously true, the correct answer may have changed
// by the time the calling function receives the return value.
// 注释：判读管道是否已经满了，满了返回TRUE
func full(c *hchan) bool {
	// c.dataqsiz is immutable (never written after the channel is created)
	// so it is safe to read at any time during channel operation.
	if c.dataqsiz == 0 {
		// Assumes that a pointer read is relaxed-atomic.
		return c.recvq.first == nil
	}
	// Assumes that a uint read is relaxed-atomic.
	return c.qcount == c.dataqsiz
}

// entry point for c <- x from compiled code
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
// 注释：写入（发送）管道数据；ep是写入的数据变量指针(element pointer)， block是否阻塞，callerpc检查数据竞争时使用
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 注释：如果管道没有初始化
	if c == nil {
		// 注释：并且是非阻塞的情况下返回FALSE，否则进入阻塞状态并且报错
		if !block {
			return false
		}
		// 注释：如果channel为nil的时候，让渡G的控制权（携程G阻塞），然后抛出异常
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// 注释：如果设置管道debug，则打印管道指针
	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	// 注释：如果开启数据竞争，则收集数据竞争需要的内容数据
	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, funcPC(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second full()).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation. However, nothing here
	// guarantees forward progress. We rely on the side effects of lock release in
	// chanrecv() and closechan() to update this thread's view of c.closed and full().
	if !block && c.closed == 0 && full(c) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock) // 注释：锁定管道，下面开始正式写入操作

	// 注释：写入一个已经关闭的管道时异常（如果管道已经处于关闭状态时：解锁，抛出异常）
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	// 注释：获取读取等待队列里最先陷入等待的G，直接写入（发送）；（如果有G说明缓冲区已经满了或没有缓冲区）
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		send(c, sg, ep, func() { unlock(&c.lock) }, 3) // 注释：直接发送，把写入的数据直接直接发送到阻塞的读取位置，设置系统调用栈，把其放到下一个要执行的G里
		return true
	}

	// 注释：有缓存的情况下写入，并直接返回
	// 注释：当c.qcount（实际数据数量）小于c.dataqsiz（可容纳的总数量）时，说明缓冲区还有地方
	if c.qcount < c.dataqsiz {
		// Space is available in the channel buffer. Enqueue the element to send.
		qp := chanbuf(c, c.sendx) // 注释：计算要插入位置的地址
		// 注释：判断是否开启数据竞争
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		typedmemmove(c.elemtype, qp, ep) // 注释：异动数据（c.elemtyep是类型，qp是插入的位置指针，ep是数据指针）
		c.sendx++
		if c.sendx == c.dataqsiz { // 注释：如果c.sendx到超过数组尾部，则重置为数组的首部，形成一个环形数据结构
			c.sendx = 0
		}
		c.qcount++      // 注释：数据总数加一
		unlock(&c.lock) // 注释：解锁，写入管道结束
		return true
	}

	// 注释：下面是无缓冲区或者缓冲区已满的情况下执行

	// 注释：缓冲区已满或无缓冲区，并且是非阻塞的情况下：解锁，并返回FALSE
	if !block {
		unlock(&c.lock)
		return false
	}

	// Block on the channel. Some receiver will complete our operation for us.
	gp := getg()           // 注释：获取当前运行的G
	mysg := acquireSudog() // 注释：获得sudog结构体（初始化sudog）
	mysg.releasetime = 0   // 注释：设置释放时间
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep // 注释：保存源数据指针
	mysg.waitlink = nil
	mysg.g = gp           // 注释：保存当前运行的G
	mysg.isSelect = false // 注释：不参与select
	mysg.c = c            // 注释：保存所在的管道
	gp.waiting = mysg     // 注释：把当前对应的sudog放到等待链表中
	gp.param = nil        // 注释：设置唤醒时不需要传递参数
	c.sendq.enqueue(mysg) // 注释：把sudog放到管道的发送队列中
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)                                                        // 注释：把&gp.parkingOnChan赋值为1（这个方法是调换两个值的内容）
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2) // 注释：阻塞，让渡控制权，保存现场
	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.
	KeepAlive(ep)

	// someone woke us up.
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	closed := !mysg.success
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	mysg.c = nil
	releaseSudog(mysg)
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		panic(plainError("send on closed channel"))
	}
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	// 注释：如果开启数据竞争则进行标记或提示
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	if sg.elem != nil {
		// 注释：（直接发送）如果元素有值则直接发送，sg是读取阻塞队列里出来的元素（读取管道的地方），ep是写入管道数据指针，c是管道
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg) // 注释：唤醒时需要使用的参数
	sg.success = true             // 注释：设置为因通道唤醒（该字段含义：是否因通道唤醒）
	if sg.releasetime != 0 {      // 注释：如果存在释放时间
		sg.releasetime = cputicks() // 注释：设置CPU的频率（每毫秒）；blockevent阻塞监听的时间是当前值减去当时的cputicks()值
	}
	goready(gp, skip+1) // 注释：把读取阻塞的G拿出来，放到下一个准备执行的G位置上
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.

// 注释：直接发送，把读取等待队列的sg拿出来（读取管道），直接执行src（写入管道的数据）
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.
	dst := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size)
}

func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

func closechan(c *hchan) {
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	lock(&c.lock)
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, funcPC(closechan))
		racerelease(c.raceaddr())
	}

	c.closed = 1

	var glist gList

	// release all readers
	for {
		sg := c.recvq.dequeue()
		if sg == nil {
			break
		}
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}

	// release all writers (they will panic)
	for {
		sg := c.sendq.dequeue()
		if sg == nil {
			break
		}
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}
	unlock(&c.lock)

	// Ready all Gs now that we've dropped the channel lock.
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		goready(gp, 3)
	}
}

// empty reports whether a read from c would block (that is, the channel is
// empty).  It uses a single atomic read of mutable state.
func empty(c *hchan) bool {
	// c.dataqsiz is immutable.
	if c.dataqsiz == 0 {
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	return atomic.Loaduint(&c.qcount) == 0
}

// entry points for <- c from compiled code
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
// 注释：管道读取内容，接收的数据会写入ep(element pointer)里，block是否阻塞
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: don't need to check ep, as it is always on the stack
	// or is new memory allocated by reflect.

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	if c == nil {
		if !block {
			return
		}
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	if !block && empty(c) {
		// After observing that the channel is not ready for receiving, we observe whether the
		// channel is closed.
		//
		// Reordering of these checks could lead to incorrect behavior when racing with a close.
		// For example, if the channel was open and not empty, was closed, and then drained,
		// reordered reads could incorrectly indicate "open and empty". To prevent reordering,
		// we use atomic loads for both checks, and rely on emptying and closing to happen in
		// separate critical sections under the same lock.  This assumption fails when closing
		// an unbuffered channel with a blocked send, but that is an error condition anyway.
		if atomic.Load(&c.closed) == 0 {
			// Because a channel cannot be reopened, the later observation of the channel
			// being not closed implies that it was also not closed at the moment of the
			// first observation. We behave as if we observed the channel at that moment
			// and report that the receive cannot proceed.
			return
		}
		// The channel is irreversibly closed. Re-check whether the channel has any pending data
		// to receive, which could have arrived between the empty and closed checks above.
		// Sequential consistency is also required here, when racing with such a send.
		if empty(c) {
			// The channel is irreversibly closed and empty.
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
	}

	var t0 int64
	// 注释：阻塞分析器，默认是0禁用
	if blockprofilerate > 0 {
		t0 = cputicks() // 注释：获取CPU时钟周期计数器
	}

	// 注释：加锁，后面开始读取管道内容
	lock(&c.lock)

	// 注释：如果管道已经关闭，并且管道里已经没有内容了则直接返回
	if c.closed != 0 && c.qcount == 0 {
		if raceenabled {
			raceacquire(c.raceaddr())
		}
		unlock(&c.lock)
		if ep != nil {
			typedmemclr(c.elemtype, ep)
		}
		return true, false
	}

	// 注释：到发送队列（sendq）里取出一个，判断是否有值，如果有值则直接读取，接收到的数据写入ep里
	if sg := c.sendq.dequeue(); sg != nil {
		// Found a waiting sender. If buffer is size 0, receive value
		// directly from sender. Otherwise, receive from head of queue
		// and add sender's value to the tail of the queue (both map to
		// the same buffer slot because the queue is full).
		recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true, true
	}

	// 注释：有缓冲区管道时
	if c.qcount > 0 {
		// Receive directly from queue
		qp := chanbuf(c, c.recvx) // 注释：获取要查询的缓冲区的数据指针
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp) // 注释：移动数据(根据类型的大小把qp移动到ep上)
		}
		typedmemclr(c.elemtype, qp)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.qcount--
		unlock(&c.lock)
		return true, true
	}

	// 判读是否阻塞，不阻塞时直接返回
	if !block {
		unlock(&c.lock)
		return false, false
	}

	// no sender available: block on this channel.
	gp := getg()           // 注释：获取当前运行的G
	mysg := acquireSudog() // 注释：获取等待的G对象（可能是等待中G也可能是空的G）(如果P中存在则返回尾部的G，如果不存在去全局G链表中取，如果没有取到则返回空的G)
	mysg.releasetime = 0   // 注释：设置释放时间
	if t0 != 0 {
		mysg.releasetime = -1 // 注释：设置释放时间，-1表示send的时候再设置时间
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.param = nil
	c.recvq.enqueue(mysg) // 注释：加入等待读取的队列中取（加到尾部）
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2) // 注释：让渡控制权

	// someone woke us up
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	success := mysg.success
	gp.param = nil
	mysg.c = nil
	releaseSudog(mysg) // 注释：释放G对象
	return true, success
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
// 1) The value sent by the sender sg is put into the channel
//    and the sender is woken up to go on its merry way.
// 2) The value received by the receiver (the current G) is
//    written to ep.
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if c.dataqsiz == 0 {
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// copy data from sender
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// Queue is full. Take the item at the
		// head of the queue. Make the sender enqueue
		// its item at the tail of the queue. Since the
		// queue is full, those are both the same slot.
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
		}
		// copy data from queue to receiver
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		// copy data from sender to queue
		typedmemmove(c.elemtype, qp, sg.elem)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1)
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on the
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	atomic.Store8(&gp.parkingOnChan, 0)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock chanLock
	// we risk gp getting readied by a channel operation and
	// so gp could continue running before everything before
	// the unlock is visible (even to gp itself).
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbrecv(&v, c) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected bool) {
	selected, _ = chanrecv(c, elem, false)
	return
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if c != nil && selectnbrecv2(&v, &ok, c) {
//		... foo
//	} else {
//		... bar
//	}
//
func selectnbrecv2(elem unsafe.Pointer, received *bool, c *hchan) (selected bool) {
	// TODO(khr): just return 2 values from this function, now that it is in Go.
	selected, *received = chanrecv(c, elem, false)
	return
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

// 注释：把G加入队列尾部（在双向链表尾部加入元素）
func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

// 注释：在队列头部取出G（在双向链表头部取出元素；元素移除队列，把链表第一个元素取出来）
func (q *waitq) dequeue() *sudog {
	for {
		sgp := q.first
		if sgp == nil {
			return nil
		}
		y := sgp.next
		// 注释：如果链表的下一个元素为nil，说明出栈以后，链表已经没有元素了
		if y == nil {
			q.first = nil
			q.last = nil
		} else {
			// 注释：把链表的下一个元素绑定到first上，然后断开出栈的元素与链表的关系
			y.prev = nil
			q.first = y
			sgp.next = nil // mark as removed (see dequeueSudog)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		// 注释：如果出栈的数据处于select中，并且原子操作，判断是否完成，如果没有关闭则设置为完成，然后跳过循环
		if sgp.isSelect && !atomic.Cas(&sgp.g.selectDone, 0, 1) {
			continue
		}

		// 注释：返回链表第一个元素，在双向链表头部取出元素
		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	// 注释：将通道上的类读和类写操作视为在此地址发生。避免使用qcount或dataqsiz的地址，因为len()和cap()内置函数读取这些地址，我们不希望它们与close()等操作竞争。
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
