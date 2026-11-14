# 02. The Go used in this repo, from the basics

Every Go feature the code uses so far, each with a real line from this repo. If you have done A Tour of Go, some of this is revision; skim until something is new.

## Modules and packages

A **module** is one Go project. It is the folder that holds `go.mod`:

```
module github.com/agnivesh13/Aegis-Distributed-Limiter

go 1.27.4
```

The first line is the module's name. Every import of this project's own code starts with it.

A **package** is one folder of `.go` files. Every file in the folder starts with the same `package` line. Other code uses a package by importing its folder path:

```go
import "github.com/agnivesh13/Aegis-Distributed-Limiter/internal/limiter"
```

After that import, code says `limiter.Limit`, `limiter.Decision`, and so on.

**The `internal` folder is special.** Go refuses to let any other module import a package under `internal/`. It is how a project says "this code is private to me".

**Capital letters mean public.** A name that starts with a capital letter (`Backend`, `Allow`) can be used from other packages. A name that starts with a small letter (`shard`, `sweep`) is private to its own package. There is no `public` or `private` keyword.

## Structs: grouping data

A struct is a group of named values. This one is the outcome of one rate-limit check:

```go
type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
	ResetAfter time.Duration
}
```

**Zero values.** Go never leaves a variable uninitialised. A `bool` starts `false`, an `int` starts `0`, a `time.Duration` starts `0`. So `limiter.Decision{}` is a complete, valid value meaning "not allowed, nothing remaining". The code relies on this when it returns an error:

```go
return limiter.Decision{}, limiter.ErrUnsupported
```

## Methods: functions attached to a type

```go
func (l Limit) Capacity() int {
	if l.Algorithm == TokenBucket && l.Burst > 0 {
		return l.Burst
	}
	return l.Rate
}
```

The `(l Limit)` before the name is the **receiver**. It makes `Capacity` a method, called as `myLimit.Capacity()`.

**Value receiver or pointer receiver?**

- `(l Limit)` gets a *copy*. Fine when the method only reads.
- `(b *Backend)` gets a *pointer* to the original. Needed when the method changes the value, or when the value must never be copied. `Backend` holds mutexes, and a copied mutex is a separate lock that protects nothing, so every `Backend` method uses a pointer.

## Pointers

`*window` means "the address of a window". `&window{}` creates a window and gives its address.

The memory backend stores `map[string]*window`, a map from key to *pointer*. That matters: when the code changes `st.cur`, it changes the one window the map points to. If the map held plain `window` values, the code would get a copy and its changes would be lost.

A pointer that points at nothing is `nil`. Looking up a key that is not in the map gives `nil`:

```go
st := s.windows[key]   // nil if this key has never been seen
```

## Interfaces: describing behaviour without saying who does it

```go
type Limiter interface {
	Allow(ctx context.Context, key string, limit Limit, cost int) (Decision, error)
	Close() error
}
```

An interface is a list of methods. **Any type that has those methods is a `Limiter` automatically.** Nothing is declared: `memory.Backend` never says "implements Limiter". It just has `Allow` and `Close`, so it fits.

This is the centre of the whole design. The gateway will hold a `limiter.Limiter` and call `Allow`. It will not know or care whether memory, Redis or gossip is behind it. Changing backend becomes one line of configuration.

## Errors are ordinary values

Go has no exceptions. A function that can fail returns an `error` as its last result, and the caller checks it:

```go
d, err := lim.Allow(ctx, key, limit, 1)
if err != nil {
	// handle it
}
```

`nil` means "no error".

**Sentinel errors.** The package creates a few errors once and gives them names:

```go
var (
	ErrUnsupported = errors.New("limiter: algorithm not supported by this backend")
	ErrInvalidCost = errors.New("limiter: cost must be between 1 and the capacity")
)
```

Callers compare against them with `errors.Is`:

```go
if errors.Is(err, limiter.ErrInvalidCost) { ... }
```

Use `errors.Is` and not `==`. Later code may wrap an error with extra detail, and `errors.Is` still finds the original inside the wrapper.

## Time: `time.Time` and `time.Duration`

- `time.Time` is a moment, such as "14 Nov 2026, 10:00:00".
- `time.Duration` is a length of time, such as "250 ms". Inside, it is just a whole number of nanoseconds.

```go
100 * time.Millisecond          // a Duration
now.Add(2 * l.Period)           // a Time, two periods later
now.UnixNano()                  // nanoseconds since 1 Jan 1970, as an integer
now.Before(w.expires)           // true if now is earlier than expires
```

Because a `Duration` is an integer, `int64(l.Period)` turns it into a plain number of nanoseconds for arithmetic.

## `context.Context`

Every `Allow` call takes a `ctx` as its first argument. A context carries a deadline and a "please stop" signal across function calls. The Redis backend will need it: if Redis does not answer in 50 ms, the context cancels the call. The memory backend never waits on anything, so it ignores the context, which Go writes as `_`:

```go
func (b *Backend) Allow(_ context.Context, key string, ...
```

`_` means "there is a value here and I am deliberately not using it".

## Maps

```go
windows map[string]*window
```

A map from string keys to window pointers. It must be created with `make` before use:

```go
b.shards[i].windows = make(map[string]*window)
```

```go
st := s.windows[key]       // read; nil if missing
s.windows[key] = st        // write
delete(s.windows, key)     // remove
for key, w := range s.windows { ... }   // visit every entry
```

**A map is not safe to use from two goroutines at once.** If one writes while another reads, the program can crash. That is the reason for the mutexes in [05-memory-backend.md](05-memory-backend.md).

## Goroutines

`go f()` starts `f` running at the same time as everything else. It costs very little; a program can run thousands.

```go
go b.janitor(o.janitorInterval)
```

That one line starts the background cleaner. `New` returns straight away and the janitor keeps running beside it.

## Mutex: one at a time

When two goroutines use the same data, a `sync.Mutex` lets only one in at a time:

```go
s.mu.Lock()
defer s.mu.Unlock()
```

**`defer`** schedules a call to run when the surrounding function returns, however it returns. Writing `defer s.mu.Unlock()` on the line after `Lock()` makes it impossible to forget the unlock.

## Channels and `select`

A channel is a pipe between goroutines. This repo uses channels only as signals:

```go
stop chan struct{}
```

`struct{}` is a type with no data. A `chan struct{}` carries no information; the only event that matters is that it gets **closed**. Closing a channel wakes up every goroutine waiting on it. That makes it a broadcast "stop now" signal.

`select` waits for whichever of several channels is ready first:

```go
select {
case <-b.stop:
	return                 // told to stop
case <-ticker.C:
	b.sweep(b.clock.Now()) // the timer fired
}
```

## `switch` without a value

A `switch` with nothing after the keyword is a tidy `if / else if / else`:

```go
switch {
case estimate+c <= limit:
	// allow
case float64(cur)+c <= limit:
	// reject; wait for the previous window to decay
default:
	// reject; wait for this window to end
}
```

The first case that is true runs, then the switch ends. There is no fall-through and no `break` to write.

## Variadic parameters

`...` after a type lets a caller pass any number of values, including none:

```go
func Run(t *testing.T, newLimiter Factory, algorithms ...limiter.Algorithm)
```

Both `Run(t, f)` and `Run(t, f, limiter.SlidingWindow)` are valid calls. Inside the function `algorithms` is a slice.

## Functional options

A common Go pattern for optional settings:

```go
type Option func(*options)

func WithJanitorInterval(d time.Duration) Option {
	return func(o *options) { o.janitorInterval = d }
}

b := memory.New(clock)                                          // defaults
b := memory.New(clock, memory.WithJanitorInterval(time.Hour))   // one override
```

Each option is a small function that changes one setting. `New` starts with the defaults and applies whatever options were passed. New settings can be added later without breaking any existing call.

## Tests

- A test lives in a file ending `_test.go`.
- A test is a function named `TestSomething(t *testing.T)`.
- `go test ./...` runs every test in every package. `./...` means "this folder and everything under it".

```go
t.Errorf("Remaining = %d, want %d", got, want)  // mark failed, keep going
t.Fatalf("Allow: %v", err)                      // mark failed, stop this test now
t.Run("name", func(t *testing.T) { ... })       // a named sub-test
t.Helper()                                      // "report failures at my caller's line"
t.Cleanup(func() { ... })                       // run this when the test ends
```

**Two kinds of test file sit in `memory/`:**

| File | Package line | Can see |
| --- | --- | --- |
| `memory_test.go` | `package memory_test` | Only public names, exactly like a real user of the package |
| `internal_test.go` | `package memory` | Private names too, such as `sweep` and `shardFor` |

The first proves the public behaviour. The second reaches inside to test things a user cannot touch, like forcing a cleanup.

## Two newer features you will see

- `for i := range 20` loops 20 times with `i` from 0 to 19 (Go 1.22 and later).
- `wg.Go(func() { ... })` starts a goroutine and tells a `sync.WaitGroup` to wait for it (Go 1.25 and later). `wg.Wait()` then blocks until all of them finish.

## Check yourself

- Why does `memory.Backend` never mention `limiter.Limiter`, yet can be used as one?
- Why do `Backend`'s methods use `*Backend` and not `Backend`?
- What is the difference between `t.Errorf` and `t.Fatalf`?
- What does closing a `chan struct{}` do to goroutines waiting on it?
- Why is `errors.Is(err, ErrInvalidCost)` safer than `err == ErrInvalidCost`?
