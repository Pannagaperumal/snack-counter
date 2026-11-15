# Snack Counter Automation

My take-home for the Framsikt Product Engineer assignment. Two offices (Oslo and Bengaluru) each have a
snack box kept stocked by volunteers who pay out of pocket and claim the money back from an external HR
system. Today it runs on goodwill, and it shows: empty boxes, double purchases, the same few people doing
everything, and nobody knowing what's spent.

This repo has:

- **[DESIGN.md](DESIGN.md)**: the design doc. Start here.
- **The core logic in Go**: the refill lifecycle, choosing who buys, and the budget check, with fake chat and HR systems.
- **Tests** for the rules that matter.
- **A small web UI** to click through every scenario and watch the rules work.

## Running it

You need **Go 1.22 or newer**. There are no other dependencies: no database, no npm, nothing to install.

```sh
# run all tests (with the race detector, since "only one person can accept" is a concurrency rule)
go test -race ./...

# interactive web UI, then open http://localhost:8080
go run ./cmd/web

# web UI on a different port
go run ./cmd/web -addr localhost:9000
```

Everything uses a **simulated clock**, so you can jump ahead two hours or five days without waiting.
Nothing is saved. Restarting the web UI (or pressing Reset) starts a fresh simulation.

## Using the web UI

- **Counters**: one card per office with its budget, who's in today, and the current refill. Report a box empty, then accept, decline or submit a receipt "as" any person.
- **People**: move someone between Oslo, Bengaluru and "away" to simulate hybrid work, holidays and visits.
- **HR claims**: play the HR system: approve or reject claims. Use the **HR down** switch at the top to simulate an outage.
- **History** and **Monthly report**: every refill so far, and the report management would get.
- **Chat activity** (right side): every message the bot sends, as people would see it.
- **+15m … +5d** (top bar): move the clock forward, which also runs the bot's timeouts and HR checks.

**How to demo** in the top bar lists a short script for each rule (happy path, nobody answers, double
buying, visitors, budget, HR outage, time zones).

## Project layout

```
.
├── DESIGN.md          design doc
├── README.md          this file
├── go.mod
├── snack.go           all the rules: lifecycle, who gets asked, budget, monthly report
├── fakes.go           simulated chat tool and HR system
├── snack_test.go      tests
└── cmd/
    └── web/           web UI (Go server + a single HTML page, embedded into the binary)
```

## The tests

`go test -race ./...` runs 17 tests, covering about 96% of `snack.go`. The ones the brief asked for directly:

- **Two people can't both commit to the same refill**: `TestConcurrentCommitOnlyOneWins` (50 goroutines accept at once, exactly one wins), plus `TestOnlyOfferedVolunteerCanCommit`.
- **A refill nobody completes is handed to someone else**: `TestOfferTimeoutDeclineAndBroadcast`, `TestCommitTimeoutReassigns`, `TestReleaseHandsOver`.
- **Budgets and totals are correct across both currencies**: `TestBudgetCapAndExhaustion`, `TestReportAcrossCurrenciesAndTimeZones`, `TestConvertRounding`, `TestRejectedClaimFreesBudget`.

Every test name in `snack_test.go` says which rule or edge case it checks.

## What's simulated or left out

- The **chat tool** and **HR system** are fakes, with the same shape the real ones would plug into.
- **"Who's in the office today"** is a simple map. In real life it would come from calendars, chat status or the badge system.
- **Storage** is in memory.
- **Exchange rates** are passed in (1 INR = 0.125 NOK in the web UI). In practice finance would supply them monthly.

