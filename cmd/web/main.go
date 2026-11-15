// Web UI: an interactive control panel over the real Service, with fake chat/HR and a simulated clock.
//
//	go run ./cmd/web            # then open http://localhost:8080
//	go run ./cmd/web -addr :9000
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"snack"
)

//go:embed index.html
var page []byte

var (
	home      = map[string]string{"anna": "Oslo", "erik": "Oslo", "lars": "Oslo", "ola": "Oslo", "kavya": "Bengaluru", "priya": "Bengaluru", "ravi": "Bengaluru"}
	people    = []string{"anna", "erik", "lars", "ola", "kavya", "priya", "ravi"}
	officeCur = map[string]string{"Oslo": "NOK", "Bengaluru": "INR"}
	rates     = map[string]int64{"INR": 125_000} // 1 INR = 0.125 NOK
	osloTZ, _ = time.LoadLocation("Europe/Oslo")
	blrTZ, _  = time.LoadLocation("Asia/Kolkata")
)

type entry struct {
	At      string `json:"at"`
	To      string `json:"to"`
	Channel bool   `json:"channel"`
	Text    string `json:"text"`
}

// logChat records every message in order, stamped with the simulated time.
type logChat struct {
	mu  sync.Mutex
	now func() time.Time
	log []entry
}

func (c *logChat) add(to string, ch bool, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, entry{At: c.now().Format("Jan 02 15:04"), To: to, Channel: ch, Text: text})
}
func (c *logChat) Direct(emp, text string)     { c.add(emp, false, text) }
func (c *logChat) Channel(office, text string) { c.add(office, true, text) }

type world struct {
	s      *snack.Service
	chat   *logChat
	hr     *snack.FakeHR
	now    time.Time
	hrDown bool
	atMu   sync.RWMutex // locator is read from concurrent goroutines in the "race" action
	at     map[string]string
}

func newWorld() *world {
	w := &world{hr: snack.NewFakeHR(), now: time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC), at: map[string]string{}}
	for p, o := range home {
		w.at[p] = o
	}
	w.chat = &logChat{now: func() time.Time { return w.now }}
	locate := func(e string, _ time.Time) string { w.atMu.RLock(); defer w.atMu.RUnlock(); return w.at[e] }
	s, err := snack.New([]snack.Office{
		{Name: "Oslo", Currency: "NOK", MonthlyBudget: 600_000, MaxPurchase: 120_000, Loc: osloTZ},
		{Name: "Bengaluru", Currency: "INR", MonthlyBudget: 1_500_000, MaxPurchase: 300_000, Loc: blrTZ},
	}, people, locate, w.chat, w.hr, snack.Config{})
	if err != nil {
		panic(err)
	}
	w.s = s
	return w
}

// ---- JSON views ----

type personView struct {
	Name string `json:"name"`
	Home string `json:"home"`
	At   string `json:"at"`
}

type refillView struct {
	ID             string   `json:"id"`
	Office         string   `json:"office"`
	Currency       string   `json:"currency"`
	Month          string   `json:"month"`
	State          string   `json:"state"`
	ReportedBy     string   `json:"reportedBy"`
	OfferedTo      string   `json:"offeredTo"`
	OfferDeadline  string   `json:"offerDeadline"`
	Broadcast      bool     `json:"broadcast"`
	Tried          []string `json:"tried"`
	Assignee       string   `json:"assignee"`
	CommitDeadline string   `json:"commitDeadline"`
	Cap            int64    `json:"cap"`
	Amount         int64    `json:"amount"`
	Items          []string `json:"items"`
	ClaimID        string   `json:"claimId"`
}

type claimView struct {
	ID          string `json:"id"`
	Employee    string `json:"employee"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type view struct {
	Now         string               `json:"now"`
	NowOslo     string               `json:"nowOslo"`
	NowBlr      string               `json:"nowBlr"`
	HRDown      bool                 `json:"hrDown"`
	People      []personView         `json:"people"`
	Refills     []refillView         `json:"refills"`
	Claims      []claimView          `json:"claims"`
	Chat        []entry              `json:"chat"`
	Month       string               `json:"month"`
	Report      *snack.MonthlyReport `json:"report"`
	ReportError string               `json:"reportError,omitempty"`
}

func fmtT(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("Jan 02 15:04") + " UTC"
}

func (w *world) view(month string) view {
	v := view{
		Now: fmtT(w.now), NowOslo: w.now.In(osloTZ).Format("Jan 02 15:04 MST"),
		NowBlr: w.now.In(blrTZ).Format("Jan 02 15:04 MST"), HRDown: w.hrDown, Month: month,
		Refills: []refillView{}, Claims: []claimView{}, // [] not null in JSON, the page iterates them
	}
	w.atMu.RLock()
	for _, p := range people {
		v.People = append(v.People, personView{p, home[p], w.at[p]})
	}
	w.atMu.RUnlock()
	for _, r := range w.s.List() {
		v.Refills = append(v.Refills, refillView{
			ID: r.ID, Office: r.Office, Currency: officeCur[r.Office], Month: r.Month, State: string(r.State),
			ReportedBy: r.ReportedBy, OfferedTo: r.OfferedTo, OfferDeadline: fmtT(r.OfferDeadline), Broadcast: r.Broadcast,
			Tried: r.Tried, Assignee: r.Assignee, CommitDeadline: fmtT(r.CommitDeadline), Cap: r.Cap,
			Amount: r.Amount, Items: r.Items, ClaimID: r.ClaimID,
		})
	}
	claims := w.hr.Claims()
	ids := make([]string, 0, len(claims))
	for id := range claims {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return atoi(a[1:]) - atoi(b[1:]) })
	for _, id := range ids {
		c := claims[id]
		st := "unknown (HR down)"
		if s, err := w.hr.Status(id); err == nil {
			st = [...]string{"pending", "approved", "rejected"}[s]
		}
		v.Claims = append(v.Claims, claimView{id, c.EmployeeID, c.Amount, c.Currency, c.Description, st})
	}
	w.chat.mu.Lock()
	v.Chat = append([]entry{}, w.chat.log...)
	w.chat.mu.Unlock()
	if t, err := time.Parse("2006-01", month); err != nil {
		v.ReportError = "month must look like 2026-03"
	} else if rep, err := w.s.Report(t.Year(), t.Month(), "NOK", rates); err != nil {
		v.ReportError = err.Error()
	} else {
		v.Report = &rep
	}
	return v
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// ---- actions ----

type action struct {
	Op      string `json:"op"`
	Office  string `json:"office"`
	Who     string `json:"who"`
	ID      string `json:"id"`
	Amount  string `json:"amount"`
	Cur     string `json:"currency"`
	Items   string `json:"items"`
	Image   bool   `json:"image"`
	Minutes int    `json:"minutes"`
	Claim   string `json:"claim"`
	Approve bool   `json:"approve"`
	Down    bool   `json:"down"`
}

// parseMoney turns "850.50" into minor units. Rejects junk and more than 2 decimals.
func parseMoney(s string) (int64, error) {
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e12 {
		return 0, errors.New("amount must be a number like 850 or 850.50")
	}
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 > 2 {
		return 0, errors.New("amount can have at most 2 decimals")
	}
	return int64(math.Round(f * 100)), nil
}

func (w *world) do(a action) (string, error) {
	switch a.Op {
	case "advance":
		if a.Minutes <= 0 || a.Minutes > 60*24*31 {
			return "", errors.New("minutes must be between 1 and 44640")
		}
		w.now = w.now.Add(time.Duration(a.Minutes) * time.Minute)
		w.s.Tick(w.now)
		return fmt.Sprintf("Clock advanced %s and Tick ran.", time.Duration(a.Minutes)*time.Minute), nil
	case "tick":
		w.s.Tick(w.now)
		return "Tick ran.", nil
	case "locate":
		if _, ok := home[a.Who]; !ok {
			return "", errors.New("unknown person")
		}
		if a.Office != "" && officeCur[a.Office] == "" {
			return "", errors.New("unknown office")
		}
		w.atMu.Lock()
		w.at[a.Who] = a.Office
		w.atMu.Unlock()
		where := a.Office
		if where == "" {
			where = "away (home/holiday)"
		}
		return fmt.Sprintf("%s is now at %s.", a.Who, where), nil
	case "report":
		r, err := w.s.ReportEmpty(a.Office, a.Who, w.now)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Refill %s (%s).", r.ID, r.State), nil
	case "commit":
		r, err := w.s.Commit(a.ID, a.Who, w.now)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s committed to %s, cap %d.%02d %s.", a.Who, r.ID, r.Cap/100, r.Cap%100, officeCur[r.Office]), nil
	case "decline":
		return a.Who + " declined.", w.s.Decline(a.ID, a.Who, w.now)
	case "release":
		return a.Who + " released the refill.", w.s.Release(a.ID, a.Who, w.now)
	case "submit":
		amt, err := parseMoney(a.Amount)
		if err != nil {
			return "", err
		}
		var items []string
		for _, it := range strings.Split(a.Items, ",") {
			if it = strings.TrimSpace(strings.ToLower(it)); it != "" {
				items = append(items, it)
			}
		}
		var img []byte
		if a.Image {
			img = []byte("receipt.jpg")
		}
		r, err := w.s.SubmitReceipt(a.ID, a.Who, amt, a.Cur, img, items, w.now)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Receipt accepted, refill %s is %s.", r.ID, r.State), nil
	case "race":
		// everyone at the refill's office clicks "accept" at the same instant
		r, err := w.s.Get(a.ID)
		if err != nil {
			return "", err
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var res []string
		w.atMu.RLock()
		var here []string
		for _, p := range people {
			if w.at[p] == r.Office {
				here = append(here, p)
			}
		}
		w.atMu.RUnlock()
		if len(here) == 0 {
			return "", errors.New("nobody is at that office")
		}
		for _, p := range here {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := w.s.Commit(a.ID, p, w.now)
				msg := p + ": WON"
				if err != nil {
					msg = p + ": " + err.Error()
				}
				mu.Lock()
				res = append(res, msg)
				mu.Unlock()
			}()
		}
		wg.Wait()
		slices.Sort(res)
		return "Simultaneous accept → " + strings.Join(res, " · "), nil
	case "hrdown":
		w.hrDown = a.Down
		w.hr.SetDown(a.Down)
		return fmt.Sprintf("HR system down = %v.", a.Down), nil
	case "decide":
		if _, ok := w.hr.Claims()[a.Claim]; !ok {
			return "", errors.New("unknown claim")
		}
		w.hr.Decide(a.Claim, a.Approve)
		verb := "rejected"
		if a.Approve {
			verb = "approved"
		}
		return fmt.Sprintf("HR %s %s. The service sees it on the next Tick.", verb, a.Claim), nil
	}
	return "", fmt.Errorf("unknown op %q", a.Op)
}

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()
	var mu sync.Mutex // one simulated world, one request at a time
	w := newWorld()

	reply := func(rw http.ResponseWriter, month, info, errMsg string) {
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(map[string]any{"state": w.view(month), "info": info, "error": errMsg})
	}
	month := func(r *http.Request) string {
		if m := r.URL.Query().Get("month"); m != "" {
			return m
		}
		return w.now.Format("2006-01")
	}

	http.HandleFunc("GET /{$}", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write(page)
	})
	http.HandleFunc("GET /api/state", func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reply(rw, month(r), "", "")
	})
	http.HandleFunc("POST /api/do", func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var a action
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<16)).Decode(&a); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			reply(rw, month(r), "", "bad request: "+err.Error())
			return
		}
		if a.Op == "reset" {
			w = newWorld()
			reply(rw, w.now.Format("2006-01"), "Simulation reset.", "")
			return
		}
		info, err := w.do(a)
		if err != nil {
			reply(rw, month(r), "", err.Error())
			return
		}
		reply(rw, month(r), info, "")
	})
	log.Printf("Snack counter UI on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
