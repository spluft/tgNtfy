// Delivery routing: resolving an event to per-user destinations.
package store

import (
	"context"
	"database/sql"
)

// --- Delivery routing ---

// RouteTarget is a resolved delivery destination for an event.
type RouteTarget struct {
	UserID   int64
	ChatID   int64
	ThreadID int
	Mode     string // 'group' or 'dm'
}

// DynamicBackend is anything the store calls back into to resolve topics / lazily create
// them. Kept as a func so store does not import tgbot.
type TopicResolver func(ctx context.Context, userID, chatID int64, service string) (int, error)

// ResolveRoutes returns the delivery targets for a routed event: for the (service,user_ref)
// find active service_users, filter by subscription, resolve the topic id in group mode.
func (s *Store) ResolveRoutes(ctx context.Context, e *Event, resolveTopic TopicResolver) ([]RouteTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT su.user_id, u.delivery_mode, u.group_chat_id
		FROM service_users su
		JOIN users u ON u.tg_user_id = su.user_id
		LEFT JOIN subscriptions sub ON sub.user_id = su.user_id AND sub.service = su.service
		WHERE su.service = ? AND su.user_ref = ? AND su.status = 'active'
			AND (sub.muted = 0 OR sub.muted IS NULL)
			AND (sub.event_types IS NULL OR sub.event_types = '' OR
				EXISTS (SELECT 1 FROM json_each(sub.event_types) WHERE json_each.value = ?))`,
		e.Service, e.UserRef, e.Type)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RouteTarget
	for rows.Next() {
		var uid int64
		var mode string
		var gci sql.NullInt64
		if err := rows.Scan(&uid, &mode, &gci); err != nil {
			return nil, err
		}
		rt := RouteTarget{UserID: uid, Mode: mode}
		switch {
		case mode == "group" && gci.Valid:
			tid, _, err := s.EnsureTopic(ctx, uid, gci.Int64, e.Service,
				func(chatID int64, svc string) (int, error) { return resolveTopic(ctx, uid, chatID, svc) })
			if err != nil {
				// Lazy-create failed; enqueue to group chat with thread 0 so the retry
				// re-creates the topic via the same idempotent path (E-11).
				rt.ChatID = gci.Int64
				rt.ThreadID = 0
				out = append(out, rt)
				continue
			}
			rt.ChatID = gci.Int64
			rt.ThreadID = tid
		default:
			rt.ChatID = int64(uid) // DM = chat with the user
			rt.ThreadID = 0
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}
