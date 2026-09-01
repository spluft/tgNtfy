// Package topic hosts the single canonical place a Telegram forum topic name is derived from
// an input string (V-7 / D-SANITIZE-1). Nothing else in the gate computes a
// topic name.
package topic

import "unicode"

// maxTopicRunes is Telegram's createForumTopic.name length limit (1-128 chars).
const maxTopicRunes = 128

// SanitizeTopicName applies the frozen V-7 rule and returns the ready-to-send topic
// name, or "" if the result is empty (the caller then falls back to the service id):
//
//  1. Collapse every run of Unicode whitespace to a single ASCII space and trim edges.
//  2. Drop control/format runes (Unicode categories Cc/Cf).
//  3. Clamp to 128 code points (rune-safe, not byte-safe).
//  4. No ellipsis is appended.
func SanitizeTopicName(s string) string {
	var b []rune
	pendingSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && len(b) > 0 {
			b = append(b, ' ')
		}
		pendingSpace = false
		b = append(b, r)
	}
	// trim handled implicitly: leading pendingSpace skipped (len(b)==0), trailing never flushed.

	out := make([]rune, 0, len(b))
	for _, r := range b {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		out = append(out, r)
	}
	if len(out) > maxTopicRunes {
		out = out[:maxTopicRunes]
	}
	return string(out)
}
