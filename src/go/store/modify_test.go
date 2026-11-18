package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestBoltDBModify(t *testing.T) {
	db := NewBoltDB()

	if err := db.Init(Endpoint("bolt://" + filepath.Join(t.TempDir(), "store.bdb"))); err != nil {
		t.Fatalf("Init(): %v", err)
	}

	original := DefaultStore
	t.Cleanup(func() { DefaultStore = original })

	DefaultStore = db

	c, _ := NewConfig("experiment/modify")
	c.Status = map[string]any{"count": 0.0}

	if err := Create(c); err != nil {
		t.Fatalf("Create(): %v", err)
	}

	count := func() float64 {
		t.Helper()

		c, _ := NewConfig("experiment/modify")
		if err := Get(c); err != nil {
			t.Fatalf("Get(): %v", err)
		}

		n, _ := c.Status["count"].(float64)

		return n
	}

	t.Run("concurrent changes are not lost", func(t *testing.T) {
		var wg sync.WaitGroup

		for range 20 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				c, _ := NewConfig("experiment/modify")

				err := Modify(c, func(c *Config) error {
					n, _ := c.Status["count"].(float64)
					c.Status["count"] = n + 1

					return nil
				})
				if err != nil {
					t.Errorf("Modify(): %v", err)
				}
			}()
		}

		wg.Wait()

		if got := count(); got != 20 {
			t.Errorf("count = %v after 20 increments, want 20", got)
		}
	})

	t.Run("a refused change stores nothing", func(t *testing.T) {
		refused := errors.New("refused")

		c, _ := NewConfig("experiment/modify")

		err := Modify(c, func(c *Config) error {
			c.Status["count"] = -1.0

			return refused
		})
		if !errors.Is(err, refused) {
			t.Errorf("Modify() returned %v, want the change's error", err)
		}

		if got := count(); got != 20 {
			t.Errorf("count = %v after a refused change, want 20", got)
		}
	})

	t.Run("a missing config does not exist", func(t *testing.T) {
		c, _ := NewConfig("experiment/missing")

		err := Modify(c, func(*Config) error { return nil })
		if !errors.Is(err, ErrNotExist) {
			t.Errorf("Modify() of a missing config returned %v, want ErrNotExist", err)
		}
	})
}
