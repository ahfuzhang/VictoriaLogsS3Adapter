// Package bytesize parses human-readable byte sizes such as "100mb".
package bytesize

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	KB int64 = 1024
	MB       = 1024 * KB
	GB       = 1024 * MB
)

// Parse parses s into a number of bytes.
//
// Supported units are b, kb, mb and gb (case-insensitive, 1024-based).
// A value without a unit is treated as bytes. Fractional values such as
// "1.5mb" are allowed. The result must be positive.
func Parse(s string) (int64, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "gb"):
		mult, s = GB, strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		mult, s = MB, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		mult, s = KB, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse byte size %q: %w", orig, err)
	}
	n := int64(f * float64(mult))
	if n <= 0 {
		return 0, fmt.Errorf("byte size %q must be positive", orig)
	}
	return n, nil
}

// Flag is a flag.Value holding a byte size.
type Flag struct {
	N   int64
	str string
}

// NewFlag returns a Flag initialized from defaultValue. It panics on an invalid default.
func NewFlag(defaultValue string) *Flag {
	f := &Flag{}
	if err := f.Set(defaultValue); err != nil {
		panic(fmt.Sprintf("BUG: invalid default byte size %q: %s", defaultValue, err))
	}
	return f
}

// String implements flag.Value.
func (f *Flag) String() string {
	if f == nil {
		return ""
	}
	return f.str
}

// Set implements flag.Value.
func (f *Flag) Set(s string) error {
	n, err := Parse(s)
	if err != nil {
		return err
	}
	f.N = n
	f.str = s
	return nil
}
