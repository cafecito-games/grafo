package kvbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/pebble/v2"
	bolt "go.etcd.io/bbolt"
)

// Engine identifies a benchmark-only embedded key/value implementation.
type Engine string

const (
	EngineBolt   Engine = "bbolt"
	EnginePebble Engine = "pebble"
)

var ErrNotFound = errors.New("kvbench: not found")

type transaction interface {
	get([]byte) ([]byte, error)
	set([]byte, []byte) error
	delete([]byte) error
	scan([]byte, func([]byte, []byte) error) error
}

type store interface {
	view(context.Context, func(transaction) error) error
	update(context.Context, func(transaction) error) error
	close() error
}

func openStore(engine Engine, path string) (store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	switch engine {
	case EngineBolt:
		db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
		if err != nil {
			return nil, err
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte("graph"))
			return err
		}); err != nil {
			_ = db.Close()
			return nil, err
		}
		return &boltStore{db: db}, nil
	case EnginePebble:
		db, err := pebble.Open(path, &pebble.Options{})
		if err != nil {
			return nil, err
		}
		return &pebbleStore{db: db}, nil
	default:
		return nil, fmt.Errorf("unknown embedded engine %q", engine)
	}
}

type boltStore struct{ db *bolt.DB }

func (s *boltStore) close() error { return s.db.Close() }

func (s *boltStore) view(ctx context.Context, fn func(transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(func(tx *bolt.Tx) error {
		return fn(boltTransaction{bucket: tx.Bucket([]byte("graph"))})
	})
}

func (s *boltStore) update(ctx context.Context, fn func(transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := fn(boltTransaction{bucket: tx.Bucket([]byte("graph"))}); err != nil {
			return err
		}
		return ctx.Err()
	})
}

type boltTransaction struct{ bucket *bolt.Bucket }

func (t boltTransaction) get(key []byte) ([]byte, error) {
	value := t.bucket.Get(key)
	if value == nil {
		return nil, ErrNotFound
	}
	return bytes.Clone(value), nil
}

func (t boltTransaction) set(key, value []byte) error { return t.bucket.Put(key, value) }
func (t boltTransaction) delete(key []byte) error     { return t.bucket.Delete(key) }

func (t boltTransaction) scan(prefix []byte, fn func([]byte, []byte) error) error {
	cursor := t.bucket.Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		if err := fn(bytes.Clone(key), bytes.Clone(value)); err != nil {
			return err
		}
	}
	return nil
}

type pebbleStore struct{ db *pebble.DB }

func (s *pebbleStore) close() error { return s.db.Close() }

func (s *pebbleStore) view(ctx context.Context, fn func(transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := s.db.NewSnapshot()
	defer func() { _ = snapshot.Close() }()
	return fn(pebbleReadable{reader: snapshot})
}

func (s *pebbleStore) update(ctx context.Context, fn func(transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	batch := s.db.NewIndexedBatch()
	defer func() { _ = batch.Close() }()
	if err := fn(pebbleReadable{reader: batch, writer: batch}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return batch.Commit(pebble.Sync)
}

type pebbleReader interface {
	Get([]byte) ([]byte, io.Closer, error)
	NewIter(*pebble.IterOptions) (*pebble.Iterator, error)
}

type pebbleWriter interface {
	Set([]byte, []byte, *pebble.WriteOptions) error
	Delete([]byte, *pebble.WriteOptions) error
}

type pebbleReadable struct {
	reader pebbleReader
	writer pebbleWriter
}

func (t pebbleReadable) get(key []byte) ([]byte, error) {
	value, closer, err := t.reader.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = closer.Close() }()
	return bytes.Clone(value), nil
}

func (t pebbleReadable) set(key, value []byte) error {
	if t.writer == nil {
		return errors.New("read-only transaction")
	}
	return t.writer.Set(key, value, nil)
}

func (t pebbleReadable) delete(key []byte) error {
	if t.writer == nil {
		return errors.New("read-only transaction")
	}
	return t.writer.Delete(key, nil)
}

func (t pebbleReadable) scan(prefix []byte, fn func([]byte, []byte) error) error {
	upper := append(bytes.Clone(prefix), 0xff)
	iterator, err := t.reader.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return err
	}
	defer func() { _ = iterator.Close() }()
	for iterator.First(); iterator.Valid(); iterator.Next() {
		if err := fn(bytes.Clone(iterator.Key()), bytes.Clone(iterator.Value())); err != nil {
			return err
		}
	}
	return iterator.Error()
}
