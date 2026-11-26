-- Historical overrides and base role grants are preserved. This migration only
-- expands which bits may be stored in a category or channel override.
ALTER TABLE chatxo.category_permissions
    DROP CONSTRAINT category_permissions_allow_bits_check,
    DROP CONSTRAINT category_permissions_deny_bits_check,
    ADD CONSTRAINT category_permissions_allow_bits_check CHECK (allow_bits >= 0 AND (allow_bits & ~987159::bigint) = 0),
    ADD CONSTRAINT category_permissions_deny_bits_check CHECK (deny_bits >= 0 AND (deny_bits & ~987159::bigint) = 0);
ALTER TABLE chatxo.channel_permissions
    DROP CONSTRAINT channel_permissions_allow_bits_check,
    DROP CONSTRAINT channel_permissions_deny_bits_check,
    ADD CONSTRAINT channel_permissions_allow_bits_check CHECK (allow_bits >= 0 AND (allow_bits & ~987159::bigint) = 0),
    ADD CONSTRAINT channel_permissions_deny_bits_check CHECK (deny_bits >= 0 AND (deny_bits & ~987159::bigint) = 0);
