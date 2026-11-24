ALTER TABLE chatxo.messages ADD COLUMN ai_visible boolean NOT NULL DEFAULT true;

-- An upload admitted against reclaimable space is staged before older files
-- are removed. Its bytes enter the quota only when it becomes ready.
ALTER TABLE chatxo.attachments ADD COLUMN quota_pending boolean NOT NULL DEFAULT false;
CREATE INDEX attachments_rollover ON chatxo.attachments(is_dm, (status='deleting') DESC, created_at, id)
    WHERE NOT quota_pending AND status IN ('ready','deleting');
CREATE INDEX attachments_quota_pending ON chatxo.attachments(id) WHERE quota_pending;

CREATE OR REPLACE FUNCTION chatxo.account_dm_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' AND OLD.is_dm AND NOT OLD.quota_pending THEN
        UPDATE chatxo.storage SET dm_attachment_bytes=dm_attachment_bytes-OLD.bytes WHERE singleton;
    END IF;
    IF TG_OP <> 'DELETE' AND NEW.is_dm AND NOT NEW.quota_pending THEN
        UPDATE chatxo.storage SET dm_attachment_bytes=dm_attachment_bytes+NEW.bytes WHERE singleton;
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER dm_attachment_account ON chatxo.attachments;
CREATE TRIGGER dm_attachment_account AFTER INSERT OR DELETE OR UPDATE OF bytes,is_dm,quota_pending
    ON chatxo.attachments FOR EACH ROW EXECUTE FUNCTION chatxo.account_dm_attachment();
