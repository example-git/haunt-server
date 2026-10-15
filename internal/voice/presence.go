package voice

import (
	"context"
	"github.com/example-git/haunt-server/internal/identity"
	"sort"
	"time"
)

type PresenceMember struct {
	ID       string    `json:"participant_id"`
	UserID   string    `json:"user_id"`
	JoinedAt time.Time `json:"joined_at"`
	State    State     `json:"state"`
}

// Presence exposes only rooms this principal can view and join. Room locks
// protect short in-memory snapshots; no database calls run under them.
func (m *Manager) Presence(ctx context.Context, p identity.Principal) (map[string][]PresenceMember, error) {
	channels, err := m.ids.Channels(ctx, p, "")
	if err != nil {
		return nil, err
	}
	rooms := map[string]*room{}
	m.mu.Lock()
	for _, ch := range channels {
		if ch.Kind == "voice" && ch.Permissions&(identity.ViewChannel|identity.ConnectVoice) == (identity.ViewChannel|identity.ConnectVoice) && m.rooms[ch.ID] != nil {
			rooms[ch.ID] = m.rooms[ch.ID]
		}
	}
	m.mu.Unlock()
	result := map[string][]PresenceMember{}
	for id, r := range rooms {
		r.mu.Lock()
		members := make([]PresenceMember, 0, len(r.members))
		for _, p := range r.members {
			members = append(members, PresenceMember{ID: p.id, UserID: p.user, JoinedAt: p.joinedAt, State: p.state})
		}
		r.mu.Unlock()
		if len(members) > 0 {
			sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
			result[id] = members
		}
	}
	return result, nil
}
