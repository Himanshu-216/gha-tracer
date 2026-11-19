// Command backfill exports historical runs as traces, or replays fixture files.
//
// From GitHub (uses the same GITHUB_APP_* env as the service):
//
//	backfill -repo acme/payments -run 9001 [-attempt 2]
//	backfill -repo acme/payments -branch v1.20.0          # every completed run for that tag
//	backfill -repo acme/payments -branch release/v1.20.0 -limit 20
//
// Offline (no GitHub access; for local demos and testing the pipeline):
//
//	backfill -run-file testdata/workflow_run_completed.json -jobs-file testdata/jobs_9001_1.json
//
// The output goes to OTLP_ENDPOINT. Deterministic IDs make re-running safe.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/Himanshu-216/gha-tracer/internal/emit"
	"github.com/Himanshu-216/gha-tracer/internal/github"
	"github.com/Himanshu-216/gha-tracer/internal/release"
	"github.com/Himanshu-216/gha-tracer/internal/tree"
)

func main() {
	var (
		repo     = flag.String("repo", "", "org/repo")
		runID    = flag.Int64("run", 0, "run ID")
		attempt  = flag.Int("attempt", 0, "run attempt (default: latest)")
		branch   = flag.String("branch", "", "tag or branch name; backfills all completed runs for it")
		limit    = flag.Int("limit", 50, "max runs with -branch")
		runFile  = flag.String("run-file", "", "workflow_run webhook payload or REST run JSON (offline mode)")
		jobsFile = flag.String("jobs-file", "", "jobs API response JSON (offline mode)")
		all      = flag.Bool("all", false, "export even if the ref isn't a release ref")
		dryRun   = flag.Bool("dry-run", false, "print the OTLP JSON instead of sending")
	)
	flag.Parse()

	if err := run(*repo, *runID, *attempt, *branch, *limit, *runFile, *jobsFile, *all, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "backfill:", err)
		os.Exit(1)
	}
}

func run(repo string, runID int64, attempt int, branch string, limit int, runFile, jobsFile string, all, dryRun bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cls, err := release.New(envOr("TAG_PATTERN", release.DefaultTagPattern), envOr("RELEASE_BRANCH_PREFIX", release.DefaultBranchPrefix))
	if err != nil {
		return err
	}
	exp := emit.NewExporter(envOr("OTLP_ENDPOINT", "http://localhost:4318/v1/traces"), nil)

	export := func(r tree.Run, jobs []tree.Job) error {
		info, ok := cls.Classify(r.HeadBranch)
		if !ok && !all {
			fmt.Printf("skip %s#%d (%s is not a release ref; use -all)\n", r.Repo, r.ID, r.HeadBranch)
			return nil
		}
		if !ok {
			info = release.Info{Type: "other"}
		}
		t := tree.Build(r, jobs, info)
		if dryRun {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(emit.Encode(t))
		}
		if err := exp.Export(ctx, t); err != nil {
			return err
		}
		fmt.Printf("exported %s#%d#%d  %s  %s %s  trace_id=%x\n", r.Repo, r.ID, r.Attempt, r.HeadBranch, info.Type, r.Conclusion, t.TraceID)
		return nil
	}

	if runFile != "" {
		r, err := readRun(runFile)
		if err != nil {
			return err
		}
		var jobs []tree.Job
		if jobsFile != "" {
			var resp gh.Jobs
			if err := readJSON(jobsFile, &resp); err != nil {
				return err
			}
			for _, j := range resp.Jobs {
				jobs = append(jobs, github.JobFromAPI(j))
			}
		}
		return export(github.RunFromAPI(r), jobs)
	}

	if repo == "" || (runID == 0 && branch == "") {
		flag.Usage()
		return fmt.Errorf("need -repo with -run or -branch, or -run-file")
	}
	appID, err := parseInt(os.Getenv("GITHUB_APP_ID"))
	if err != nil {
		return fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	key := []byte(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if f := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); len(key) == 0 && f != "" {
		if key, err = os.ReadFile(f); err != nil {
			return err
		}
	}
	api, err := github.New(appID, key, os.Getenv("GITHUB_API_URL"), nil)
	if err != nil {
		return err
	}
	inst, err := api.InstallationForRepo(ctx, repo)
	if err != nil {
		return err
	}

	var runs []*gh.WorkflowRun
	if runID != 0 {
		if attempt == 0 {
			// Find the latest attempt by walking forward until one is missing.
			if _, err := api.Run(ctx, inst, repo, runID, 1); err != nil {
				return err
			}
			attempt = 1
			for a := 2; ; a++ {
				if _, err := api.Run(ctx, inst, repo, runID, a); err != nil {
					break
				}
				attempt = a
			}
		}
		for a := 1; a <= attempt; a++ {
			r, err := api.Run(ctx, inst, repo, runID, a)
			if err != nil {
				return err
			}
			runs = append(runs, r)
		}
	} else {
		if runs, err = api.CompletedRuns(ctx, inst, repo, branch, limit); err != nil {
			return err
		}
		slices.Reverse(runs) // oldest first
	}

	for _, r := range runs {
		if r.GetStatus() != "completed" {
			fmt.Printf("skip %s#%d (status %s)\n", repo, r.GetID(), r.GetStatus())
			continue
		}
		tr := github.RunFromAPI(r)
		jobs, err := api.Jobs(ctx, inst, repo, tr.ID, tr.Attempt)
		if err != nil {
			return err
		}
		if err := export(tr, jobs); err != nil {
			return err
		}
	}
	return nil
}

// readRun accepts either a workflow_run webhook payload or a bare run object.
func readRun(path string) (*gh.WorkflowRun, error) {
	var ev gh.WorkflowRunEvent
	if err := readJSON(path, &ev); err != nil {
		return nil, err
	}
	if ev.WorkflowRun != nil {
		return ev.WorkflowRun, nil
	}
	var r gh.WorkflowRun
	if err := readJSON(path, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}
