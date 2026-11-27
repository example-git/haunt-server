ALTER TABLE chatxo.ai_channel_settings ADD COLUMN timezone text NOT NULL DEFAULT '' CHECK (octet_length(timezone) <= 128);

CREATE TABLE chatxo.ai_media (
    attachment_id uuid PRIMARY KEY REFERENCES chatxo.attachments(id) ON DELETE CASCADE,
    message_id uuid NOT NULL REFERENCES chatxo.messages(id) ON DELETE CASCADE,
    provenance jsonb,
    spec text NOT NULL DEFAULT '' CHECK(octet_length(spec) <= 4096),
    editor_id uuid REFERENCES chatxo.users(id) ON DELETE SET NULL,
    edited_at timestamptz
);
CREATE INDEX ai_media_message ON chatxo.ai_media(message_id);
CREATE INDEX ai_media_parent ON chatxo.ai_media((provenance->>'parent_id')) WHERE provenance IS NOT NULL;
ALTER TABLE chatxo.ai_media ADD COLUMN bytes bigint NOT NULL DEFAULT 0;
CREATE TRIGGER ai_media_measure BEFORE INSERT OR UPDATE ON chatxo.ai_media FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_media_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_media FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();
