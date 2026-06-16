package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxRejectedSamples caps the number of rejected/missing relative paths kept
// for the command summary so a pathological source can't balloon the result.
const maxRejectedSamples = 20

// AttachmentCopyResult summarizes a blob copy.
type AttachmentCopyResult struct {
	Copied   int64 // files written
	Skipped  int64 // files already present with matching content
	Bytes    int64 // bytes written (copied files only)
	Missing  int64 // referenced blobs absent from the source dir
	Rejected int64 // relative paths rejected as unsafe (traversal/absolute)

	// MissingSample / RejectedSample carry up to maxRejectedSamples example
	// relative paths so the command summary can surface concrete offenders
	// without printing an unbounded list.
	MissingSample  []string
	RejectedSample []string
}

// noteMissing records a missing-blob occurrence, keeping a capped sample.
func (r *AttachmentCopyResult) noteMissing(rel string) {
	r.Missing++
	if len(r.MissingSample) < maxRejectedSamples {
		r.MissingSample = append(r.MissingSample, rel)
	}
}

// noteRejected records an unsafe-path occurrence, keeping a capped sample.
func (r *AttachmentCopyResult) noteRejected(rel string) {
	r.Rejected++
	if len(r.RejectedSample) < maxRejectedSamples {
		r.RejectedSample = append(r.RejectedSample, rel)
	}
}

// CopyAttachments copies on-disk attachment blobs referenced by src.attachments
// from srcDir to dstDir, preserving the content-addressed relative path layout.
// Both storage_path and thumbnail_path are copied. Parent directories are
// created as needed. A destination file that already exists with a matching size
// is skipped (idempotent — supports re-runs).
//
// Callers MUST resolve srcDir/dstDir before calling; passing an empty string for
// either is a hard error so blobs are never silently dropped.
func CopyAttachments(ctx context.Context, src *Store, srcDir, dstDir string) (*AttachmentCopyResult, error) {
	if srcDir == "" {
		return nil, errors.New("source attachments directory not resolved (pass --from-home or use a config vault, or --no-attachments)")
	}
	if dstDir == "" {
		return nil, errors.New("destination attachments directory not resolved (pass --to-home or use a config vault, or --no-attachments)")
	}

	paths, err := distinctAttachmentPaths(ctx, src)
	if err != nil {
		return nil, err
	}

	res := &AttachmentCopyResult{}
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		// Defense-in-depth: rel comes from the DB. Reject anything that could
		// escape srcDir/dstDir even though today's paths are content-addressed
		// (<hash[:2]>/<hash>) and never absolute or traversing.
		if !safeRelPath(rel, srcDir, dstDir) {
			res.noteRejected(rel)
			continue
		}

		srcPath := filepath.Join(srcDir, rel)
		dstPath := filepath.Join(dstDir, rel)

		srcInfo, err := os.Stat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				// The blob is referenced in the DB but missing on disk. This is
				// a pre-existing source inconsistency, not something the copy can
				// fix; count it and continue rather than aborting the migration.
				res.noteMissing(rel)
				continue
			}
			return res, fmt.Errorf("stat source attachment %q: %w", rel, err)
		}

		if dstInfo, err := os.Stat(dstPath); err == nil {
			// Size is only a fast pre-filter; on a match, confirm the dest
			// content actually equals the source before skipping (a same-size
			// but different file must NOT be silently accepted).
			if dstInfo.Size() == srcInfo.Size() {
				match, err := sameFileContent(srcPath, dstPath)
				if err != nil {
					return res, fmt.Errorf("verify attachment %q: %w", rel, err)
				}
				if match {
					res.Skipped++
					continue
				}
				// Size matched but content diverged — re-copy from source.
			}
		}

		n, err := copyFile(ctx, srcPath, dstPath)
		if err != nil {
			return res, fmt.Errorf("copy attachment %q: %w", rel, err)
		}
		res.Copied++
		res.Bytes += n
	}
	return res, nil
}

// safeRelPath reports whether rel is a safe relative path that, when joined
// onto srcDir and dstDir, stays inside both directories. It rejects absolute
// paths and any path that cleans to ".." or escapes via "../".
func safeRelPath(rel, srcDir, dstDir string) bool {
	if filepath.IsAbs(rel) {
		return false
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return withinDir(srcDir, rel) && withinDir(dstDir, rel)
}

// withinDir reports whether filepath.Join(dir, rel) resolves to a path inside
// dir (or dir itself's subtree), guarding against traversal.
func withinDir(dir, rel string) bool {
	base := filepath.Clean(dir) + string(filepath.Separator)
	joined := filepath.Clean(filepath.Join(dir, rel))
	return strings.HasPrefix(joined+string(filepath.Separator), base)
}

// sameFileContent reports whether the two files have identical content by
// comparing their SHA-256 digests. Used to confirm a same-size destination blob
// truly matches the source before skipping the copy.
func sameFileContent(aPath, bPath string) (bool, error) {
	aHash, err := fileSHA256(aPath)
	if err != nil {
		return false, err
	}
	bHash, err := fileSHA256(bPath)
	if err != nil {
		return false, err
	}
	return aHash == bHash, nil
}

// fileSHA256 returns the lowercase hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// distinctAttachmentPaths returns every non-empty storage_path and
// thumbnail_path referenced by the attachments table, de-duplicated.
func distinctAttachmentPaths(ctx context.Context, src *Store) ([]string, error) {
	rows, err := src.DB().QueryContext(ctx,
		"SELECT storage_path, thumbnail_path FROM attachments")
	if err != nil {
		return nil, fmt.Errorf("read attachment paths: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for rows.Next() {
		var sp, tp any
		if err := rows.Scan(&sp, &tp); err != nil {
			return nil, fmt.Errorf("scan attachment paths: %w", err)
		}
		add(asString(sp))
		add(asString(tp))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachment paths: %w", err)
	}
	return out, nil
}

// asString coerces a scanned value (string, []byte, or nil) to a string.
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

// copyFile copies srcPath to dstPath, creating parent directories. Returns the
// number of bytes written. The copy is cancellable: it checks ctx between
// chunks so a long copy aborts promptly on context cancellation.
func copyFile(ctx context.Context, srcPath, dstPath string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0700); err != nil {
		return 0, fmt.Errorf("create parent dir: %w", err)
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	tmp := dstPath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, &ctxReader{ctx: ctx, r: in})
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// ctxReader wraps an io.Reader so each Read first checks for context
// cancellation, making an otherwise opaque io.Copy promptly cancellable.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
