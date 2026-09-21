package guard

import (
	"sync"
	"time"
)

// Cooldown remembers when each session was last acted upon so a session is
// not terminated (or reported in dry-run) more than once per window. It is
// safe for concurrent use.
type Cooldown struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// NewCooldown creates a Cooldown with the given window. A zero window
// disables the cooldown (every call to Allow succeeds).
func NewCooldown(window time.Duration) *Cooldown {
	return &Cooldown{window: window, now: time.Now, last: map[string]time.Time{}}
}

// Allow reports whether id may be acted upon now. When it may, the current
// time is recorded and remaining is zero; otherwise remaining is the time
// left in the window.
func (c *Cooldown) Allow(id string) (allowed bool, remaining time.Duration) {
	if c.window <= 0 {
		return true, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if t, ok := c.last[id]; ok {
		if elapsed := now.Sub(t); elapsed < c.window {
			return false, c.window - elapsed
		}
	}
	c.last[id] = now
	return true, 0
}

// Prune drops entries whose window has expired.
func (c *Cooldown) Prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, t := range c.last {
		if now.Sub(t) >= c.window {
			delete(c.last, id)
		}
	}
}

// Len returns the number of tracked sessions.
func (c *Cooldown) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.last)
}
