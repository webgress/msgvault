package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AttachmentCopyResult summarizes a blob copy.
type AttachmentCopyResult struct {
	Copied  int64 // files written
	Skipped int64 // files already present with matching size
	Bytes   int64 // bytes written (copied files only)
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
		srcPath := filepath.Join(srcDir, rel)
		dstPath := filepath.Join(dstDir, rel)

		srcInfo, err := os.Stat(srcPath)
		if err != nil {
			if os.IsNotExist(err) {
				// The blob is referenced in the DB but missing on disk. This is
				// a pre-existing source inconsistency, not something the copy can
				// fix; skip it rather than aborting the whole migration.
				continue
			}
			return res, fmt.Errorf("stat source attachment %q: %w", rel, err)
		}

		if dstInfo, err := os.Stat(dstPath); err == nil {
			if dstInfo.Size() == srcInfo.Size() {
				res.Skipped++
				continue
			}
		}

		n, err := copyFile(srcPath, dstPath)
		if err != nil {
			return res, fmt.Errorf("copy attachment %q: %w", rel, err)
		}
		res.Copied++
		res.Bytes += n
	}
	return res, nil
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
// number of bytes written.
func copyFile(srcPath, dstPath string) (int64, error) {
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
	n, err := io.Copy(out, in)
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
