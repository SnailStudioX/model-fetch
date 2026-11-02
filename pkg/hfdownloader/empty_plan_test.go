// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"strings"
	"testing"
)

// A filtered download that selects nothing moves no bytes, and it used to report
// success anyway. Filters only narrow the LFS files (a repository's small config
// files always come along), so the reachable case is a quant that is not there.

func lfsNode(name, oid string, size int64) hfNode {
	return hfNode{Type: "file", Path: name, Size: size, LFS: &hfLfsInfo{Oid: oid, Size: size}}
}

func TestDownload_FilterMatchesNothingFails(t *testing.T) {
	files := []hfNode{
		lfsNode("model-Q4_K_M.gguf", strings.Repeat("a", 64), 100),
		lfsNode("model-Q6_K.gguf", strings.Repeat("b", 64), 100),
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	job := Job{Repo: "owner/repo", Revision: "main", Filters: []string{"Q8_0"}}
	cfg := Settings{Endpoint: srv.URL, CacheDir: t.TempDir()}

	err := Download(context.Background(), job, cfg, nil)
	if err == nil {
		t.Fatal("a filter that matched nothing returned success")
	}
	msg := err.Error()
	for _, want := range []string{`"Q8_0"`, "2 files", "owner/repo", "main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	// The count is the actionable part: the files are there, the pattern is wrong.
	if strings.Contains(msg, "no files") {
		t.Errorf("a repository with files was described as empty: %q", msg)
	}
}

func TestDownload_ExcludesEverythingFails(t *testing.T) {
	files := []hfNode{
		lfsNode("model-Q4_K_M.gguf", strings.Repeat("a", 64), 100),
		{Type: "file", Path: "config.json", Size: 10},
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()

	job := Job{Repo: "owner/repo", Revision: "main", Excludes: []string{".gguf", ".json"}}
	cfg := Settings{Endpoint: srv.URL, CacheDir: t.TempDir()}

	err := Download(context.Background(), job, cfg, nil)
	if err == nil {
		t.Fatal("excludes that removed every file returned success")
	}
	msg := err.Error()
	if !strings.Contains(msg, "excludes") || !strings.Contains(msg, "2 files") {
		t.Errorf("error %q must name the excludes and how many files they removed", msg)
	}
}

// A repository with no files at all is nothing to do rather than a mistake the
// caller made, so the download stays a success - but a job that moved no bytes
// has to say why, not leave the caller to guess which of the two cases it was.
func TestDownload_EmptyRepositoryWarnsInsteadOfPassingSilently(t *testing.T) {
	srv := mockHFServerForPlan(t, nil)
	defer srv.Close()

	var warnings []string
	err := Download(context.Background(), Job{Repo: "owner/repo", Revision: "main"}, Settings{
		Endpoint: srv.URL,
		CacheDir: t.TempDir(),
	}, func(ev ProgressEvent) {
		if ev.Event == "warning" {
			warnings = append(warnings, ev.Message)
		}
	})
	if err != nil {
		t.Fatalf("an empty repository must stay a no-op, not a failure: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "no files") || !strings.Contains(joined, "main") {
		t.Errorf("no warning explained why nothing was downloaded: %q", joined)
	}
	if strings.Contains(joined, "filter") {
		t.Errorf("the warning blames a filter that was never set: %q", joined)
	}
}

// A dry run is where a bad filter is usually written, so the plan itself carries
// enough to read: the hub's file count next to the empty selection.
func TestPlanReportsWhatTheHubActuallyHas(t *testing.T) {
	files := []hfNode{
		lfsNode("model-Q4_K_M.gguf", strings.Repeat("a", 64), 100),
		lfsNode("model-Q6_K.gguf", strings.Repeat("b", 64), 100),
	}
	srv := mockHFServerForPlan(t, files)
	defer srv.Close()
	cfg := Settings{Endpoint: srv.URL}

	plan, err := PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", Filters: []string{"Q8_0"}}, cfg)
	if err != nil {
		t.Fatalf("a dry run must still answer for a selection that matches nothing: %v", err)
	}
	if len(plan.Items) != 0 {
		t.Fatalf("items = %d, want none selected", len(plan.Items))
	}
	if plan.ScannedFiles != 2 {
		t.Errorf("scannedFiles = %d, want the 2 files the hub reported", plan.ScannedFiles)
	}

	// A selection that does match still works, and reports the same count it
	// narrowed from.
	plan, err = PlanRepo(context.Background(), Job{Repo: "owner/repo", Revision: "main", Filters: []string{"Q4_K_M"}}, cfg)
	if err != nil {
		t.Fatalf("PlanRepo: %v", err)
	}
	if len(plan.Items) == 0 {
		t.Fatal("a matching filter selected nothing")
	}
	if plan.ScannedFiles != 2 {
		t.Errorf("scannedFiles = %d, want 2", plan.ScannedFiles)
	}
}
