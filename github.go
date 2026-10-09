package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type client struct {
	cfg        config
	httpClient *http.Client
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Message) }

type ref struct {
	SHA string `json:"sha"`
}

type pull struct {
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
	Head   ref  `json:"head"`
	Base   ref  `json:"base"`
}

func (c *client) getPull(ctx context.Context, n int) (pull, error) {
	var p pull
	err := c.getJSON(ctx, fmt.Sprintf("/repos/%s/pulls/%d", c.cfg.repo, n), &p)
	return p, err
}

func (c *client) listPRFiles(ctx context.Context, n int) ([]string, error) {
	var files []string
	for page := 1; ; page++ {
		var batch []struct {
			Filename string `json:"filename"`
		}
		u := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", c.cfg.repo, n, page)
		if err := c.getJSON(ctx, u, &batch); err != nil {
			return nil, err
		}
		for _, f := range batch {
			files = append(files, f.Filename)
		}
		if len(batch) < 100 {
			return files, nil
		}
	}
}

func (c *client) mergeBase(ctx context.Context, base, head string) (string, error) {
	var cmp struct {
		MergeBaseSHA string `json:"merge_base_sha"`
		BehindBy     int    `json:"behind_by"`
	}
	u := fmt.Sprintf("/repos/%s/compare/%s...%s", c.cfg.repo, base, head)
	if err := c.getJSON(ctx, u, &cmp); err != nil {
		return "", err
	}
	if cmp.MergeBaseSHA != "" {
		return cmp.MergeBaseSHA, nil
	}
	// When the base is a direct ancestor of the head (behind_by == 0), the
	// API leaves merge_base_sha empty: the base itself is the merge base.
	if cmp.BehindBy == 0 {
		return base, nil
	}
	return "", fmt.Errorf("no merge base between %s and %s", base, head)
}

// getFile returns the raw content of file at ref, or nil if the file does
// not exist at that ref.
func (c *client) getFile(ctx context.Context, file, ref string) ([]byte, error) {
	u := "/repos/" + c.cfg.repo + "/contents/" + escapePath(file) + "?ref=" + url.QueryEscape(ref)
	resp, err := c.do(ctx, http.MethodGet, u, nil, "application/vnd.github.raw")
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// apply adds or removes the label according to the decision.
func (c *client) apply(ctx context.Context, cfg config, d decision) error {
	writeOutputs(d)
	if !d.Label {
		fmt.Printf("removing label %q if present.\n", cfg.label)
		if err := c.removeLabel(ctx, c.cfg.pr, cfg.label); err != nil {
			return fmt.Errorf("remove label: %w", err)
		}
		return nil
	}
	fmt.Printf("adding label %q.\n", cfg.label)
	if err := c.addLabel(ctx, c.cfg.pr, cfg.label); err != nil {
		return fmt.Errorf("add label: %w", err)
	}
	return nil
}

func (c *client) addLabel(ctx context.Context, n int, label string) error {
	u := fmt.Sprintf("/repos/%s/issues/%d/labels", c.cfg.repo, n)
	body := map[string]any{"labels": []string{label}}
	err := c.call(ctx, http.MethodPost, u, body)
	var apiErr *apiError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return err
	}
	// The label does not exist in the repository yet: create it, then retry.
	if cerr := c.call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/labels", c.cfg.repo), map[string]any{
		"name":        label,
		"color":       "0E8A16",
		"description": "PR only bumps Go module dependencies",
	}); cerr != nil {
		return fmt.Errorf("create label %q: %w", label, cerr)
	}
	return c.call(ctx, http.MethodPost, u, body)
}

func (c *client) removeLabel(ctx context.Context, n int, label string) error {
	u := fmt.Sprintf("/repos/%s/issues/%d/labels/%s", c.cfg.repo, n, url.PathEscape(label))
	if err := c.call(ctx, http.MethodDelete, u, nil); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil // The label is not on the PR: nothing to remove.
		}
		return err
	}
	return nil
}

func (c *client) getJSON(ctx context.Context, u string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, u, nil, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) call(ctx context.Context, method, u string, body any) error {
	resp, err := c.do(ctx, method, u, body, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *client) do(ctx context.Context, method, u string, body any, accept string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.apiURL+u, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(b))
		}
		return nil, &apiError{Status: resp.StatusCode, Message: msg.Message}
	}
	return resp, nil
}

func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
