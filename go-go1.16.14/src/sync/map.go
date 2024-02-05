// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination.
// Loads, stores, and deletes run in amortized constant time.
//
// The Map type is specialized. Most code should use a plain Go map instead,
// with separate locking or coordination, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow,
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint
// sets of keys. In these two cases, use of a Map may significantly reduce lock
// contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use. A Map must not be copied after first use.
// 注释：译：Map类似于Go映射[interface{}]接口{}，但可以安全地由多个goroutine并发使用，而无需额外的锁定或协调。加载、存储和删除以摊销的恒定时间运行。
//		Map类型是专用的。大多数代码应该使用带有单独锁定或协调的普通Go映射，以获得更好的类型安全性，并更容易维护映射内容中的其他不变量。
//		Map类型针对两种常见的用例进行了优化：
//			（1）当给定键的条目只写一次但读多次时，如在只增长的缓存中
//			（2）当多个goroutine读取、写入和覆盖不相交的键集的条目时。
//		在这两种情况下，与单独的Mutex或RWMutex配对的Go映射相比，使用Map可以显著减少锁争用。
//		zero Map为空，可供使用。首次使用后不得复制贴图。
// 注释：sync.Map的结构体，这个结构适合多读少新增修改的情况下使用，可以实现几乎不加锁读取
type Map struct {
	mu Mutex // 注释：互斥锁

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	// 注释：译：read包含对并发访问安全的映射内容部分（无论是否持有mu）。
	//		读取字段本身总是可以安全加载的，但必须仅在保持mu的情况下存储。
	//		存储在read中的条目可以在没有mu的情况下同时更新，但更新之前删除的条目需要将该条目复制到脏映射中，并在保持mu时取消删除。
	read atomic.Value // 注释：存储读取的数据(只读)，存储的结构体题是 readOnly  // readOnly

	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	// 注释：译：dirty包含映射内容中需要保存mu的部分。为了确保脏映射可以快速升级为读取映射，它还包括读取映射中所有未删除的条目。
	//		删除的条目不会存储在脏映射中。必须先取消清除干净映射中已删除的条目并将其添加到脏映射中，然后才能将新值存储到其中。
	//		如果脏映射为nil，则下一次对映射的写入将通过制作干净映射的浅拷贝来初始化它，省略过时的条目。
	dirty map[interface{}]*entry // 注释：新增数据放在这里(读写)，当read里没有读到的时候会到这里读取

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	// 注释：译：miss统计自上次更新读取映射以来需要锁定mu以确定密钥是否存在的加载次数。
	//		一旦发生了足够多的未命中以支付复制脏映射的成本，脏映射将升级为已读映射（处于未修改状态），映射的下一个存储将生成新的脏拷贝。
	misses int // 注释：从read中未读到数据的次数，如果次数大于len(Map.dirty)时，就会把dirty赋值到read里，并清空dirty和misses
}

// readOnly is an immutable struct stored atomically in the Map.read field.
// 注释：译：readOnly是一个原子存储在Map.read字段中的不可变结构。
// 注释：首先会到 Map.read.m 中找，如果没有找到，并且 amended 为true时，则会到 Map.dirty 中继续寻找
type readOnly struct {
	m       map[interface{}]*entry // 注释：只读数据里的map数据，该字段没有找到，并且 amended 是true则会到 Map.dirty 里取寻找
	amended bool                   // 注释：是否允许到 Map.dirty 里查找(是否允许修正数据) // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
// 注释：译：expunged是一个任意指针，用于标记已从脏映射中删除的条目。
var expunged = unsafe.Pointer(new(interface{})) // 注释：这个地址标记已经删除

// An entry is a slot in the map corresponding to a particular key.
// 注释：译：条目是映射中与特定键相对应的槽。
// 注释：(sync.Map 中read字段map的value)存储sync.Map的只读数，是sync.Map.read字段
type entry struct {
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted and m.dirty == nil.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	// 注释：译：p指向为该条目存储的接口｛｝值。
	//		如果p==nil，则表示该条目已被删除，m.dirty==nil。
	//		如果p==已删除，则该条目已被删除，m.dirty！=nil，并且m.dirty中缺少该条目。
	//		否则，该条目是有效的，并记录在m.read.m[key]中，如果m.dirty！=nil，用m.dirty[key]表示。
	//		可以通过用nil进行原子替换来删除条目：当下次创建m.dirty时，它将用expunged原子替换nil，并保留m.dirty[key]未设置。
	//		条目的相关值可以通过原子替换来更新，前提是p！=删除。如果p==expunged，则只有在首次设置m.dirty[key]=e以便使用脏映射查找条目后，才能更新条目的关联值。
	p unsafe.Pointer // 注释：(数据)具体的数据指针，原子操作，最终会断言成接口类型 // *interface{}
}

func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
// 注释：译：Load返回存储在映射中的键的值，如果没有值，则返回nil。ok结果表示是否在映射中找到值。
// 注释：sync.Map读取数据：(两个数据源)
//			1.Map.read(不加锁)
//			2.Map.dirty(加锁)
// 注释：步骤：
//		1.Map.read.m中找(只读映射中找)
//		2.如果没有找并且 Map.read.amended 为true时，原子读Map.dirty操作
//			a.加锁(这里加锁成功后会重新查找Map.read.m，这里操作时原子操作查找，如果依然没有找到则到脏映射里找)
//			b.到Map.dirty中找(脏映射中找)
//			c.累加Map.misses次数（Map.read.m未找到的次数），如果次数大于len(Map.dirty)时，就会把dirty赋值到read里，并清空dirty和misses
//			d.解锁
//		3.如果没有找到返回nil,false，否则原子获取entry.p数据，并转换成接口类型
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly) // 注释：获取 Map.read，并断言成readOnly类型
	e, ok := read.m[key]                // 注释：获取read的map中的数据
	if !ok && read.amended {
		m.mu.Lock() // 注释：如果没有读到，则加锁到dirty里读取
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked() // 注释：累加未读到数据的次数(Map.misses)，如果次数达到len(dirty)时，会把dirty赋值到raed里，并清空Map.dirty和Map.misses
		}
		m.mu.Unlock() // 注释：解锁
	}
	if !ok {
		return nil, false
	}
	return e.load() // 注释：原子获取entry.p数据，并转换成接口类型
}

// 注释：原子获取数据
func (e *entry) load() (value interface{}, ok bool) {
	p := atomic.LoadPointer(&e.p)  // 注释：原子获取数据地址
	if p == nil || p == expunged { // 注释：如果地址不存在或者等于删除（expunged）则直接返回nil，false
		return nil, false
	}
	return *(*interface{})(p), true // 注释：返回接口和布尔值
}

// Store sets the value for a key.
// 注释：译：存储设置键的值
// 注释：sync.Map写入数据(原子操作)
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	// 注释：read里有数据（之前软删除），则修改read和dirty里对应的值
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			m.dirty[key] = e
		}
		e.storeLocked(&value)
	} else if e, ok := m.dirty[key]; ok { // 注释：如果dirty里有数据（之前软删除），则修改dirty里对应的数据
		e.storeLocked(&value)
	} else {
		// 注释：如果没有找到对应的值时，在dirty里新增数据
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true}) // 注释：如果amended是true时，标记m里没有对应key的数据
		}
		m.dirty[key] = newEntry(value) // 注释：在dirty里新增数据
	}
	m.mu.Unlock()
}

// tryStore stores a value if the entry has not been expunged.
//
// If the entry is expunged, tryStore returns false and leaves the entry
// unchanged.
func (e *entry) tryStore(i *interface{}) bool {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == expunged {
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// storeLocked unconditionally stores a value to the entry.
//
// The entry must be known not to be expunged.
func (e *entry) storeLocked(i *interface{}) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
// 注释：如果不存在则插入，如果存在就返回
func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	// Avoid locking if it's a clean hit.
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked()
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i interface{}) (actual interface{}, loaded, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == expunged {
		return nil, false, false
	}
	if p != nil {
		return *(*interface{})(p), true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	ic := i
	for {
		if atomic.CompareAndSwapPointer(&e.p, nil, unsafe.Pointer(&ic)) {
			return i, false, true
		}
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return nil, false, false
		}
		if p != nil {
			return *(*interface{})(p), true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			delete(m.dirty, key)
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if ok {
		return e.delete()
	}
	return nil, false
}

// Delete deletes the value for a key.
func (m *Map) Delete(key interface{}) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value interface{}, ok bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == expunged {
			return nil, false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*interface{})(p), true
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value interface{}) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read, _ := m.read.Load().(readOnly)
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		if read.amended {
			read = readOnly{m: m.dirty}
			m.read.Store(read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// 注释：计数读取不到的次数，如果大于len(m.dirty)时，把dirty拷贝到read里，并清空dirty和misses
func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		return
	}
	m.read.Store(readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil {
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged
}
