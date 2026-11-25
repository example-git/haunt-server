ALTER TABLE chatxo.ai_channel_settings
    ADD COLUMN allow_consultations boolean NOT NULL DEFAULT false,
    ADD COLUMN max_consultations integer NOT NULL DEFAULT 5 CHECK (max_consultations BETWEEN 1 AND 100);
ALTER TABLE chatxo.messages ADD COLUMN ai_streaming boolean NOT NULL DEFAULT false;
CREATE INDEX streaming_ai_messages ON chatxo.messages(conversation_id, (COALESCE(ai_response_request_id, request_id))) WHERE ai_streaming;

CREATE TABLE chatxo.message_consultations (
    message_id uuid NOT NULL REFERENCES chatxo.messages(id) ON DELETE CASCADE,
    call_id text NOT NULL CHECK (length(call_id) BETWEEN 1 AND 256),
    position integer NOT NULL,
    preset_id uuid REFERENCES chatxo.ai_presets(id) ON DELETE SET NULL,
    question text NOT NULL CHECK (length(question) BETWEEN 1 AND 1000),
    answer text NOT NULL DEFAULT '' CHECK (length(answer) <= 4000),
    status text NOT NULL CHECK (status IN ('waiting','answered','unavailable')),
    PRIMARY KEY(message_id,call_id),
    UNIQUE(message_id,position)
);
CREATE INDEX consultation_personality ON chatxo.message_consultations(preset_id) WHERE preset_id IS NOT NULL;
