-- An explicitly remembered device remains authorized until its session is
-- revoked. Existing sessions retain their original expiry and opt in normally.
ALTER TABLE chatxo.sessions ALTER COLUMN expires_at DROP NOT NULL;
