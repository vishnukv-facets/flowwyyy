// Package gitremote detects a checkout's origin remote URL from its .git
// config. It is pure (filesystem + regexp only) and imports NO flowdb, so both
// core (workdirreg, ghpr) and the flowwyyy product layer (monitor) can use it
// without dragging in the DB layer. Extracted from internal/workdirreg in T13
// so product packages reach git-remote detection without transitively importing
// flowdb (Phase-3 ownership model, seam §11). The DB-backed workdir registry
// stays in internal/workdirreg.
package gitremote

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var gitRemoteURLRE = regexp.MustCompile(`^\s*url\s*=\s*(.+?)\s*$`)

// DetectGitRemote reads <path>/.git/config and extracts the origin remote URL.
// It handles .git being either a directory or a git-worktree pointer file.
func DetectGitRemote(path string) string {
	configPath := resolveGitConfigPath(path)
	if configPath == "" {
		return ""
	}
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inOrigin := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inOrigin = trimmed == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		if m := gitRemoteURLRE.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func resolveGitConfigPath(path string) string {
	gitPath := filepath.Join(path, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return filepath.Join(gitPath, "config")
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	// A linked worktree's gitdir holds per-worktree state only (HEAD, index,
	// commondir); the shared config — including remotes — lives in the common
	// dir, located via the `commondir` pointer file. Follow it so origin
	// detection works from a worktree checkout, not just the main working
	// tree. A plain .git-file gitdir (e.g. a submodule) has no commondir and
	// keeps its own config, so we fall through to <target>/config there.
	if cd, err := os.ReadFile(filepath.Join(target, "commondir")); err == nil {
		if common := strings.TrimSpace(string(cd)); common != "" {
			if !filepath.IsAbs(common) {
				common = filepath.Join(target, common)
			}
			return filepath.Join(common, "config")
		}
	}
	return filepath.Join(target, "config")
}
