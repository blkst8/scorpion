-- delete_if_owner.lua
-- Atomically deletes a key only if its value matches the expected owner.
-- KEYS[1] = connection key
-- ARGV[1] = expected instance ID (owner)
-- Returns: 1 if deleted, 0 if not owner or not found

if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('DEL', KEYS[1])
end
return 0

