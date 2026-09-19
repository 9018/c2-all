package logging

import (
	"strings"
	"testing"
)

func TestScrubSecrets(t *testing.T) {
	cases := []struct{ in, want, notWant string }{
		{
			in:     "CCAddress is: wss://relay.example.com/ws/prod-room-a?role=cc&secret=dec9c2b46d21f6cb6db8fa60d007f5dc2b4f3da7c0a592cf",
			want:   "secret=***",
			notWant: "dec9c2b46d21f6cb6db8fa60d007f5dc2b4f3da7c0a592cf",
		},
		{
			in:     "Authorization: Bearer dec9c2b46d21f6cb6db8fa60d007f5dc2b4f3da7c0a592cf",
			want:   "Bearer ***",
			notWant: "dec9c2b46d21f6cb6db8fa60d007f5dc2b4f3da7c0a592cf",
		},
		{
			in:      "DoH: https://relay.example.com/dns?secret=abc123Xyz",
			want:    "secret=***",
			notWant: "abc123Xyz",
		},
		{
			// non-secret query params stay intact
			in:      "GET /ws/room-a?role=cc HTTP/1.1",
			want:    "role=cc",
			notWant: "role=***",
		},
	}
	for _, c := range cases {
		got := scrubSecrets(c.in)
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("scrubSecrets(%q) = %q, want contains %q", c.in, got, c.want)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("scrubSecrets(%q) = %q, must not contain %q", c.in, got, c.notWant)
		}
	}
}
