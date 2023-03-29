// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go's map type.
//
// A map is just a hash table. The data is arranged
// into an array of buckets. Each bucket contains up to
// 8 key/elem pairs. The low-order bits of the hash are
// used to select a bucket. Each bucket contains a few
// high-order bits of each hash to distinguish the entries
// within a single bucket.
//
// If more than 8 keys hash to a bucket, we chain on
// extra buckets.
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big. Buckets are incrementally
// copied from the old bucket array to the new bucket array.
//
// Map iterators walk through the array of buckets and
// return the keys in walk order (bucket #, then overflow
// chain order, then bucket index).  To maintain iteration
// semantics, we never move keys within their bucket (if
// we did, keys might be returned 0 or 2 times).  When
// growing the table, iterators remain iterating through the
// old table and must check the new table if the bucket
// they are iterating through has been moved ("evacuated")
// to the new table.

// Picking loadFactor: too large and we have lots of overflow
// buckets, too small and we waste a lot of space. I wrote
// a simple program to check some stats for different loads:
// (64-bit, 8 byte keys and elems)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/elem pair
// hitprobe    = # of entries to check when looking up a present key
// missprobe   = # of entries to check when looking up an absent key
//
// Keep in mind this data is for maximally loaded tables, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// Maximum number of key/elem pairs a bucket can hold.
	bucketCntBits = 3
	bucketCnt     = 1 << bucketCntBits // 注释：一个桶最大可以存放8个数据

	// Maximum average load of a bucket that triggers growth is 6.5.
	// Represent as loadFactorNum/loadFactorDen, to allow integer math.
	loadFactorNum = 13
	loadFactorDen = 2

	// Maximum key or elem size to keep inline (instead of mallocing per element).
	// Must fit in a uint8.
	// Fast versions cannot handle big elems - the cutoff size for
	// fast versions in cmd/compile/internal/gc/walk.go must be at most this elem.
	maxKeySize  = 128
	maxElemSize = 128

	// data offset should be the size of the bmap struct, but needs to be
	// aligned correctly. For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	// 注释：bmap占用的数据内存大小；结构体头指针到属性v指针的大小，这里就是计算bmap的大小(目前bmap里只有tophash,当前返回tophash后面的指针)
	dataOffset = unsafe.Offsetof(struct {
		b bmap // 注释：现在的bmap里只有一个元素tophash,这里返回tophash元素后面的位置,后面方便追加数据到bmap结构体里
		v int64
	}{}.v)

	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	emptyRest      = 0 // 注释：当前桶和链接的所有桶全部为空时 // this cell is empty, and there are no more non-empty cells at higher indexes or overflows.
	emptyOne       = 1 // 注释：只有当前桶为空时 // this cell is empty
	evacuatedX     = 2 // 注释：一倍扩容或二倍扩容的前半部分扩容 // key/elem is valid.  Entry has been evacuated to first half of larger table.
	evacuatedY     = 3 // 注释：二倍扩容的后半部分 (x=旧桶相同的位置的数据；y=x+旧桶总数的位置)// same as above, but evacuated to second half of larger table.
	evacuatedEmpty = 4 // 注释：旧桶的该位置已经被迁移了 // cell is empty, bucket is evacuated.
	minTopHash     = 5 // 注释：正常填充单元格的最小topHash。(5以内的数都是保留数) // minimum tophash for a normal filled cell.

	// flags
	iterator     = 1 // 注释：新桶迭代中标识 // there may be an iterator using buckets
	oldIterator  = 2 // 注释：旧桶迭代中标识 // there may be an iterator using oldbuckets
	hashWriting  = 4 // 注释：正在写入标识 // a goroutine is writing to the map
	sameSizeGrow = 8 // 注释：等量扩容标识 // the current map growth is to a new map of the same size

	// sentinel bucket ID for iterator checks
	noCheck = 1<<(8*sys.PtrSize) - 1
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
// 注释：判断桶内下标的位置是否为空的
func isEmpty(x uint8) bool {
	return x <= emptyOne // 注释：如果下标小于等于1时说明该下标位置为空
}

// A header for a Go map.
// 注释：map结构体
type hmap struct {
	// Note: the format of the hmap is also encoded in cmd/compile/internal/gc/reflect.go.
	// Make sure this stays in sync with the compiler's definition.
	count      int            // 注释：map元素总数 // # live cells == size of map.  Must be first (used by len() builtin)
	flags      uint8          // 注释：标识，用来记录map当时的状态，比如正在扩容中、新桶迭代中、旧桶迭代中、正在写入
	B          uint8          // 注释：桶数是2的B次方，所以桶数都是2的整数倍 // log_2 of # of buckets (can hold up to loadFactor * 2^B items)
	noverflow  uint16         // 注释：溢出桶数量，计算如何发生二倍扩容时需要 // approximate number of overflow buckets; see incrnoverflow for details
	hash0      uint32         // 注释：hash种子，每次初始化时随机生成，等倍扩容是会从新生成，用来打散元素分配到不同桶里 // hash seed
	buckets    unsafe.Pointer // 注释：桶指针，桶指针指向的是一个数组的头指针，数组会在末尾处预留一些溢出桶位置，如果count=0时为nil // array of 2^B Buckets. may be nil if count==0.
	oldbuckets unsafe.Pointer // 注释：旧桶指针，只有发生扩容时才会出现 // previous bucket array of half the size, non-nil only when growing
	nevacuate  uintptr        // 注释：旧桶移动到新桶的数量（移动的进度） // progress counter for evacuation (buckets less than this have been evacuated)
	extra      *mapextra      // 注释：扩展字段，用来记录溢出桶指针和旧溢出桶指针（发生扩容时出现），这里的溢出桶是对象指针，不是数组头指针 // optional fields
}

// mapextra holds fields that are not present on all maps.
// 注释：hmap的扩展字段
type mapextra struct {
	// If both key and elem do not contain pointers and are inline, then we mark bucket
	// type as containing no pointers. This avoids scanning such maps.
	// However, bmap.overflow is a pointer. In order to keep overflow buckets
	// alive, we store pointers to all overflow buckets in hmap.extra.overflow and hmap.extra.oldoverflow.
	// overflow and oldoverflow are only used if key and elem do not contain pointers.
	// overflow contains overflow buckets for hmap.buckets.
	// oldoverflow contains overflow buckets for hmap.oldbuckets.
	// The indirection allows to store a pointer to the slice in hiter.
	overflow    *[]*bmap // 注释：溢出桶对象指针
	oldoverflow *[]*bmap // 注释：旧溢出桶对象指针，只有扩容的时候才会出现

	// nextOverflow holds a pointer to a free overflow bucket.
	nextOverflow *bmap // 注释：下一个空溢出桶对象指针，但需要新溢出桶时会直接使用这个并加入到溢出桶链表中，然后这里会生成一个新的指针
}

// A bucket for a Go map.
// 注释：桶结构体（每个桶8个数据）
type bmap struct {
	// tophash generally contains the top byte of the hash value
	// for each key in this bucket. If tophash[0] < minTopHash,
	// tophash[0] is a bucket evacuation state instead.
	tophash [bucketCnt]uint8 // 注释：记录key的hash的高8位，这里会存8个数据（每个桶8个数据）
	// 注释：后面还有3个字段，是在运行时出现的，分别时keys,values(旧版本中时elem),overflow
	// 注释：keys和values的存储方式是：8个key和8个value（旧版本中是key/elem/key/elem的方式共8组）
	// 注释：overflow是溢出桶的地址
	// Followed by bucketCnt keys and then bucketCnt elems.
	// NOTE: packing all the keys together and then all the elems together makes the
	// code a bit more complicated than alternating key/elem/key/elem/... but it allows
	// us to eliminate padding which would be needed for, e.g., map[int64]int8.
	// Followed by an overflow pointer.
}

// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/gc/reflect.go to indicate
// the layout of this structure.
// 注释：map遍历迭代的时候使用的结构体
type hiter struct {
	key         unsafe.Pointer // 注释：迭代返回的key // Must be in first position.  Write nil to indicate iteration end (see cmd/compile/internal/gc/range.go).
	elem        unsafe.Pointer // 注释：迭代返回的value // Must be in second position (see cmd/compile/internal/gc/range.go).
	t           *maptype       // 注释：map的类型结构体指针
	h           *hmap          // 注释：hmap的结构体指针
	buckets     unsafe.Pointer // 注释：hmap桶指针 // bucket ptr at hash_iter initialization time
	bptr        *bmap          // 注释：当前桶的指针 // current bucket
	overflow    *[]*bmap       // keeps overflow buckets of hmap.buckets alive
	oldoverflow *[]*bmap       // keeps overflow buckets of hmap.oldbuckets alive
	startBucket uintptr        // 注释：开始遍历的桶号（遍历初始化时随机） // bucket iteration started at
	offset      uint8          // 注释：桶内偏移量小标（遍历初始化时随机） // intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1)
	wrapped     bool           // 注释：标记已经到过桶的尾部过了（已经从bucket数组的末尾绕到了开头）already wrapped around from end of bucket array to beginning
	B           uint8          // 注释：获取桶数量B快照
	i           uint8          // 注释：单个桶内遍历的下标（真实下标是：offi := (i + it.offset) & (bucketCnt - 1)）
	bucket      uintptr        // 注释：当前正在遍历的桶的编号
	checkBucket uintptr        // 注释：记录需要检查的桶号，如果不需要检查桶号时，就放一个最大数，这个特殊的数是不会被检查
}

// bucketShift returns 1<<b, optimized for code generation.
// 注释：最大桶下标。返回 1 << b ; 最后一个桶的下标
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.
	// 注释：sys.PtrSize是系统指针的大小，32位系统是4,64位系统是8
	// 注释：(b & (sys.PtrSize*8 - 1))是为了防止b超出
	return uintptr(1) << (b & (sys.PtrSize*8 - 1))
}

// bucketMask returns 1<<b - 1, optimized for code generation.
// 注释：返回 1<<b - 1, 就是最大有效桶的数量，桶的掩码
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// tophash calculates the tophash value for hash.
// 注释：取出hash的高8位
func tophash(hash uintptr) uint8 {
	// 注释：向右移动，保留8位
	top := uint8(hash >> (sys.PtrSize*8 - 8))
	// 注释：确保高8位一定大于minTopHash,minTopHash数以内的都是预留的数，有特殊含义的预留数字
	if top < minTopHash {
		top += minTopHash
	}
	return top
}

// 注释：判断是否全部迁移到新的桶中
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	// 注释：evacuatedEmpty、evacuatedX、evacuatedY 这三个值之一，说明此bucket中的key全部被搬迁到了新bucket
	return h > emptyOne && h < minTopHash
}

func (b *bmap) overflow(t *maptype) *bmap {
	// 获取当前桶最后一个指针类型的地址,即*bmap.overflow;转换成**bmap类型，然后通过*获取内容
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize))
}

func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) = ovf
}

func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth.
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16.
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	if h.B < 16 {
		h.noverflow++
		return
	}
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and fastrand & 7 == 0 with probability 1/8.
	// 注释：数值越大概率越低,概率是1/(2^(h.B-15))
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		ovf = h.extra.nextOverflow  // 注释：指向下一个空的预分配的溢出桶指针
		if ovf.overflow(t) == nil { // 注释：下一个溢出桶（预分配的溢出桶）中最后一个元素（值是溢出桶链表的指针）
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.bucketsize))) // 注释：指向第三个预分配的溢出桶
		} else {
			// 注释：如果非空（初始化的时候赋的一个值,就是一个标识位）时，这个非空值已经无意义了，重新设置为空
			// This is the last preallocated overflow bucket.
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value.
			ovf.setoverflow(t, nil)    // 注释：清空初始化的哪个标识位
			h.extra.nextOverflow = nil // 注释：设置成nil说明已经用完了所有预分配的溢出桶
		}
	} else {
		ovf = (*bmap)(newobject(t.bucket))
	}
	h.incrnoverflow() // 注释：自增溢出桶数量，这个数量是大概的估算值(B < 16时是准确值，大于时是预估值)
	if t.bucket.ptrdata == 0 {
		h.createOverflow()                                 // 注释：创建已经使用的溢出桶数组
		*h.extra.overflow = append(*h.extra.overflow, ovf) // 注释：追加使用的溢出桶
	}
	b.setoverflow(t, ovf) // 注释：把新的空溢出桶地址加入链表尾部（溢出桶链表bmap.overflow）
	return ovf
}

func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	// hint 是map的输入数量，这里保证map的数量不能超过系统的int值,如果超过了设置为0
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap_small implements Go map creation for make(map[k]v) and
// make(map[k]v, hint) when hint is known to be at most bucketCnt
// at compile time and the map needs to be allocated on the heap.
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand()
	return h
}

// makemap implements Go map creation for make(map[k]v, hint).
// If the compiler has determined that the map or the first bucket
// can be created on the stack, h and/or bucket may be non-nil.
// If h != nil, the map can be created directly in h.
// If h.buckets != nil, bucket pointed to can be used as the first bucket.
// 注释：t *maptype对象记键值对和桶的大小等必要信息,hint是map元素大小（输入时的参数），h是hmap结构体
func makemap(t *maptype, hint int, h *hmap) *hmap {
	mem, overflow := math.MulUintptr(uintptr(hint), t.bucket.size)
	if overflow || mem > maxAlloc {
		hint = 0
	}

	// initialize Hmap
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = fastrand()

	// Find the size parameter B which will hold the requested # of elements.
	// For hint < 0 overLoadFactor returns false since hint < bucketCnt.
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// allocate initial hash table
	// if B == 0, the buckets field is allocated lazily later (in mapassign)
	// If hint is large zeroing this memory could take a while.
	// 注释：如果make的时候指定了map的长度时执行下面分支
	if h.B != 0 {
		var nextOverflow *bmap                                 // 注释：下一个空闲溢出桶地址
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil) // 注释：创建桶和溢出桶的数组,返回正常桶首地址和溢出桶首地址
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// makeBucketArray initializes a backing array for map buckets.
// 1<<b is the minimum number of buckets to allocate.
// dirtyalloc should either be nil or a bucket array previously 注释：桶地址
// allocated by makeBucketArray with the same t and b parameters.
// If dirtyalloc is nil a new backing array will be alloced and
// otherwise dirtyalloc will be cleared and reused as backing array.
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	base := bucketShift(b) // 注释：桶的数量:1<<b
	nbuckets := base       // 注释：变量表示：正常桶+溢出桶
	// For small b, overflow buckets are unlikely.
	// Avoid the overhead of the calculation.
	// 注释：正常桶数量大于等于16时,创建溢出桶
	if b >= 4 {
		// Add on the estimated number of overflow buckets
		// required to insert the median number of elements
		// used with this value of b.
		nbuckets += bucketShift(b - 4) // 注释：根据1/16的方式添加溢出桶；相当于nbuckets += nbuckets/16
		sz := t.bucket.size * nbuckets // 注释：正常桶数-16的桶数的容量(所需要的栈空间)
		up := roundupsize(sz)          // 注释：返回对齐后需要申请的内存块大小
		// 注释：判断对齐后的空间和对齐前的空间是否相等
		if up != sz {
			// 注释：使用对齐后的空间对应的桶数
			nbuckets = up / t.bucket.size
		}
	}

	// 注释：如果桶地址为nil则创建桶地址
	if dirtyalloc == nil {
		buckets = newarray(t.bucket, int(nbuckets)) // 注释：创建桶数组
	} else {
		// dirtyalloc was previously generated by
		// the above newarray(t.bucket, int(nbuckets))
		// but may not be empty.
		buckets = dirtyalloc
		size := t.bucket.size * nbuckets
		if t.bucket.ptrdata != 0 {
			memclrHasPointers(buckets, size)
		} else {
			memclrNoHeapPointers(buckets, size)
		}
	}

	// 注释：base是正常桶数，nbuckets是正常桶加溢出桶的数量
	if base != nbuckets {
		// We preallocated some overflow buckets.
		// To keep the overhead of tracking these overflow buckets to a minimum,
		// we use the convention that if a preallocated overflow bucket's overflow
		// pointer is nil, then there are more available by bumping the pointer.
		// We need a safe non-nil pointer for the last overflow bucket; just use buckets.
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.bucketsize)))  // 注释：溢出桶首地址
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.bucketsize))) // 注释：计算最后一个溢出桶地址
		last.setoverflow(t, (*bmap)(buckets))                             // 注释：最后一个overflow赋了一个值(就是一个标识位)
	}
	return buckets, nextOverflow
}

// mapaccess1 returns a pointer to h[key].  Never returns nil, instead
// it will return a reference to the zero object for the elem type if
// the key is not in the map.
// NOTE: The returned pointer may keep the whole map live, so don't
// hold onto it for very long.
// 注释：使用 v := m["xx"] 的形式访问时执行的方法
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	// 注释：判断是否启用数据竞争检测
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	// 注释：判断是否启用支持与内存清理程序的交互操作
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	// 注释：如果map是nil或长度是0时返回对应反射类型的零值
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}
	// 注释：判断是否处于写的状态
	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}
	hash := t.hasher(key, uintptr(h.hash0)) // 注释：根据key和随机种子计算hash值
	m := bucketMask(h.B)                    // 注释：新桶掩码(低h.B位)
	// 注释：桶首地址+（hash值对应低B位的位置*桶的尺寸）
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize))) // 注释：key对应的桶地址
	// 注释：判断旧桶是否有值
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() { // 注释：是否处于扩容状态
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1 // 注释：旧桶的掩码
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize))) // 注释：旧桶对应的key地址
		if !evacuated(oldb) {                                   // 注释：判断b.tophash高8位是否等于预留的值，如果不是则使用旧桶地址
			b = oldb
		}
	}
	top := tophash(hash) // 注释：计算hash的高8位
bucketloop:
	// 注释：遍历所有溢出桶；b = bmap.overflow是溢出桶地址,每个溢出桶都是通过overflow字段连接起来的单向链表
	for ; b != nil; b = b.overflow(t) { // 注释：通过overflow字段遍历链表，拉链法解决哈希冲突
		for i := uintptr(0); i < bucketCnt; i++ { // 注释：循环单个桶数据，每个桶有8个元素，开放寻址发中的线性探测策略解决哈希冲突
			if b.tophash[i] != top { // 注释：判断hash的高8位是否相等
				if b.tophash[i] == emptyRest { // 注释：判断当前桶和后面链接的桶是否全部都是空的
					break bucketloop
				}
				continue
			}
			// 注释：通过地址偏移找到key的；当前桶地址+偏移量dataOffset+key的偏移量；dataOffset=8
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() { // 注释：(判断是否是指针)判断是否是间接指向key，如果是间接指向，说明key是指针，指向真正的key
				k = *((*unsafe.Pointer)(k)) // 注释：获取key
			}
			if t.key.equal(key, k) { // 注释：比较key是否相等
				// 注释：桶头指针+hash高8位（dataOffset=tophash=8）+ 桶内最大元素数(bucketCnt)*key的容量+桶内编号*值的尺寸
				// 注释：真实bmap的内存数据是:8位tophash+[8]key(单个key是指针大小)+[8]value(单个value是指针大小)+overflow指针大小
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize)) // 注释：具体的value值
				if t.indirectelem() {                                                                      // 注释：（判断是否是指针）判断值是否是间接的,如果不是指针，则说明value就是自己
					e = *((*unsafe.Pointer)(e)) // 注释：获取value
				}
				return e
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0])
}

// 注释：使用 v,ok := m["xx"]的形式访问时执行的方法
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}
	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// returns both key and elem. Used by map iterator
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		return nil, nil
	}
	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return k, e
			}
		}
	}
	return nil, nil
}

func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// 注释：创建或修改map函数(map的分配)
// Like mapaccess, but allocates a slot for the key if it is not present in the map.
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}
	// 注释：数据竟成是否开启
	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	// 注释：内存msan是否开启
	if msanenabled {
		msanread(key, t.key.size)
	}
	// 注释：如果标志位写位置如果开启说明正在有其他协成在写入，报错
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}
	// 注释：通过key和随机种子获取hash值
	hash := t.hasher(key, uintptr(h.hash0))

	// 注释：写标志位加锁
	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write.
	h.flags ^= hashWriting

	// 注释：判断是否有桶，如果没有就创建一个同
	if h.buckets == nil {
		h.buckets = newobject(t.bucket) // newarray(t.bucket, 1)
	}

again:
	// 注释：新桶序号：通过桶掩码获取key的目标桶号
	bucket := hash & bucketMask(h.B)
	// 注释：判断数据是否需要迁移；判断是否正在扩容(旧桶有值就说明正在扩容)
	if h.growing() {
		// 注释：数据迁移
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize))) // 注释：桶号对应的桶地址

	top := tophash(hash) // 注释：hash高8位

	var inserti *uint8         // 注释：准备写入的topbits地址
	var insertk unsafe.Pointer // 注释：准备写入的key地址
	var elem unsafe.Pointer    // 注释：准备写入的value地址
bucketloop:
	for {
		for i := uintptr(0); i < bucketCnt; i++ { // 注释：遍历桶内数据，共8组数据
			if b.tophash[i] != top {
				if isEmpty(b.tophash[i]) && inserti == nil { // 注释：如果bmap里的topbits为空时(包括当前桶里为空或后面链表桶的tophash全部为空)
					inserti = &b.tophash[i]                                                                      // 注释：把空对应的指针赋值，等待后面插入，方便后面修改
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))                            // 注释：映射key的指针，方便后面修改
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize)) // 注释：映射val的指针，方便后面修改
				}
				if b.tophash[i] == emptyRest { // 注释：判断后面链表桶里全部为空
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if !t.key.equal(key, k) {
				continue
			}
			// already have a mapping for key. Update it.
			if t.needkeyupdate() {
				typedmemmove(t.key, k, key)
			}
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			goto done
		}
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// Did not find mapping for key. Allocate new cell & add entry.

	// If we hit the max load factor or we have too many overflow buckets,
	// and we're not already in the middle of growing, start growing.
	// 注释：判断是否需要扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		// 注释：修改hmap结构体完成后，重新执行并执行数据迁移
		goto again // Growing the table invalidates everything, so try again
	}

	if inserti == nil {
		// The current bucket and all the overflow buckets connected to it are full, allocate a new one.
		newb := h.newoverflow(t, b)
		inserti = &newb.tophash[0]
		insertk = add(unsafe.Pointer(newb), dataOffset)
		elem = add(insertk, bucketCnt*uintptr(t.keysize))
	}

	// store new key/elem at insert position
	if t.indirectkey() {
		kmem := newobject(t.key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	if t.indirectelem() {
		vmem := newobject(t.elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	typedmemmove(t.key, insertk, key)
	*inserti = top
	h.count++

done:
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
	if t.indirectelem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}

func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	hash := t.hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	bOrig := b
	top := tophash(hash)
search:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			k2 := k
			if t.indirectkey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.key.equal(key, k2) {
				continue
			}
			// Only clear key if there are pointers in it.
			if t.indirectkey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.key.ptrdata != 0 {
				memclrHasPointers(k, t.key.size)
			}
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			if t.indirectelem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.elem.ptrdata != 0 {
				memclrHasPointers(e, t.elem.size)
			} else {
				memclrNoHeapPointers(e, t.elem.size)
			}
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			if i == bucketCnt-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					goto notLast
				}
			}
			for {
				b.tophash[i] = emptyRest
				if i == 0 {
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = bucketCnt - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			h.count--
			// Reset the hash seed to make it more difficult for attackers to
			// repeatedly trigger hash collisions. See issue 25237.
			if h.count == 0 {
				h.hash0 = fastrand()
			}
			break search
		}
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// mapiterinit initializes the hiter struct used for ranging over maps.
// The hiter struct pointed to by 'it' is allocated on the stack
// by the compilers order pass or on the heap by reflect_mapiterinit.
// Both need to have zeroed hiter since the struct contains pointers.
// 注释：for range循环map时初始化map，这里会做随机偏移量操作
// 注释：map迭代器初始化
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiterinit))
	}

	// 注释：如果没有数据或0个元素是直接返回
	if h == nil || h.count == 0 {
		return
	}

	if unsafe.Sizeof(hiter{})/sys.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/gc/reflect.go
	}
	it.t = t // 注释：类型结构体
	it.h = h // 注释：复制hmap结构体数据

	// grab snapshot of bucket state
	it.B = h.B             // 注释：获取桶数量快照
	it.buckets = h.buckets // 桶指针
	if t.bucket.ptrdata == 0 {
		// Allocate the current slice and remember pointers to both current and old.
		// This preserves all relevant overflow buckets alive even if
		// the table grows and/or overflow buckets are added to the table
		// while we are iterating.
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// decide where to start
	r := uintptr(fastrand())    // 注释：随机数32位
	if h.B > 31-bucketCntBits { // 注释：如果桶的次幂数大于28，则超出了随机数长度，需要加随机数长度
		r += uintptr(fastrand()) << 31
	}
	it.startBucket = r & bucketMask(h.B)          // 注释：随机桶（控制随机数在有效桶号内）。计算起始位置的桶号， r是随机数，bucketMask(h.B)是桶掩码（最大可以有多少个桶）
	it.offset = uint8(r >> h.B & (bucketCnt - 1)) // 注释：随机桶内数据（一个桶内是8个数据）

	// iterator state
	it.bucket = it.startBucket // 注释：记录开始遍历的桶的小标

	// Remember we have an iterator.
	// Can run concurrently with another mapiterinit().
	// 注释：判断map是否处于新桶迭代中(iterator)或旧桶迭代中(oldIterator)
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator)
	}

	mapiternext(it)
}

// 注释：map迭代器，迭代下一个
func mapiternext(it *hiter) {
	h := it.h // 注释：hmap的结构体指针
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiternext))
	}
	// 注释：判断是否正在写入（其他协成写入），如果正在写入则报错（发生了数据竞争）
	if h.flags&hashWriting != 0 {
		throw("concurrent map iteration and map write")
	}
	t := it.t                     // 注释：map的类型结构体指针
	bucket := it.bucket           // 注释：记录开始循环的桶号
	b := it.bptr                  // 注释：当前桶的指针，第一次nil，当前是nil
	i := it.i                     // 注释：初始化循环桶内的偏移量（默认是0，这里是0）
	checkBucket := it.checkBucket // 注释：记录需要检查的桶号，如果不需要检查桶号时，就放一个最大数，这个特殊的数是不会被检查

next:
	// 注释：如果没有要遍历的桶指针时，通过桶号获取桶指针，或者判断是否循环已经结束，然后返回
	// 注释：找到一个需要遍历的桶
	if b == nil {
		// 注释：当遍历的桶等于起始的桶号，并且标记已经经过了桶的末尾的时候表示已经循环完毕了，清空key和value并退出
		if bucket == it.startBucket && it.wrapped {
			// end of iteration
			it.key = nil
			it.elem = nil
			return
		}
		// 注释：判断是否处于扩容当中 和 桶数量快照如果定于桶数
		if h.growing() && it.B == h.B {
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			oldbucket := bucket & it.h.oldbucketmask()                      // 注释：去旧桶里找数据（找旧桶的桶号）
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))) // 注释：旧桶的桶指针
			// 注释：如果没有迁移完成时
			if !evacuated(b) {
				checkBucket = bucket // 注释：记录需要检查的桶号
			} else {
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize))) // 注释：如果全部迁移完成，则使用新桶的桶地址
				checkBucket = noCheck                                      // 注释：如果不需要检查桶号时，就放一个最大数，这个特殊的数是不会被检查
			}
		} else {
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize))) // 注释：如果没有处于扩容中时，使用新桶的地址
			checkBucket = noCheck                                      // 注释：如果不需要检查桶号时，就放一个最大数，这个特殊的数是不会被检查
		}
		bucket++
		// 注释：如果桶号等于最大桶号，表示已经到最后了，这个时候需要回到头部，并且标价已经到过桶尾部了
		if bucket == bucketShift(it.B) {
			bucket = 0
			it.wrapped = true // 注释：标记已经到过桶的尾部过了
		}
		i = 0 // 注释：每次选择新的桶时；桶内循环的下标需要回到0位置
	}
	// 注释：开始循环桶内数据，每个桶可以容纳8个数据
	for ; i < bucketCnt; i++ {
		offi := (i + it.offset) & (bucketCnt - 1)                          // 注释：根据随机起点，获取偏移量
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty { // 注释：当前下标位置为空时或者旧桶全部迁移完成了就跳过
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			continue
		}
		// 注释：找map的key，这里是用bmap的头指针+bmap大小（这里的大小是只有一个元素tophash的时候）+ (偏移量 * key的大小)；bmap在初始胡的时候就已预留了足够的空间
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.keysize))
		if t.indirectkey() { // 注释：判断是否是间接的key,如果是间接的key就取地址里的值
			k = *((*unsafe.Pointer)(k))
		}
		// 注释：取map的value，这里是用bmap的头指针 + bmap的大小（只有tophash元素）+ （8 * key的大小）+ （偏移量*value的大小）
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+uintptr(offi)*uintptr(t.elemsize))
		if checkBucket != noCheck && !h.sameSizeGrow() { // 注释：如果待检查的桶不是特殊的最大值时，并且不是等量扩容中时
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			if t.reflexivekey() || t.key.equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				hash := t.hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.reflexivekey() || t.key.equal(k, k)) {
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			it.key = k
			if t.indirectelem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			rk, re := mapaccessK(t, h, k) // 注释：正在扩容中时，迭代map
			if rk == nil {
				continue // key has been deleted
			}
			it.key = rk  // 注释：返回的key
			it.elem = re // 注释：返回的value
		}
		it.bucket = bucket // 注释：校准桶号
		if it.bptr != b {  // avoid unnecessary write barrier; see issue 14921
			it.bptr = b // 注释：如果桶指针发生变化时，修改对应的指针
		}
		it.i = i + 1                 // 注释：单个桶内的下标加1
		it.checkBucket = checkBucket // 注释：标记需要检查的桶号
		return
	}
	b = b.overflow(t) // 注释：处理溢出桶
	i = 0             // 注释：重置单个桶内的数据下标
	goto next
}

// mapclear deletes all keys from a map.
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	if h == nil || h.count == 0 {
		return
	}

	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	h.flags ^= hashWriting

	h.flags &^= sameSizeGrow
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// Reset the hash seed to make it more difficult for attackers to
	// repeatedly trigger hash collisions. See issue 25237.
	h.hash0 = fastrand()

	// Keep the mapextra allocation but clear any extra information.
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 注释：需要扩容时，修hmap结构体的部分数据,方便后期数据迁移
func hashGrow(t *maptype, h *hmap) {
	// If we've hit the load factor, get bigger.
	// Otherwise, there are too many overflow buckets,
	// so keep the same number of buckets and "grow" laterally.
	bigger := uint8(1) // 注释：扩容倍数（B的增长倍数），1二倍扩容，0等量扩容
	// 注释：判断是否需要等量扩容
	if !overLoadFactor(h.count+1, h.B) {
		bigger = 0
		h.flags |= sameSizeGrow // 注释：添加等量扩容标识
	}
	oldbuckets := h.buckets                                         // 注释：移动桶
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil) // 注释：创建新桶，申请内存

	flags := h.flags &^ (iterator | oldIterator) // 注释：创建新的map标识,清空迭代器标识
	// 注释：判断旧值是否处于迭代中,如果处于迭代中时，把新的标识设置为旧桶迭代中
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}
	// commit the grow (atomic wrt gc)
	h.B += bigger
	h.flags = flags
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// Promote current overflow buckets to the old generation.
		// 注释：判断旧的溢出桶是否完全移动完成，如果未完成报错
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow // 注释：设置旧的溢出桶
		h.extra.overflow = nil                 // 注释：清空新溢出桶
	}
	// 注释：如果有空溢出桶地址时这里指定到扩展结构体里
	if nextOverflow != nil {
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// the actual copying of the hash table data is done incrementally
	// by growWork() and evacuate().
}

// overLoadFactor reports whether count items placed in 1<<B buckets is over loadFactor.
// 注释：判断负载因子是否大于6.5
func overLoadFactor(count int, B uint8) bool {
	// 注释：当map元素总数大于8个，并且负载因子大于6.5（负载因子=元素数/正常桶数量）
	// 注释：uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen是为了防止浮点运算
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// Note that most of these overflow buckets must be in sparse use;
// if use was dense, then we'd have already triggered regular map growth.
// 注释： 判断溢出桶是否过多
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	// 注释：其实最后实现的是：noverflow >= 2^B （当B>15时是个近似值）
	return noverflow >= uint16(1)<<(B&15) // 注释：当B>15时，noverflow是随机增长（把大数缩写用随机填充），如果随机完全理想的话，无线接近:noverflow >= 2^B；当B<=15时：noverflow >= 2^B
}

// growing reports whether h is growing. The growth may be to the same size or bigger.
// 注释：判断是否处于正在扩容
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow reports whether the current growth is to a map of the same size.
// 注释：判断是否是等量扩容
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// noldbuckets calculates the number of buckets prior to the current map growth.
// 注释：旧桶的桶数
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() { // 注释：判断是否是2倍扩容
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask provides a mask that can be applied to calculate n % noldbuckets().
// 注释：旧桶的掩码
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// 注释：t 对象记键值对和桶的大小等必要信息； h map结构体； bucket 新桶桶号（序号）；
func growWork(t *maptype, h *hmap, bucket uintptr) {
	// make sure we evacuate the oldbucket corresponding
	// to the bucket we're about to use
	// 注释：执行数据迁移，将当前需要处理的桶搬迁，旧桶的序号:bucket&h.oldbucketmask()
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	// 注释：再多搬迁一个桶
	if h.growing() {
		evacuate(t, h, h.nevacuate)
	}
}

func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.bucketsize)))
	return evacuated(b)
}

// evacDst is an evacuation destination.
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/elem index into b
	k unsafe.Pointer // pointer to current key storage
	e unsafe.Pointer // pointer to current elem storage
}

// 注释：执行数据迁移
// 注释：t 对象记键值对和桶的大小等必要信息；h map结构体；oldbucket旧桶序号
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	// 注释：旧桶序号对应的指针
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
	// 注释：旧桶的总桶数量
	newbit := h.noldbuckets()
	// 注释：判断是否未迁移 , evacuatedEmpty、evacuatedX、evacuatedY 这三个值之一，说明此bucket中的key全部被搬迁到了新bucket
	if !evacuated(b) {
		// TODO: reuse overflow buckets instead of using new ones, if there
		// is no iterator using the old buckets.  (If !oldIterator.)

		// xy contains the x and y (low and high) evacuation destinations.
		// 注释：要转移数据的结构（二倍扩容时需要两个元素，等量扩容时需要一个元素）
		// 注释：xy是新桶的指针；这里会根据旧桶大小把新桶分为两部分，(新桶对应1倍旧桶为x区域,新桶对应2倍旧桶为y区域),x=旧桶相同的位置的数据；y=x+旧桶总数的位置
		// 注释：把要修改的地方的地址放在这里，然后下面针对这个结构体类型的数组进行操作
		var xy [2]evacDst
		// 注释：x的结构数据是：tophash1/tophash2/tophash3/tophash4/tophash5/tophash6/tophash7/tophash8/key1/key2/key3/key4/key5/key6/key7/key8/val1/val2/val3/val4/val5/val6/val7/val8）
		x := &xy[0] // 注释：新桶对应1倍旧桶为x区域
		// 注释：新桶序号对应的指针(旧桶和新桶的桶序号不变)
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.bucketsize)))
		// 注释：key的指针地址;dataOffset的值是8,值topbits的长度(编译时候赋值的)
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		// 注释：value的指针地址（key和val的内存分布是key1/key2/key3/key4/key5/key6/key7/key8/val1/val2/val3/val4/val5/val6/val7/val8）
		x.e = add(x.k, bucketCnt*uintptr(t.keysize))

		// 注释：判断是否是非等量扩容
		if !h.sameSizeGrow() {
			// Only calculate y pointers if we're growing bigger.
			// Otherwise GC can see bad pointers.
			y := &xy[1] // 注释：新桶对应2倍旧桶为y区域
			// 注释：新桶序号对应的第二个地址指针：新桶首地址+(oldbucket旧桶序号+旧桶的总桶数量)*桶的尺寸
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.bucketsize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, bucketCnt*uintptr(t.keysize))
		}

		// 注释：循环溢出桶（单向链表）,b.overflow中的b是旧桶序号对应的指针；返回溢出桶地址
		for ; b != nil; b = b.overflow(t) {
			// 注释：dataOffset的值是8,是bmap结构体大小（目前里面只有一个属性topbits）值topbits的长度(编译时候赋值的)
			k := add(unsafe.Pointer(b), dataOffset)   // 注释：溢出桶里key的首地址（一共8个）
			e := add(k, bucketCnt*uintptr(t.keysize)) // 注释：溢出桶value的首地址（一共8个）
			// 注释：遍历桶里的数据, i, k, e :对应下标，key和value
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.keysize)), add(e, uintptr(t.elemsize)) {
				top := b.tophash[i] // 注释：hash高8位或则自定义的站位标识，这里不会是预留的数字，后面有判断，如果是预留的则抛出异常
				if isEmpty(top) {   // 注释：(过滤空数据)判断该站位标识表示的数据是否是空
					b.tophash[i] = evacuatedEmpty // 注释：更改状态标识(这里会把emptyOne或emptyRest状态为更改为evacuatedEmpty）
					continue
				}
				if top < minTopHash {
					throw("bad map state")
				}
				k2 := k
				if t.indirectkey() { // 注释：判断是否是间接的key(如果int可以存下key的话，那么key就是其本身，如果int存不下，则是通过指针指向key)
					k2 = *((*unsafe.Pointer)(k2))
				}
				var useY uint8 // 注释：是否使用y
				// 注释：判断是否是等量扩容，（如果不是等量扩容则是2倍扩容）
				if !h.sameSizeGrow() {
					// Compute hash to make our evacuation decision (whether we need
					// to send this key/elem to bucket x or bucket y).
					hash := t.hasher(k2, uintptr(h.hash0)) // 注释：计算hash值；根据key和hash种子
					if h.flags&iterator != 0 && !t.reflexivekey() && !t.key.equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						useY = top & 1      // 注释：通过桶里的top获取第一位数据,如果是1则说明该位置是空的
						top = tophash(hash) // 注释：计算key的hash的高8位
					} else {
						// 注释：通过和旧桶的桶数对应的哪一位来判断是否是把数据放在y里。
						if hash&newbit != 0 {
							useY = 1
						}
					}
				}

				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				b.tophash[i] = evacuatedX + useY // 注释：移动数据位置打上标记 // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // 注释：要移动的数据 // evacuation destination

				// 注释：判断是否超出最后一个桶，如果超出的话，链接溢出桶
				if dst.i == bucketCnt {
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, bucketCnt*uintptr(t.keysize))
				}
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // mask dst.i as an optimization, to avoid a bounds check
				if t.indirectkey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.key, dst.k, k) // copy elem
				}
				if t.indirectelem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.elem, dst.e, e)
				}
				dst.i++
				// These updates might push these pointers past the end of the
				// key or elem arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				dst.k = add(dst.k, uintptr(t.keysize))
				dst.e = add(dst.e, uintptr(t.elemsize))
			}
		}
		// Unlink the overflow buckets & clear key/elem to help GC.
		if h.flags&oldIterator == 0 && t.bucket.ptrdata != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))
			// Preserve b.tophash because the evacuation
			// state is maintained there.
			ptr := add(b, dataOffset)
			n := uintptr(t.bucketsize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}

func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		h.flags &^= sameSizeGrow
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.key.equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.key.size > maxKeySize && (!t.indirectkey() || t.keysize != uint8(sys.PtrSize)) ||
		t.key.size <= maxKeySize && (t.indirectkey() || t.keysize != uint8(t.key.size)) {
		throw("key size wrong")
	}
	if t.elem.size > maxElemSize && (!t.indirectelem() || t.elemsize != uint8(sys.PtrSize)) ||
		t.elem.size <= maxElemSize && (t.indirectelem() || t.elemsize != uint8(t.elem.size)) {
		throw("elem size wrong")
	}
	if t.key.align > bucketCnt {
		throw("key align too big")
	}
	if t.elem.align > bucketCnt {
		throw("elem align too big")
	}
	if t.key.size%uintptr(t.key.align) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.elem.size%uintptr(t.elem.align) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.key.align) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.elem.align) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapassign reflect.mapassign
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap) *hiter {
	it := new(hiter)
	mapiterinit(t, h, it)
	return it
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

const maxZero = 1024 // must match value in reflect/value.go:maxZero cmd/compile/internal/gc/walk.go:zeroValSize
var zeroVal [maxZero]byte
