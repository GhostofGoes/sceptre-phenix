package store

// modifier is a store that reads, changes and writes a config in one atomic
// step.
type modifier interface {
	Modify(c *Config, change func(*Config) error) error
}

// Modify reads the stored config c names into c, calls change with it, and
// stores c unless change returns an error, which Modify returns. No other
// write, from this process or another, lands between the read and the write:
// BoltDB reads and writes in one transaction, and Etcd writes only if the
// config is unchanged since its read, and otherwise reads it again and calls
// change again, so change must depend on nothing but the config it is given.
// Other stores read, then write. change must not call the store: BoltDB holds
// its lock and its write transaction while change runs, so such a call would
// never return.
func Modify(c *Config, change func(*Config) error) error {
	if m, ok := DefaultStore.(modifier); ok {
		return m.Modify(c, change)
	}

	if err := Get(c); err != nil {
		return err
	}

	if err := change(c); err != nil {
		return err
	}

	return Update(c)
}
