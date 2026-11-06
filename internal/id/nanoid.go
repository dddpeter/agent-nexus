// Package id 生成 nanoid 风格短随机标识（agents/sessions 等 PRIMARY KEY）。
package id

import (
	"crypto/rand"
	"math/bits"
)

const (
	alphabet = "-_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	size     = 21 // nanoid 默认长度
)

// New 返回 21 字符 nanoid（crypto/rand）。
func New() string { return NewSize(size) }

// NewSize 返回指定长度的 nanoid。
func NewSize(n int) string {
	if n <= 0 {
		n = size
	}
	b := make([]byte, n)
	// 按 64 位块生成，取低 bits.Len(mask)-1 位避免取模偏置。
	const mask = 0x3F // 2^6 - 1，alphabet 长度 64
	r := make([]byte, 8)
	for i := 0; i < n; {
		if _, err := rand.Read(r); err != nil {
			panic("id: crypto/rand unavailable: " + err.Error())
		}
		for _, v := range r {
			if i >= n {
				break
			}
			b[i] = alphabet[int(v)&mask]
			i++
		}
	}
	_ = bits.Len8
	return string(b)
}
