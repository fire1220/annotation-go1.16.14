// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"runtime/internal/atomic"
	"unsafe"
)

const debugSelect = false

// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/internal/gc/select.go's scasetype.
// 注释：select case中的case结构体
type scase struct {
	c    *hchan         // 注释：case的管道数据指针 // chan
	elem unsafe.Pointer // 注释：数据指针，用来发送或接收数据的指针(管道发送时这个时发送的数据指针，管道接收时这个是存放管道数据的数据指针) // data element
}

var (
	chansendpc = funcPC(chansend)
	chanrecvpc = funcPC(chanrecv)
)

func selectsetpc(pc *uintptr) {
	*pc = getcallerpc()
}

// 注释：把case里不为nil的管道加锁
func sellock(scases []scase, lockorder []uint16) {
	var c *hchan                  // 注释：声明一个为nil的管道
	for _, o := range lockorder { // 注释：循环锁（需要加锁的管道slice）
		c0 := scases[o].c // 注释：取出对应的管道
		if c0 != c {      // 注释：判断管道是否为nil，如果存在则加锁
			c = c0        // 注释：把指针复制一份给声明的管道指针
			lock(&c.lock) // 注释：把管道的指针对应的管道加锁
		}
	}
}

// 注释：设置case对应的管道解锁
func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// 注释：我们必须非常小心，不要在解锁最后一把锁后触摸sel，因为sel可以在最后一次解锁后立即释放。
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// 注释：考虑以下情况。第一个M调用runtime·park（）在runtime·selectgo（）中传递sel。
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// 注释：一旦runtime·park（）解锁了最后一个锁，另一个M会使调用select的G再次可运行，并安排执行。
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	// 注释：当G在另一个M上运行时，它锁定所有锁并释放sel。现在，如果第一个M触摸sel，它将访问释放的内存。
	for i := len(lockorder) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration // 注释：将在下一次迭代中解锁
		}
		unlock(&c.lock) // 注释：解锁
	}
}

func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on a
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	atomic.Store8(&gp.parkingOnChan, 0)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock any of the
	// channel locks we risk gp getting readied by a channel operation
	// and so gp could continue running before everything before the
	// unlock is visible (even to gp itself).

	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			unlock(&lastc.lock)
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock)
	}
	return true
}

func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceEvGoStop, 1) // forever
}

// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16 where ncases must be <= 65536.
// Both reside on the goroutine's stack (regardless of any escaping in
// selectgo).
//
// For race detector builds, pc0 points to an array of type
// [ncases]uintptr (also on the stack); for other builds, it's set to
// nil.
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// 注释：selectgo返回所选scase的索引，该索引与相应select｛recv，send，default｝调用的序号位置相匹配。
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
// 注释：此外，如果选择的scase是一个接收操作，它会报告是否接收到值。
//
// 注释：运行select case语句的时候会执行该函数
// 注释：cas0 存放的是case管道数组首指针；
// 注释：order0 存放的值是case管道数组的下标数组首指针。
// 注释：nsends 存放case里发送管道类型的管道数量
// 注释：nrecvs 存放case里接受管道类型的管道数量
// 注释：block 是否需要阻塞
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr, nsends, nrecvs int, block bool) (int, bool) {
	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// NOTE: In order to maintain a lean stack size, the number of scases
	// is capped at 65536.
	// 注释：为了保持精简堆栈大小，scase的数量上限为65536。
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))      // 注释：case是存放在数组里的，数组个数是1<<16个
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0)) // 注释：（大小是case的两倍）存放排序后的数组下标，前半部分是乱序的下标，后半部分是下标对应的锁

	ncases := nsends + nrecvs                    // 注释：接受管道和发送管道的个数
	scases := cas1[:ncases:ncases]               // 注释：管道数组转换成slice
	pollorder := order1[:ncases:ncases]          // 注释：(order1数组的上半部分)存储slice打乱后的数组下标
	lockorder := order1[ncases:][:ncases:ncases] // 注释：(order1数组的下半部分)存储slice下标对应的锁数据
	// NOTE: pollorder/lockorder's underlying array was not zero-initialized by compiler.

	// Even when raceenabled is true, there might be select
	// statements in packages compiled without -race (e.g.,
	// ensureSigM in runtime/signal_unix.go).
	var pcs []uintptr
	if raceenabled && pc0 != nil { // 注释：判读是否开启数据竞争
		pc1 := (*[1 << 16]uintptr)(unsafe.Pointer(pc0))
		pcs = pc1[:ncases:ncases]
	}
	casePC := func(casi int) uintptr {
		if pcs == nil {
			return 0
		}
		return pcs[casi]
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out. The general code handles those
	// cases correctly, and they are rare enough not to bother
	// optimizing (and needing to test).

	// generate permuted order
	norder := 0             // 注释：没有随机的case切片下标
	for i := range scases { // 注释：遍历case切片数据，打乱case切片数据，实现case的随机执行
		cas := &scases[i] // 注释：获取case的数据

		// Omit cases without channels from the poll and lock orders.
		if cas.c == nil { // 注释：跳过值为nil的管道
			cas.elem = nil // allow GC
			continue
		}

		j := fastrandn(uint32(norder + 1)) // 注释：随机出一个下标，通过存放下标，进行数据打乱，实现case随机执行
		pollorder[norder] = pollorder[j]   // 注释：交换数据，当前数组下标数据和随机后的下标数据进行交换，数据为case切片的小标编号
		pollorder[j] = uint16(i)           // 注释：把当前的小标编号存放到随机的下标编号的位置。
		norder++                           // 注释：当前下标加一
	}
	pollorder = pollorder[:norder] // 注释：记录打乱后的case切片有效数据的下标
	lockorder = lockorder[:norder] // 注释：用来记录case切片有效数据的下标，标记case下标对应的锁

	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	// 注释：根据Hchan地址对cases进行排序以获得锁定顺序。简单的堆排序，以保证log n时间和恒定的堆栈占用空间。
	// 注释：大堆排序
	// 注释：构建大堆二叉树
	for i := range lockorder { // 注释：程序向后遍历
		j := i // 注释：记录当前节点坐标
		// Start with the pollorder to permute cases on the same channel.
		// 注释：从轮询顺序开始，在同一频道上排列案例。
		c := scases[pollorder[i]].c // 注释：(当前case的下标)获取打乱后的case的管道数据
		// 注释：程序向前遍历
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() { // 注释：父节点和子节点比较，子节点大于父节点是交换数据
			k := (j - 1) / 2            // 注释：父节点位置
			lockorder[j] = lockorder[k] // 注释：交换节点数据
			j = k                       // 注释：从父节点位置重新循环比较
		}
		lockorder[j] = pollorder[i] // 注释：最大节点位置存储当前case的下标
	}
	// 注释：从后开始循环，里面是从前循环，里面循环结束条件是大于外面循环的key或者父节点本身就大于子节点。
	// 注释：从大堆二叉树踢出根节点，组成倒叙数组
	for i := len(lockorder) - 1; i >= 0; i-- {
		o := lockorder[i]           // 注释：当前下标(记录最后一个节点)
		c := scases[o].c            // 注释：（当前遍历的节点是：最后一个子节点）当前case
		lockorder[i] = lockorder[0] // 注释：把跟节点放到最后一个节点的位置(其实就是踢出跟节点,因为下次循环的时候i--正好把最后一个节点踢出了)
		j := 0                      // 注释：每次都是从跟节点开始遍历
		for {
			k := j*2 + 1 // 注释：左子节点(右子节点是j*2+2)
			if k >= i {  // 注释：如果到最后一个子节点则退出循环(左子节点大于最后一个节点时退出)
				break
			}
			// 注释：比较左子节点和右子节点，记录大节点的key
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++ // 注释：右子节点
			}
			// 注释：子节点和父节点比较，如果子节点大的话就和父节点交换数据，然后跳到子节点位置重新循环比较(这个顺序是从跟节点到子节点遍历)
			if c.sortkey() < scases[lockorder[k]].c.sortkey() { // 注释：父节点和子节点比较
				lockorder[j] = lockorder[k] // 注释：子节点赋值给父节点
				j = k                       // 注释：(替换节点位置)从子节点开始向后遍历
				continue
			}
			break // 注释：如果父节点本身就是大值则退出循环
		}
		lockorder[j] = o // 注释：每次取出一个最大节点（把最大节点从树中移除，插入到这个数组里面）
	}

	if debugSelect {
		for i := 0; i+1 < len(lockorder); i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}

	// lock all the channels involved in the select // 注释：锁定选择中涉及的所有通道
	sellock(scases, lockorder) // 注释：把case里不为nil的管道加锁(执行完case会解锁)

	var (
		gp     *g
		sg     *sudog
		c      *hchan
		k      *scase
		sglist *sudog
		sgnext *sudog
		qp     unsafe.Pointer
		nextp  **sudog
	)

	// pass 1 - look for something already waiting
	// 注释：情况1 发现已经准好的管道，
	// 注释：发送时:接收阻塞队列有值或者缓冲区有空位置时;
	// 注释：接收时:发送阻塞队列有值或者缓冲区有值;
	var casi int
	var cas *scase
	var caseSuccess bool
	var caseReleaseTime int64 = -1
	var recvOK bool
	for _, casei := range pollorder { // 注释：遍历打乱顺序后的下标(就是实现随机执行case，这里根据这个乱序循环执行对应的case)
		casi = int(casei)   // 注释：(要执行的case元素下标)拿出当前下标
		cas = &scases[casi] // 注释：case的元素，当前下标对应的case
		c = cas.c           // 注释：chan管道

		if casi >= nsends { // 注释：(处理管道读操作)大于发送总数，代表case是接收管道
			sg = c.sendq.dequeue() // 注释：到写入阻塞队列中取出数据，（接收管道，首先是看发送阻塞管道里是否有数据，如果有数据优先取出来）
			if sg != nil {         // 注释：如果发送阻塞队列中有数据，则跳到接收位置处理接收管道数据
				goto recv // 注释：跳到处理接收管道数据的代码位置
			}
			if c.qcount > 0 { // 如果走到这里代表阻塞队列中没有数据，并且管道里有数据，则说明管道缓冲区里有数据，跳到处理管道缓冲区位置处理读取数据
				goto bufrecv // 注释：跳到处理管道缓冲区代码位置，读取管道数据
			}
			if c.closed != 0 { // 注释：如果管道已经关闭则，跳到处理管道读取关闭时处理的函数位置
				goto rclose // 注释：跳到度关闭管道代码位置
			}
		} else { // 注释：处理管道写操作
			if raceenabled { // 注释：判断是否开启数据竞争
				racereadpc(c.raceaddr(), casePC(casi), chansendpc) // 注释：检测数据竞争
			}
			if c.closed != 0 { // 注释：如果管道已经关闭，则不能向管道写入数据，跳到处理向已经关闭的管道写数据的代码位置，（其实就是panic）
				goto sclose // 注释：跳到处理向已经关闭的管道写数据的代码位置
			}
			sg = c.recvq.dequeue() // 注释：踢出接收管道的首元素
			if sg != nil {         // 注释：如果有值，则表示接收阻塞队列里有值，则优先发送到阻塞接收队列里的G
				goto send // 注释：执行发送到接收阻塞队列G里
			}
			if c.qcount < c.dataqsiz { // 注释：如果当前元素数量小于最大存储的容量时，需要向缓冲区里添加元素
				goto bufsend // 注释：向缓冲区里添加元素
			}
		}
	}

	if !block { // 注释：如果没有设置必须阻塞，则解锁，并且返回case下标和读取的状态
		selunlock(scases, lockorder) // 注释：解锁
		casi = -1                    // 注释：重置case下标。设置成无效
		goto retc                    // 注释：执行返回代码
	}

	// pass 2 - enqueue on all chans
	// 注释：情况2 【ing】
	gp = getg()
	if gp.waiting != nil {
		throw("gp.waiting != nil")
	}
	nextp = &gp.waiting
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c
		sg := acquireSudog()
		sg.g = gp
		sg.isSelect = true
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		sg.elem = cas.elem
		sg.releasetime = 0
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c
		// Construct waiting list in lock order.
		*nextp = sg
		nextp = &sg.waitlink

		if casi < nsends {
			c.sendq.enqueue(sg)
		} else {
			c.recvq.enqueue(sg)
		}
	}

	// wait for someone to wake us up
	gp.param = nil
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)
	gopark(selparkcommit, nil, waitReasonSelect, traceEvGoBlockSelect, 1)
	gp.activeStackChans = false

	sellock(scases, lockorder)

	gp.selectDone = 0
	sg = (*sudog)(gp.param)
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	// 注释：从不成功的通道中通过3-出列，否则它们会堆积在安静的通道上，记录成功的情况（如果有的话）。我们把SudoG单独按锁定顺序连接起来。
	casi = -1
	cas = nil
	caseSuccess = false
	sglist = gp.waiting
	// Clear all elem before unlinking from gp.waiting.
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false
		sg1.elem = nil
		sg1.c = nil
	}
	gp.waiting = nil

	for _, casei := range lockorder {
		k = &scases[casei]
		if sg == sglist {
			// sg has already been dequeued by the G that woke us up.
			casi = int(casei)
			cas = k
			caseSuccess = sglist.success
			if sglist.releasetime > 0 {
				caseReleaseTime = sglist.releasetime
			}
		} else {
			c = k.c
			if int(casei) < nsends {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	if cas == nil {
		throw("selectgo: bad wakeup")
	}

	c = cas.c

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " send=", casi < nsends, "\n")
	}

	if casi < nsends {
		if !caseSuccess {
			goto sclose
		}
	} else {
		recvOK = caseSuccess
	}

	if raceenabled {
		if casi < nsends {
			raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
		} else if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
	}
	if msanenabled {
		if casi < nsends {
			msanread(cas.elem, c.elemtype.size)
		} else if cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.size)
		}
	}

	selunlock(scases, lockorder)
	goto retc

bufrecv: // 注释：跳到处理管道缓冲区代码位置，读取管道数据
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
		racenotify(c, c.recvx, nil)
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.size)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder)
	goto retc

bufsend: // 注释：向缓冲区里添加元素
	// can send to buffer
	if raceenabled {
		racenotify(c, c.sendx, nil)
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder)
	goto retc

recv: // 注释：处理接收管道数据的代码位置
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose: // 注释：跳到度关闭管道代码位置
	// read at end of closed channel
	selunlock(scases, lockorder)
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send: // 注释：执行发送到接收阻塞队列G里
	// can send to a sleeping receiver (sg)
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc: // 注释：执行返回代码，返回case下标和读取时的状态
	if caseReleaseTime > 0 {
		blockevent(caseReleaseTime-t0, 1)
	}
	return casi, recvOK

sclose: // 注释：处理向已经关闭的管道写数据(会触发panic)
	// send on closed channel
	selunlock(scases, lockorder) // 注释：解锁
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	return uintptr(unsafe.Pointer(c))
}

// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)
	ch  *hchan         // channel
	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	if len(cases) == 0 {
		block()
	}
	sel := make([]scase, len(cases))
	orig := make([]int, len(cases))
	nsends, nrecvs := 0, 0
	dflt := -1
	for i, rc := range cases {
		var j int
		switch rc.dir {
		case selectDefault:
			dflt = i
			continue
		case selectSend:
			j = nsends
			nsends++
		case selectRecv:
			nrecvs++
			j = len(cases) - nrecvs
		}

		sel[j] = scase{c: rc.ch, elem: rc.val}
		orig[j] = i
	}

	// Only a default case.
	if nsends+nrecvs == 0 {
		return dflt, false
	}

	// Compact sel and orig if necessary.
	if nsends+nrecvs < len(cases) {
		copy(sel[nsends:], sel[len(cases)-nrecvs:])
		copy(orig[nsends:], orig[len(cases)-nrecvs:])
	}

	order := make([]uint16, 2*(nsends+nrecvs))
	var pc0 *uintptr
	if raceenabled {
		pcs := make([]uintptr, nsends+nrecvs)
		for i := range pcs {
			selectsetpc(&pcs[i])
		}
		pc0 = &pcs[0]
	}

	chosen, recvOK := selectgo(&sel[0], &order[0], pc0, nsends, nrecvs, dflt == -1)

	// Translate chosen back to caller's ordering.
	if chosen < 0 {
		chosen = dflt
	} else {
		chosen = orig[chosen]
	}
	return chosen, recvOK
}

func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
