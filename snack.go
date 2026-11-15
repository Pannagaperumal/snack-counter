// Package snack automates the snack counter refill lifecycle:
// empty -> offered -> committed -> purchased -> submitted to HR -> reimbursed/rejected.
package snack

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
	_ "time/tzdata" // office time zones must resolve on any machine
)

// ---- External systems (simulated in fakes.go) ----

// Chat is the company chat tool.
type Chat interface {
	Direct(employeeID, text string)
	Channel(office, text string)
}

// Claim is what the HR system accepts.
type Claim struct {
	Amount      int64 // minor units (øre, paise)
	Currency    string
	Description string
	Receipt     []byte
	EmployeeID  string
}

type ClaimStatus int

const (
	ClaimPending ClaimStatus = iota
	ClaimApproved
	ClaimRejected
)

// HR is the external reimbursement system.
type HR interface {
	CreateClaim(c Claim) (claimID string, err error)
	Status(claimID string) (ClaimStatus, error)
}

// Locator says which office an employee is physically at on a given moment
// ("" = home, holiday, travelling). Fed from calendar/chat presence in reality.
type Locator func(employeeID string, now time.Time) string

// ---- Domain ----

type Office struct {
	Name          string
	Currency      string
	MonthlyBudget int64 // minor units
	MaxPurchase   int64 // minor units, upper end of a typical purchase
	Loc           *time.Location
}

type Config struct {
	OfferTimeout   time.Duration // how long an offered volunteer has to accept
	CommitTimeout  time.Duration // how long a committed volunteer has to submit a receipt
	FairnessWindow time.Duration // look-back for counting someone's refills
}

type State string

const (
	Open       State = "open"
	Committed  State = "committed"
	Purchased  State = "purchased" // receipt recorded, HR claim not yet created (retrying)
	Submitted  State = "submitted"
	Reimbursed State = "reimbursed"
	Rejected   State = "rejected"
)

type Refill struct {
	ID         string
	Office     string
	Month      string // budget month "2006-01" in office local time, fixed at open
	State      State
	OpenedAt   time.Time
	ReportedBy string

	OfferedTo     string
	OfferDeadline time.Time
	Broadcast     bool     // nobody eligible left: anyone at the office may claim
	Tried         []string // volunteers already offered/assigned in this round

	Assignee       string
	CommitDeadline time.Time
	Cap            int64

	Amount      int64
	Currency    string
	Items       []string
	Receipt     []byte
	PurchasedAt time.Time
	ClaimID     string
}

var (
	ErrUnknownOffice   = errors.New("unknown office")
	ErrNotFound        = errors.New("refill not found")
	ErrAlreadyTaken    = errors.New("refill already taken or no longer open")
	ErrNotOffered      = errors.New("refill is not offered to you")
	ErrNotAtOffice     = errors.New("you are not at this office today")
	ErrNotAssignee     = errors.New("you are not the assignee of this refill")
	ErrInvalidState    = errors.New("refill is not in a state that allows this")
	ErrBudgetExhausted = errors.New("monthly budget exhausted")
	ErrInvalidAmount   = errors.New("amount must be positive")
	ErrWrongCurrency   = errors.New("receipt currency does not match office currency")
	ErrOverCap         = errors.New("amount exceeds the agreed spending cap")
	ErrNoReceipt       = errors.New("receipt image is required")
	ErrMissingRate     = errors.New("missing exchange rate")
	ErrInvalidEmployee = errors.New("employee ID is required")
)

// Service holds all state; one mutex makes every transition atomic.
// shortcut: in-memory state, swap for a DB with row locks / conditional updates when persisted.
type Service struct {
	mu         sync.Mutex
	offices    []Office
	officeIdx  map[string]int
	volunteers []string
	locate     Locator
	chat       Chat
	hr         HR
	cfg        Config

	refills []*Refill
	byID    map[string]*Refill
	active  map[string]*Refill // office -> refill not yet purchased (at most one)
}

func New(offices []Office, volunteers []string, locate Locator, chat Chat, hr HR, cfg Config) (*Service, error) {
	if cfg.OfferTimeout <= 0 {
		cfg.OfferTimeout = 2 * time.Hour
	}
	if cfg.CommitTimeout <= 0 {
		cfg.CommitTimeout = 24 * time.Hour
	}
	if cfg.FairnessWindow <= 0 {
		cfg.FairnessWindow = 90 * 24 * time.Hour
	}
	if locate == nil || chat == nil || hr == nil {
		return nil, errors.New("locator, chat and hr are required")
	}
	idx := map[string]int{}
	for i, o := range offices {
		if o.Name == "" || o.Currency == "" || o.Loc == nil || o.MonthlyBudget <= 0 || o.MaxPurchase <= 0 {
			return nil, fmt.Errorf("invalid office config %q", o.Name)
		}
		if _, dup := idx[o.Name]; dup {
			return nil, fmt.Errorf("duplicate office %q", o.Name)
		}
		idx[o.Name] = i
	}
	return &Service{
		offices: slices.Clone(offices), officeIdx: idx, volunteers: slices.Clone(volunteers),
		locate: locate, chat: chat, hr: hr, cfg: cfg,
		byID: map[string]*Refill{}, active: map[string]*Refill{},
	}, nil
}

// ReportEmpty is called by anyone who sees the box empty. Idempotent per office:
// while a refill is in progress the existing one is returned and nobody else is pinged.
func (s *Service) ReportEmpty(office, reporter string, now time.Time) (Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.officeIdx[office]
	if !ok {
		return Refill{}, ErrUnknownOffice
	}
	if reporter == "" {
		return Refill{}, ErrInvalidEmployee
	}
	if r := s.active[office]; r != nil {
		return r.snapshot(), nil
	}
	o := s.offices[i]
	month := monthKey(now.In(o.Loc))
	if o.MonthlyBudget-s.spent(office, month) <= 0 {
		s.chat.Channel(office, fmt.Sprintf("Snack box is empty but the %s budget for %s is used up.", office, month))
		return Refill{}, ErrBudgetExhausted
	}
	r := &Refill{
		ID: fmt.Sprintf("R%d", len(s.refills)+1), Office: office, Month: month,
		State: Open, OpenedAt: now, ReportedBy: reporter,
	}
	s.refills = append(s.refills, r)
	s.byID[r.ID] = r
	s.active[office] = r
	s.offerNext(r, now)
	return r.snapshot(), nil
}

// Commit claims the refill. Exactly one caller can win.
func (s *Service) Commit(refillID, emp string, now time.Time) (Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[refillID]
	if !ok {
		return Refill{}, ErrNotFound
	}
	if emp == "" {
		return Refill{}, ErrInvalidEmployee
	}
	if r.State != Open {
		return Refill{}, ErrAlreadyTaken
	}
	if r.Broadcast {
		if s.locate(emp, now) != r.Office {
			return Refill{}, ErrNotAtOffice
		}
	} else if r.OfferedTo != emp {
		return Refill{}, ErrNotOffered
	}
	o := s.offices[s.officeIdx[r.Office]]
	limit := min(o.MaxPurchase, o.MonthlyBudget-s.spent(r.Office, r.Month))
	if limit <= 0 {
		return Refill{}, ErrBudgetExhausted
	}
	r.State, r.Assignee, r.Cap = Committed, emp, limit
	r.OfferedTo, r.OfferDeadline, r.Broadcast = "", time.Time{}, false
	r.CommitDeadline = now.Add(s.cfg.CommitTimeout)
	if !slices.Contains(r.Tried, emp) {
		r.Tried = append(r.Tried, emp)
	}
	s.chat.Direct(emp, fmt.Sprintf("Thanks! Refill %s is yours. Spend at most %s and submit the receipt by %s.",
		r.ID, money(limit, o.Currency), r.CommitDeadline.In(o.Loc).Format(time.RFC1123)))
	s.chat.Channel(r.Office, fmt.Sprintf("%s is refilling the snack box (%s). Please don't buy.", emp, r.ID))
	return r.snapshot(), nil
}

// Decline passes the offer on to the next candidate.
func (s *Service) Decline(refillID, emp string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[refillID]
	if !ok {
		return ErrNotFound
	}
	if r.State != Open || r.Broadcast || r.OfferedTo != emp {
		return ErrNotOffered
	}
	s.offerNext(r, now)
	return nil
}

// Release lets a committed volunteer back out before buying.
func (s *Service) Release(refillID, emp string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[refillID]
	if !ok {
		return ErrNotFound
	}
	if r.Assignee != emp || emp == "" {
		return ErrNotAssignee
	}
	if r.State != Committed {
		return ErrInvalidState
	}
	s.reopen(r, now)
	return nil
}

// SubmitReceipt records the purchase and creates the HR claim. If HR is down the
// receipt is kept (state Purchased) and Tick retries; the volunteer is never reassigned.
func (s *Service) SubmitReceipt(refillID, emp string, amount int64, currency string, receipt []byte, items []string, now time.Time) (Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[refillID]
	if !ok {
		return Refill{}, ErrNotFound
	}
	if r.Assignee != emp || emp == "" {
		return Refill{}, ErrNotAssignee
	}
	if r.State != Committed {
		return Refill{}, ErrInvalidState
	}
	o := s.offices[s.officeIdx[r.Office]]
	switch {
	case amount <= 0:
		return Refill{}, ErrInvalidAmount
	case currency != o.Currency:
		return Refill{}, ErrWrongCurrency
	case amount > r.Cap:
		return Refill{}, ErrOverCap
	case len(receipt) == 0:
		return Refill{}, ErrNoReceipt
	}
	r.State, r.Amount, r.Currency, r.PurchasedAt = Purchased, amount, currency, now
	r.Receipt, r.Items = slices.Clone(receipt), slices.Clone(items)
	delete(s.active, r.Office) // box is full again; a new empty report starts a new refill
	s.chat.Channel(r.Office, fmt.Sprintf("Snack box refilled by %s. Thank you!", emp))
	s.sendClaim(r)
	return r.snapshot(), nil
}

// Tick runs periodically (e.g. every few minutes): expires offers and commitments,
// retries failed HR submissions, and polls HR for decisions.
func (s *Service) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.refills {
		switch r.State {
		case Open:
			if now.Before(r.OfferDeadline) {
				continue
			}
			if r.Broadcast {
				// nobody claimed during a full round: start over, people may have arrived at the office
				r.Tried, r.Broadcast = nil, false
			} else {
				s.chat.Direct(r.OfferedTo, fmt.Sprintf("Offer for refill %s expired; passing it on.", r.ID))
			}
			s.offerNext(r, now)
		case Committed:
			if !now.Before(r.CommitDeadline) {
				s.chat.Direct(r.Assignee, fmt.Sprintf("Refill %s timed out and was handed to someone else. Please don't buy.", r.ID))
				s.reopen(r, now)
			}
		case Purchased:
			s.sendClaim(r)
		case Submitted:
			st, err := s.hr.Status(r.ClaimID)
			if err != nil {
				continue // try again next tick
			}
			o := s.offices[s.officeIdx[r.Office]]
			switch st {
			case ClaimApproved:
				r.State = Reimbursed
				s.chat.Direct(r.Assignee, fmt.Sprintf("Your claim for %s was approved.", money(r.Amount, r.Currency)))
			case ClaimRejected:
				r.State = Rejected
				s.chat.Direct(r.Assignee, fmt.Sprintf("Your claim for %s was rejected by HR. Please contact HR.", money(r.Amount, r.Currency)))
				s.chat.Channel(o.Name, fmt.Sprintf("HR rejected the claim for refill %s; a coordinator should follow up.", r.ID))
			}
		}
	}
}

// Get returns a copy of a refill.
func (s *Service) Get(refillID string) (Refill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[refillID]
	if !ok {
		return Refill{}, ErrNotFound
	}
	return r.snapshot(), nil
}

// List returns copies of all refills, oldest first.
func (s *Service) List() []Refill {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Refill, len(s.refills))
	for i, r := range s.refills {
		out[i] = r.snapshot()
	}
	return out
}

// ---- Reporting ----

type OfficeReport struct {
	Office                string
	Currency              string
	Budget                int64
	Spent                 int64
	Remaining             int64
	Refills               int
	ByVolunteer           map[string]int
	Items                 map[string]int
	SpentInReportCurrency int64
}

type MonthlyReport struct {
	Month    string
	Currency string
	Offices  []OfficeReport
	Total    int64 // in Currency, minor units
}

// Report builds the monthly management report. ratesPPM maps a currency to how many
// report-currency units one unit is worth, times 1 000 000 (INR->NOK 0.125 = 125000).
func (s *Service) Report(year int, month time.Month, reportCurrency string, ratesPPM map[string]int64) (MonthlyReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%04d-%02d", year, int(month))
	rep := MonthlyReport{Month: key, Currency: reportCurrency}
	for _, o := range s.offices {
		or := OfficeReport{Office: o.Name, Currency: o.Currency, Budget: o.MonthlyBudget,
			ByVolunteer: map[string]int{}, Items: map[string]int{}}
		for _, r := range s.refills {
			if r.Office != o.Name || r.Month != key || !counts(r.State) {
				continue
			}
			or.Spent += r.Amount
			or.Refills++
			or.ByVolunteer[r.Assignee]++
			for _, it := range r.Items {
				or.Items[it]++
			}
		}
		or.Remaining = o.MonthlyBudget - or.Spent
		rate := int64(1_000_000)
		if o.Currency != reportCurrency {
			var ok bool
			if rate, ok = ratesPPM[o.Currency]; !ok || rate <= 0 {
				return MonthlyReport{}, fmt.Errorf("%w: %s->%s", ErrMissingRate, o.Currency, reportCurrency)
			}
		}
		or.SpentInReportCurrency = convert(or.Spent, rate)
		rep.Total += or.SpentInReportCurrency
		rep.Offices = append(rep.Offices, or)
	}
	return rep, nil
}

// ---- internals (caller holds s.mu) ----

// counts reports whether a refill's money is spent from the budget.
func counts(st State) bool { return st == Purchased || st == Submitted || st == Reimbursed }

func (s *Service) spent(office, month string) int64 {
	var sum int64
	for _, r := range s.refills {
		if r.Office == office && r.Month == month && counts(r.State) {
			sum += r.Amount
		}
	}
	return sum
}

func (s *Service) reopen(r *Refill, now time.Time) {
	r.State, r.Assignee, r.Cap, r.CommitDeadline = Open, "", 0, time.Time{}
	s.chat.Channel(r.Office, fmt.Sprintf("Refill %s needs a new volunteer.", r.ID))
	s.offerNext(r, now)
}

// offerNext offers the refill to the fairest eligible volunteer, or broadcasts if none.
func (s *Service) offerNext(r *Refill, now time.Time) {
	r.OfferDeadline = now.Add(s.cfg.OfferTimeout)
	c := s.candidates(r, now)
	if len(c) == 0 {
		r.OfferedTo, r.Broadcast = "", true
		s.chat.Channel(r.Office, fmt.Sprintf("Snack box at %s is empty and no volunteer is available. First to claim refill %s takes it.", r.Office, r.ID))
		return
	}
	r.OfferedTo, r.Broadcast = c[0], false
	r.Tried = append(r.Tried, c[0])
	s.chat.Direct(c[0], fmt.Sprintf("The %s snack box is empty. Can you refill it (%s)? Accept or decline within %s.", r.Office, r.ID, s.cfg.OfferTimeout))
}

// candidates: volunteers from any office who are at this office now and not yet tried,
// people still waiting for money only if nobody else is left, fewest recent refills first.
func (s *Service) candidates(r *Refill, now time.Time) []string {
	type cand struct {
		id    string
		n     int
		last  time.Time
		owing bool
	}
	var free, owing []cand
	since := now.Add(-s.cfg.FairnessWindow)
	for _, v := range s.volunteers {
		if v == "" || slices.Contains(r.Tried, v) || s.locate(v, now) != r.Office {
			continue
		}
		c := cand{id: v}
		busy := false
		for _, x := range s.refills {
			if x.Assignee != v {
				continue
			}
			if x.State == Committed {
				busy = true // already on another refill
			}
			if x.State == Purchased || x.State == Submitted {
				c.owing = true
			}
			if !x.PurchasedAt.IsZero() {
				if x.PurchasedAt.After(since) {
					c.n++
				}
				if x.PurchasedAt.After(c.last) {
					c.last = x.PurchasedAt
				}
			}
		}
		if busy {
			continue
		}
		if c.owing {
			owing = append(owing, c)
		} else {
			free = append(free, c)
		}
	}
	pool := free
	if len(pool) == 0 {
		pool = owing
	}
	sort.Slice(pool, func(i, j int) bool {
		a, b := pool[i], pool[j]
		if a.n != b.n {
			return a.n < b.n
		}
		if !a.last.Equal(b.last) {
			return a.last.Before(b.last)
		}
		return a.id < b.id
	})
	out := make([]string, len(pool))
	for i, c := range pool {
		out[i] = c.id
	}
	return out
}

func (s *Service) sendClaim(r *Refill) {
	id, err := s.hr.CreateClaim(Claim{
		Amount: r.Amount, Currency: r.Currency, Receipt: r.Receipt, EmployeeID: r.Assignee,
		Description: fmt.Sprintf("Snacks for %s office, refill %s (%s)", r.Office, r.ID, r.Month),
	})
	if err != nil {
		return // stays Purchased; Tick retries. Receipt is kept, volunteer keeps the refill.
	}
	r.State, r.ClaimID = Submitted, id
	s.chat.Direct(r.Assignee, fmt.Sprintf("Claim %s for %s submitted to HR (refill %s).", id, money(r.Amount, r.Currency), r.ID))
}

func (r *Refill) snapshot() Refill {
	c := *r
	c.Tried, c.Items, c.Receipt = slices.Clone(r.Tried), slices.Clone(r.Items), slices.Clone(r.Receipt)
	return c
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

// convert applies a parts-per-million rate with half-up rounding (amounts are non-negative).
func convert(amount, ppm int64) int64 { return (amount*ppm + 500_000) / 1_000_000 }

func money(minor int64, cur string) string {
	return fmt.Sprintf("%d.%02d %s", minor/100, minor%100, cur)
}
