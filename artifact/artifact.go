// Package artifact is the artifact-store seam (FACTORY.md §4, §8). Binary
// outputs are referenced by artifact-ref records and never stored in varvig;
// Factory owns registry credentials and varvig must never acquire them.
//
// The seam's shape encodes the §8 retention rule structurally rather than by
// convention: Put is always cell-local, and Replicate is the only method that
// can send bytes anywhere. A caller cannot accidentally replicate on attempt,
// because the attempt path has no method that would.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/varvig/varvig-factory/cell"
)

// Store is the artifact-store seam.
type Store interface {
	Name() string
	// Fragment reports this adapter's slice of the environment. For most stores
	// that is nothing — a store does not change what a test means. It exists so
	// a store that *is* the sandbox's image source can contribute the container
	// digest from one place.
	Fragment(ctx context.Context) (cell.Fragment, error)

	// Put records a local file as an artifact and returns its ref. It is
	// cell-local by contract: it must not transfer bytes off the cell.
	// Speculative artifacts stay local — a federation that replicated every
	// attempt's build output has turned speculation into a bandwidth bill (§8).
	Put(ctx context.Context, path string) (cell.ArtifactRef, error)

	// Replicate transfers an already-Put artifact to shared storage and returns
	// the ref with the new locator added. Called on promotion, never on
	// attempt.
	Replicate(ctx context.Context, ref cell.ArtifactRef) (cell.ArtifactRef, error)

	// Open reads an artifact's bytes back, for a verifier that needs them.
	Open(ctx context.Context, ref cell.ArtifactRef) (io.ReadCloser, error)

	// Release deletes the cell-local copy. It is how storage pressure is
	// relieved (§7) and it is Factory's action, never varvig's: varvig reports
	// unreachable artifact hashes and holds no credentials anywhere.
	Release(ctx context.Context, ref cell.ArtifactRef) error

	// UsedBytes is the cell-local footprint, for the storage budget.
	UsedBytes(ctx context.Context) (int64, error)
}

// ErrNotFound is returned when an artifact is not held locally.
var ErrNotFound = errors.New("artifact: not held by this cell")

// LocalCAS is a content-addressed directory on the cell's own disk. It is the
// base store: every other store composes with it, because "keep the bytes
// locally until promotion" is the §8 rule and there is no reason to implement it
// twice.
type LocalCAS struct {
	// Root is the directory holding the objects.
	Root string
}

// Name implements Store.
func (l *LocalCAS) Name() string { return "local-cas" }

// Fragment implements Store. A local CAS does not change what a check means, so
// it contributes nothing.
func (l *LocalCAS) Fragment(context.Context) (cell.Fragment, error) { return cell.Fragment{}, nil }

// path is where a content hash lives on disk. The hash is fanned out one byte
// deep, the usual arrangement, so a cell with a hundred thousand artifacts does
// not have a hundred thousand entries in one directory.
func (l *LocalCAS) path(contentHash string) (string, error) {
	hex, ok := strings.CutPrefix(contentHash, cell.HashAlgorithm+":")
	if !ok || len(hex) < 4 {
		return "", fmt.Errorf("artifact: unsupported content hash %q", contentHash)
	}
	for _, c := range hex {
		// The hash is interpolated into a filesystem path, so it is validated
		// rather than trusted: a ref that arrived from a peer is untrusted
		// input, and "../.." in a hash would be a path traversal.
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", fmt.Errorf("artifact: malformed content hash %q", contentHash)
		}
	}
	return filepath.Join(l.Root, hex[:2], hex), nil
}

// Put implements Store: hash the file, store it under its hash, return the ref.
// The local copy's locator is a file:// URI, which is honest — it tells another
// cell that reads the record that these bytes are not fetchable from here.
func (l *LocalCAS) Put(_ context.Context, src string) (cell.ArtifactRef, error) {
	f, err := os.Open(src)
	if err != nil {
		return cell.ArtifactRef{}, err
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return cell.ArtifactRef{}, err
	}
	contentHash := cell.HashAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))

	dst, err := l.path(contentHash)
	if err != nil {
		return cell.ArtifactRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return cell.ArtifactRef{}, err
	}
	// Already held: content addressing makes a re-Put a no-op rather than a
	// rewrite, so two attempts producing byte-identical outputs cost one copy.
	if _, err := os.Stat(dst); err == nil {
		return l.ref(contentHash, size, dst), nil
	}
	if err := copyFile(src, dst); err != nil {
		return cell.ArtifactRef{}, err
	}
	return l.ref(contentHash, size, dst), nil
}

func (l *LocalCAS) ref(contentHash string, size int64, dst string) cell.ArtifactRef {
	abs, err := filepath.Abs(dst)
	if err != nil {
		abs = dst
	}
	r := cell.ArtifactRef{
		ContentHash: contentHash,
		Size:        size,
		Locators:    []string{"file://" + abs},
	}
	r.Normalize()
	return r
}

// Replicate implements Store. A local CAS has nowhere to replicate to, so it
// returns the ref unchanged. That is not a stub: a single-cell deployment with
// no registry is a legitimate configuration, and promotion there means the bytes
// stay where they are.
func (l *LocalCAS) Replicate(_ context.Context, ref cell.ArtifactRef) (cell.ArtifactRef, error) {
	return ref, nil
}

// Open implements Store.
func (l *LocalCAS) Open(_ context.Context, ref cell.ArtifactRef) (io.ReadCloser, error) {
	p, err := l.path(ref.ContentHash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref.ContentHash)
	}
	return f, err
}

// Release implements Store.
func (l *LocalCAS) Release(_ context.Context, ref cell.ArtifactRef) error {
	p, err := l.path(ref.ContentHash)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// UsedBytes implements Store.
func (l *LocalCAS) UsedBytes(context.Context) (int64, error) {
	var total int64
	err := filepath.WalkDir(l.Root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// List returns every artifact the cell holds locally, largest first — the order
// storage-pressure relief wants, since releasing the biggest artifact frees the
// most disk for the fewest retention obligations dropped (§7).
func (l *LocalCAS) List() ([]cell.ArtifactRef, error) {
	var out []cell.ArtifactRef
	err := filepath.WalkDir(l.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, cell.ArtifactRef{
			ContentHash: cell.HashAlgorithm + ":" + filepath.Base(path),
			Size:        info.Size(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].ContentHash < out[j].ContentHash
	})
	return out, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Write to a temporary name and rename, so a crash mid-copy cannot leave a
	// truncated file sitting at the name of its own content hash — the one
	// corruption a content-addressed store cannot detect by looking.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Remote is a store that keeps bytes locally and replicates them on promotion by
// running a command. It covers the OCI-registry and S3-compatible rows of the §4
// table: oras, skopeo, crane, aws s3, rclone, or a site-specific script all plug
// in as argv.
//
// A command rather than an SDK, for two reasons. This module has no third-party
// dependencies. And the credentials stay in the tool the operator already
// configured — Factory does not learn a registry's auth model, it just holds the
// right to invoke something that has.
type Remote struct {
	// Local is the cell-local store. Put and Open go here.
	Local *LocalCAS
	// PushCommand is argv run to replicate. The tokens "{{path}}" (the local
	// file), "{{hash}}" (the content hash) and "{{hex}}" (the hash without its
	// label) are substituted.
	PushCommand []string
	// LocatorTemplate renders the locator recorded after a successful push,
	// with the same substitutions, e.g.
	// "oci://registry.internal/varvig/app@{{hash}}".
	LocatorTemplate string
	// Label names this adapter in logs, e.g. "oci" or "s3".
	Label string
}

// Name implements Store.
func (r *Remote) Name() string {
	if r.Label != "" {
		return r.Label
	}
	return "remote"
}

// Fragment implements Store.
func (r *Remote) Fragment(context.Context) (cell.Fragment, error) { return cell.Fragment{}, nil }

// Put implements Store by storing locally only. This is the §8 rule in the type
// system: there is no path from an attempt to a network transfer.
func (r *Remote) Put(ctx context.Context, path string) (cell.ArtifactRef, error) {
	return r.Local.Put(ctx, path)
}

// Replicate implements Store by running PushCommand and recording the resulting
// locator alongside the local one.
func (r *Remote) Replicate(ctx context.Context, ref cell.ArtifactRef) (cell.ArtifactRef, error) {
	if len(r.PushCommand) == 0 {
		return ref, fmt.Errorf("artifact: %s has no push command configured", r.Name())
	}
	local, err := r.Local.path(ref.ContentHash)
	if err != nil {
		return ref, err
	}
	if _, err := os.Stat(local); err != nil {
		return ref, fmt.Errorf("%w: %s", ErrNotFound, ref.ContentHash)
	}
	hexOnly := strings.TrimPrefix(ref.ContentHash, cell.HashAlgorithm+":")
	subst := func(s string) string {
		s = strings.ReplaceAll(s, "{{path}}", local)
		s = strings.ReplaceAll(s, "{{hash}}", ref.ContentHash)
		s = strings.ReplaceAll(s, "{{hex}}", hexOnly)
		return s
	}
	argv := make([]string, 0, len(r.PushCommand))
	for _, a := range r.PushCommand {
		argv = append(argv, subst(a))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ref, fmt.Errorf("artifact: %s replicate: %w: %s", r.Name(), err, strings.TrimSpace(string(out)))
	}
	if r.LocatorTemplate != "" {
		ref.Locators = append(ref.Locators, subst(r.LocatorTemplate))
		ref.Normalize()
	}
	return ref, nil
}

// Open implements Store.
func (r *Remote) Open(ctx context.Context, ref cell.ArtifactRef) (io.ReadCloser, error) {
	return r.Local.Open(ctx, ref)
}

// Release implements Store. It releases the *local* copy only: a replicated
// artifact's registry bytes are the operator's to delete, informed by
// `varvig gc --report-external` (§8).
func (r *Remote) Release(ctx context.Context, ref cell.ArtifactRef) error {
	return r.Local.Release(ctx, ref)
}

// UsedBytes implements Store.
func (r *Remote) UsedBytes(ctx context.Context) (int64, error) { return r.Local.UsedBytes(ctx) }
