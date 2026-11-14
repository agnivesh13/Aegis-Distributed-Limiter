# 03. The limiter contract

File: [internal/limiter/limiter.go](../internal/limiter/limiter.go)

## What it is

A **contract** is an agreement about how two pieces of code talk to each other. This file is the agreement between the gateway (which asks "may this request pass?") and a limiter backend (which answers).

It holds no logic. It only defines the shapes of the question and the answer.

## The question: `Allow`

```go
Allow(ctx context.Context, key string, limit Limit, cost int) (Decision, error)
```

In words: "Charge `cost` against `key`, under the rule `limit`. Tell me what you decided."

| Argument | Meaning | Example |
| --- | --- | --- |
| `ctx` | Deadline and cancel signal | "give up after 50 ms" |
| `key` | Who is being limited: `route:client` | `search:key-123` |
| `limit` | The rule to apply | 100 requests per second |
| `cost` | How much this request spends | 1 normally; 5 for an expensive search |

## The rule: `Limit`

```go
type Limit struct {
	Algorithm Algorithm
	Rate      int
	Period    time.Duration
	Burst     int
}
```

"Allow `Rate` requests per `Period`." So `Rate: 100, Period: time.Second` means 100 per second.

`Burst` belongs to the token bucket only. It is the bucket's size: how many requests can go through in one instant after a quiet spell. `0` means "same as Rate".

**Why is the limit passed on every call, and not stored in the limiter?** Because each route has its own limit. `/api/` may allow 100 per second and `/api/search` 50. One limiter serves them all, so it has to be told which rule applies each time.

## The answer: `Decision`

```go
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
	ResetAfter time.Duration
}
```

Each field becomes something the client sees in the HTTP response:

| Field | Becomes | Meaning for the client |
| --- | --- | --- |
| `Allowed` | `200` or `429` | Pass or reject |
| `Limit` | `X-RateLimit-Limit` | "Your limit is 100" |
| `Remaining` | `X-RateLimit-Remaining` | "You have 37 left" |
| `RetryAfter` | `Retry-After` | "Rejected. Try again in 2 seconds" |
| `ResetAfter` | `X-RateLimit-Reset` | "In 5 seconds you will have your full limit back" |

A good limiter does more than say no. It tells a well-behaved client how long to wait, so the client can back off instead of hammering.

## Two ways to say no

Look at the return type: `(Decision, error)`. There are two different kinds of "no":

- **`Decision{Allowed: false}` with a nil error:** the limiter worked and decided to reject. The client gets `429`.
- **A non-nil error:** the limiter could not decide at all. For example Redis is down.

Keeping these apart matters. When Redis is down, the gateway must choose a policy: let everyone through (`fail_open`) or block everyone (`fail_closed`). It can only make that choice if "I could not decide" looks different from "I decided no".

## The `Capacity` helper

```go
func (l Limit) Capacity() int {
	if l.Algorithm == TokenBucket && l.Burst > 0 {
		return l.Burst
	}
	return l.Rate
}
```

The capacity is the most that can ever be available at once. A single request costing more than that can never succeed, however long it waits, so it is refused with `ErrInvalidCost` instead of a `RetryAfter` that would be a lie.

This helper is my addition; the plan describes the rule in a sentence but gives no function. All three backends need the same answer, and writing the same `if` in three places is how they drift apart.

## The clock, and why it is injected

```go
type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
```

This looks like needless ceremony. Why not call `time.Now()` directly?

Think about how to test "after 1 second, the limit resets". With `time.Now()` inside the limiter, the test must really wait:

```go
time.Sleep(time.Second)   // the whole test suite now takes minutes
```

It is slow, and worse, it is unreliable. On a busy machine the sleep takes 1.03 seconds and a test that passed yesterday fails today.

**Dependency injection** fixes it. The limiter does not fetch the time itself; it is *given* something that knows the time:

```go
memory.New(clock)
```

- In production, `clock` is `SystemClock{}`, which returns the real time.
- In tests, `clock` is a `FakeClock`, which returns whatever the test says and moves only when the test calls `Advance`.

Now a test can jump forward an hour in zero real time, and it gets the same result on every run. The plan puts it as: "every limiter takes an injected clock, so tests never sleep".

The same trick is used again later for a second purpose. To measure accuracy, the analyzer replays a recorded run through this limiter, setting the fake clock to each request's recorded send time. That replay would be impossible if the limiter read the real clock.

## Why an interface at all

The project's purpose is to compare three backends. A comparison is only fair if everything except the backend stays the same. The interface guarantees that: the gateway, the metrics and the load generator are written once against `Limiter`, and the backend is swapped underneath.

## Check yourself

- What are the two different ways `Allow` can refuse a request, and why must they look different?
- Why is the `Limit` an argument of `Allow` and not a field of the limiter?
- What would be wrong with returning `RetryAfter: 10s` for a request whose cost is larger than the capacity?
- Explain dependency injection using the clock as your example.
