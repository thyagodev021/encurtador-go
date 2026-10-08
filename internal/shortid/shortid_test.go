package shortid

import (
	"strings"
	"testing"
)

func TestNewLengthAndAlphabet(t *testing.T) {
	for _, n := range []int{1, 7, 32, 100} {
		id, err := New(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != n {
			t.Fatalf("tamanho = %d, esperado %d", len(id), n)
		}
		for _, c := range id {
			if !strings.ContainsRune(Alphabet, c) {
				t.Fatalf("caractere fora do alfabeto: %q", c)
			}
		}
	}
}

func TestNewInvalidSize(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := New(n); err == nil {
			t.Fatalf("New(%d) deveria falhar", n)
		}
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ {
		id, _ := New(8)
		if seen[id] {
			t.Fatalf("colisão em %d IDs: %s", i, id)
		}
		seen[id] = true
	}
}

// Com viés de módulo, os 8 primeiros caracteres ('0'..'7') apareceriam ~25% mais.
// Aqui exigimos que a frequência de cada caractere fique perto da esperada.
func TestNewHasNoModuloBias(t *testing.T) {
	const total = 620000
	counts := make(map[rune]int)
	for i := 0; i < total/62; i++ {
		id, _ := New(62)
		for _, c := range id {
			counts[c]++
		}
	}
	expected := float64(total) / 62
	for _, c := range Alphabet {
		got := float64(counts[c])
		if got < expected*0.93 || got > expected*1.07 {
			t.Errorf("caractere %q: %.0f ocorrências, esperado ~%.0f", c, got, expected)
		}
	}
}

func BenchmarkNew(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = New(7)
	}
}
