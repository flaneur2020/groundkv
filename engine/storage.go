package engine

import (
	"errors"
	"os"
	"path/filepath"
)

// Storage abstracts the underlying page-addressable file store so GroundKV can
// run on real OS files in production and on deterministic, crash-injectable
// storage in unit tests, DST, and FizzBee MBT harnesses.
type Storage interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Size() (int64, error)
	Truncate(size int64) error
	Close() error
}

// OSFileStorage backs Storage with a real *os.File.
type OSFileStorage struct {
	f *os.File
}

// OpenOSFile opens or creates a database file at path and acquires an exclusive
// non-blocking advisory lock (flock) to prevent multi-process corruption.
// When creating a new file, it also fsyncs the parent directory so the directory
// entry itself survives power loss on POSIX filesystems.
func OpenOSFile(path string) (*OSFileStorage, error) {
	_, statErr := os.Stat(path)
	isNew := errors.Is(statErr, os.ErrNotExist)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if isNew {
		if err := syncParentDir(path); err != nil {
			_ = unlockFile(f)
			_ = f.Close()
			return nil, err
		}
	}
	return &OSFileStorage{f: f}, nil
}

func syncParentDir(path string) error {
	dirPath := filepath.Dir(path)
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (s *OSFileStorage) ReadAt(p []byte, off int64) (int, error)  { return s.f.ReadAt(p, off) }
func (s *OSFileStorage) WriteAt(p []byte, off int64) (int, error) { return s.f.WriteAt(p, off) }
func (s *OSFileStorage) Sync() error                              { return s.f.Sync() }
func (s *OSFileStorage) Truncate(size int64) error                { return s.f.Truncate(size) }

func (s *OSFileStorage) Close() error {
	_ = unlockFile(s.f)
	return s.f.Close()
}

func (s *OSFileStorage) Size() (int64, error) {
	fi, err := s.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
