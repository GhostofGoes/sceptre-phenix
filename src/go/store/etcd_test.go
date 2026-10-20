package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.etcd.io/etcd/v3/clientv3"
	pb "go.etcd.io/etcd/v3/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/v3/mvcc/mvccpb"
)

// emptyKV is an Etcd key-value API that holds no keys.
type emptyKV struct {
	clientv3.KV
}

func (emptyKV) Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return new(clientv3.GetResponse), nil
}

func (emptyKV) Delete(context.Context, string, ...clientv3.OpOption) (*clientv3.DeleteResponse, error) {
	return new(clientv3.DeleteResponse), nil
}

// errUnreachable is the error unreachableKV returns.
var errUnreachable = errors.New("etcdserver: request timed out")

// unreachableKV is an Etcd key-value API whose reads fail as clientv3's do:
// with a nil response and an error.
type unreachableKV struct {
	clientv3.KV
}

func (unreachableKV) Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	return nil, errUnreachable
}

// memKV is an Etcd key-value API that holds its keys in memory, with the
// revision of each key's last change.
type memKV struct {
	clientv3.KV

	mu   sync.Mutex
	rev  int64
	keys map[string]*mvccpb.KeyValue
}

func newMemKV() *memKV {
	return &memKV{keys: make(map[string]*mvccpb.KeyValue)}
}

func (m *memKV) Get(_ context.Context, key string, _ ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	resp := new(clientv3.GetResponse)

	if kv, ok := m.keys[key]; ok {
		copied := *kv
		resp.Kvs = []*mvccpb.KeyValue{&copied}
		resp.Count = 1
	}

	return resp, nil
}

func (m *memKV) Put(_ context.Context, key, val string, _ ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.put(key, val)

	return new(clientv3.PutResponse), nil
}

func (m *memKV) put(key, val string) {
	m.rev++
	m.keys[key] = &mvccpb.KeyValue{Key: []byte(key), Value: []byte(val), ModRevision: m.rev}
}

// Txn returns a transaction that supports what Etcd.Modify sends: a compare
// of one key's last change revision, then puts.
func (m *memKV) Txn(context.Context) clientv3.Txn { //nolint:ireturn // implements clientv3.KV
	return &memTxn{kv: m}
}

type memTxn struct {
	kv   *memKV
	cmps []clientv3.Cmp
	then []clientv3.Op
}

func (t *memTxn) If(cs ...clientv3.Cmp) clientv3.Txn { //nolint:ireturn // implements clientv3.Txn
	t.cmps = append(t.cmps, cs...)

	return t
}

func (t *memTxn) Then(ops ...clientv3.Op) clientv3.Txn { //nolint:ireturn // implements clientv3.Txn
	t.then = append(t.then, ops...)

	return t
}

func (t *memTxn) Else(...clientv3.Op) clientv3.Txn { //nolint:ireturn // implements clientv3.Txn
	return t
}

func (t *memTxn) Commit() (*clientv3.TxnResponse, error) {
	t.kv.mu.Lock()
	defer t.kv.mu.Unlock()

	for _, cmp := range t.cmps {
		want, ok := cmp.TargetUnion.(*pb.Compare_ModRevision)
		if !ok || cmp.Result != pb.Compare_EQUAL {
			return nil, errors.New("memKV supports only mod revision equality compares")
		}

		var rev int64
		if kv, ok := t.kv.keys[string(cmp.Key)]; ok {
			rev = kv.ModRevision
		}

		if rev != want.ModRevision {
			return &clientv3.TxnResponse{Succeeded: false}, nil
		}
	}

	for _, op := range t.then {
		if !op.IsPut() {
			return nil, errors.New("memKV transactions support only puts")
		}

		t.kv.put(string(op.KeyBytes()), string(op.ValueBytes()))
	}

	return &clientv3.TxnResponse{Succeeded: true}, nil
}

func etcdTestConfig(name string) *Config {
	return &Config{Kind: "Experiment", Metadata: ConfigMetadata{Name: name}}
}

// TestEtcdMissingConfigIsErrNotExist is a regression test: the Etcd store
// reported a missing config with an error of its own, so callers that check
// for ErrNotExist, such as the port-forward reaper, took a deleted experiment
// for one the store could not read and asked minimega about it, which
// re-created its namespace. Delete of a missing config also succeeded, where
// BoltDB reports ErrNotExist.
func TestEtcdMissingConfigIsErrNotExist(t *testing.T) {
	e := Etcd{cli: &clientv3.Client{KV: emptyKV{}}}

	c := etcdTestConfig("gone")

	if err := e.Get(c); !errors.Is(err, ErrNotExist) {
		t.Errorf("Get of a missing config = %v, want ErrNotExist", err)
	}

	if err := e.Update(c); !errors.Is(err, ErrNotExist) {
		t.Errorf("Update of a missing config = %v, want ErrNotExist", err)
	}

	if err := e.Delete(c); !errors.Is(err, ErrNotExist) {
		t.Errorf("Delete of a missing config = %v, want ErrNotExist", err)
	}
}

// TestEtcdReadFailureIsAnError is a regression test: Create and Update
// ignored the error of their read and dereferenced its nil response, so one
// Etcd timeout panicked.
func TestEtcdReadFailureIsAnError(t *testing.T) {
	e := Etcd{cli: &clientv3.Client{KV: unreachableKV{}}}

	if err := e.Create(etcdTestConfig("x")); !errors.Is(err, errUnreachable) {
		t.Errorf("Create with a failing read = %v, want the read error", err)
	}

	if err := e.Update(etcdTestConfig("x")); !errors.Is(err, errUnreachable) {
		t.Errorf("Update with a failing read = %v, want the read error", err)
	}
}

// TestEtcdExistingConfigIsErrExist is a regression test: Create reported an
// existing config with an error of its own, so the REST handlers that answer
// ErrExist with "already exists" did not recognize it.
func TestEtcdExistingConfigIsErrExist(t *testing.T) {
	kv := newMemKV()
	kv.put("experiment/x", "{}")

	e := Etcd{cli: &clientv3.Client{KV: kv}}

	if err := e.Create(etcdTestConfig("x")); !errors.Is(err, ErrExist) {
		t.Errorf("Create of an existing config = %v, want ErrExist", err)
	}
}

// TestEtcdCreateKeepsCreated checks that Create, as BoltDB's, keeps the
// created timestamp a config already has, which a rename carries over from
// the old config.
func TestEtcdCreateKeepsCreated(t *testing.T) {
	e := Etcd{cli: &clientv3.Client{KV: newMemKV()}}

	c := etcdTestConfig("renamed")
	c.Metadata.Created = "2020-01-02T03:04:05Z"

	if err := e.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stored := etcdTestConfig("renamed")
	if err := e.Get(stored); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if stored.Metadata.Created != "2020-01-02T03:04:05Z" {
		t.Errorf("created timestamp = %q, want the renamed config's", stored.Metadata.Created)
	}
}

// TestEtcdModifyIsACompareAndSet is a regression test: Modify read the config
// and wrote it back with no check, so a stop that another process stored
// between the read and the write was overwritten and the stopped experiment
// was stored as running again.
func TestEtcdModifyIsACompareAndSet(t *testing.T) {
	kv := newMemKV()
	kv.put("experiment/x", `{"kind":"Experiment","metadata":{"name":"x"},"status":{"startTime":"RUN-1"}}`)

	original := DefaultStore
	t.Cleanup(func() { DefaultStore = original })

	DefaultStore = &Etcd{cli: &clientv3.Client{KV: kv}}

	calls := 0

	// As WriteToStore does, refuse to write when the stored run is not the
	// one read, and stop the experiment in "another process" during the
	// first call.
	err := Modify(etcdTestConfig("x"), func(c *Config) error {
		calls++

		if calls == 1 {
			kv.put("experiment/x", `{"kind":"Experiment","metadata":{"name":"x"},"status":{"startTime":""}}`)
		}

		if c.Status["startTime"] != "RUN-1" {
			return errors.New("run changed")
		}

		c.Status["note"] = "written by RUN-1"

		return nil
	})
	if err == nil || err.Error() != "run changed" {
		t.Errorf("Modify across a concurrent stop = %v, want the change's refusal", err)
	}

	if calls != 2 {
		t.Errorf("change ran %d times, want 2 (the second on the stopped config)", calls)
	}

	stored := etcdTestConfig("x")
	if err := Get(stored); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if start := stored.Status["startTime"]; start != "" {
		t.Errorf("stored start time = %v, want the concurrent stop's empty one", start)
	}
}

// TestEtcdModifyWrites checks that Modify stores what change made of the
// config when nothing else writes it.
func TestEtcdModifyWrites(t *testing.T) {
	kv := newMemKV()
	kv.put("experiment/x", `{"kind":"Experiment","metadata":{"name":"x"},"status":{"count":1}}`)

	original := DefaultStore
	t.Cleanup(func() { DefaultStore = original })

	DefaultStore = &Etcd{cli: &clientv3.Client{KV: kv}}

	err := Modify(etcdTestConfig("x"), func(c *Config) error {
		n, _ := c.Status["count"].(float64)
		c.Status["count"] = n + 1

		return nil
	})
	if err != nil {
		t.Fatalf("Modify: %v", err)
	}

	stored := etcdTestConfig("x")
	if err := Get(stored); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if n := stored.Status["count"]; n != 2.0 {
		t.Errorf("stored count = %v, want 2", n)
	}

	if err := Modify(etcdTestConfig("gone"), func(*Config) error { return nil }); !errors.Is(err, ErrNotExist) {
		t.Errorf("Modify of a missing config = %v, want ErrNotExist", err)
	}
}
