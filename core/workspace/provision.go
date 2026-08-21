package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const bindingMarkerName = ".zotigo-binding.json"

type bindingMarker struct {
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
	SourceID    string `json:"source_id"`
}

type gitWorktree struct {
	Path   string
	Branch string
}

func (s *Store) ProvisionWorkspace(ctx context.Context, workspaceID string) (Workspace, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	workspace, nonce, err := s.workspaceWithNonce(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if err := s.requireActiveProject(ctx, workspace.ProjectID); err != nil {
		return Workspace{}, err
	}
	if workspace.Status == WorkspaceStatusReady {
		if err := validateOwnerMarker(workspace.RootPath, workspace.ProjectID, workspace.ID, nonce); err != nil {
			return Workspace{}, err
		}
		checkouts, folders, err := s.workspaceBindings(ctx, workspace.ID)
		if err != nil {
			return Workspace{}, err
		}
		for _, checkout := range checkouts {
			if err := s.provisionCheckout(ctx, workspace, checkout); err != nil {
				_ = s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "error", err.Error())
				if cleanupErr := s.cancelUnownedCheckout(ctx, workspace, checkout); cleanupErr != nil {
					return Workspace{}, errors.Join(err, cleanupErr)
				}
				return Workspace{}, err
			}
			if err := s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "ready", ""); err != nil {
				return Workspace{}, err
			}
		}
		for _, folder := range folders {
			if err := s.provisionFolder(ctx, workspace, folder); err != nil {
				_ = s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "error", err.Error())
				if cleanupErr := s.cancelFailedFolderBinding(ctx, workspace, folder); cleanupErr != nil {
					return Workspace{}, errors.Join(err, cleanupErr)
				}
				return Workspace{}, err
			}
			if err := s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "ready", ""); err != nil {
				return Workspace{}, err
			}
		}
		return workspace, nil
	}
	if workspace.Status != WorkspaceStatusProvisioning && workspace.Status != WorkspaceStatusError {
		return Workspace{}, fmt.Errorf("%w: workspace cannot be provisioned from %s", ErrConflict, workspace.Status)
	}
	if workspace.Status == WorkspaceStatusError {
		if err := s.setWorkspaceStatus(ctx, workspace.ID, WorkspaceStatusProvisioning, ""); err != nil {
			return Workspace{}, err
		}
		workspace.Status = WorkspaceStatusProvisioning
		workspace.Error = ""
	}
	if err := provisionScaffold(workspace, nonce); err != nil {
		return s.failWorkspaceProvision(ctx, workspace.ID, err)
	}
	checkouts, folders, err := s.workspaceBindings(ctx, workspace.ID)
	if err != nil {
		return s.failWorkspaceProvision(ctx, workspace.ID, err)
	}
	for _, checkout := range checkouts {
		if err := s.provisionCheckout(ctx, workspace, checkout); err != nil {
			_ = s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "error", err.Error())
			return s.failWorkspaceProvision(ctx, workspace.ID, err)
		}
		if err := s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "ready", ""); err != nil {
			return s.failWorkspaceProvision(ctx, workspace.ID, err)
		}
	}
	for _, folder := range folders {
		if err := s.provisionFolder(ctx, workspace, folder); err != nil {
			_ = s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "error", err.Error())
			return s.failWorkspaceProvision(ctx, workspace.ID, err)
		}
		if err := s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "ready", ""); err != nil {
			return s.failWorkspaceProvision(ctx, workspace.ID, err)
		}
	}
	if err := s.setWorkspaceStatus(ctx, workspace.ID, WorkspaceStatusReady, ""); err != nil {
		return Workspace{}, err
	}
	return s.GetWorkspace(ctx, workspace.ID)
}

func (s *Store) failWorkspaceProvision(ctx context.Context, workspaceID string, cause error) (Workspace, error) {
	_ = s.setWorkspaceStatus(ctx, workspaceID, WorkspaceStatusError, cause.Error())
	return Workspace{}, cause
}

func (s *Store) provisionCheckout(ctx context.Context, workspace Workspace, checkout Checkout) error {
	if err := s.validateWorkspaceBindingTarget(workspace, checkout.WorktreePath); err != nil {
		return err
	}
	source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
	if err != nil {
		return err
	}
	if err := verifySourceIdentity(ctx, source); err != nil {
		return err
	}
	if _, err := runGitMutation(ctx, source.CanonicalPath, "check-ref-format", "--branch", checkout.BranchName); err != nil {
		return fmt.Errorf("%w: invalid workspace branch", ErrInvalid)
	}
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	branchRef := "refs/heads/" + checkout.BranchName
	ownershipRef := checkoutOwnershipRef(workspace.ID, source.SourceKey)
	var exact *gitWorktree
	for index := range worktrees {
		candidate := worktrees[index]
		if samePath(candidate.Path, checkout.WorktreePath) {
			exact = &candidate
			continue
		}
		if candidate.Branch == branchRef {
			return fmt.Errorf("%w: workspace branch is used by another worktree", ErrConflict)
		}
	}
	if exact != nil {
		if exact.Branch != branchRef {
			return fmt.Errorf("%w: workspace target belongs to another branch", ErrConflict)
		}
		if err := verifyCheckoutOwnership(ctx, source, checkout, ownershipRef); err != nil {
			return err
		}
		info, statErr := os.Lstat(checkout.WorktreePath)
		if statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: workspace checkout target is not a directory", ErrConflict)
			}
			return nil
		}
		if !os.IsNotExist(statErr) {
			return statErr
		}
		_, _ = runGitMutation(ctx, source.CanonicalPath, "worktree", "unlock", checkout.WorktreePath)
		if _, err := runGitMutation(ctx, source.CanonicalPath, "worktree", "remove", "--force", checkout.WorktreePath); err != nil {
			return fmt.Errorf("remove missing workspace worktree registration: %w", err)
		}
	}
	if _, err := os.Lstat(checkout.WorktreePath); err == nil {
		return fmt.Errorf("%w: workspace checkout target is occupied", ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	branchHead, branchErr := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", branchRef+"^{commit}")
	ownershipHead, ownershipErr := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", ownershipRef+"^{commit}")
	reason := "zotigo workspace " + workspace.ID
	if ownershipErr == nil {
		if branchErr != nil || strings.TrimSpace(ownershipHead) != checkout.BaseCommit ||
			(checkout.Status != "ready" && strings.TrimSpace(branchHead) != checkout.BaseCommit) {
			return fmt.Errorf("%w: workspace branch ownership does not match", ErrConflict)
		}
	} else {
		if branchErr == nil {
			return fmt.Errorf("%w: workspace branch already exists without Zotigo ownership", ErrConflict)
		}
		resolved, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", checkout.BaseRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("%w: base ref is unavailable", ErrConflict)
		}
		if strings.TrimSpace(resolved) != checkout.BaseCommit {
			return fmt.Errorf("%w: base ref changed", ErrConflict)
		}
		commands := "create " + branchRef + " " + checkout.BaseCommit + "\n" +
			"create " + ownershipRef + " " + checkout.BaseCommit + "\n"
		if _, err := runGitMutationInput(ctx, source.CanonicalPath, commands, "update-ref", "--stdin"); err != nil {
			return fmt.Errorf("create workspace branch refs: %w", err)
		}
	}
	_, err = runGitMutation(ctx, source.CanonicalPath, "worktree", "add", "--lock", "--reason", reason,
		checkout.WorktreePath, checkout.BranchName)
	if err != nil {
		return fmt.Errorf("create workspace worktree: %w", err)
	}
	return nil
}

func checkoutOwnershipRef(workspaceID string, sourceKey string) string {
	return "refs/zotigo/workspaces/" + workspaceID + "/" + sourceKey
}

func verifyCheckoutOwnership(ctx context.Context, source Source, checkout Checkout, ownershipRef string) error {
	head, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", ownershipRef+"^{commit}")
	if err != nil || strings.TrimSpace(head) != checkout.BaseCommit {
		return fmt.Errorf("%w: workspace branch ownership is missing or changed", ErrConflict)
	}
	return nil
}

func samePath(left string, right string) bool {
	canonical := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		} else if absolute, absErr := filepath.Abs(path); absErr == nil {
			current := absolute
			missing := make([]string, 0, 2)
			for {
				if resolvedParent, resolveErr := filepath.EvalSymlinks(current); resolveErr == nil {
					path = resolvedParent
					for index := len(missing) - 1; index >= 0; index-- {
						path = filepath.Join(path, missing[index])
					}
					break
				}
				parent := filepath.Dir(current)
				if parent == current {
					break
				}
				missing = append(missing, filepath.Base(current))
				current = parent
			}
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		return filepath.Clean(path)
	}
	return canonical(left) == canonical(right)
}

func (s *Store) provisionFolder(ctx context.Context, workspace Workspace, binding FolderBinding) error {
	if err := s.validateWorkspaceBindingTarget(workspace, binding.TargetPath); err != nil {
		return err
	}
	source, err := s.GetSource(ctx, workspace.ProjectID, binding.SourceID)
	if err != nil {
		return err
	}
	if err := verifySourceIdentity(ctx, source); err != nil {
		return err
	}
	if binding.Mode == FolderModeDirect {
		return ensureDirectBinding(binding.TargetPath, source.CanonicalPath)
	}
	return ensureCopiedBinding(workspace, binding, source.CanonicalPath)
}

func ensureDirectBinding(target string, source string) error {
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%w: folder binding target is occupied", ErrConflict)
		}
		destination, err := filepath.EvalSymlinks(target)
		if err != nil {
			return err
		}
		if filepath.Clean(destination) != filepath.Clean(source) {
			return fmt.Errorf("%w: folder binding points elsewhere", ErrConflict)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(source, target); err != nil {
		return fmt.Errorf("create direct folder binding: %w", err)
	}
	return nil
}

func ensureCopiedBinding(workspace Workspace, binding FolderBinding, source string) error {
	if info, err := os.Lstat(binding.TargetPath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: copied folder binding target is not a managed directory", ErrConflict)
		}
		resolvedTarget, resolveErr := filepath.EvalSymlinks(binding.TargetPath)
		if resolveErr != nil {
			return fmt.Errorf("%w: resolve copied folder binding target: %v", ErrConflict, resolveErr)
		}
		resolvedParent, resolveErr := filepath.EvalSymlinks(filepath.Dir(binding.TargetPath))
		if resolveErr != nil || filepath.Dir(resolvedTarget) != resolvedParent {
			return fmt.Errorf("%w: copied folder binding target escapes its managed parent", ErrConflict)
		}
		return validateBindingMarker(binding.TargetPath, workspace.ID, binding.SourceID)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(source, bindingMarkerName)); err == nil {
		return fmt.Errorf("%w: source contains reserved marker name", ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	staging := binding.TargetPath + ".staging"
	if _, err := os.Lstat(staging); err == nil {
		if err := validateBindingMarker(staging, workspace.ID, binding.SourceID); err != nil {
			return err
		}
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := writeBindingMarker(staging, workspace.ID, binding.SourceID); err != nil {
		return err
	}
	if err := copyDirectoryContents(source, staging, source, binding.Mode == FolderModeReference); err != nil {
		return err
	}
	if err := os.Rename(staging, binding.TargetPath); err != nil {
		return fmt.Errorf("publish folder binding: %w", err)
	}
	published = true
	return nil
}

func copyDirectoryContents(source string, target string, sourceRoot string, readOnly bool) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if source == sourceRoot && entry.Name() == bindingMarkerName {
			return fmt.Errorf("%w: source contains reserved marker name", ErrConflict)
		}
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsDir():
			if err := os.Mkdir(targetPath, info.Mode().Perm()); err != nil {
				return err
			}
			if err := copyDirectoryContents(sourcePath, targetPath, sourceRoot, readOnly); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRegularFile(sourcePath, targetPath, info.Mode().Perm(), readOnly); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("%w: absolute source symlink is not portable", ErrConflict)
			}
			resolved, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(sourcePath), link))
			if err != nil {
				return fmt.Errorf("resolve source symlink: %w", err)
			}
			relative, err := filepath.Rel(sourceRoot, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
				return fmt.Errorf("%w: source symlink escapes source", ErrConflict)
			}
			if err := os.Symlink(link, targetPath); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported source entry %s", ErrConflict, sourcePath)
		}
	}
	return nil
}

func copyRegularFile(source string, target string, mode os.FileMode, readOnly bool) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	if readOnly {
		mode &^= 0o222
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func writeBindingMarker(root string, workspaceID string, sourceID string) error {
	data, err := json.Marshal(bindingMarker{Version: 1, WorkspaceID: workspaceID, SourceID: sourceID})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, bindingMarkerName), data, 0o600)
}

func validateBindingMarker(root string, workspaceID string, sourceID string) error {
	data, err := os.ReadFile(filepath.Join(root, bindingMarkerName))
	if err != nil {
		return fmt.Errorf("%w: binding marker missing", ErrConflict)
	}
	var marker bindingMarker
	if err := json.Unmarshal(data, &marker); err != nil || marker.Version != 1 ||
		marker.WorkspaceID != workspaceID || marker.SourceID != sourceID {
		return fmt.Errorf("%w: binding marker does not match", ErrConflict)
	}
	return nil
}

func listGitWorktrees(ctx context.Context, sourcePath string) ([]gitWorktree, error) {
	output, err := runGitMutation(ctx, sourcePath, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	blocks := strings.Split(strings.TrimSpace(output), "\n\n")
	result := make([]gitWorktree, 0, len(blocks))
	for _, block := range blocks {
		var worktree gitWorktree
		for _, line := range strings.Split(block, "\n") {
			key, value, found := strings.Cut(line, " ")
			if !found {
				continue
			}
			switch key {
			case "worktree":
				worktree.Path = value
			case "branch":
				worktree.Branch = value
			}
		}
		if worktree.Path != "" {
			result = append(result, worktree)
		}
	}
	return result, nil
}

func runGitMutation(ctx context.Context, directory string, args ...string) (string, error) {
	return runGitMutationInput(ctx, directory, "", args...)
}

func runGitMutationInput(ctx context.Context, directory string, input string, args ...string) (string, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	commandArgs := append([]string{"-C", directory}, args...)
	cmd := exec.CommandContext(gitCtx, "git", commandArgs...)
	cmd.Env = sanitizedGitEnvironment(os.Environ())
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	output, err := cmd.CombinedOutput()
	if len(output) > 256*1024 {
		return "", fmt.Errorf("git output exceeded limit")
	}
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return string(output), nil
}
