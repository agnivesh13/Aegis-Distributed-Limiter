# Learnings

Notes that explain the code in this repo from the ground up: what was built, why it was built that way, what went wrong on the way and how it was fixed.

[PLAN.md](../PLAN.md) says what to build. [TASKS.md](../TASKS.md) tracks what is done. This folder is for understanding it.

## Reading order

Read them in order the first time. Each file assumes the ones before it.

| File | What you will understand after reading it |
| --- | --- |
| [01-what-was-built.md](01-what-was-built.md) | Everything done so far, task by task, in the order it happened |
| [02-go-basics.md](02-go-basics.md) | The Go features the code uses, each shown with a line from this repo |
| [03-limiter-contract.md](03-limiter-contract.md) | `limiter.go`: the one interface every backend implements, and why time is injected |
| [04-sliding-window.md](04-sliding-window.md) | The sliding window counter algorithm, with worked numbers, then the code line by line |
| [05-memory-backend.md](05-memory-backend.md) | Data races, mutexes, sharding, the janitor goroutine and a clean shutdown |
| [06-conformance-suite.md](06-conformance-suite.md) | How the tests are designed, the fake clock, floating-point traps, and how the tests themselves were tested |
| [07-problems-and-fixes.md](07-problems-and-fixes.md) | Every problem met during the work: what it looked like, the cause, the fix, the lesson |
| [08-redis-backend.md](08-redis-backend.md) | Why GET-then-SET over-admits, how a Lua script fixes it, deadlines and retries, and testing against a real Redis |

## How to study with these

1. Open the note and the source file side by side.
2. Read one section, then find the lines it talks about in the code.
3. Close the note and explain that piece aloud in your own words. If you cannot, read it again. This is the same skill an interview tests.
4. Change something small in the code and run the tests to see what breaks:

   ```
   go test ./...
   ```

Each note ends with a "Check yourself" list. If you can answer those without looking, you know the piece well enough to defend it.

## Adding to this folder

Each new piece of work gets a note here: one file per topic, numbered so the reading order stays clear. The section headings to keep are "What it is", "Why it is built this way" and "Check yourself", and anything that went wrong goes into [07-problems-and-fixes.md](07-problems-and-fixes.md).
