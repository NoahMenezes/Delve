package db

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
)

// StoredEmbedding pairs a file's metadata with its decoded meaning
// vector — the unit of work for the brute-force semantic scan.
type StoredEmbedding struct {
	Record FileRecord
	Vector []float32
}

// UpsertEmbedding stores a file's embedding vector and stamps
// embedded_at, looked up by path (the codebase's universal key —
// callers never handle raw row ids). Both writes happen in one
// transaction so a crash can't leave a vector without its timestamp
// or vice versa. Re-embedding simply replaces the row.
func UpsertEmbedding(database *sql.DB, path string, vector []float32, embeddedAt int64) error {
	blob, err := encodeVector(vector)
	if err != nil {
		return err
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	// Rollback on any failure; committed explicitly below. The
	// named-return dance isn't needed — inline handling is clearer.
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO embeddings (file_id, dim, vector)
		SELECT id, ?, ? FROM files WHERE path = ?;`,
		len(vector), blob, path,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	res, err := tx.Exec(`UPDATE files SET embedded_at = ? WHERE path = ?;`, embeddedAt, path)
	if err != nil {
		tx.Rollback()
		return err
	}
	// The INSERT..SELECT above affects 0 rows when path is unknown,
	// so the UPDATE is the reliable existence check: 0 rows means
	// the path isn't indexed — fail loudly instead of committing a
	// silent no-op.
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return err
	}
	if n == 0 {
		tx.Rollback()
		return fmt.Errorf("no such file in index: %s", path)
	}
	return tx.Commit()
}

// GetAllEmbeddings loads every stored vector with its file metadata
// in one JOIN — the brute-force semantic scan reads this whole set.
// Vectors decode (and dim-check) here so bad rows fail loudly at load,
// never as silent mis-scores mid-search.
// Column list + scan order come from fileColumns/scanFileRecord
// (files.go) — one place to update when FileRecord gains a field.
func GetAllEmbeddings(database *sql.DB) ([]StoredEmbedding, error) {
	rows, err := database.Query(
		"SELECT\n\t\t" + fileColumns + ",\n\t\tembeddings.dim, embeddings.vector\n" +
			"\tFROM embeddings\n\tJOIN files ON files.id = embeddings.file_id;",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredEmbedding
	for rows.Next() {
		var se StoredEmbedding
		var dim int
		var blob []byte
		// Single Scan consumes all 13 columns: the 11 file columns
		// via the shared helper plus trailing dim+vector.
		if err := scanFileRecord(rows, &se.Record, &dim, &blob); err != nil {
			return nil, err
		}
		vec, err := decodeVector(blob, dim)
		if err != nil {
			return nil, fmt.Errorf("corrupt embedding for %s: %w", se.Record.Path, err)
		}
		se.Vector = vec
		out = append(out, se)
	}
	return out, rows.Err()
}

// EmbeddingStatus reports whether path already has an embedding newer
// than (or equal to) the given content timestamp — the scanner's
// freshness check for skipping unchanged files. ok=false means "embed
// it"; a nil embeddedAt also means "embed it".
func EmbeddingStatus(database *sql.DB, path string, contentTs int64) (ok bool, err error) {
	var embeddedAt sql.NullInt64
	err = database.QueryRow(`SELECT embedded_at FROM files WHERE path = ?;`, path).Scan(&embeddedAt)
	if err != nil {
		return false, err
	}
	return embeddedAt.Valid && embeddedAt.Int64 >= contentTs, nil
}

// encodeVector serializes a []float32 to little-endian bytes.
// encoding/binary (not unsafe casts) keeps this portable across
// architectures — endianness is explicit, not assumed.
func encodeVector(vec []float32) ([]byte, error) {
	if len(vec) == 0 {
		return nil, fmt.Errorf("cannot store empty embedding vector")
	}
	blob := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(v))
	}
	return blob, nil
}

// decodeVector is encodeVector in reverse, with the dim cross-check
// that catches model swaps (a 768-dim row read as 384 would otherwise
// silently score garbage).
func decodeVector(blob []byte, dim int) ([]float32, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("invalid stored dimension %d", dim)
	}
	if len(blob) != 4*dim {
		return nil, fmt.Errorf("blob is %d bytes, want %d for dim %d", len(blob), 4*dim, dim)
	}
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec, nil
}
