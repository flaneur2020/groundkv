package engine

import "errors"

const (
	DefaultPageSize = 4096
	MinPageSize     = 256
)

var (
	ErrNotFound       = errors.New("groundkv: key not found")
	ErrTxClosed       = errors.New("groundkv: transaction closed")
	ErrTxReadOnly     = errors.New("groundkv: transaction is read-only")
	ErrDBClosed       = errors.New("groundkv: database closed")
	ErrEmptyKey       = errors.New("groundkv: key must not be empty")
	ErrKeyTooLarge    = errors.New("groundkv: key exceeds page capacity")
	ErrValueTooLarge  = errors.New("groundkv: value exceeds page capacity")
	ErrCorruptedMeta  = errors.New("groundkv: both meta pages are corrupted")
	ErrCorruptedPage  = errors.New("groundkv: page checksum or format mismatch")
	ErrWriteTxActive  = errors.New("groundkv: write transaction already active")
	ErrInvalidOptions = errors.New("groundkv: invalid options")
)

// Options configures a GroundKV database instance.
type Options struct {
	// PageSize is the fixed byte size of every page on disk.
	// Must be a power of two >= 256. Defaults to DefaultPageSize (4096).
	PageSize int

	// MaxLeafKeys optionally caps the number of key/value entries per leaf page
	// before splitting. Zero means split only when the page byte capacity is
	// exceeded. Setting this to a small number (e.g. 2) in tests forces deep
	// B+Tree splits even with tiny FizzBee MBT key domains.
	MaxLeafKeys int

	// MaxBranchKeys optionally caps the number of child pointers per branch page
	// before splitting. Zero means split only when byte capacity is exceeded.
	MaxBranchKeys int

	// MaxFreelistIDs optionally caps how many freed PageIDs are stored per
	// PageFlagFreelist page before chaining to a next freelist page. Zero uses
	// full page capacity. Setting a small cap (e.g. 3) in tests/MBT forces
	// multi-page linked freelist chains even on small key domains.
	MaxFreelistIDs int

	// NoSync disables fsync calls on commit. Leave false for crash durability.
	NoSync bool

	// NoFreelist disables page reclamation, keeping the database strictly
	// append-only so every historical root remains readable without pinning an
	// MVCC reader. Leave false (default) to reclaim freed pages like BoltDB.
	NoFreelist bool
}

func (o *Options) withDefaults() (Options, error) {
	var cfg Options
	if o != nil {
		cfg = *o
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = DefaultPageSize
	}
	if cfg.PageSize < MinPageSize || (cfg.PageSize&(cfg.PageSize-1)) != 0 {
		return Options{}, ErrInvalidOptions
	}
	if cfg.MaxLeafKeys < 0 || cfg.MaxBranchKeys < 0 || cfg.MaxFreelistIDs < 0 {
		return Options{}, ErrInvalidOptions
	}
	if cfg.MaxLeafKeys == 1 || cfg.MaxBranchKeys == 1 {
		// Splitting requires at least 2 entries to split cleanly.
		return Options{}, ErrInvalidOptions
	}
	if cfg.MaxFreelistIDs > 0 && cfg.MaxFreelistIDs < 3 {
		// A dual-meta COW freelist holds up to 2 pending chain generations
		// simultaneously (pending[T] + current tx.freed), so convergence of
		// chain length C requires perPage > 2 (spectral radius 2/perPage < 1).
		return Options{}, ErrInvalidOptions
	}
	return cfg, nil
}
