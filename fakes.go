package snack

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// FakeChat records every message instead of sending it.
type FakeChat struct {
	mu       sync.Mutex
	direct   map[string][]string
	channels map[string][]string
}

func NewFakeChat() *FakeChat {
	return &FakeChat{direct: map[string][]string{}, channels: map[string][]string{}}
}

func (c *FakeChat) Direct(emp, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.direct[emp] = append(c.direct[emp], text)
}

func (c *FakeChat) Channel(office, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels[office] = append(c.channels[office], text)
}

func (c *FakeChat) DirectTo(emp string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.direct[emp])
}

func (c *FakeChat) ChannelOf(office string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.channels[office])
}

// FakeHR stores claims; tests decide them and can simulate an outage.
type FakeHR struct {
	mu     sync.Mutex
	claims map[string]Claim
	status map[string]ClaimStatus
	down   bool
}

func NewFakeHR() *FakeHR {
	return &FakeHR{claims: map[string]Claim{}, status: map[string]ClaimStatus{}}
}

func (h *FakeHR) CreateClaim(c Claim) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return "", errors.New("hr unavailable")
	}
	if c.Amount <= 0 || c.Currency == "" || c.EmployeeID == "" || len(c.Receipt) == 0 {
		return "", errors.New("invalid claim")
	}
	id := fmt.Sprintf("C%d", len(h.claims)+1)
	h.claims[id] = c
	h.status[id] = ClaimPending
	return id, nil
}

func (h *FakeHR) Status(id string) (ClaimStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return 0, errors.New("hr unavailable")
	}
	st, ok := h.status[id]
	if !ok {
		return 0, errors.New("unknown claim")
	}
	return st, nil
}

func (h *FakeHR) SetDown(down bool) { h.mu.Lock(); h.down = down; h.mu.Unlock() }

func (h *FakeHR) Decide(id string, approved bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if approved {
		h.status[id] = ClaimApproved
	} else {
		h.status[id] = ClaimRejected
	}
}

func (h *FakeHR) Claims() map[string]Claim {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]Claim, len(h.claims))
	for k, v := range h.claims {
		out[k] = v
	}
	return out
}
