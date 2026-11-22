package engine

import (
	"fmt"
	"sync"
)

// DB is a BoltDB-inspired embedded KV store backed by an append-only COW B+Tree.
// It supports multiple concurrent MVCC readers and a single serialized writer.
type DB struct {
	mu            sync.RWMutex
	storage       Storage
	opts          Options
	meta          Meta
	writeTx       *Tx
	activeReaders int
	closed        bool
}

// Open opens or initializes a GroundKV database backed by a file at path.
func Open(path string, opts *Options) (*DB, error) {
	st, err := OpenOSFile(path)
	if err != nil {
		return nil, err
	}
	db, err := OpenStorage(st, opts)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return db, nil
}

// OpenStorage opens or initializes a GroundKV database backed by a custom Storage.
func OpenStorage(st Storage, opts *Options) (*DB, error) {
	cfg, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}

	size, err := st.Size()
	if err != nil {
		return nil, err
	}

	db := &DB{
		storage: st,
		opts:    cfg,
	}

	if size == 0 {
		if err := db.initEmpty(); err != nil {
			return nil, err
		}
		return db, nil
	}

	if err := db.recover(); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *DB) initEmpty() error {
	m := Meta{
		Magic:     metaMagic,
		Version:   metaVersion,
		PageSize:  uint32(db.opts.PageSize),
		Root:      0,
		HighWater: 2,
		TxID:      0,
	}
	for slot := PageID(0); slot < 2; slot++ {
		buf := encodeMetaPage(db.opts.PageSize, slot, m)
		if _, err := db.storage.WriteAt(buf, int64(slot)*int64(db.opts.PageSize)); err != nil {
			return err
		}
	}
	if !db.opts.NoSync {
		if err := db.storage.Sync(); err != nil {
			return err
		}
	}
	db.meta = m
	return nil
}

func (db *DB) recover() error {
	// Auto-detect on-disk PageSize so reopening with nil Options works for any
	// configured PageSize (even if meta page 0 was torn).
	candidateSizes := []int{db.opts.PageSize}
	for ps := MinPageSize; ps <= 65536; ps <<= 1 {
		if ps != db.opts.PageSize {
			candidateSizes = append(candidateSizes, ps)
		}
	}

	var lastErr0, lastErr1 error
	for _, ps := range candidateSizes {
		meta0, err0 := db.readMetaSlotWithSize(0, ps)
		meta1, err1 := db.readMetaSlotWithSize(1, ps)

		if err0 == nil && int(meta0.PageSize) != ps {
			err0 = fmt.Errorf("%w: meta0 pageSize %d != %d", ErrCorruptedMeta, meta0.PageSize, ps)
		}
		if err1 == nil && int(meta1.PageSize) != ps {
			err1 = fmt.Errorf("%w: meta1 pageSize %d != %d", ErrCorruptedMeta, meta1.PageSize, ps)
		}

		switch {
		case err0 == nil && err1 == nil:
			if meta0.TxID >= meta1.TxID {
				db.meta = meta0
			} else {
				db.meta = meta1
			}
			db.opts.PageSize = ps
			return nil
		case err0 == nil:
			db.meta = meta0
			db.opts.PageSize = ps
			return nil
		case err1 == nil:
			db.meta = meta1
			db.opts.PageSize = ps
			return nil
		default:
			if ps == db.opts.PageSize {
				lastErr0, lastErr1 = err0, err1
			}
		}
	}

	return fmt.Errorf("%w: meta0=(%v), meta1=(%v)", ErrCorruptedMeta, lastErr0, lastErr1)
}

func (db *DB) readMetaSlot(slot PageID) (Meta, error) {
	return db.readMetaSlotWithSize(slot, db.opts.PageSize)
}

func (db *DB) readMetaSlotWithSize(slot PageID, pageSize int) (Meta, error) {
	buf := make([]byte, pageSize)
	n, err := db.storage.ReadAt(buf, int64(slot)*int64(pageSize))
	if err != nil && n < pageSize {
		return Meta{}, err
	}
	return decodeMetaPage(buf, slot)
}

func (db *DB) readCommittedPage(pgid PageID) ([]byte, pageHeader, error) {
	buf := make([]byte, db.opts.PageSize)
	off := int64(pgid) * int64(db.opts.PageSize)
	n, err := db.storage.ReadAt(buf, off)
	if err != nil && n < db.opts.PageSize {
		return nil, pageHeader{}, err
	}
	h, err := readAndVerifyPageHeader(buf, pgid)
	return buf, h, err
}

// BeginRead starts a read-only MVCC snapshot transaction pinned to the latest committed TxID.
func (db *DB) BeginRead() (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrDBClosed
	}
	db.activeReaders++
	return &Tx{
		db:       db,
		writable: false,
		meta:     db.meta,
	}, nil
}

// BeginWrite starts a single-writer copy-on-write transaction.
// Returns ErrWriteTxActive if another write transaction is currently open.
func (db *DB) BeginWrite() (*Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrDBClosed
	}
	if db.writeTx != nil {
		return nil, ErrWriteTxActive
	}
	tx := &Tx{
		db:       db,
		writable: true,
		meta:     db.meta,
		dirty:    make(map[PageID][]byte),
	}
	db.writeTx = tx
	return tx, nil
}

// View executes fn inside a read-only MVCC snapshot transaction.
func (db *DB) View(fn func(tx *Tx) error) error {
	tx, err := db.BeginRead()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Update executes fn inside a read-write transaction, committing if fn returns nil
// and rolling back if fn returns an error.
func (db *DB) Update(fn func(tx *Tx) error) error {
	tx, err := db.BeginWrite()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Get returns the latest committed value for key, or ErrNotFound.
func (db *DB) Get(key []byte) ([]byte, error) {
	var val []byte
	err := db.View(func(tx *Tx) error {
		var getErr error
		val, getErr = tx.Get(key)
		return getErr
	})
	return val, err
}

// Put atomically writes key -> value in a single-key write transaction.
func (db *DB) Put(key, value []byte) error {
	return db.Update(func(tx *Tx) error {
		return tx.Put(key, value)
	})
}

// Delete atomically deletes key in a single-key write transaction.
func (db *DB) Delete(key []byte) error {
	return db.Update(func(tx *Tx) error {
		return tx.Delete(key)
	})
}

// Meta returns a copy of the currently active committed Meta.
func (db *DB) Meta() Meta {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.meta
}

// MetaSlot describes one of the two on-disk meta pages, including whether its
// CRC32 checksum and header are currently valid (false after a torn meta write).
type MetaSlot struct {
	Meta  Meta
	Valid bool
}

// InspectMeta reads both on-disk meta pages (Page 0 and Page 1) without failing
// if one slot is torn/corrupted. Used by crash-recovery tests and FizzBee MBT.
func (db *DB) InspectMeta() (slot0 MetaSlot, slot1 MetaSlot, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return MetaSlot{}, MetaSlot{}, ErrDBClosed
	}
	if m0, err0 := db.readMetaSlot(0); err0 == nil {
		slot0 = MetaSlot{Meta: m0, Valid: true}
	}
	if m1, err1 := db.readMetaSlot(1); err1 == nil {
		slot1 = MetaSlot{Meta: m1, Valid: true}
	}
	return slot0, slot1, nil
}

// DualMeta reads both on-disk meta pages (Page 0 and Page 1) for verification
// and FizzBee MBT state inspection.
func (db *DB) DualMeta() (meta0 Meta, meta1 Meta, err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return Meta{}, Meta{}, ErrDBClosed
	}
	meta0, err = db.readMetaSlot(0)
	if err != nil {
		return Meta{}, Meta{}, err
	}
	meta1, err = db.readMetaSlot(1)
	if err != nil {
		return Meta{}, Meta{}, err
	}
	return meta0, meta1, nil
}

// ReadTreeAtRoot returns all key-value pairs reachable from a specific committed
// root page ID. Used by diagnostics and FizzBee MBT state verification.
func (db *DB) ReadTreeAtRoot(root PageID) (map[string]string, error) {
	db.mu.RLock()
	if db.closed {
		db.mu.RUnlock()
		return nil, ErrDBClosed
	}
	tx := &Tx{
		db:       db,
		writable: false,
		meta:     Meta{Root: root, HighWater: db.meta.HighWater, TxID: db.meta.TxID},
	}
	db.mu.RUnlock()
	return tx.All()
}

// RawPage reads the raw bytes of a committed page from storage.
// Used by COW immutability tests and FizzBee MBT adapters.
func (db *DB) RawPage(pgid PageID) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, ErrDBClosed
	}
	buf, _, err := db.readCommittedPage(pgid)
	return buf, err
}

// Validate checks the structural integrity of the active committed B+Tree.
func (db *DB) Validate() error {
	return db.View(func(tx *Tx) error {
		return tx.Validate()
	})
}

// Close closes the database and its underlying storage.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	if db.writeTx != nil {
		db.writeTx.closed = true
		db.writeTx.dirty = nil
		db.writeTx = nil
	}
	return db.storage.Close()
}
