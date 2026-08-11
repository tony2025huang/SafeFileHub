package download

import "testing"

func TestRangeSingleAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		header              string
		size, start, length int64
		ok                  bool
	}{
		{"bytes=2-5", 10, 2, 4, true}, {"bytes=7-", 10, 7, 3, true}, {"bytes=-3", 10, 7, 3, true},
		{"bytes=10-", 10, 0, 0, false}, {"bytes=2-1", 10, 0, 0, false}, {"bytes=0-1,3-4", 10, 0, 0, false},
	} {
		got, err := ParseRange(tc.header, tc.size)
		if tc.ok {
			if err != nil || got.Start != tc.start || got.Length != tc.length {
				t.Fatalf("%q = %#v, %v", tc.header, got, err)
			}
		} else if err == nil {
			t.Fatalf("%q accepted: %#v", tc.header, got)
		}
	}
}
