package chat

import (
	"context"
	"github.com/jackc/pgx/v5"
	"regexp"
	"strings"
)

var mentionPattern = regexp.MustCompile(`(^|[\s([{"'.,!?:;])@([a-zA-Z0-9_]{3,32})`)

func MentionNames(body string) []string {
	names := []string{}
	seen := map[string]bool{}
	for _, match := range mentionPattern.FindAllStringSubmatchIndex(body, -1) {
		end := match[1]
		if end < len(body) {
			c := body[end]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
				continue
			}
		}
		name := strings.ToLower(body[match[4]:match[5]])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}
func (s *Store) resolveMentions(ctx context.Context, tx pgx.Tx, body string) ([]string, error) {
	ids := []string{}
	names := MentionNames(body)
	if len(names) == 0 {
		return ids, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM chatxo.users WHERE instance_id=$1 AND lower(username)=ANY($2::text[]) ORDER BY id`, s.InstanceID, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
