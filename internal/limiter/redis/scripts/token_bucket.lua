-- Token bucket, decided atomically inside Redis.
--
-- KEYS[1] = rl:tb:<route>:<client>
-- ARGV    = rate_per_sec, capacity, cost, [now_ms]
-- Returns   {allowed, remaining, retry_after_ms, reset_after_ms}, all integers
--
-- now_ms is passed only by tests. Production calls leave it out and the
-- script reads the time from Redis, so gateway clocks cannot disagree.
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])

local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end

local state  = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts     = tonumber(state[2])
if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = math.max(0, now - ts)
tokens = math.min(capacity, tokens + elapsed * rate / 1000)

local allowed = 0
local retry_ms = 0
if tokens >= cost then
  allowed = 1
  tokens = tokens - cost
else
  retry_ms = math.ceil((cost - tokens) * 1000 / rate)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(capacity * 1000 / rate) + 1000)

local reset_ms = math.ceil((capacity - tokens) * 1000 / rate)
return {allowed, math.floor(tokens), retry_ms, reset_ms}
