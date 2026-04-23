-- Atomically validates and deletes a pre-auth ticket.
-- Returns: 1 = success, 0 = not found, -1 = jti mismatch

local key = KEYS[1]
local expected_jti = ARGV[1]

local stored_jti = redis.call('GET', key)
if stored_jti == false then
    return 0  -- ticket not found
end
if stored_jti ~= expected_jti then
    return -1 -- jti mismatch
end
redis.call('DEL', key)
return 1      -- success, ticket consumed

