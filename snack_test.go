package snack

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)

type env struct {
	s    *Service
	chat *FakeChat
	hr   *FakeHR
	mu   sync.Mutex
	at   map[string]string // employee -> office they are at ("" = away)
}

func (e *env) set(emp, office string) { e.mu.Lock(); e.at[emp] = office; e.mu.Unlock() }

func setup(t *testing.T) *env {
	t.Helper()
	oslo, _ := time.LoadLocation("Europe/Oslo")
	blr, _ := time.LoadLocation("Asia/Kolkata")
	e := &env{chat: NewFakeChat(), hr: NewFakeHR(), at: map[string]string{
		"o1": "Oslo", "o2": "Oslo", "o3": "Oslo", "b1": "Bengaluru", "b2": "Bengaluru",
	}}
	locate := func(emp string, _ time.Time) string { e.mu.Lock(); defer e.mu.Unlock(); return e.at[emp] }
	s, err := New([]Office{
		{Name: "Oslo", Currency: "NOK", MonthlyBudget: 600_000, MaxPurchase: 120_000, Loc: oslo},
		{Name: "Bengaluru", Currency: "INR", MonthlyBudget: 1_500_000, MaxPurchase: 300_000, Loc: blr},
	}, []string{"o1", "o2", "o3", "b1", "b2"}, locate, e.chat, e.hr, Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.s = s
	return e
}

var cur = map[string]string{"Oslo": "NOK", "Bengaluru": "INR"}

// doRefill runs empty -> commit -> receipt with whoever the system offers it to.
func (e *env) doRefill(t *testing.T, office string, amount int64, now time.Time, items ...string) Refill {
	t.Helper()
	r, err := e.s.ReportEmpty(office, "reporter", now)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if r.OfferedTo == "" {
		t.Fatalf("expected an offer, got broadcast")
	}
	if _, err := e.s.Commit(r.ID, r.OfferedTo, now); err != nil {
		t.Fatalf("commit: %v", err)
	}
	r, err = e.s.SubmitReceipt(r.ID, r.OfferedTo, amount, cur[office], []byte("img"), items, now)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return r
}

// must fails the test (via panic) on error; Go can't pass t alongside a multi-value call.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}

func TestConcurrentCommitOnlyOneWins(t *testing.T) {
	e := setup(t)
	for _, v := range []string{"o1", "o2", "o3"} {
		e.set(v, "") // nobody available -> broadcast, anyone at the office may claim
	}
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	if !r.Broadcast {
		t.Fatal("expected broadcast")
	}
	const n = 50
	for i := range n {
		e.set(fmt.Sprintf("e%d", i), "Oslo")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.s.Commit(r.ID, fmt.Sprintf("e%d", i), base)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if !errors.Is(err, ErrAlreadyTaken) {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins = %d, want 1", wins)
	}
}

func TestDuplicateEmptyReportIsIdempotent(t *testing.T) {
	e := setup(t)
	r1 := must(e.s.ReportEmpty("Oslo", "a", base))
	r2 := must(e.s.ReportEmpty("Oslo", "b", base.Add(time.Minute)))
	if r1.ID != r2.ID {
		t.Fatalf("second report opened a new refill %s", r2.ID)
	}
	total := 0
	for _, v := range []string{"o1", "o2", "o3"} {
		total += len(e.chat.DirectTo(v))
	}
	if total != 1 {
		t.Fatalf("offers sent = %d, want 1", total)
	}
	if n := len(e.s.List()); n != 1 {
		t.Fatalf("refills = %d, want 1", n)
	}
	// after commit a report still returns the same refill: nobody else buys
	must(e.s.Commit(r1.ID, r1.OfferedTo, base))
	r3 := must(e.s.ReportEmpty("Oslo", "c", base))
	if r3.ID != r1.ID || r3.State != Committed {
		t.Fatalf("got %s/%s", r3.ID, r3.State)
	}
}

func TestOnlyOfferedVolunteerCanCommit(t *testing.T) {
	e := setup(t)
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	if r.OfferedTo != "o1" {
		t.Fatalf("offered to %s, want o1", r.OfferedTo)
	}
	_, err := e.s.Commit(r.ID, "o2", base)
	wantErr(t, err, ErrNotOffered)
	_, err = e.s.Commit(r.ID, "", base)
	wantErr(t, err, ErrInvalidEmployee)
	must(e.s.Commit(r.ID, "o1", base))
	_, err = e.s.Commit(r.ID, "o2", base)
	wantErr(t, err, ErrAlreadyTaken)
	_, err = e.s.Commit(r.ID, "o1", base)
	wantErr(t, err, ErrAlreadyTaken)
	_, err = e.s.Commit("nope", "o1", base)
	wantErr(t, err, ErrNotFound)
}

func TestOfferTimeoutDeclineAndBroadcast(t *testing.T) {
	e := setup(t)
	e.set("visitor", "Oslo") // not a volunteer
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	e.s.Tick(base.Add(time.Hour))
	if r = must(e.s.Get(r.ID)); r.OfferedTo != "o1" {
		t.Fatalf("offer moved too early to %s", r.OfferedTo)
	}
	e.s.Tick(base.Add(2 * time.Hour))
	if r = must(e.s.Get(r.ID)); r.OfferedTo != "o2" {
		t.Fatalf("after timeout offered to %s, want o2", r.OfferedTo)
	}
	if err := e.s.Decline(r.ID, "o1", base); !errors.Is(err, ErrNotOffered) {
		t.Fatalf("expired offeree could decline: %v", err)
	}
	if err := e.s.Decline(r.ID, "o2", base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r = must(e.s.Get(r.ID)); r.OfferedTo != "o3" {
		t.Fatalf("after decline offered to %s, want o3", r.OfferedTo)
	}
	if err := e.s.Decline(r.ID, "o3", base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r = must(e.s.Get(r.ID))
	if !r.Broadcast || r.OfferedTo != "" {
		t.Fatalf("expected broadcast, got %+v", r)
	}
	if len(e.chat.ChannelOf("Oslo")) == 0 {
		t.Fatal("no channel broadcast")
	}
	wantErr(t, e.s.Decline(r.ID, "o3", base), ErrNotOffered)
	_, err := e.s.Commit(r.ID, "b1", base.Add(2*time.Hour)) // in Bengaluru
	wantErr(t, err, ErrNotAtOffice)
	must(e.s.Commit(r.ID, "visitor", base.Add(2*time.Hour)))
}

func TestBroadcastRoundRestartsWhenSomeoneArrives(t *testing.T) {
	e := setup(t)
	for _, v := range []string{"o1", "o2", "o3"} {
		e.set(v, "")
	}
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	if !r.Broadcast {
		t.Fatal("expected broadcast")
	}
	e.set("o2", "Oslo")
	e.s.Tick(base.Add(2 * time.Hour))
	if r = must(e.s.Get(r.ID)); r.Broadcast || r.OfferedTo != "o2" {
		t.Fatalf("got %+v, want offer to o2", r)
	}
}

func TestCommitTimeoutReassigns(t *testing.T) {
	e := setup(t)
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	must(e.s.Commit(r.ID, "o1", base))
	e.s.Tick(base.Add(23 * time.Hour))
	if r = must(e.s.Get(r.ID)); r.State != Committed {
		t.Fatalf("released too early: %s", r.State)
	}
	e.s.Tick(base.Add(24 * time.Hour))
	r = must(e.s.Get(r.ID))
	if r.State != Open || r.OfferedTo != "o2" || r.Assignee != "" {
		t.Fatalf("not reassigned: %+v", r)
	}
	msgs := e.chat.DirectTo("o1")
	if !strings.Contains(msgs[len(msgs)-1], "timed out") {
		t.Fatalf("o1 not told: %v", msgs)
	}
	_, err := e.s.SubmitReceipt(r.ID, "o1", 50_000, "NOK", []byte("img"), nil, base.Add(25*time.Hour))
	wantErr(t, err, ErrNotAssignee)
	must(e.s.Commit(r.ID, "o2", base.Add(25*time.Hour)))
	r = must(e.s.SubmitReceipt(r.ID, "o2", 50_000, "NOK", []byte("img"), nil, base.Add(26*time.Hour)))
	if r.State != Submitted {
		t.Fatalf("state %s", r.State)
	}
}

func TestReleaseHandsOver(t *testing.T) {
	e := setup(t)
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	must(e.s.Commit(r.ID, "o1", base))
	wantErr(t, e.s.Release(r.ID, "o2", base), ErrNotAssignee)
	if err := e.s.Release(r.ID, "o1", base); err != nil {
		t.Fatal(err)
	}
	if r = must(e.s.Get(r.ID)); r.State != Open || r.OfferedTo != "o2" {
		t.Fatalf("got %+v", r)
	}
}

func TestCandidateFiltering(t *testing.T) {
	e := setup(t)
	e.set("o1", "")     // working from home
	e.set("o2", "")     // on holiday
	e.set("b1", "Oslo") // Bengaluru volunteer visiting Oslo
	r := must(e.s.ReportEmpty("Oslo", "x", base))
	if r.OfferedTo != "b1" { // b1 and o3 tie on history; "b1" < "o3"
		t.Fatalf("offered to %s, want visiting b1", r.OfferedTo)
	}
	must(e.s.Commit(r.ID, "b1", base))
	must(e.s.SubmitReceipt(r.ID, "b1", 10_000, "NOK", []byte("img"), nil, base))

	// b1 is now out of pocket (claim pending) -> skipped while someone else is available
	r = must(e.s.ReportEmpty("Oslo", "x", base.Add(time.Hour)))
	if r.OfferedTo != "o3" {
		t.Fatalf("offered to %s, want o3", r.OfferedTo)
	}
	must(e.s.Commit(r.ID, "o3", base.Add(time.Hour)))
	must(e.s.SubmitReceipt(r.ID, "o3", 10_000, "NOK", []byte("img"), nil, base.Add(time.Hour)))

	// only owing people are around -> still offered rather than leaving the box empty
	e.set("o3", "")
	r = must(e.s.ReportEmpty("Oslo", "x", base.Add(2*time.Hour)))
	if r.OfferedTo != "b1" {
		t.Fatalf("offered to %s, want b1 as last resort", r.OfferedTo)
	}
}

func TestFairnessRotation(t *testing.T) {
	e := setup(t)
	var got []string
	for i := range 4 {
		now := base.Add(time.Duration(i) * time.Hour)
		r := e.doRefill(t, "Oslo", 10_000, now)
		got = append(got, r.Assignee)
		e.hr.Decide(r.ClaimID, true)
		e.s.Tick(now)
	}
	want := []string{"o1", "o2", "o3", "o1"} // fewest refills first, then longest since last
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rotation %v, want %v", got, want)
	}
}

func TestBudgetCapAndExhaustion(t *testing.T) {
	e := setup(t)
	for i := range 4 {
		e.doRefill(t, "Oslo", 120_000, base.Add(time.Duration(i)*time.Hour))
	}
	r := must(e.s.ReportEmpty("Oslo", "x", base.Add(5*time.Hour)))
	r = must(e.s.Commit(r.ID, r.OfferedTo, base.Add(5*time.Hour)))
	if r.Cap != 120_000 {
		t.Fatalf("cap %d, want max purchase 120000", r.Cap)
	}
	must(e.s.SubmitReceipt(r.ID, r.Assignee, 100_000, "NOK", []byte("img"), nil, base.Add(5*time.Hour)))

	now := base.Add(6 * time.Hour)
	r = must(e.s.ReportEmpty("Oslo", "x", now))
	r = must(e.s.Commit(r.ID, r.OfferedTo, now))
	if r.Cap != 20_000 {
		t.Fatalf("cap %d, want remaining 20000", r.Cap)
	}
	sub := func(amount int64, currency string, img []byte) error {
		_, err := e.s.SubmitReceipt(r.ID, r.Assignee, amount, currency, img, nil, now)
		return err
	}
	wantErr(t, sub(20_001, "NOK", []byte("img")), ErrOverCap)
	wantErr(t, sub(0, "NOK", []byte("img")), ErrInvalidAmount)
	wantErr(t, sub(-5, "NOK", []byte("img")), ErrInvalidAmount)
	wantErr(t, sub(10_000, "INR", []byte("img")), ErrWrongCurrency)
	wantErr(t, sub(10_000, "NOK", nil), ErrNoReceipt)
	if err := sub(20_000, "NOK", []byte("img")); err != nil {
		t.Fatal(err)
	}
	wantErr(t, sub(20_000, "NOK", []byte("img")), ErrInvalidState) // double submit

	_, err := e.s.ReportEmpty("Oslo", "x", now.Add(time.Hour))
	wantErr(t, err, ErrBudgetExhausted)
	if ch := e.chat.ChannelOf("Oslo"); !strings.Contains(ch[len(ch)-1], "budget") {
		t.Fatalf("no budget notice: %v", ch)
	}
	// Bengaluru is independent
	must(e.s.ReportEmpty("Bengaluru", "x", now))
	// next month resets
	must(e.s.ReportEmpty("Oslo", "x", time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)))
}

func TestRejectedClaimFreesBudget(t *testing.T) {
	e := setup(t)
	var last Refill
	for i := range 5 {
		last = e.doRefill(t, "Oslo", 120_000, base.Add(time.Duration(i)*time.Hour))
	}
	_, err := e.s.ReportEmpty("Oslo", "x", base.Add(6*time.Hour))
	wantErr(t, err, ErrBudgetExhausted)
	e.hr.Decide(last.ClaimID, false)
	e.s.Tick(base.Add(6 * time.Hour))
	r := must(e.s.ReportEmpty("Oslo", "x", base.Add(7*time.Hour)))
	r = must(e.s.Commit(r.ID, r.OfferedTo, base.Add(7*time.Hour)))
	if r.Cap != 120_000 {
		t.Fatalf("cap %d", r.Cap)
	}
}

func TestVisitorClaimsInOfficeCurrency(t *testing.T) {
	e := setup(t)
	for _, v := range []string{"o1", "o2", "o3"} {
		e.set(v, "")
	}
	e.set("b1", "Oslo")
	r := e.doRefill(t, "Oslo", 50_000, base)
	if r.Assignee != "b1" {
		t.Fatalf("assignee %s", r.Assignee)
	}
	c := e.hr.Claims()[r.ClaimID]
	if c.Currency != "NOK" || c.Amount != 50_000 || c.EmployeeID != "b1" || !strings.Contains(c.Description, r.ID) {
		t.Fatalf("claim %+v", c)
	}
	rep := must(e.s.Report(2026, time.March, "NOK", map[string]int64{"INR": 125_000}))
	if rep.Offices[0].Spent != 50_000 || rep.Offices[1].Spent != 0 {
		t.Fatalf("report %+v", rep.Offices)
	}
}

func TestHROutageKeepsReceiptAndRetries(t *testing.T) {
	e := setup(t)
	e.hr.SetDown(true)
	r := e.doRefill(t, "Oslo", 50_000, base)
	if r.State != Purchased || r.ClaimID != "" {
		t.Fatalf("got %s / %q", r.State, r.ClaimID)
	}
	e.s.Tick(base.Add(48 * time.Hour)) // well past commit timeout
	if r = must(e.s.Get(r.ID)); r.State != Purchased || r.Assignee != "o1" {
		t.Fatalf("purchased refill was touched: %+v", r)
	}
	rep := must(e.s.Report(2026, time.March, "NOK", map[string]int64{"INR": 125_000}))
	if rep.Offices[0].Spent != 50_000 {
		t.Fatal("pending-submission spend not counted")
	}
	// box is full again, so a new empty report is a new refill
	if r2 := must(e.s.ReportEmpty("Oslo", "x", base.Add(49*time.Hour))); r2.ID == r.ID {
		t.Fatal("reused purchased refill")
	}
	e.hr.SetDown(false)
	e.s.Tick(base.Add(50 * time.Hour))
	if r = must(e.s.Get(r.ID)); r.State != Submitted || r.ClaimID == "" {
		t.Fatalf("not retried: %+v", r)
	}
	if len(e.hr.Claims()) != 1 {
		t.Fatalf("claims %d", len(e.hr.Claims()))
	}
}

func TestHRDecisions(t *testing.T) {
	e := setup(t)
	a := e.doRefill(t, "Oslo", 40_000, base)
	b := e.doRefill(t, "Oslo", 60_000, base.Add(time.Hour))
	e.s.Tick(base.Add(2 * time.Hour)) // pending: nothing changes
	if got := must(e.s.Get(a.ID)); got.State != Submitted {
		t.Fatal(got.State)
	}
	e.hr.Decide(a.ClaimID, true)
	e.hr.Decide(b.ClaimID, false)
	e.hr.SetDown(true)
	e.s.Tick(base.Add(3 * time.Hour)) // HR unreachable: still submitted
	if got := must(e.s.Get(a.ID)); got.State != Submitted {
		t.Fatal(got.State)
	}
	e.hr.SetDown(false)
	e.s.Tick(base.Add(4 * time.Hour))
	if got := must(e.s.Get(a.ID)); got.State != Reimbursed {
		t.Fatal(got.State)
	}
	if got := must(e.s.Get(b.ID)); got.State != Rejected {
		t.Fatal(got.State)
	}
	if m := e.chat.DirectTo(b.Assignee); !strings.Contains(m[len(m)-1], "rejected") {
		t.Fatalf("not notified: %v", m)
	}
	rep := must(e.s.Report(2026, time.March, "NOK", map[string]int64{"INR": 125_000}))
	if rep.Offices[0].Spent != 40_000 || rep.Offices[0].Refills != 1 {
		t.Fatalf("rejected claim still counted: %+v", rep.Offices[0])
	}
}

func TestReportAcrossCurrenciesAndTimeZones(t *testing.T) {
	e := setup(t)
	edge := time.Date(2026, 1, 31, 20, 0, 0, 0, time.UTC) // Oslo: Jan 31 21:00, Bengaluru: Feb 1 01:30
	e.doRefill(t, "Oslo", 100_000, edge, "chips", "chocolate")
	e.doRefill(t, "Bengaluru", 200_000, edge, "chips")
	rates := map[string]int64{"INR": 125_000} // 1 INR = 0.125 NOK

	jan := must(e.s.Report(2026, time.January, "NOK", rates))
	if jan.Offices[0].Spent != 100_000 || jan.Offices[1].Spent != 0 || jan.Total != 100_000 {
		t.Fatalf("jan %+v", jan)
	}
	if jan.Offices[0].Remaining != 500_000 || jan.Offices[0].Items["chips"] != 1 || jan.Offices[0].ByVolunteer["o1"] != 1 {
		t.Fatalf("jan details %+v", jan.Offices[0])
	}
	feb := must(e.s.Report(2026, time.February, "NOK", rates))
	bl := feb.Offices[1]
	if bl.Spent != 200_000 || bl.SpentInReportCurrency != 25_000 || bl.Remaining != 1_300_000 || feb.Total != 25_000 {
		t.Fatalf("feb %+v", feb)
	}
	// same month for both offices, total in NOK and in INR
	e.doRefill(t, "Oslo", 80_000, time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC))
	feb = must(e.s.Report(2026, time.February, "NOK", rates))
	if feb.Total != 105_000 {
		t.Fatalf("feb total %d, want 105000 øre", feb.Total)
	}
	inr := must(e.s.Report(2026, time.February, "INR", map[string]int64{"NOK": 8_000_000}))
	if inr.Total != 840_000 {
		t.Fatalf("INR total %d, want 840000 paise", inr.Total)
	}
	_, err := e.s.Report(2026, time.February, "NOK", nil)
	wantErr(t, err, ErrMissingRate)
}

func TestConvertRounding(t *testing.T) {
	for _, c := range []struct{ amt, ppm, want int64 }{
		{1, 125_000, 0}, {4, 125_000, 1}, {3, 125_000, 0}, {12, 125_000, 2}, {100, 1_000_000, 100}, {0, 125_000, 0},
	} {
		if got := convert(c.amt, c.ppm); got != c.want {
			t.Errorf("convert(%d,%d)=%d want %d", c.amt, c.ppm, got, c.want)
		}
	}
}

func TestInputValidation(t *testing.T) {
	e := setup(t)
	_, err := e.s.ReportEmpty("Paris", "x", base)
	wantErr(t, err, ErrUnknownOffice)
	_, err = e.s.ReportEmpty("Oslo", "", base)
	wantErr(t, err, ErrInvalidEmployee)
	_, err = e.s.Get("nope")
	wantErr(t, err, ErrNotFound)
	if _, err := New([]Office{{Name: "A"}}, nil, func(string, time.Time) string { return "" }, e.chat, e.hr, Config{}); err == nil {
		t.Fatal("accepted invalid office")
	}
	if _, err := New(nil, nil, nil, e.chat, e.hr, Config{}); err == nil {
		t.Fatal("accepted nil locator")
	}
}
