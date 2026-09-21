package guard

import (
	"testing"
	"time"
)

func TestCooldownWindow(t *testing.T) {
	now := time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC)
	c := NewCooldown(60 * time.Second)
	c.now = func() time.Time { return now }

	if ok, _ := c.Allow("s1"); !ok {
		t.Fatal("first attempt must be allowed")
	}
	now = now.Add(30 * time.Second)
	ok, remaining := c.Allow("s1")
	if ok || remaining != 30*time.Second {
		t.Fatalf("second attempt inside window: ok=%v remaining=%v", ok, remaining)
	}
	if ok, _ := c.Allow("s2"); !ok {
		t.Fatal("other sessions are independent")
	}
	now = now.Add(30 * time.Second)
	if ok, _ := c.Allow("s1"); !ok {
		t.Fatal("attempt after the window must be allowed")
	}
	if c.Len() != 2 {
		t.Fatalf("len %d", c.Len())
	}
	now = now.Add(2 * time.Minute)
	c.Prune()
	if c.Len() != 0 {
		t.Fatalf("prune left %d entries", c.Len())
	}
}

func TestCooldownDisabled(t *testing.T) {
	c := NewCooldown(0)
	for i := 0; i < 3; i++ {
		if ok, _ := c.Allow("s1"); !ok {
			t.Fatal("zero window must always allow")
		}
	}
	if c.Len() != 0 {
		t.Fatal("zero window must not track sessions")
	}
}
