package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRotatedFilesDeletesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "accesslog_geo.log")
	errBasePath := filepath.Join(dir, "accesslog_geo_err.log")
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	files := map[string]bool{
		basePath:                        true,
		basePath + "-20260628":          false,
		basePath + "-20260628.1":        false,
		basePath + "-20260629":          false,
		basePath + "-20260705":          false,
		basePath + "-20260706":          true,
		basePath + "-not-a-date":        true,
		errBasePath + "-20260628":       false,
		errBasePath + "-20260704":       false,
		errBasePath + "-20260705":       false,
		errBasePath + "-20260706":       true,
		filepath.Join(dir, "other.log"): true,
	}

	for path := range files {
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatalf("write test file %s: %v", path, err)
		}
	}

	deleted, failed := cleanupRotatedFiles([]string{basePath, errBasePath}, 1, now)
	if deleted != 7 {
		t.Fatalf("deleted = %d, want 7", deleted)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}

	for path, wantExists := range files {
		_, err := os.Stat(path)
		exists := err == nil
		if exists != wantExists {
			t.Fatalf("exists(%s) = %v, want %v", path, exists, wantExists)
		}
	}
}
