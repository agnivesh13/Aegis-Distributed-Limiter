-- Sliding window counter, decided atomically inside Redis.
--
-- KEYS[1] = rl:sw:<route>:<client>
-- ARGV    = limit, window_ms, cost, [now_ms]
-- Returns   {allowed, remaining, retry_after_ms, reset_after_ms}, all integers
--
-- It follows "Sliding window counter: exact behaviour" in PLAN.md and uses
-- the same formulas, in the same order, as limiter.SlidingWindowDecision in
-- Go, so both round the same way. learnings/04-sliding-window.md walks
-- through the algorithm.
--
-- now_ms is passed only by tests. Production calls leave it out and the
-- script reads the time from Redis, so gateway clocks cannot disagree.
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local cost   = tonumber(ARGV[3])

local now = tonumber(ARGV[4])
if now == nil then
  local t = redis.call('TIME')
  now = t[1] * 1000 + math.floor(t[2] / 1000)
end

-- 1. Which window is this, and how far into it are we?
local idx     = math.floor(now / window)
local elapsed = now - idx * window

-- 2. Bring the stored counts up to the current window. These are local
-- copies: nothing is written unless the request is allowed.
local state = redis.call('HMGET', KEYS[1], 'idx', 'cur', 'prev')
local sidx  = tonumber(state[1])
local cur   = tonumber(state[2]) or 0
local prev  = tonumber(state[3]) or 0
if sidx == nil or sidx < idx - 1 or sidx > idx then
  -- No state, or state too old (or from the future) to mean anything.
  cur = 0
  prev = 0
elseif sidx == idx - 1 then
  -- A new window has begun: last window's count becomes the previous one.
  prev = cur
  cur = 0
end

-- 3. The previous window counts in proportion to how much of it the last
-- one window-length still covers.
local estimate = cur + prev * (1 - elapsed / window)

local allowed = 0
local retry_ms = 0
if estimate + cost <= limit then
  -- 4. Allowed: charge it and store. The key expires two windows after
  -- the last write, when its counts could no longer matter.
  allowed = 1
  cur = cur + cost
  estimate = estimate + cost
  redis.call('HSET', KEYS[1], 'idx', idx, 'cur', cur, 'prev', prev)
  redis.call('PEXPIRE', KEYS[1], window * 2)
elseif cur + cost <= limit then
  -- 6a. The current window has room; wait for the previous window's
  -- weight to decay far enough. prev cannot be 0 in this branch.
  retry_ms = math.max(1, math.ceil(window * (1 - (limit - cur - cost) / prev) - elapsed))
else
  -- 6b. The current window alone is too full: wait for it to end. This
  -- is a lower bound.
  retry_ms = window - elapsed
end

-- 5 and 7. Redis cuts fractions off returned numbers, so everything is in
-- whole milliseconds.
local remaining = math.max(0, math.floor(limit - estimate))
local reset_ms = 0
if cur > 0 then
  reset_ms = 2 * window - elapsed
elseif prev > 0 then
  reset_ms = window - elapsed
end
return {allowed, remaining, retry_ms, reset_ms}
