package httpapi

import "sort"

// Presence is owned by this installation's gateway, not by a device or a voice
// room. Only first-connect/last-disconnect transitions wake interested sockets.
// No database queries, extra timers or durable events are used for presence.
func (g *gateway) setOnlineUser(connection *gatewayConnection, user string) {
	g.mu.Lock()
	previous := connection.user
	if previous == user {
		g.mu.Unlock()
		return
	}
	changed := false
	if previous != "" {
		g.online[previous]--
		if g.online[previous] == 0 {
			delete(g.online, previous)
			changed = true
		}
	}
	connection.user = user
	if user != "" {
		changed = changed || g.online[user] == 0
		g.online[user]++
	}
	targets := []*gatewayConnection{}
	if changed {
		for _, target := range g.connections {
			if target.user != "" && target.watchPresence {
				targets = append(targets, target)
			}
		}
	}
	g.mu.Unlock()
	for _, target := range targets {
		target.note(hintPresence, "")
	}
}

func (g *gateway) onlineUsers() []string {
	g.mu.Lock()
	users := make([]string, 0, len(g.online))
	for user := range g.online {
		users = append(users, user)
	}
	g.mu.Unlock()
	sort.Strings(users)
	return users
}
