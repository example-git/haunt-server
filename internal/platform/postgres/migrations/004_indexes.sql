-- FK cleanup and bounded retention/replay must not scan the whole replay log.
CREATE INDEX events_by_message ON chatxo.events(message_id) WHERE message_id IS NOT NULL;
CREATE INDEX attachments_by_message ON chatxo.attachments(message_id) WHERE message_id IS NOT NULL;
CREATE INDEX conversation_members_by_user ON chatxo.conversation_members(user_id,conversation_id);
CREATE INDEX invites_expiry ON chatxo.invites(expires_at);
