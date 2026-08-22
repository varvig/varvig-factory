package artifact

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varvig/varvig-factory/cell"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocalCASPutIsContentAddressedAndIdempotent(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	l := &LocalCAS{Root: root}
	ctx := context.Background()

	a := write(t, src, "a.bin", "payload")
	b := write(t, src, "b.bin", "payload") // same bytes, different name

	ra, err := l.Put(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := l.Put(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if ra.ContentHash != rb.ContentHash {
		t.Fatal("identical bytes produced different content hashes")
	}
	if !strings.HasPrefix(ra.ContentHash, cell.HashAlgorithm+":") {
		t.Fatalf("content hash %q has no self-describing label", ra.ContentHash)
	}
	if ra.Size != int64(len("payload")) {
		t.Fatalf("size = %d, want %d", ra.Size, len("payload"))
	}
	// Two attempts producing byte-identical outputs must cost one copy.
	used, err := l.UsedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used != int64(len("payload")) {
		t.Fatalf("used = %d, want %d (a re-Put should not store a second copy)", used, len("payload"))
	}
}

func TestLocalCASRoundTripsBytes(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	l := &LocalCAS{Root: root}
	ctx := context.Background()

	ref, err := l.Put(ctx, write(t, src, "x", "hello world"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Fatalf("read back %q", got)
	}
}

func TestOpenAnUnheldArtifactIsNotFound(t *testing.T) {
	l := &LocalCAS{Root: t.TempDir()}
	_, err := l.Open(context.Background(), cell.ArtifactRef{ContentHash: cell.HashAlgorithm + ":" + strings.Repeat("ab", 32)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPathRejectsATraversingContentHash(t *testing.T) {
	// A ref that arrived from a peer is untrusted input, and it is interpolated
	// into a filesystem path.
	l := &LocalCAS{Root: t.TempDir()}
	for _, bad := range []string{
		"sha256:../../etc/passwd",
		"sha256:zz",
		"md5:abcd",
		"abcd",
		"sha256:",
	} {
		if _, err := l.path(bad); err == nil {
			t.Fatalf("malformed content hash %q produced a path", bad)
		}
	}
}

func TestReleaseFreesLocalBytesAndIsIdempotent(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	l := &LocalCAS{Root: root}
	ctx := context.Background()

	ref, err := l.Put(ctx, write(t, src, "big", strings.Repeat("x", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if used, _ := l.UsedBytes(ctx); used != 0 {
		t.Fatalf("used = %d after release, want 0", used)
	}
	// Releasing twice must not fail: storage-pressure relief runs repeatedly and
	// a second pass should not error on what the first already dropped.
	if err := l.Release(ctx, ref); err != nil {
		t.Fatalf("second release failed: %v", err)
	}
}

func TestListIsLargestFirst(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	l := &LocalCAS{Root: root}
	ctx := context.Background()
	for _, n := range []int{10, 5000, 300} {
		if _, err := l.Put(ctx, write(t, src, "f", strings.Repeat("y", n))); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d artifacts, want 3", len(got))
	}
	// Largest first: releasing the biggest frees the most disk for the fewest
	// retention obligations dropped (FACTORY.md §7).
	for i := 1; i < len(got); i++ {
		if got[i-1].Size < got[i].Size {
			t.Fatalf("List is not largest-first: %v", got)
		}
	}
}

func TestRemotePutStaysLocal(t *testing.T) {
	// FACTORY.md §8: speculative artifacts stay cell-local; replicate on
	// promotion, not on attempt. The seam encodes that structurally, and this
	// test holds the structure: Put must not invoke the push command, even
	// when the push command is configured and would fail loudly if run.
	root, src := t.TempDir(), t.TempDir()
	r := &Remote{
		Local:           &LocalCAS{Root: root},
		PushCommand:     []string{"definitely-not-a-real-binary-xyz", "{{path}}"},
		LocatorTemplate: "oci://registry.internal/app@{{hash}}",
		Label:           "oci",
	}
	ctx := context.Background()

	ref, err := r.Put(ctx, write(t, src, "app", "bytes"))
	if err != nil {
		t.Fatalf("Put reached for the network: %v", err)
	}
	for _, loc := range ref.Locators {
		if !strings.HasPrefix(loc, "file://") {
			t.Fatalf("Put recorded a non-local locator: %s", loc)
		}
	}
}

func TestRemoteReplicateRunsThePushAndRecordsTheLocator(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	receipt := filepath.Join(t.TempDir(), "pushed")
	r := &Remote{
		Local: &LocalCAS{Root: root},
		// A push that records what it was handed, so the substitution is
		// verifiable rather than assumed.
		PushCommand:     []string{"sh", "-c", "printf '%s' \"$1\" > " + receipt, "sh", "{{hex}}"},
		LocatorTemplate: "oci://registry.internal/app@{{hash}}",
		Label:           "oci",
	}
	ctx := context.Background()

	ref, err := r.Put(ctx, write(t, src, "app", "bytes"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Replicate(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	hexOnly := strings.TrimPrefix(ref.ContentHash, cell.HashAlgorithm+":")
	got, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hexOnly {
		t.Fatalf("push received %q, want the bare hex %q", got, hexOnly)
	}
	found := false
	for _, loc := range out.Locators {
		if loc == "oci://registry.internal/app@"+ref.ContentHash {
			found = true
		}
	}
	if !found {
		t.Fatalf("replicate did not record the remote locator: %v", out.Locators)
	}
	// The local locator survives: the bytes are still here, and dropping the
	// fact would make a later Release look like it deleted nothing.
	if len(out.Locators) < 2 {
		t.Fatalf("replicate dropped the local locator: %v", out.Locators)
	}
}

func TestReplicateRefusesAnArtifactItDoesNotHold(t *testing.T) {
	r := &Remote{Local: &LocalCAS{Root: t.TempDir()}, PushCommand: []string{"true"}}
	_, err := r.Replicate(context.Background(), cell.ArtifactRef{ContentHash: cell.HashAlgorithm + ":" + strings.Repeat("ab", 32)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReplicateWithoutAPushCommandSaysSo(t *testing.T) {
	root, src := t.TempDir(), t.TempDir()
	r := &Remote{Local: &LocalCAS{Root: root}}
	ctx := context.Background()
	ref, err := r.Put(ctx, write(t, src, "app", "bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Replicate(ctx, ref); err == nil {
		t.Fatal("replicate with no push command succeeded silently")
	}
}

func TestLocalCASReplicateIsANoOpNotAnError(t *testing.T) {
	// A single-cell deployment with no registry is a legitimate configuration:
	// promotion there means the bytes stay where they are.
	root, src := t.TempDir(), t.TempDir()
	l := &LocalCAS{Root: root}
	ctx := context.Background()
	ref, err := l.Put(ctx, write(t, src, "app", "bytes"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := l.Replicate(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if out.ContentHash != ref.ContentHash {
		t.Fatal("local replicate changed the ref")
	}
}

// Both stores must satisfy the seam.
var (
	_ Store = (*LocalCAS)(nil)
	_ Store = (*Remote)(nil)
)
