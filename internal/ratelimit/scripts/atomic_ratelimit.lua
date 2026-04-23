-- atomic_ratelimit.lua
-- Atomically increments the rate-limit counter and sets TTL only on first hit.
-- KEYS[1] = rate-limit key
-- ARGV[1] = window duration in seconds
-- Returns: current count after increment

local count = redis.call('INCR', KEYS[1])
if count == 1 then
    redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
end
return count

