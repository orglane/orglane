package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// The process-crash suite cannot simulate loss of kernel buffers. These tests
// observe the actual sync boundaries and inject writeback errors instead.
func TestExtractSyncsContentMetadataAndDirectories(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "nested", "deep", "value")
	if err := os.WriteFile(file, []byte("acknowledged"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := archiveDir(src, &archive); err != nil {
		t.Fatal(err)
	}
	want := []string{"nested/deep/value", "nested/deep", "nested", "."}
	failure := errors.New("injected writeback failure")
	for _, failAt := range append([]string{""}, want...) {
		t.Run("sync-"+failAt, func(t *testing.T) {
			dst := t.TempDir()
			var synced []string
			err := extractArchiveSync(bytes.NewReader(archive.Bytes()), dst, false, func(f *os.File) error {
				name, err := filepath.Rel(dst, f.Name())
				if err != nil {
					return err
				}
				name = filepath.ToSlash(name)
				synced = append(synced, name)
				if name == want[0] {
					info, err := f.Stat()
					if err != nil {
						return err
					}
					if !info.ModTime().Equal(stamp) || info.Size() != 12 || info.Mode().Perm() != 0600 {
						t.Fatalf("sync before content/metadata: %+v", info)
					}
				}
				if name == failAt {
					return failure
				}
				return f.Sync()
			})
			if failAt == "" {
				if err != nil || !slices.Equal(synced, want) {
					t.Fatalf("synced %v: %v", synced, err)
				}
			} else if !errors.Is(err, failure) || synced[len(synced)-1] != failAt {
				t.Fatalf("writeback failure ignored: %v, synced %v", err, synced)
			}
		})
	}
}

func TestSnapshotCompletionRequiresParentSyncs(t *testing.T) {
	failure := errors.New("injected directory writeback failure")
	for _, failAt := range []string{snapshotData, "snapshots", "instance", ""} {
		t.Run(failAt, func(t *testing.T) {
			instance := filepath.Join(t.TempDir(), "instance")
			dir := filepath.Join(instance, snapshotsDir, uuid.Must(uuid.NewV7()).String())
			if err := os.MkdirAll(filepath.Join(dir, snapshotData), 0700); err != nil {
				t.Fatal(err)
			}
			var synced []string
			err := completeSnapshot(dir, &agentapi.Snapshot{ID: uuid.Must(uuid.NewV7())}, func(path string) error {
				name := filepath.Base(path)
				synced = append(synced, name)
				if _, err := os.Stat(filepath.Join(dir, snapshotComplete)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("COMPLETE published before syncing %s", path)
				}
				if name == failAt {
					return failure
				}
				return syncDir(path)
			})
			_, completeErr := os.Stat(filepath.Join(dir, snapshotComplete))
			if failAt != "" {
				if !errors.Is(err, failure) || !errors.Is(completeErr, os.ErrNotExist) {
					t.Fatalf("completion survived writeback failure: %v, %v", err, completeErr)
				}
			} else if err != nil || completeErr != nil || !slices.Equal(synced, []string{snapshotData, "snapshots", "instance"}) {
				t.Fatalf("completion: %v %v, synced %v", err, completeErr, synced)
			}
		})
	}
}
