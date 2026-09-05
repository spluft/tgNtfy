package topic

import "testing"

// SAN-1 (V-7): SanitizeTopicName vectors frozen in the binding SPEC §5.
func TestSanitizeTopicName(t *testing.T) {
	longAscii := ""
	for i := 0; i < 200; i++ {
		longAscii += "a"[0:1]
	}
	clampedAscii := longAscii[:128]

	longEmoji := ""
	for i := 0; i < 128; i++ {
		longEmoji += "😀"
	}

	longCJK := ""
	for i := 0; i < 130; i++ {
		longCJK += "你"
	}
	cjk128 := ""
	for i := 0; i < 128; i++ {
		cjk128 += "你"
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trim+collapse", "  goYouTube ", "goYouTube"},
		{"newlines collapse", "a\n\n  b", "a b"},
		{"clamp 200 ascii -> 128", longAscii, clampedAscii},
		{"128 emoji unchanged", longEmoji, longEmoji},
		{"control chars stripped", "\x00\x07x", "x"},
		{"whitespace-only -> empty", "   ", ""},
		{"zero-width+BOM stripped", "\u200B\uFEFFMail", "Mail"},
		{"clamp 130 CJK -> 128", longCJK, cjk128},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeTopicName(c.in)
			if got != c.want {
				t.Fatalf("SanitizeTopicName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// SAN-2: length is measured in code points (runes), not bytes — CJK/emoji clamp at 128
// characters, and the result must stay <= 128 runes and never exceed Telegram's limit.
func TestSanitizeTopicNameRuneSafe(t *testing.T) {
	in := ""
	for i := 0; i < 200; i++ {
		in += "你"
	}
	got := []rune(SanitizeTopicName(in))
	if len(got) != 128 {
		t.Fatalf("clamped CJK result has %d runes, want 128", len(got))
	}
}
