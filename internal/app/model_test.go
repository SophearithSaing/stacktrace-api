package app

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{" Go_Lang ", "go_lang"},
		{"abc", "abc"},
		{strings.Repeat("A", 32), strings.Repeat("a", 32)},
	} {
		got, err := NormalizeHandle(tt.input)
		if err != nil || got != tt.want {
			t.Errorf("NormalizeHandle(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
		}
	}
	for _, input := range []string{"", "  ", "ab", strings.Repeat("a", 33), "@golang", "go lang", "go-lang", "gólang", "Kotlin", "go\x00lang"} {
		if _, err := NormalizeHandle(input); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("expected invalid handle for %q, got %v", input, err)
		}
	}
}
