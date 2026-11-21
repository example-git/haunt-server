ALTER TABLE chatxo.ai_channel_settings
    ADD COLUMN allow_agent_switch boolean NOT NULL DEFAULT false,
    ADD COLUMN last_human_selection_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE chatxo.ai_presets
    ADD COLUMN description text NOT NULL DEFAULT '' CHECK (octet_length(description) <= 4096),
    ADD COLUMN create_hash bytea;
ALTER TABLE chatxo.ai_turns
    ADD COLUMN action text NOT NULL DEFAULT '',
    ADD COLUMN detail text NOT NULL DEFAULT '',
    ADD COLUMN last_progress_at timestamptz;

CREATE TEMP TABLE ai_previous_selections ON COMMIT DROP AS
SELECT m.conversation_id, m.seq
FROM chatxo.messages m JOIN chatxo.ai_channel_settings s ON s.channel_id=m.conversation_id
WHERE m.author_id IS NOT NULL AND m.body ~ '<@ai:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}>';
CREATE INDEX ON ai_previous_selections(conversation_id,seq);
UPDATE chatxo.ai_channel_settings s SET last_human_selection_seq=sel.seq
FROM (SELECT conversation_id,max(seq) AS seq FROM ai_previous_selections GROUP BY conversation_id) sel
WHERE s.channel_id=sel.conversation_id;
UPDATE chatxo.ai_turns t SET snapshot=snapshot || jsonb_build_object('selection_seq',
    COALESCE((SELECT max(seq) FROM ai_previous_selections s
              WHERE s.conversation_id=t.channel_id AND s.seq<=t.boundary_seq),0))
WHERE t.status IN ('queued','waiting','running','requires_auth');
DELETE FROM chatxo.ai_turn_steps;

ALTER TABLE chatxo.attachments ALTER COLUMN user_id DROP NOT NULL,
    ADD COLUMN ai_generation_id uuid,
    ADD CONSTRAINT attachment_owner CHECK ((user_id IS NULL) = (ai_generation_id IS NOT NULL));
CREATE UNIQUE INDEX ai_generated_attachment ON chatxo.attachments(ai_generation_id) WHERE ai_generation_id IS NOT NULL;
