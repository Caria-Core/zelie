package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Source fetches an app's code.
type Source interface {
	// Resolve returns the commit a branch points at.
	Resolve(ctx context.Context, repo, branch string) (string, error)
	// Archive returns the code at a commit as a gzipped tar.
	Archive(ctx context.Context, repo, commit string) (io.ReadCloser, error)
}

var (
	validRepo   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	validBranch = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,199}$`)
	validCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var (
	errBadRepo   = msg.Define(http.StatusBadRequest, "app.bad_repo", "The repository must look like owner/name.")
	errBadBranch = msg.Define(http.StatusBadRequest, "app.bad_branch", "That is not a valid branch name.")
)

func checkRepo(repo, branch string) *msg.Error {
	if !validRepo.MatchString(repo) || strings.HasSuffix(repo, ".git") {
		return errBadRepo.Err()
	}
	if !validBranch.MatchString(branch) || strings.Contains(branch, "..") || strings.HasSuffix(branch, "/") {
		return errBadBranch.Err()
	}
	return nil
}

// PublicGitHub reads public repositories without an account. Private ones
// need the GitHub connection.
type PublicGitHub struct {
	Client   *http.Client
	API      string // https://api.github.com
	Codeload string // https://codeload.github.com
}

func NewPublicGitHub() *PublicGitHub {
	return &PublicGitHub{Client: &http.Client{}, API: "https://api.github.com", Codeload: "https://codeload.github.com"}
}

func (g *PublicGitHub) Resolve(ctx context.Context, repo, branch string) (string, error) {
	if err := checkRepo(repo, branch); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.API+"/repos/"+repo+"/commits/"+branch, nil)
	if err != nil {
		return "", err
	}
	// This media type makes GitHub answer with the bare commit hash.
	req.Header.Set("Accept", "application/vnd.github.sha")
	resp, err := g.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		return "", fmt.Errorf("GitHub has no public repository %s with a branch %s", repo, branch)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return "", errors.New("GitHub's limit for requests without an account was reached; try again within an hour")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100))
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(b))
	if !validCommit.MatchString(commit) {
		return "", errors.New("GitHub gave an unexpected answer")
	}
	return commit, nil
}

func (g *PublicGitHub) Archive(ctx context.Context, repo, commit string) (io.ReadCloser, error) {
	if !validRepo.MatchString(repo) || !validCommit.MatchString(commit) {
		return nil, errors.New("invalid repository or commit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Codeload+"/"+repo+"/tar.gz/"+commit, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download from GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download from GitHub: %s", resp.Status)
	}
	return resp.Body, nil
}
