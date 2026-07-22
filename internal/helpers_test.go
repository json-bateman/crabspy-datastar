package internal

import (
	"strings"
	"testing"
)

func TestGenerateRoomCode_Length(t *testing.T) {
	for _, length := range []int{1, 5, 10} {
		code := GenerateRoomCode(length)
		if len(code) != length {
			t.Errorf("length %d: got len %d", length, len(code))
		}
	}
}

func TestGenerateRoomCode_OnlyCharset(t *testing.T) {
	for range 100 {
		code := GenerateRoomCode(5)
		for _, c := range code {
			if !strings.ContainsRune(charset, c) {
				t.Errorf("char %q not in charset", c)
			}
		}
	}
}

func TestGenerateRoomCode_NoAmbiguousChars(t *testing.T) {
	ambiguous := "0O1I"
	for range 200 {
		code := GenerateRoomCode(5)
		for _, c := range code {
			if strings.ContainsRune(ambiguous, c) {
				t.Errorf("ambiguous char %q found in code %q", c, code)
			}
		}
	}
}

func TestGenerateRoomCode_Randomness(t *testing.T) {
	seen := make(map[string]bool)
	for range 5000 {
		seen[GenerateRoomCode(5)] = true
	}
	if len(seen) < 4990 {
		t.Errorf("too many collisions in 5000 codes: only %d unique", len(seen))
	}
}
