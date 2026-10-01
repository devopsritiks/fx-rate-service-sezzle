package cache

import (
	"sync"
	"testing"
	"time"
)

func TestLRU_SetAndGet(t *testing.T) {
	c := NewLRU(10, Hooks{})
	c.Set("USD:CAD", Entry{Base: "USD", RatesDate: "2024-01-01", FetchedAt: time.Now()})

	entry, ok := c.Get("USD:CAD")
	if !ok {
		t.Fatal("expected entry to be found")
	}
	if entry.Base != "USD" {
		t.Errorf("unexpected entry: %+v", entry)
	}

	if _, ok := c.Get("EUR:GBP"); ok {
		t.Error("expected miss for unset key")
	}
}

func TestLRU_EvictsLeastRecentlyUsed(t *testing.T) {
	var evicted []string
	c := NewLRU(2, Hooks{
		OnEvict: func(key string, reason EvictReason) {
			evicted = append(evicted, key)
		},
	})

	c.Set("a", Entry{FetchedAt: time.Now()})
	c.Set("b", Entry{FetchedAt: time.Now()})
	c.Get("a")                               // touch "a" so "b" becomes the least-recently-used
	c.Set("c", Entry{FetchedAt: time.Now()}) // should evict "b", not "a"

	if _, ok := c.Get("b"); ok {
		t.Error("expected 'b' to have been evicted as least-recently-used")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("expected 'a' to survive eviction (was touched more recently)")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("expected 'c' to be present (just inserted)")
	}
	if len(evicted) != 1 || evicted[0] != "b" {
		t.Errorf("expected OnEvict to fire once for 'b', got %v", evicted)
	}
	if c.Len() != 2 {
		t.Errorf("expected cache to hold exactly capacity (2) entries, got %d", c.Len())
	}
}

func TestLRU_SetExistingKeyDoesNotGrowOrEvict(t *testing.T) {
	var evicted int
	c := NewLRU(2, Hooks{OnEvict: func(key string, reason EvictReason) { evicted++ }})

	c.Set("a", Entry{FetchedAt: time.Now()})
	c.Set("b", Entry{FetchedAt: time.Now()})
	c.Set("a", Entry{Base: "updated", FetchedAt: time.Now()}) // overwrite, not a new entry

	if c.Len() != 2 {
		t.Errorf("expected overwrite to keep cache at 2 entries, got %d", c.Len())
	}
	if evicted != 0 {
		t.Errorf("expected no eviction from overwriting an existing key, got %d evictions", evicted)
	}
	entry, _ := c.Get("a")
	if entry.Base != "updated" {
		t.Errorf("expected overwrite to take effect, got %+v", entry)
	}
}

func TestLRU_OldestAge(t *testing.T) {
	c := NewLRU(10, Hooks{})

	if age := c.OldestAge(); age != 0 {
		t.Errorf("expected 0 age for empty cache, got %v", age)
	}

	c.Set("old", Entry{FetchedAt: time.Now().Add(-2 * time.Hour)})
	c.Set("new", Entry{FetchedAt: time.Now()})

	age := c.OldestAge()
	if age < 90*time.Minute || age > 3*time.Hour {
		t.Errorf("expected oldest age to reflect the ~2h old entry, got %v", age)
	}
}

func TestLRU_ConcurrentAccess(t *testing.T) {
	c := NewLRU(50, Hooks{})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key"
			c.Set(key, Entry{FetchedAt: time.Now()})
			c.Get(key)
			c.Len()
			c.OldestAge()
		}(i)
	}
	wg.Wait()
}
