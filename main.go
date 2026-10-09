package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
)

type config struct {
	apiURL string
	repo   string
	pr     int
	label  string
	token  string
}

func loadConfig() (config, error) {
	cfg := config{
		apiURL: envOr("GITHUB_API_URL", "https://api.github.com"),
		repo:   envOr("LABELER_REPO", os.Getenv("GITHUB_REPOSITORY")),
		label:  envOr("LABELER_LABEL", "only-dependency-bump"),
		token:  os.Getenv("GITHUB_TOKEN"),
	}
	if cfg.repo == "" || cfg.token == "" {
		return cfg, fmt.Errorf("GITHUB_TOKEN and LABELER_REPO (or GITHUB_REPOSITORY) must be set")
	}
	pr, err := strconv.Atoi(os.Getenv("LABELER_PR"))
	if err != nil {
		return cfg, fmt.Errorf("LABELER_PR must be set to a pull request number: %w", err)
	}
	cfg.pr = pr
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "only-dep-bump: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cl := &client{cfg: cfg, httpClient: new(http.Client)}

	pr, err := cl.getPull(ctx, cfg.pr)
	if err != nil {
		return fmt.Errorf("fetch PR #%d: %w", cfg.pr, err)
	}
	fmt.Printf("PR #%d: base=%s head=%s draft=%t\n", pr.Number, short(pr.Base.SHA), short(pr.Head.SHA), pr.Draft)

	if pr.Draft {
		fmt.Printf("PR is a draft: skipping analysis and removing label %q if present.\n", cfg.label)
		return cl.apply(ctx, cfg, decision{})
	}

	files, err := cl.listPRFiles(ctx, cfg.pr)
	if err != nil {
		return fmt.Errorf("list changed files: %w", err)
	}
	fmt.Printf("%d file(s) changed: %s\n", len(files), strings.Join(files, ", "))

	offenders, goMods := classifyFiles(files)
	if len(offenders) > 0 {
		fmt.Printf("not only a dependency bump: files changed that are not go.mod/go.sum:\n  %s\n", strings.Join(offenders, "\n  "))
		return cl.apply(ctx, cfg, decision{Reasons: []string{
			"files changed that are not go.mod/go.sum: " + strings.Join(offenders, ", "),
		}})
	}
	if len(goMods) == 0 {
		fmt.Printf("no go.mod changed (go.sum-only or empty diff): not a dependency bump.\n")
		return cl.apply(ctx, cfg, decision{Reasons: []string{"no go.mod changes"}})
	}

	baseSHA, err := cl.mergeBase(ctx, pr.Base.SHA, pr.Head.SHA)
	if err != nil {
		return fmt.Errorf("determine merge base: %w", err)
	}

	var changes []moduleChange
	for _, gm := range goMods {
		ch := moduleChange{Path: gm}
		data, err := cl.getFile(ctx, gm, baseSHA)
		if err != nil {
			return fmt.Errorf("fetch %s at merge base: %w", gm, err)
		}
		if data != nil {
			ch.Base, ch.BaseErr = parseGoMod(gm, data)
		}
		data, err = cl.getFile(ctx, gm, pr.Head.SHA)
		if err != nil {
			return fmt.Errorf("fetch %s at head: %w", gm, err)
		}
		if data != nil {
			ch.Head, ch.HeadErr = parseGoMod(gm, data)
		}
		changes = append(changes, ch)
	}

	d := evaluate(changes)
	for _, u := range d.Upgrades {
		fmt.Printf("upgrade: %s\n", u)
	}
	for _, n := range d.Notes {
		fmt.Printf("neutral change: %s\n", n)
	}
	for _, r := range d.Reasons {
		fmt.Printf("disqualifier: %s\n", r)
	}
	if d.Label {
		fmt.Printf("decision: only a Go dependency bump.\n")
	} else {
		fmt.Printf("decision: NOT only a Go dependency bump.\n")
	}
	return cl.apply(ctx, cfg, d)
}

func parseGoMod(path string, data []byte) (*modfile.File, error) {
	return modfile.Parse(path, data, nil)
}

func writeOutputs(d decision) {
	out := os.Getenv("GITHUB_OUTPUT")
	if out == "" {
		return
	}
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot write GITHUB_OUTPUT: %v\n", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "decision=%t\n", d.Label)
	fmt.Fprintf(f, "reasons=%s\n", strings.Join(d.Reasons, "; "))
	fmt.Fprintf(f, "upgrades=%s\n", strings.Join(d.Upgrades, "; "))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
