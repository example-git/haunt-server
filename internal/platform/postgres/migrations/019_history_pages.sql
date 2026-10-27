ALTER TABLE chatxo.messages ADD COLUMN change_seq bigint NOT NULL DEFAULT 0;
WITH revisions AS MATERIALIZED (
    SELECT m.id,s.event_seq+row_number() OVER (ORDER BY m.id) AS change_seq
    FROM chatxo.messages m CROSS JOIN chatxo.storage s
    WHERE m.edited_at IS NOT NULL OR m.deleted
), updated AS (
    UPDATE chatxo.messages m SET change_seq=r.change_seq FROM revisions r WHERE m.id=r.id RETURNING m.change_seq
)
UPDATE chatxo.storage SET event_seq=COALESCE((SELECT max(change_seq) FROM updated),event_seq) WHERE singleton;
CREATE INDEX messages_changes ON chatxo.messages(conversation_id,change_seq) WHERE change_seq>0;
CREATE INDEX messages_retention ON chatxo.messages(conversation_id,created_at,seq);
