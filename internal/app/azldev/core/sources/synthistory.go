// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sources

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/microsoft/azure-linux-dev-tools/internal/global/opctx"
	"github.com/microsoft/azure-linux-dev-tools/internal/projectconfig"
	toml "github.com/pelletier/go-toml/v2"
)

// CommitMetadata holds full metadata for a commit in the project repository.
type CommitMetadata struct {
	Hash        string
	Author      string
	AuthorEmail string
	Timestamp   int64
	Message     string
}

// FingerprintChange records a project commit that changed a component's lock file
// fingerprint. [UpstreamCommit] is the value of the 'upstream-commit' field in the
// lock file at the point of the change.
type FingerprintChange struct {
	CommitMetadata

	// UpstreamCommit is the upstream dist-git commit hash recorded in the lock
	// file at the time the fingerprint changed.
	UpstreamCommit string
}

// interleavedEntry represents a single commit in the rebuilt dist-git history.
// Exactly one of upstreamCommit or syntheticChange is non-nil.
type interleavedEntry struct {
	upstreamCommit  *object.Commit
	syntheticChange *FingerprintChange
}

// LockFilePath returns the relative path to a component's lock file within the
// project repository. The path follows the same letter-prefix convention used by
// [components.RenderedSpecDir]: specs/<letter>/<name>/<name>.lock.
func LockFilePath(componentName string) string {
	prefix := strings.ToLower(componentName[:1])

	return filepath.Join("specs", prefix, componentName, componentName+".lock")
}

// lockFileFields holds the subset of lock file fields needed for fingerprint
// change detection. This avoids importing the full [lockfile.ComponentLock]
// struct and decouples the synthetic history logic from lock file versioning.
type lockFileFields struct {
	ImportCommit     string `toml:"import-commit"`
	UpstreamCommit   string `toml:"upstream-commit"`
	InputFingerprint string `toml:"input-fingerprint"`
}

// FindFingerprintChanges walks the git log of the project repository for commits
// that changed the given lock file and returns metadata for each commit where the
// 'input-fingerprint' field changed. Results are sorted chronologically (oldest
// first).
func FindFingerprintChanges(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	projectRepoDir string,
	lockFileRelPath string,
) ([]FingerprintChange, error) {
	// Get the list of commit hashes that touched the lock file.
	hashes, err := gitLogFileHashes(ctx, cmdFactory, projectRepoDir, lockFileRelPath)
	if err != nil {
		return nil, err
	}

	if len(hashes) == 0 {
		return nil, nil
	}

	// Build a chronological list of (hash, lockFileFields) for each commit.
	type entry struct {
		hash   string
		fields lockFileFields
		meta   CommitMetadata
	}

	var entries []entry //nolint:prealloc // size not known ahead of time.

	for _, hash := range hashes {
		fields, err := gitShowLockFile(ctx, cmdFactory, projectRepoDir, hash, lockFileRelPath)
		if err != nil {
			slog.Warn("Failed to read lock file at commit; skipping",
				"commit", hash, "error", err)

			continue
		}

		meta, err := gitCommitMetadata(ctx, cmdFactory, projectRepoDir, hash)
		if err != nil {
			return nil, fmt.Errorf("failed to get metadata for commit %#q:\n%w", hash, err)
		}

		entries = append(entries, entry{hash: hash, fields: fields, meta: meta})
	}

	if len(entries) == 0 {
		return nil, nil
	}

	// Entries are newest-first (from git log order). Reverse to chronological.
	slices.Reverse(entries)

	// Walk chronologically and detect fingerprint changes.
	var changes []FingerprintChange

	prevFingerprint := ""

	for _, change := range entries {
		if change.fields.InputFingerprint != prevFingerprint {
			changes = append(changes, FingerprintChange{
				CommitMetadata: change.meta,
				UpstreamCommit: change.fields.UpstreamCommit,
			})
		}

		prevFingerprint = change.fields.InputFingerprint
	}

	return changes, nil
}

// CommitInterleavedHistory rebuilds the dist-git history by interleaving
// synthetic commits with the existing upstream commits. Synthetic commits
// referencing an older upstream commit are placed directly after that commit;
// those referencing the latest upstream commit are appended on top. The very
// last synthetic commit carries the overlay file changes; all others are empty.
//
// The resulting git history looks like:
//
//	U1 → F1 → F2 → U2' → U3' → F3 → F4
//
// where U1 is the import-commit (kept as-is), F1/F2 are synthetic commits
// interleaved after U1, U2'/U3' are upstream commits replayed with new parents,
// and F3/F4 are synthetic commits on top (F4 carries overlay changes).
//
// When importCommit is non-empty, only upstream commits from importCommit
// onward are considered for interleaving.
func CommitInterleavedHistory(
	repo *gogit.Repository,
	changes []FingerprintChange,
	importCommit string,
) error {
	// Collect upstream commits BEFORE staging, so the temporary commit
	// created by stageAndCaptureOverlayTree is not included.
	upstreamCommits, err := collectUpstreamCommits(repo, importCommit)
	if err != nil {
		return err
	}

	// Stage overlay changes and capture the resulting tree hash.
	overlayTreeHash, err := stageAndCaptureOverlayTree(repo)
	if err != nil {
		return err
	}

	// Build the full interleaved sequence of upstream and synthetic commits.
	sequence := buildInterleavedSequence(upstreamCommits, changes)

	return replayInterleavedHistory(repo, sequence, overlayTreeHash)
}

// stageAndCaptureOverlayTree stages all working tree changes and creates a
// temporary commit to capture the resulting tree hash. The tree hash is used
// later to set the content of the final synthetic commit.
func stageAndCaptureOverlayTree(repo *gogit.Repository) (plumbing.Hash, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to get worktree:\n%w", err)
	}

	if err := worktree.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to stage changes:\n%w", err)
	}

	tempHash, err := worktree.Commit("temp: capture overlay tree", &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "azldev", When: time.Unix(0, 0).UTC()},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to create temporary commit:\n%w", err)
	}

	tempCommit, err := repo.CommitObject(tempHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to read temporary commit:\n%w", err)
	}

	return tempCommit.TreeHash, nil
}

// buildInterleavedSequence produces the full commit sequence for the rebuilt
// history. Upstream commits appear in chronological order; synthetic commits
// that reference an older upstream are inserted directly after it. Synthetic
// commits referencing the latest upstream (or orphaned ones whose upstream is
// not found) are appended at the end.
func buildInterleavedSequence(
	upstreamCommits []*object.Commit,
	changes []FingerprintChange,
) []interleavedEntry {
	latestUpstream := changes[len(changes)-1].UpstreamCommit

	var interleaved, top []FingerprintChange

	for i := range changes {
		if changes[i].UpstreamCommit == latestUpstream {
			top = append(top, changes[i])
		} else {
			interleaved = append(interleaved, changes[i])
		}
	}

	// Build a lookup from upstream-commit hash → synthetic commits.
	interleavedByUpstream := make(map[string][]FingerprintChange)

	for i := range interleaved {
		hash := interleaved[i].UpstreamCommit
		interleavedByUpstream[hash] = append(interleavedByUpstream[hash], interleaved[i])
	}

	// Walk upstream commits, inserting synthetics after their referenced commit.
	sequence := make([]interleavedEntry, 0, len(upstreamCommits)+len(changes))

	for i := range upstreamCommits {
		sequence = append(sequence, interleavedEntry{upstreamCommit: upstreamCommits[i]})

		hash := upstreamCommits[i].Hash.String()
		if synthetics, ok := interleavedByUpstream[hash]; ok {
			for j := range synthetics {
				synth := synthetics[j]
				sequence = append(sequence, interleavedEntry{syntheticChange: &synth})
			}

			delete(interleavedByUpstream, hash)
		}
	}

	// Orphaned changes whose upstream-commit wasn't found are dropped —
	// they reference an upstream commit outside the known dist-git history.
	for hash, orphaned := range interleavedByUpstream {
		slog.Warn("Upstream commit referenced by fingerprint change not found in dist-git history; "+
			"dropping",
			"upstreamCommit", hash,
			"count", len(orphaned))
	}

	// Append "top" synthetic commits at the end.
	for i := range top {
		topChange := top[i]
		sequence = append(sequence, interleavedEntry{syntheticChange: &topChange})
	}

	return sequence
}

// replayInterleavedHistory walks the interleaved sequence and creates new
// commit objects with correct tree hashes and parent chains. The first upstream
// commit (import-commit) is kept as-is; subsequent upstream commits are
// recreated with updated parents. Synthetic commits are empty except for the
// very last one, which carries the overlay tree.
func replayInterleavedHistory(
	repo *gogit.Repository,
	sequence []interleavedEntry,
	overlayTreeHash plumbing.Hash,
) error {
	syntheticCount := countSyntheticEntries(sequence)

	var (
		lastHash     plumbing.Hash
		lastTreeHash plumbing.Hash
		syntheticIdx int
	)

	for idx, entry := range sequence {
		if idx == 0 && entry.upstreamCommit != nil {
			lastHash = entry.upstreamCommit.Hash
			lastTreeHash = entry.upstreamCommit.TreeHash

			continue
		}

		if entry.upstreamCommit != nil {
			hash, err := createCommitObject(repo,
				entry.upstreamCommit.TreeHash, lastHash,
				entry.upstreamCommit.Author, entry.upstreamCommit.Committer,
				entry.upstreamCommit.Message)
			if err != nil {
				return fmt.Errorf("failed to replay upstream commit:\n%w", err)
			}

			lastHash = hash
			lastTreeHash = entry.upstreamCommit.TreeHash

			continue
		}

		syntheticIdx++

		isLast := syntheticIdx == syntheticCount

		treeHash := lastTreeHash
		if isLast {
			treeHash = overlayTreeHash
		}

		change := entry.syntheticChange
		author := object.Signature{
			Name:  change.Author,
			Email: change.AuthorEmail,
			When:  unixToTime(change.Timestamp),
		}

		message := fmt.Sprintf("%s\n\nProject commit: %s", change.Message, change.Hash)

		slog.Info("Creating synthetic commit",
			"commit", syntheticIdx,
			"total", syntheticCount,
			"projectHash", change.Hash,
			"upstreamCommit", change.UpstreamCommit,
			"isLast", isLast,
		)

		hash, err := createCommitObject(repo, treeHash, lastHash, author, author, message)
		if err != nil {
			return fmt.Errorf("failed to create synthetic commit %d:\n%w", syntheticIdx, err)
		}

		lastHash = hash
		lastTreeHash = treeHash
	}

	if err := updateHead(repo, lastHash); err != nil {
		return err
	}

	slog.Info("Interleaved synthetic history complete",
		"syntheticCommits", syntheticCount,
		"totalCommits", len(sequence))

	return nil
}

// countSyntheticEntries returns the number of synthetic entries in the sequence.
func countSyntheticEntries(sequence []interleavedEntry) int {
	count := 0

	for _, entry := range sequence {
		if entry.syntheticChange != nil {
			count++
		}
	}

	return count
}

// createCommitObject creates a new commit in the repository's object store with
// the given tree, parent, author, committer, and message.
func createCommitObject(
	repo *gogit.Repository,
	treeHash, parentHash plumbing.Hash,
	author, committer object.Signature,
	message string,
) (plumbing.Hash, error) {
	commit := &object.Commit{
		Author:       author,
		Committer:    committer,
		Message:      message,
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parentHash},
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit:\n%w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit:\n%w", err)
	}

	return hash, nil
}

// updateHead updates the HEAD reference (or the branch it points to) to the
// given commit hash.
func updateHead(repo *gogit.Repository, commitHash plumbing.Hash) error {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return fmt.Errorf("failed to read HEAD reference:\n%w", err)
	}

	// Resolve symbolic ref (e.g., HEAD → refs/heads/main).
	name := plumbing.HEAD
	if head.Type() != plumbing.HashReference {
		name = head.Target()
	}

	ref := plumbing.NewHashReference(name, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update HEAD to %s:\n%w", commitHash, err)
	}

	return nil
}

// buildSyntheticCommits resolves the project repository from the component's
// config file, walks the lock file's git history for fingerprint changes, and
// returns the matching [FingerprintChange] entries sorted chronologically.
// If no fingerprint changes are found, a single default commit is returned.
// The second return value is the import-commit hash from the lock file, used
// to scope the upstream commit walk in [CommitInterleavedHistory].
func buildSyntheticCommits(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	config *projectconfig.ComponentConfig,
	componentName string,
	defaultAuthorEmail string,
) (changes []FingerprintChange, importCommit string, err error) {
	configFilePath, err := resolveConfigFilePath(config, componentName)
	if err != nil {
		slog.Debug("Cannot resolve config file for synthetic commits; skipping",
			"component", componentName, "error", err)

		return nil, "", nil
	}

	projectRepoDir, err := resolveProjectRepoDir(ctx, cmdFactory, configFilePath)
	if err != nil {
		return nil, "", err
	}

	lockRelPath := LockFilePath(componentName)

	// Read the current lock file at HEAD to get the import-commit boundary.
	headHash, err := gitHeadHash(ctx, cmdFactory, projectRepoDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get HEAD hash:\n%w", err)
	}

	headFields, headErr := gitShowLockFile(ctx, cmdFactory, projectRepoDir, headHash, lockRelPath)
	if headErr == nil {
		importCommit = headFields.ImportCommit
	}

	fpChanges, err := FindFingerprintChanges(ctx, cmdFactory, projectRepoDir, lockRelPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to find fingerprint changes for component %#q:\n%w",
			componentName, err)
	}

	if len(fpChanges) == 0 {
		slog.Info("No fingerprint changes found in lock file history; "+
			"creating default commit",
			"component", componentName)

		commit, commitErr := defaultOverlayCommit(ctx, cmdFactory, projectRepoDir, componentName, defaultAuthorEmail)
		if commitErr != nil {
			return nil, "", commitErr
		}

		return []FingerprintChange{commit}, importCommit, nil
	}

	slog.Info("Found fingerprint changes for component",
		"component", componentName,
		"changeCount", len(fpChanges))

	return fpChanges, importCommit, nil
}

// defaultOverlayCommit returns a single [FingerprintChange] entry that represents
// a generic commit when no fingerprint changes exist in the lock file history.
// The [FingerprintChange.UpstreamCommit] is read from the current lock file HEAD.
func defaultOverlayCommit(
	ctx context.Context,
	cmdFactory opctx.CmdFactory,
	projectRepoDir string,
	componentName string,
	defaultAuthorEmail string,
) (FingerprintChange, error) {
	if defaultAuthorEmail == "" {
		slog.Warn("No default author email configured; synthetic commit will have an empty author email",
			"hint", "set 'project.default-author-email' in the project config")
	}

	hash, err := gitHeadHash(ctx, cmdFactory, projectRepoDir)
	if err != nil {
		return FingerprintChange{}, fmt.Errorf("failed to get HEAD hash for default overlay commit:\n%w", err)
	}

	meta, err := gitCommitMetadata(ctx, cmdFactory, projectRepoDir, hash)
	if err != nil {
		return FingerprintChange{}, fmt.Errorf("failed to get HEAD metadata for default overlay commit:\n%w", err)
	}

	// Try to read the lock file at HEAD to get the upstream-commit.
	lockRelPath := LockFilePath(componentName)

	var upstreamCommit string

	fields, lockErr := gitShowLockFile(ctx, cmdFactory, projectRepoDir, hash, lockRelPath)
	if lockErr == nil {
		upstreamCommit = fields.UpstreamCommit
	}

	return FingerprintChange{
		CommitMetadata: CommitMetadata{
			Hash:        hash,
			Author:      "azldev",
			AuthorEmail: defaultAuthorEmail,
			Timestamp:   meta.Timestamp,
			Message:     "Latest state for " + componentName,
		},
		UpstreamCommit: upstreamCommit,
	}, nil
}

// resolveConfigFilePath extracts and validates the source config file path from
// the component config.
func resolveConfigFilePath(config *projectconfig.ComponentConfig, componentName string) (string, error) {
	configFile := config.SourceConfigFile
	if configFile == nil {
		return "", fmt.Errorf("component %#q has no source config file reference", componentName)
	}

	configFilePath := configFile.SourcePath()
	if configFilePath == "" {
		return "", fmt.Errorf("component %#q source config file has no path", componentName)
	}

	return configFilePath, nil
}

// resolveProjectRepoDir returns the root directory of the git repository
// containing configFilePath by running 'git rev-parse --show-toplevel'.
func resolveProjectRepoDir(
	ctx context.Context, cmdFactory opctx.CmdFactory, configFilePath string,
) (string, error) {
	var stderr bytes.Buffer

	rawCmd := exec.CommandContext(ctx, "git", "-C", filepath.Dir(configFilePath),
		"rev-parse", "--show-toplevel")
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return "", fmt.Errorf("failed to create git command:\n%w", err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to find project repository for config file %#q:\n%v\n%w",
			configFilePath, stderr.String(), err)
	}

	return strings.TrimSpace(output), nil
}

// collectUpstreamCommits returns commits in the repository in chronological
// order (oldest first). When importCommit is non-empty, only commits from
// importCommit (inclusive) onward are returned. When empty, the full history
// is returned.
func collectUpstreamCommits(repo *gogit.Repository, importCommit string) ([]*object.Commit, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("failed to get HEAD reference:\n%w", err)
	}

	iter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, fmt.Errorf("failed to iterate commit log:\n%w", err)
	}

	var commits []*object.Commit

	err = iter.ForEach(func(c *object.Commit) error {
		commits = append(commits, c)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk commit log:\n%w", err)
	}

	// git log returns newest-first; reverse to chronological.
	slices.Reverse(commits)

	// If an import-commit boundary is set, trim commits before it.
	if importCommit != "" {
		for idx, commit := range commits {
			if commit.Hash.String() == importCommit {
				commits = commits[idx:]

				return commits, nil
			}
		}

		slog.Warn("Import-commit not found in upstream history; using full history",
			"importCommit", importCommit)
	}

	return commits, nil
}

// unixToTime converts a Unix timestamp to a [time.Time] in UTC.
func unixToTime(unix int64) time.Time {
	return time.Unix(unix, 0).UTC()
}

// --- git CLI helpers ---

// gitLogFileHashes returns the commit hashes (newest-first) that touched the
// given file path, scoped to the project repository at repoDir.
func gitLogFileHashes(
	ctx context.Context, cmdFactory opctx.CmdFactory, repoDir, filePath string,
) ([]string, error) {
	var stderr bytes.Buffer

	rawCmd := exec.CommandContext(ctx, "git", "-C", repoDir,
		"log", "--format=%H", "--follow", "--", filePath)
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return nil, fmt.Errorf("failed to create git log command:\n%w", err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list commits for %#q:\n%v\n%w",
			filePath, stderr.String(), err)
	}

	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}

	return strings.Split(output, "\n"), nil
}

// gitShowLockFile reads the lock file content at a specific commit and parses
// the 'upstream-commit' and 'input-fingerprint' TOML fields.
func gitShowLockFile(
	ctx context.Context, cmdFactory opctx.CmdFactory,
	repoDir, commitHash, lockFileRelPath string,
) (lockFileFields, error) {
	var stderr bytes.Buffer

	ref := commitHash + ":" + lockFileRelPath

	rawCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "show", ref)
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return lockFileFields{}, fmt.Errorf("failed to create git show command:\n%w", err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return lockFileFields{}, fmt.Errorf("failed to read lock file at %#q:\n%v\n%w",
			ref, stderr.String(), err)
	}

	var fields lockFileFields
	if err := toml.Unmarshal([]byte(output), &fields); err != nil {
		return lockFileFields{}, fmt.Errorf("failed to parse lock file at %#q:\n%w", ref, err)
	}

	return fields, nil
}

// gitCommitMetadata returns the [CommitMetadata] for a single commit hash.
func gitCommitMetadata(
	ctx context.Context, cmdFactory opctx.CmdFactory, repoDir, commitHash string,
) (CommitMetadata, error) {
	var stderr bytes.Buffer

	// Format: hash, author name, author email, author date (unix), subject.
	rawCmd := exec.CommandContext(ctx, "git", "-C", repoDir,
		"log", "-1", "--format=%H%n%an%n%ae%n%at%n%s", commitHash)
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return CommitMetadata{}, fmt.Errorf("failed to create git log command:\n%w", err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return CommitMetadata{}, fmt.Errorf("failed to get commit metadata for %#q:\n%v\n%w",
			commitHash, stderr.String(), err)
	}

	return ParseCommitMetadata(output)
}

// commitMetadataFieldCount is the number of fields expected in the output of
// 'git log -1 --format=%H%n%an%n%ae%n%at%n%s'.
const commitMetadataFieldCount = 5

// ParseCommitMetadata parses the output of 'git log -1 --format=%H%n%an%n%ae%n%at%n%s'.
func ParseCommitMetadata(output string) (CommitMetadata, error) {
	lines := strings.SplitN(strings.TrimSpace(output), "\n", commitMetadataFieldCount)

	if len(lines) < commitMetadataFieldCount {
		return CommitMetadata{}, fmt.Errorf(
			"unexpected git log output (expected %d lines, got %d):\n%v",
			commitMetadataFieldCount, len(lines), output)
	}

	var timestamp int64
	if _, err := fmt.Sscanf(lines[3], "%d", &timestamp); err != nil {
		return CommitMetadata{}, fmt.Errorf("failed to parse timestamp %#q:\n%w", lines[3], err)
	}

	return CommitMetadata{
		Hash:        lines[0],
		Author:      lines[1],
		AuthorEmail: lines[2],
		Timestamp:   timestamp,
		Message:     lines[4],
	}, nil
}

// gitHeadHash returns the HEAD commit hash of the repository at repoDir.
func gitHeadHash(
	ctx context.Context, cmdFactory opctx.CmdFactory, repoDir string,
) (string, error) {
	var stderr bytes.Buffer

	rawCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "rev-parse", "HEAD")
	rawCmd.Stderr = &stderr

	cmd, err := cmdFactory.Command(rawCmd)
	if err != nil {
		return "", fmt.Errorf("failed to create git rev-parse command:\n%w", err)
	}

	output, err := cmd.RunAndGetOutput(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD hash:\n%v\n%w", stderr.String(), err)
	}

	return strings.TrimSpace(output), nil
}
