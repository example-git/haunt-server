ALTER TABLE chatxo.ai_channel_settings ADD COLUMN switch_during_reply boolean NOT NULL DEFAULT false;
-- Additional messages from the same response can have different speakers.
ALTER TABLE chatxo.messages ADD COLUMN ai_response_request_id uuid;
