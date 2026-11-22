package rtc

import (
	"testing"
)

// Output that is not valid UTF-8 (Latin-1 text, binary) must not delay later
// bytes, otherwise the end marker is never recognised and the command hangs.
func TestInvalidUTF8DoesNotHideEndMarker(t *testing.T) {
	const start = "__MCP_BEGIN_latin__"
	const endPrefix = "__MCP_END_latin_"

	tests := []struct {
		name string
		body string
		want string
	}{
		{"latin1 byte followed by ASCII", "caf\xe9 au lait", "caf� au lait"},
		{"latin1 byte at end of line", "caf\xe9", "caf�"},
		{"truncated 3-byte sequence then ASCII", "a\xe2\x82b", "a��b"},
		{"lone continuation bytes", "x\x80\x80y", "x��y"},
		{"invalid byte before escape sequence", "red\xe9\x1b[0m done", "red� done"},
		{"valid multibyte still decodes", "café € \U0001F680", "café € \U0001F680"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stream := []byte(start + "\r\n" + tc.body + "\r\n" + endPrefix + "0__\r\n")

			// Whole stream at once, and byte by byte.
			for _, step := range []int{len(stream), 1} {
				c := newOutputCollector(start, endPrefix, 1000, false)
				done := false
				for i := 0; i < len(stream) && !done; i += step {
					var err error
					done, err = c.Feed(stream[i:min(i+step, len(stream))])
					if err != nil {
						t.Fatalf("step %d: unexpected error: %v", step, err)
					}
				}
				if !done {
					t.Fatalf("step %d: end marker was not recognised; command would hang until timeout", step)
				}
				if got := c.Result().Stdout; got != tc.want {
					t.Errorf("step %d: stdout = %q, want %q", step, got, tc.want)
				}
			}
		})
	}
}
