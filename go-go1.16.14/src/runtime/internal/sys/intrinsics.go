// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !386

// TODO finish intrinsifying 386, deadcode the assembly, remove build tags, merge w/ intrinsics_common
// TODO replace all uses of CtzXX with TrailingZerosXX; they are the same.

package sys

// Using techniques from http://supertech.csail.mit.edu/papers/debruijn.pdf

const deBruijn64ctz = 0x0218a392cd3d5dbf

var deBruijnIdx64ctz = [64]byte{
	0, 1, 2, 7, 3, 13, 8, 19,
	4, 25, 14, 28, 9, 34, 20, 40,
	5, 17, 26, 38, 15, 46, 29, 48,
	10, 31, 35, 54, 21, 50, 41, 57,
	63, 6, 12, 18, 24, 27, 33, 39,
	16, 37, 45, 47, 30, 53, 49, 56,
	62, 11, 23, 32, 36, 44, 52, 55,
	61, 22, 43, 51, 60, 42, 59, 58,
}

const deBruijn32ctz = 0x04653adf

var deBruijnIdx32ctz = [32]byte{
	0, 1, 2, 6, 3, 11, 7, 16,
	4, 14, 12, 21, 8, 23, 17, 26,
	31, 5, 10, 15, 13, 20, 22, 25,
	30, 9, 19, 24, 29, 18, 28, 27,
}

// Ctz64 counts trailing (low-order) zeroes,
// and if all are zero, then 64.
// 注释：Ctz64计数结尾（低位）零，如果全部为零，则为64。
// 注释：(返回尾部0的个数)返回地位的0的个数
func Ctz64(x uint64) int {
	x &= -x                       // 注释：获取到右边第一个1的值(比如：1010100值为100) // isolate low-order bit
	y := x * deBruijn64ctz >> 58  // 注释：把一个大数字转换成数组的下标(2的n次方减去n,这里是2的6次方减去6等于58) // extract part of deBruijn sequence
	i := int(deBruijnIdx64ctz[y]) // 注释：用数组映射对应的位数 // convert to bit index
	z := int((x - 1) >> 57 & 64)  // 注释：对0单独处理，如果是0则返回64（这个方式省去了用if语句了） // adjustment if zero
	return i + z
}

// Ctz32 counts trailing (low-order) zeroes,
// and if all are zero, then 32.
func Ctz32(x uint32) int {
	x &= -x                       // isolate low-order bit
	y := x * deBruijn32ctz >> 27  // 注释：把一个大数字转换成数组的下标(2的n次方减去n,这里是2的5次方减去5等于27) // extract part of deBruijn sequence
	i := int(deBruijnIdx32ctz[y]) // convert to bit index
	z := int((x - 1) >> 26 & 32)  // adjustment if zero
	return i + z
}

// Ctz8 returns the number of trailing zero bits in x; the result is 8 for x == 0.
func Ctz8(x uint8) int {
	return int(ntz8tab[x])
}

// Bswap64 returns its input with byte order reversed
// 0x0102030405060708 -> 0x0807060504030201
// 注释：字节翻转
func Bswap64(x uint64) uint64 {
	c8 := uint64(0x00ff00ff00ff00ff)  // 注释：8位的奇偶数掩码
	a := x >> 8 & c8                  // 注释：获取偶数并右移8位
	b := (x & c8) << 8                // 注释：取出奇数并且左移8位
	x = a | b                         // 注释：8位的奇数和偶数调换位置
	c16 := uint64(0x0000ffff0000ffff) // 注释：16位的奇偶数掩码
	a = x >> 16 & c16                 // 注释：获取偶数并右移16位
	b = (x & c16) << 16               // 注释：获取奇数并左移16位
	x = a | b                         // 注释：16位的奇数和偶数调换位置
	c32 := uint64(0x00000000ffffffff) // 注释：32位的奇偶数掩码
	a = x >> 32 & c32                 // 注释：获取偶数并右移32位
	b = (x & c32) << 32               // 注释：获取奇数并左移32位
	x = a | b                         // 注释：32位的奇数和偶数调换位置
	return x                          // 注释：返回翻转结果
}

// Bswap32 returns its input with byte order reversed
// 0x01020304 -> 0x04030201
func Bswap32(x uint32) uint32 {
	c8 := uint32(0x00ff00ff)
	a := x >> 8 & c8
	b := (x & c8) << 8
	x = a | b
	c16 := uint32(0x0000ffff)
	a = x >> 16 & c16
	b = (x & c16) << 16
	x = a | b
	return x
}
