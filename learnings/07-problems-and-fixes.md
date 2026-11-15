# 07. Problems met and how each was solved

Every problem from the work so far, in the order it came up. Each has the same four parts: what it looked like, the cause, the fix, and the lesson to keep.

New problems get added at the bottom.

## 1. `go` is "not recognized"

**What it looked like**

```text
go: The term 'go' is not recognized as a name of a cmdlet, function, script file, or executable program.
```

**Cause.** Go was installed, at `C:\Program Files\Go\bin\go.exe`. But when you type a command name, the shell only searches the folders listed in the `PATH` environment variable, and Go's folder was not in the `PATH` that this terminal had loaded.

**Fix.** Check the default install location, find `go.exe` there, and add that folder to `PATH` for each command:

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
```

To fix it for good on your machine: restart VS Code (a terminal reads `PATH` once, when it starts), or add the folder under "Edit environment variables for your account" in Windows.

**Lesson.** "Command not found" does not mean "not installed". It means "not in a folder on `PATH`". Look for the program before reinstalling it.

## 2. The race detector would not build

**What it looked like**

```text
# runtime/cgo
cc1.exe: sorry, unimplemented: 64-bit mode not compiled in
```

**Cause.** `go test -race` is not pure Go. The race detector includes a part written in C, so Go needs a C compiler to build it. The only one on this machine is `C:\MinGW\bin\gcc.exe`, an old MinGW that can only produce 32-bit programs. Go here is 64-bit. The compiler was saying, quite literally, "I cannot make 64-bit code".

**What I tried, in order**

1. Looked for another C compiler (MSYS2, TDM-GCC, LLVM). None installed.
2. Looked inside WSL. Ubuntu is there but has neither Go nor gcc.
3. Looked at Docker. Installed, but not running.

Installing a new compiler would change your system, so I took the route that changed nothing permanent: start Docker Desktop and run the tests inside a Linux container that already has Go and a 64-bit compiler.

**Fix**

```powershell
docker run --rm -v "d:\agnivesh\Projects\Aegis-Distributed-Limiter:/src:ro" -w /src golang:1.27 sh -c "go vet ./... && go test -race ./..."
```

Reading the command:

| Part | Meaning |
| --- | --- |
| `docker run --rm` | Start a container; delete it when it finishes |
| `-v "...:/src:ro"` | Make the repo folder appear inside the container at `/src`, read-only |
| `-w /src` | Run the command from `/src` |
| `golang:1.27` | The official image with Go already installed |
| `sh -c "..."` | The commands to run inside |

**Lesson.** When a tool cannot work on your machine, a container gives you a clean machine where it can. This is also why CI runs on Linux: the same commands behave the same everywhere. If you would like `-race` to work directly on Windows, install a 64-bit MinGW (for example `choco install mingw` from an administrator terminal).

## 3. Docker was installed but "failed to connect"

**What it looked like**

```text
failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine
```

**Cause.** Docker has two parts: the `docker` command you type (the client) and a background service that does the work (the daemon). The client was there; the daemon was not running because Docker Desktop had not been started.

**Fix.** `docker desktop start`, then `docker info` to confirm.

**Lesson.** `docker --version` working proves only that the client exists. `docker info` tells you whether the daemon is up.

## 4. Git Bash rewrote a path

**What it looked like**

```text
docker: Error response from daemon: the working directory 'C:/Program Files/Git/src' is invalid
```

I had typed `-w /src`. Something turned it into `C:/Program Files/Git/src`.

**Cause.** Git Bash on Windows tries to be helpful: when it sees an argument that looks like a Unix path (`/src`), it converts it to a Windows path before passing it on. That is right for files on your disk and wrong here, because `/src` was a path *inside the container*.

**Fix.** Run that command from PowerShell, which leaves arguments alone. (Inside Git Bash, writing `//src` or setting `MSYS_NO_PATHCONV=1` also stops the conversion.)

**Lesson.** When a path in an error message is not the one you typed, something between you and the program rewrote it. Look at the shell first.

## 5. A failed command did not stop the commit

This one was my mistake, and it is a good one to learn from.

**What happened.** I ran a chain like this:

```bash
docker run ... | tail -3 && git commit ...
```

The intent: run the race tests, and commit only if they pass. The `docker` command failed (problem 4). The commit happened anyway.

**Cause.** `A && B` runs `B` only if `A` succeeded. But here `A` was a *pipeline*, `docker ... | tail -3`. A pipeline's success is decided by its **last** command. `tail` printed the error text perfectly well, so the pipeline "succeeded" and `&&` carried on.

**Fix.** I ran the race tests again properly straight afterwards, on the committed code. They passed, so nothing wrong was committed. But that was luck in the sense that the check did not guard the commit.

**Lesson.** Never put `| tail`, `| grep` or `| head` between a check and the thing the check is supposed to guard. Either leave the pipe out, or in bash turn on `set -o pipefail`, which makes a pipeline fail if any part of it fails.

## 6. Git warned about line endings

**What it looked like**

```text
warning: in the working copy of 'go.mod', LF will be replaced by CRLF the next time Git touches it
```

**Cause.** A line of text ends with an invisible character. Linux and macOS use one (LF). Windows traditionally uses two (CR then LF). Git on Windows is often set to convert between them. Go's formatter, `gofmt`, expects LF and reports files with CRLF as wrongly formatted, so the same file could pass in CI on Linux and fail on your laptop.

**Fix.** A `.gitattributes` file with one rule:

```text
* text=auto eol=lf
```

"Treat text files as text, and always use LF, on every machine."

**Lesson.** Settle line endings in the repo itself, with `.gitattributes`, and not in each person's Git settings. Then everyone gets the same bytes.

## 7. The module path did not match the repo

**What it looked like.** Nothing failed. The plan said to name the module `github.com/<your-username>/ratelimit-gateway`, but the real repo is `github.com/agnivesh13/Aegis-Distributed-Limiter`.

**Cause.** The plan was written before the repo had a name.

**Why it matters.** A Go module path doubles as a web address. Go tools and people both use it to find the code. With a path that points at a repo that does not exist, `go install` from GitHub fails and anyone clicking through from an import lands on a 404.

**Fix.** Changed the path in `go.mod` and in every import, and updated the plan in the same commit (the plan's rule 2 requires that when a spec item changes).

**Lesson.** Fix naming mistakes early. With three packages it was one find-and-replace. With thirty it would have been a chore.

## 8. Tests that could fail without a bug

**What it looked like.** While designing the tests I worked some cases through by hand and found that natural-looking numbers could fail by a hair. For example, the Lua script computes one wait as `1000 × (1 − 0.99)`, which comes out as `10.000000000000009`, and rounding up turns that into 11 ms where the exact answer is 10 ms.

**Cause.** Binary floating point cannot store most decimal fractions exactly. Full explanation in [06-conformance-suite.md](06-conformance-suite.md).

**Fix.** Three rules in the suite: use fractions binary can store exactly where a test needs millisecond precision (a limit of 8), never accumulate fractional refills before an exact check, and accept durations up to 1 ms above the exact value but never below.

**Lesson.** Work a test's numbers through by hand before trusting it. A test that fails at random, or fails on one backend for no real reason, teaches people to ignore failing tests, which is worse than having no test.

## 9. Tests for code I was not allowed to write

**What it looked like.** The suite needed token bucket cases. To know the cases were correct I needed an implementation to run them against. But the plan reserves the token bucket for you to write by hand.

**Fix.** Wrote a throwaway token bucket, ran the suite against it (all 29 cases passed), injected bugs to confirm the cases catch them, then deleted the throwaway and restored the stub. I kept backup copies of the real files outside the repo during the swap so nothing could be lost, and ran the tests again afterwards to confirm the repo was back as it should be.

**Lesson.** You can verify a test without shipping the code it tests. The general idea, a temporary stand-in that proves the surroundings work, comes up constantly in real projects.

## 10. A commit ended up holding more than its message says

**What happened.** I renamed the plan with `git mv`, which stages the rename immediately. Then I staged three other files and committed them. The already staged rename went into that commit too, so commit `347a032` ("Add .gitattributes, .gitignore and the skills lock file") also contains the rename, and the next commit's message mentions a rename it does not contain.

**Cause.** `git commit` commits *everything staged*, not just what you added most recently.

**Fix.** Left it. The content is correct and rewriting history to fix a message is rarely worth it.

**Lesson.** Run `git status` before every commit and read the "Changes to be committed" list.

## 11. A test that passed for the wrong reason

**What it looked like.** A test meant to prove "when Redis accepts a connection and never replies, `Allow` gives up at its 200 ms deadline" passed, and logged:

```text
gave up after 543.6µs with: ... An established connection was aborted by the software in your host machine.
```

**Cause.** Two clues in that one line. A 200 ms deadline cannot expire in half a millisecond. And the error is "connection aborted", not a timeout. To put the test proxy into its silent mode, the test cut all open connections. The backend's next call picked up one of those dead connections from its pool and failed instantly. The call never reached a silent connection, so the deadline was never involved. The test only checked "an error came back, and quickly", and a dead connection satisfies that.

**Fix.** The test now keeps calling until one call actually waits (at least half the timeout), checks that no call waits much longer than the timeout, and fails outright if no call ever waited. It now logs `gave up after 200.2ms with: i/o timeout`.

**Lesson.** A green test is a claim, and the claim can be false. Read what a passing test printed, and ask "would this still pass if the feature were broken?" Here the answer was yes.

## 12. Two packages with the same name

**What it looked like.** Our package is `internal/limiter/redis`. The library is also called `redis`. Inside our package, the name `redis` would be ambiguous.

**Fix.** Rename the import where it is used:

```go
import goredis "github.com/redis/go-redis/v9"
```

After that the library is `goredis.NewClient`, `goredis.NewScript`, and so on.

**Lesson.** An import can be given any local name. It is the standard way out of a name clash.

## 13. The editor flagged a file that compiles fine

**What it looked like**

```text
No packages found for open file ...\integration_test.go.
This file may be excluded due to its build tags
```

**Cause.** The file starts with `//go:build integration`, so it is compiled only when that tag is given. The editor's Go plugin does not pass the tag, sees a file that belongs to no build, and warns.

**Fix.** `.vscode/settings.json` tells the plugin to use the tag:

```json
{ "gopls": { "build.buildFlags": ["-tags=integration"] } }
```

**Lesson.** The editor and the `go` command are two separate tools with separate settings. A warning in one does not always mean a problem in the other; `go vet -tags integration ./...` was clean throughout.

## 14. A formula with the wrong denominator

**What it looked like.** A test failed:

```text
TestRejectEverything: false rejects = 0%, over = 0%; want 100, 0
```

Every request had been rejected, and the analyzer said the false-reject rate was 0%.

**Cause.** One helper computed "how far `a` exceeds `b`, as a percentage of `b`". Over-admission called it as `(admitted, ideal)`, which is right. For false rejects I swapped the arguments to `(ideal, admitted)`. That gave the right difference but divided by the admitted count, where the plan's formula divides by the ideal count. With nothing admitted, the helper avoided dividing by zero and returned 0.

**Fix.** The helper now takes the difference and the ideal count as separate arguments, so the denominator is always the ideal count and cannot be swapped.

**Lesson.** A wrong formula in measuring code does not crash. It prints believable numbers. Test the extreme inputs, all admitted and all rejected, where a wrong formula gives an answer that is obviously absurd. Full story in [10-measuring.md](10-measuring.md).

## 15. A test that depended on which goroutine ran first

**What it looked like.** While writing the load-schedule tests I wrote one that cancelled the run from inside the tenth request's goroutine, then checked that fewer than 100 requests were sent. I caught the flaw before running it.

**Cause.** With the fake timer nothing really waits, so the main loop can start all 100 requests before the tenth goroutine gets a turn to cancel. Whether the test passed would depend on how the scheduler happened to order things: it would pass most days and fail on a busy machine.

**Fix.** Cancel *before* calling `Run` and check that nothing is sent. No timing involved; the same result every time.

**Lesson.** If a test's outcome depends on which of two goroutines runs first, it is a flaky test, even if it passes a hundred times in a row. Arrange the test so only one order is possible.

## 16. A shell command broken by an apostrophe

**What it looked like**

```text
bash: unexpected EOF while looking for matching `''
```

**Cause.** I passed a long script to Python inline on the command line. The text contained apostrophes (as in "plan's"), and the shell read one of them as the start of a quoted string that never ended.

**Fix.** Saved the script to a file and ran the file.

**Lesson.** Once a script is longer than a few lines or contains quotes, put it in a file. Getting quotes through a shell correctly is harder than it looks.

## 17. A check that looked like a failure: 105 requests, no 429

**What it looked like.** To see a `429`, I sent 105 requests with `curl` in a loop against a limit of 100 per second. All 105 got `200`.

**Cause.** Starting a new `curl` process 105 times took about 2 seconds. The sliding window counts requests in the last second, so the limit was never reached. The limiter was right; the test was too slow to test it.

**Fix.** Send the burst with the load generator: 300 requests in 300 ms. Exactly 100 got through and the next `curl` got `429` with `Retry-After: 1`.

**Lesson.** Before calling something broken, check that your test actually creates the condition you are testing. A rate limit test must send faster than the rate.

## 18. memberlist panics when closed twice

**What it looked like.** The transport test passed every check and then crashed while cleaning up:

```text
panic: leave after shutdown
```

**Cause.** The test closed one node on purpose, then the clean-up closed every node, so that node was closed twice. memberlist panics if you ask it to leave after it has shut down. The gossip `Node` also closes its transport, so in the gateway a double close could happen too.

**Fix.** `Close` runs its body inside `sync.Once`, the same pattern the memory backend uses, and remembers the first result.

**Lesson.** Make `Close` safe to call more than once. Callers cannot always know whether something else already closed it, and Go code commonly does `defer x.Close()` as well as closing explicitly.

## 19. Every request allowed, and nothing said why

**What it looked like.** Three gateways on Redis, 6,000 requests against a limit of 100 per second: all 6,000 allowed.

**Cause.** The route used the sliding window. The Redis sliding-window script is still a stub, so every `Allow` returned "unsupported", and `fail_open` let each request through, exactly as configured. The only sign was `gateway_limiter_errors_total`, which nobody reads during a curl test.

**Fix.** At start-up the gateway now asks its backend once per route whether it can decide that route's algorithm, and logs an error naming any route it cannot. Details in [11-the-gateway.md](11-the-gateway.md). The exactness check moved to the token-bucket route, whose script exists: 319 admitted, the exact figure.

**Lesson.** "Fail open" is a sensible policy for an outage and a terrible one for a misconfiguration, because both look like success. When a failure would be silent, make it loud somewhere a person will see it.

## 20. A NUL byte typed into the source code

**What it looked like**

```text
cmd\gateway\main.go:152:34: illegal character NUL
```

**Cause.** I wanted the six characters `"\x00"` in Go source: an escape that means "byte zero". The text went through bash, then Python, and each layer treated `\x00` as an escape of its own. The file ended up with an actual zero byte in it, which Go refuses. My first fix went through the same layers and failed the same way.

**Fix.** A small script in a file, which built the four characters from their byte values (`0x5C 0x78 0x30 0x30`), so no layer had a chance to interpret them.

**Lesson.** Problem 16 again, in a sharper form: every layer of quoting is a chance for an escape to be eaten. When exact bytes matter, put the script in a file, and check the result rather than trusting it.

## 21. A summary that did not match its own raw data

**What it looked like.** I wrote the microbenchmark summary table from one run, then saved the raw output of a *second* run under it. The numbers were close but not the same.

**Cause.** Benchmarks vary a little from run to run. Two runs, two sets of numbers, one file.

**Fix.** Rebuilt the table by reading the medians out of the raw lines saved in the same file.

**Lesson.** Plan rule 7: every number must trace to a file. The safest way is to compute the summary *from* the saved file, never alongside it.

## 22. A test expectation off by one interval

**What it looked like.** The burst test failed: "run took 398.6 ms, want at least 400 ms".

**Cause.** 100 requests at 500 per second leave at 0, 2, 4, ..., 198 ms. The last one leaves at 198 ms, not 200. My expectation counted the gaps wrong: 100 requests have 99 gaps between them. This is the classic **fencepost error** (a 10-metre fence with posts every metre has 11 posts).

**Fix.** Expect at least 398 ms, with the reason written in a comment.

**Lesson.** When a test fails by almost exactly one step, suspect the test's arithmetic before the code.

## 23. A histogram bucket boundary that is not quite 15 µs

**What it looked like.** A metrics test expected a 15 µs observation in the second bucket. It landed in the first.

**Cause.** Buckets start at 10 µs and grow by 1.5. In binary floating point, `0.00001 × 1.5` is `1.5000000000000002e-05`, a hair *above* 15 µs. So 15 µs is "at or below" the first upper bound, not above it. The same binary-fraction effect as problem 8.

**Fix.** Use 12 µs, which is clearly inside the second bucket, and check the bucket labels as Prometheus actually prints them.

**Lesson.** Never test with a value that sits exactly on a computed floating-point boundary.

## 24. A warning that blamed the wrong thing

**What it looked like.** During the `fail_closed` outage run, the load generator printed "this machine may not sustain the rate".

**Cause.** One warning covered two situations: late sends (the laptop cannot keep up, so the run is invalid) and errors (which, in a fault run, are the expected 503s).

**Fix.** Two separate messages. Late sends get the warning; non-2xx, non-429 answers get a note saying they are expected during fault runs.

**Lesson.** A warning that is wrong half the time trains people to ignore it. Say exactly what happened and what it means.

## 25. Two services on one host port

**What it looked like.** While reading metrics I noticed Prometheus was set to host port 9091, inside the 9090 to 9094 range the gateway replicas use for their admin ports.

**Cause.** I picked the next free-looking number without checking the range a few lines above.

**Fix.** Prometheus moved to 9099.

**Lesson.** With port ranges, check the whole range, not just the first number. Nothing had failed yet; with the observability profile and three or more gateways it would have.

## 26. A count of zero that meant "did not compile"

**What it looked like.** To test the merge tests, I broke the merge four ways and counted failing tests each time. One mutation reported 0 failing tests, as if the tests had missed the bug.

**Cause.** That edit left a variable unused, which Go refuses to compile. The package never built, no test ran, and so no line said "FAIL: TestSomething". My counter looked only for those lines and found none.

**Fix.** Rewrote the mutation so it compiled. The tests then caught it (two failing cases).

**Lesson.** "No failures" and "nothing ran" look the same if you only count failures. When checking a result, make sure the thing ran at all. This is problem 11 again: a check that passes for the wrong reason.

## 27. Windows refused a port it had just handed out

**What it looked like**

```text
failed to start UDP listener on "127.0.0.1" port 58067: bind: An attempt was made to access a socket in a way forbidden by its access permissions.
```

A test that had passed many times failed once.

**Cause.** The test asks for "any free port". memberlist needs the same port number for TCP and UDP. Windows keeps some UDP port ranges reserved for Hyper-V and Docker, so now and then the number that was free for TCP is forbidden for UDP.

**Fix.** The test tries up to ten times, getting a different port each time.

**Lesson.** A test that fails one time in fifty is still a broken test, and it will fail at the worst moment. Find the cause; do not just run it again. Here the cause was outside our code, so retrying is the honest fix.

## 28. A blank line that looked like missing data

**What it looked like.** A script printed each gateway's totals after a gossip run. One of the three lines came out blank, as if that gateway had no counts.

**Cause.** The gateway was fine. My PowerShell one-liner mishandled a list with a single item. Printing the raw JSON showed all three gateways with identical counts.

**Fix.** Looked at the raw output before drawing a conclusion.

**Lesson.** When a summary looks wrong, look at the raw data before believing the summary or the system. The bug may be in the thing doing the summarising.

## The pattern behind all of these

Looking back over the list:

- **Read the error message slowly.** Problems 1, 2, 3 and 4 each stated their cause in the message. "64-bit mode not compiled in" is a complete diagnosis once you stop and read it.
- **Find out what is true before changing anything.** Each fix started with a look: where is `go.exe`, which compilers exist, is the daemon up.
- **Prefer the fix that changes least.** Running tests in a container changed nothing on your system. Installing a compiler would have.
- **Check the check.** Problems 5, 8 and 9 are all about a safety net that might itself have a hole.
