package voice

import (
	"context"
	"sync"

	"github.com/example-git/haunt-server/internal/identity"
)

type restrictionGuard struct {
	mu    sync.Mutex
	users int
}

func (m *Manager) lockRestrictions(channel string) func() {
	m.mu.Lock()
	if m.restrictionLocks == nil {
		m.restrictionLocks = map[string]*restrictionGuard{}
	}
	guard := m.restrictionLocks[channel]
	if guard == nil {
		guard = &restrictionGuard{}
		m.restrictionLocks[channel] = guard
	}
	guard.users++
	m.mu.Unlock()
	guard.mu.Lock()
	return func() {
		guard.mu.Unlock()
		m.mu.Lock()
		guard.users--
		if guard.users == 0 {
			delete(m.restrictionLocks, channel)
		}
		m.mu.Unlock()
	}
}

func effectiveState(self State, restriction identity.VoiceRestriction) State {
	self.Deafened = self.Deafened || restriction.Deafened
	self.Muted = self.Muted || restriction.Muted || self.Deafened
	return self
}

func (m *Manager) Moderate(ctx context.Context, actor identity.Principal, channel, user string, muted, deafened *bool) (identity.VoiceRestriction, error) {
	if !identity.ValidID(channel) {
		return identity.VoiceRestriction{}, identity.ErrInvalid
	}
	unlock := m.lockRestrictions(channel)
	defer unlock()
	v, err := m.ids.SetVoiceRestriction(ctx, actor, channel, user, muted, deafened)
	if err != nil {
		return v, err
	}
	m.mu.Lock()
	r := m.rooms[channel]
	m.mu.Unlock()
	if r == nil {
		return v, nil
	}
	updates := []Snapshot{}
	r.mu.Lock()
	for _, p := range r.members {
		if p.user != user {
			continue
		}
		previous := p.state
		p.restriction = v
		p.state = effectiveState(p.self, v)
		resetRelayVideo(p, previous)
		updates = append(updates, snapshot(p))
	}
	r.mu.Unlock()
	for _, update := range updates {
		m.broadcast(r, map[string]any{"type": "media.participant.state", "participant": update})
	}
	return v, nil
}
