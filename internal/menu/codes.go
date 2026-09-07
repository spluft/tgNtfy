// 6-digit code issuance for /link and /setup.
package menu

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// issueLinkCode issues a 6-digit link code for a service (UC-2 step 2).
func (h *Handler) issueLinkCode(ctx context.Context, uid, chatID int64, svc string) {
	disp, _ := h.gate.Store.DisplayName(ctx, svc)
	code := h.newCode()
	if err := h.gate.Store.CreateLinkCode(ctx, code, svc, uid, 10*time.Minute, nil); err != nil {
		_ = h.send(ctx, chatID, "Failed to create a code — try again.")
		return
	}
	_ = h.send(ctx, chatID, fmt.Sprintf("Enter this code in **%s** (Profile → Notifications):\n**%s**\n(expires in 10 min; single use)", disp, code))
}

// newCode generates a 6-digit code via crypto/rand with rejection sampling
// (avoiding modulo bias toward the top of the byte range).
func (h *Handler) newCode() string {
	const digits = "0123456789"
	for {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		var sb strings.Builder
		acceptable := true
		for _, c := range b {
			// Rejection sampling: 0..249 give uniform digit classes; 250..255 rejected,
			// forcing a full redraw so all 10 digits are equally likely.
			if c >= 250 {
				acceptable = false
				break
			}
			sb.WriteByte(digits[int(c)%10])
		}
		if acceptable {
			return sb.String()
		}
	}
}
