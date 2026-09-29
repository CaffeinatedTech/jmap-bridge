package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/CaffeinatedTech/jmap-bridge/internal/jmapapi"
)

// BlobStore stores immutable blobs (attachments, client uploads and
// contact photos) as files under {data_dir}/blobs/{aa}/{blobId}, with
// SQLite holding only metadata (PLAN §4, §7.3). Bodies never live in
// rows.
type BlobStore struct {
	root string
}

// newBlobStore points a BlobStore at the data directory.
func newBlobStore(dataDir string) *BlobStore {
	return &BlobStore{root: dataDir}
}

// put writes one blob file and its metadata row inside tx, so a rolled
// back hydration never leaves a row pointing at nothing. (A file whose
// tx rolls back is harmless debris; ids are never reused.)
func (b *BlobStore) put(ctx context.Context, tx *sql.Tx, account, id, mediaType string, data []byte) error {
	rel := filepath.Join("blobs", id[:2], id)
	abs := filepath.Join(b.root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("store: blob dir: %w", err)
	}
	if err := os.WriteFile(abs, data, 0o640); err != nil {
		return fmt.Errorf("store: blob write: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO blobs(id, account, path, media_type, size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET media_type = excluded.media_type,
		                               size = excluded.size`,
		id, account, rel, mediaType, len(data), time.Now().Unix()); err != nil {
		return fmt.Errorf("store: blob row: %w", err)
	}
	return nil
}

// ReadBlob returns a blob's bytes and media type. The account must
// match the one that stored it (FR-A.11: ids never cross accounts,
// FR-M.17 rejects foreign ids): an id this account does not hold fails
// with [jmapapi.ErrBlobNotFound], which the download endpoint turns
// into a 404.
func (s *Store) ReadBlob(ctx context.Context, account, id string) ([]byte, string, error) {
	var rel, mediaType string
	err := s.db.QueryRowContext(ctx,
		`SELECT path, COALESCE(media_type, 'application/octet-stream') FROM blobs
		 WHERE id = ? AND account = ?`, id, account).Scan(&rel, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: %s", jmapapi.ErrBlobNotFound, id)
	}
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(filepath.Join(s.data, rel))
	if err != nil {
		return nil, "", fmt.Errorf("store: blob read: %w", err)
	}
	return data, mediaType, nil
}
