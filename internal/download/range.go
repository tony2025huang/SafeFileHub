// Package download parses the deliberately limited single-byte-range policy.
package download

import (
	"errors"
	"strconv"
	"strings"
)

var ErrInvalidRange = errors.New("invalid byte range")

type Range struct{ Start, Length int64 }

// ParseRange accepts precisely one RFC 9110 bytes range. Multi-ranges are
// refused rather than constructing multipart responses.
func ParseRange(header string, size int64) (Range, error) {
	if size < 0 || !strings.HasPrefix(header, "bytes=") {
		return Range{}, ErrInvalidRange
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if spec == "" || strings.Contains(spec, ",") {
		return Range{}, ErrInvalidRange
	}
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return Range{}, ErrInvalidRange
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 || size == 0 {
			return Range{}, ErrInvalidRange
		}
		if n > size {
			n = size
		}
		return Range{Start: size - n, Length: n}, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return Range{}, ErrInvalidRange
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return Range{}, ErrInvalidRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return Range{Start: start, Length: end - start + 1}, nil
}
