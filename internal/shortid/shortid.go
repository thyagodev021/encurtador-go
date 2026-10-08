// Package shortid gera identificadores curtos aleatórios em base62.
package shortid

import (
	"crypto/rand"
	"fmt"
)

// Alphabet são os 62 caracteres usados nos IDs.
const Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// maxUnbiased é o maior múltiplo de 62 que cabe em um byte (62*4 = 248).
// Bytes >= 248 são descartados: usar "byte % 62" direto favoreceria os
// primeiros caracteres do alfabeto (viés de módulo).
const maxUnbiased = 248

// New devolve um ID aleatório de n caracteres, usando crypto/rand.
func New(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("shortid: tamanho inválido %d", n)
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/2+8)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("shortid: %w", err)
		}
		for _, b := range buf {
			if b >= maxUnbiased {
				continue
			}
			out = append(out, Alphabet[b%62])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
