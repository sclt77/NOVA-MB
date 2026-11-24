package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// randomHex 生成 n 字节的随机十六进制字符串。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机数失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RandomHex 供其他包使用（生成密码、token）。
func RandomHex(n int) (string, error) {
	return randomHex(n)
}

// RandomPassword 生成可读的随机密码，去掉了容易混淆的字符。
func RandomPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机数失败: %w", err)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
