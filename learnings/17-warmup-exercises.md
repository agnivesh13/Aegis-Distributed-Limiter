# 17. The warm-up exercises

Files: [warmup/workerpool/](../warmup/workerpool/), [warmup/counter/](../warmup/counter/), [warmup/middleware/](../warmup/middleware/)

Phase 0 of the plan lists four warm-ups to do before the project. Three are small programs, and they are now in `warmup/` as tested examples. The fourth, **A Tour of Go, is reading only you can do**: https://go.dev/tour. Do it; everything else here assumes it.

Each exercise is a small version of something the gateway does for real. A good way to use them: read one, delete its `.go` file (not the test), and write it again until the test passes.

## Exercise 2: a worker pool

**The problem.** You have many jobs and want only a few running at once. Starting a goroutine per job is easy but uncontrolled: ten thousand jobs would open ten thousand connections.

**The shape.** A fixed number of worker goroutines all read from one `jobs` channel and write to one `results` channel:

```go
for range workers {
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job, ok := <-jobs:
				if !ok {
					return
				}
				...
```

A channel hands each value to exactly one receiver, so the workers share the jobs without any lock.

**Three details that are easy to get wrong:**

1. **`job, ok := <-jobs`.** When a channel is closed and empty, receiving gives `ok == false`. That is how a worker knows there is no more work.
2. **Sending the result must also watch for cancel.**

   ```go
   select {
   case results <- result:
   case <-ctx.Done():
   	return
   }
   ```

   If the reader has gone away, a plain `results <- result` blocks for ever and the goroutine never exits. That is a **goroutine leak**. The test `TestCancelStopsThePool` stops reading on purpose and checks the pool still shuts down.
3. **Who closes `results`?** Only after *every* worker has finished, so a separate goroutine waits for them and then closes. Closing earlier makes a worker panic by sending on a closed channel.

**Where the project uses this:** the load generator and the gossip loop both stop through a context, and both wait for their goroutines before returning.

## Exercise 3: one counter, three ways

```go
func (c *Unprotected) Inc() { c.n++ }
```

`n++` looks like one step. It is three: read `n`, add one, write `n`. Two goroutines can both read 41 and both write 42. One increment is lost.

Run it and watch:

```
go test -race -tags racedemo ./warmup/counter/
```

In a Linux container this printed `WARNING: DATA RACE`, and the count came to **34,354 out of 100,000**: almost two thirds of the increments were lost. The two correct versions give exactly 100,000 every time.

| Version | How it is safe | Use it when |
| --- | --- | --- |
| `WithMutex` | Only one goroutine at a time may run the three steps | Several values must change together |
| `WithAtomic` | The CPU does the three steps as one instruction | A single number |

**Why the failing test is behind a build tag.** A test that is *meant* to fail would turn every normal test run and the CI red. The line `//go:build racedemo` at the top of its file keeps it out unless you ask for it with `-tags racedemo`.

**Where the project uses this:** this lost update is the same bug as two requests both reading a count of 99 ([05-memory-backend.md](05-memory-backend.md)) and two gateways both reading it from Redis ([08-redis-backend.md](08-redis-backend.md)). The memory backend uses a mutex because several fields change together; the gossip node's packet counters are atomics because each is a single number.

## Exercise 4: a logging middleware and a table-driven test

```go
func Logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start))
	})
}
```

The gateway's chain ([11-the-gateway.md](11-the-gateway.md)) is seven of these.

**The one tricky part: reading the status code.** A handler *writes* a status to the `ResponseWriter`, but nothing lets you read it back afterwards. So the middleware wraps the writer in a small struct that notes the code as it passes:

```go
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
```

Writing `http.ResponseWriter` with no field name is **embedding**: `statusRecorder` gets all of the writer's methods for free, and replaces just one. The status starts at 200 because a handler that never calls `WriteHeader` sends 200.

**The table-driven test:**

```go
for _, tc := range []struct {
	name       string
	method     string
	path       string
	wantStatus int
	wantLog    string
}{
	{"ok", "GET", "/hello", 200, "status=200"},
	{"handler sets a status", "GET", "/teapot", 418, "status=418"},
	...
} {
	t.Run(tc.name, func(t *testing.T) { ... })
}
```

The cases are data; one loop runs them all. Adding a case is adding a line, and a failure names the case. Nearly every test in this repo is written this way.

## Check yourself

- Why must a worker's send on `results` also watch `ctx.Done()`?
- Who closes the `results` channel, and when?
- Name the three steps hidden in `n++` and show how an increment gets lost.
- When would you choose an atomic over a mutex, and when not?
- Why is the failing counter test behind a build tag?
- How does the logging middleware learn the status code?
- What does embedding `http.ResponseWriter` give `statusRecorder`?
