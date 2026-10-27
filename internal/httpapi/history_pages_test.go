package httpapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/example-git/haunt-server/internal/chat"
)

func TestHistoryPagesAndDurableChanges(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	channel := textChannel(t, f)
	base := "/api/v1/conversations/" + channel.ID + "/messages"
	messages := make([]chat.Message, 7)
	for i := range messages {
		messages[i] = sendMessage(t, f, f.owner.Token, channel.ID, fmt.Sprint(i))
	}
	var page chat.History
	f.call(t, "GET", base+"?latest=true&limit=3", f.owner.Token, nil, 200, &page)
	if !page.HasMore || len(page.Messages) != 3 || page.Messages[0].Seq != 5 || page.Messages[2].Seq != 7 {
		t.Fatalf("latest: %+v", page)
	}
	checkpoint := page.EventCursor
	var changes chat.History
	f.call(t, "GET", base+"?changes_after=0", f.owner.Token, nil, 200, &changes)
	if len(changes.Messages) != 0 || changes.NextChange != checkpoint {
		t.Fatalf("unmodified creations entered revision catch-up: %+v", changes)
	}
	f.call(t, "GET", base+"?before=5&limit=3", f.owner.Token, nil, 200, &page)
	if !page.HasMore || len(page.Messages) != 3 || page.Messages[0].Seq != 2 || page.Messages[2].Seq != 4 {
		t.Fatalf("older: %+v", page)
	}
	f.call(t, "GET", base+"?before=2&limit=3", f.owner.Token, nil, 200, &page)
	if page.HasMore || len(page.Messages) != 1 || page.Messages[0].Seq != 1 {
		t.Fatalf("oldest: %+v", page)
	}
	f.call(t, "PUT", base+"/"+messages[0].ID, f.owner.Token, map[string]any{"body": "old message edited"}, 200, nil)
	f.call(t, "DELETE", base+"/"+messages[1].ID, f.owner.Token, nil, 200, nil)
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM chatxo.events; UPDATE chatxo.storage SET event_floor=event_seq,text_bytes=(SELECT sum(bytes) FROM chatxo.messages)`); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", fmt.Sprintf("%s?changes_after=%d&limit=1", base, checkpoint), f.owner.Token, nil, 200, &page)
	if !page.HasMore || len(page.Messages) != 1 || page.Messages[0].Body != "old message edited" || page.NextChange <= checkpoint {
		t.Fatalf("edit after replay expiry: %+v", page)
	}
	f.call(t, "GET", fmt.Sprintf("%s?changes_after=%d&limit=1", base, page.NextChange), f.owner.Token, nil, 200, &page)
	if page.HasMore || len(page.Messages) != 1 || !page.Messages[0].Deleted || page.Messages[0].ID != messages[1].ID {
		t.Fatalf("delete after replay expiry: %+v", page)
	}
	f.call(t, "GET", fmt.Sprintf("%s?changes_after=%d", base, page.NextChange), f.owner.Token, nil, 200, &page)
	if page.HasMore || len(page.Messages) != 0 {
		t.Fatalf("unchanged: %+v", page)
	}
	for _, query := range []string{"latest=invalid", "latest=true&after=1", "latest=true&before=2", "before=-1", "changes_after=-1", "changes_after=1&before=2", "before=2&after=1"} {
		f.call(t, "GET", base+"?"+query, f.owner.Token, nil, 400, nil)
	}
	var single chat.Message
	f.call(t, "GET", base+"/"+messages[0].ID, f.owner.Token, nil, 200, &single)
	if single.Body != "old message edited" {
		t.Fatalf("reply target: %+v", single)
	}
	f.call(t, "GET", base+"/"+messages[1].ID, f.owner.Token, nil, 404, nil)
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '31 days' WHERE conversation_id=$1 AND seq<=3`, channel.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", base+"?latest=true", f.owner.Token, nil, 200, &page)
	if len(page.Messages) != 4 || page.PrunedThrough != 3 {
		t.Fatalf("retention: %+v", page)
	}
}
