package acpclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const dataMountPath = "/data"

func resolveRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	eval, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root symlinks: %w", err)
	}
	return filepath.Clean(eval), nil
}

// ResolvePathUnderRoot resolves raw as a host path under root and rejects
// lexical and symlink escapes. The local /data alias is accepted for parity
// with existing workspace tools, but ACP still receives the real host path.
func ResolvePathUnderRoot(root, raw string) (string, error) {
	rootEval, err := resolveRoot(root)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(raw)
	switch {
	case target == "":
		target = rootEval
	case target == dataMountPath:
		target = rootEval
	case strings.HasPrefix(target, dataMountPath+"/"):
		target = filepath.Join(rootEval, strings.TrimPrefix(target, dataMountPath+"/"))
	case filepath.IsAbs(target):
		target = filepath.Clean(target)
	default:
		target = filepath.Join(rootEval, target)
	}
	return resolveExistingAware(rootEval, target)
}

func resolveExistingAware(root, target string) (string, error) {
	targetAbs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	existing := targetAbs
	var suffix []string
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("path %q has no existing parent", target)
		}
		suffix = append([]string{filepath.Base(existing)}, suffix...)
		existing = parent
	}

	evalExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve path symlinks: %w", err)
	}
	resolved := filepath.Clean(evalExisting)
	for _, part := range suffix {
		resolved = filepath.Join(resolved, part)
	}
	if !isUnderRoot(root, resolved) {
		return "", fmt.Errorf("path %q escapes workspace root %q", rawForError(target), root)
	}
	return resolved, nil
}

func isUnderRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func rawForError(path string) string {
	if path == "" {
		return "."
	}
	return path
}
