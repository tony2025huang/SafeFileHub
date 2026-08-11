package httpapi

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

const publishedConcurrencyWorkers = 16

func concurrentPublishedCalls(t *testing.T, f publishedFixture, method, target, body string) []int {
	t.Helper()
	start := make(chan struct{})
	codes := make(chan int, publishedConcurrencyWorkers)
	var wg sync.WaitGroup
	for range publishedConcurrencyWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- f.call(t, method, target, body, f.owner.ID).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	out := make([]int, 0, publishedConcurrencyWorkers)
	for code := range codes {
		out = append(out, code)
	}
	return out
}

func assertOneCreatedAndConflicts(t *testing.T, codes []int) {
	t.Helper()
	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected concurrent create status %d (all=%v)", code, codes)
		}
	}
	if created != 1 {
		t.Fatalf("created=%d, want exactly one (all=%v)", created, codes)
	}
}

// This is deliberately bounded. It exercises the same handler instance from
// 16 goroutines, which is enough to expose check-then-create races without
// turning the test suite into an unbounded load test.
func TestConcurrentPublishedCreatesLeaveOneMetadataAndObject(t *testing.T) {
	f := newPublishedFixture(t)
	codes := concurrentPublishedCalls(t, f, http.MethodPost, "/api/files", `{"root_id":1,"path":"same-file"}`)
	assertOneCreatedAndConflicts(t, codes)
	file, err := f.repo.FileByRootAndPath(context.Background(), f.root.ID, "/same-file")
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if _, err := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, "/same-file"); err == nil {
		t.Fatal("file path was also recorded as a directory")
	}
	objectFiles, err := filepath.Glob(filepath.Join(f.root.Path, "objects", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objectFiles) != 1 {
		t.Fatalf("objects=%v, want exactly one for file id=%d", objectFiles, file.ID)
	}

	f = newPublishedFixture(t)
	codes = concurrentPublishedCalls(t, f, http.MethodPost, "/api/directories", `{"root_id":1,"path":"same-directory"}`)
	assertOneCreatedAndConflicts(t, codes)
	if _, err := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, "/same-directory"); err != nil {
		t.Fatalf("read created directory: %v", err)
	}
	if _, err := f.repo.FileByRootAndPath(context.Background(), f.root.ID, "/same-directory"); err == nil {
		t.Fatal("directory path was also recorded as a file")
	}
}

func TestConcurrentCreateChildAndDeleteDirectoryPreservesTreeInvariant(t *testing.T) {
	f := newPublishedFixture(t)
	for round := 0; round < 8; round++ {
		parentPath := "/parent-" + itoa(int64(round))
		if got := f.call(t, http.MethodPost, "/api/directories", `{"root_id":1,"path":"parent-`+itoa(int64(round))+`"}`, f.owner.ID).Code; got != http.StatusCreated {
			t.Fatalf("create parent round=%d status=%d", round, got)
		}
		parent, err := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, parentPath)
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var childCode, deleteCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			childCode = f.call(t, http.MethodPost, "/api/directories", `{"root_id":1,"path":"parent-`+itoa(int64(round))+`/child"}`, f.owner.ID).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteCode = f.call(t, http.MethodDelete, "/api/directories/"+itoa(parent.ID), "", f.owner.ID).Code
		}()
		close(start)
		wg.Wait()
		if childCode != http.StatusCreated && childCode != http.StatusNotFound && childCode != http.StatusConflict {
			t.Fatalf("round=%d child status=%d", round, childCode)
		}
		if deleteCode != http.StatusNoContent && deleteCode != http.StatusConflict {
			t.Fatalf("round=%d delete status=%d", round, deleteCode)
		}
		_, parentErr := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, parentPath)
		_, childErr := f.repo.DirectoryByRootAndPath(context.Background(), f.root.ID, parentPath+"/child")
		if childErr == nil && parentErr != nil {
			t.Fatalf("round=%d child survived after parent deletion: child=%v parent=%v", round, childErr, parentErr)
		}
		if childCode == http.StatusCreated && deleteCode == http.StatusNoContent {
			t.Fatalf("round=%d created child while deleting non-empty parent", round)
		}
	}
}
