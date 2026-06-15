package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func gitBranch(dir string) string {
	out, err := runGit(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// cachedGitBranch wraps gitBranch with a per-server TTL cache so the hot
// uiAgent loop doesn't fork `git rev-parse` once per task per SSE tick.
func (s *Server) cachedGitBranch(dir string) string {
	if s == nil || s.caches == nil || dir == "" {
		return gitBranch(dir)
	}
	if v, ok := s.caches.gitBranch.get(dir); ok {
		return v
	}
	v := gitBranch(dir)
	s.caches.gitBranch.set(dir, v)
	return v
}

// cachedGitBranches wraps gitBranches with a per-server TTL cache. Keyed by
// dir+current so a branch switch (which changes `current`) immediately gets a
// fresh list; same-branch repeats within the 5s window are free.
func (s *Server) cachedGitBranches(dir, current string) []string {
	if s == nil || s.caches == nil || dir == "" {
		return gitBranches(dir, current)
	}
	key := dir + "\x00" + current
	if v, ok := s.caches.gitBranches.get(key); ok {
		return v
	}
	v := gitBranches(dir, current)
	s.caches.gitBranches.set(key, v)
	return v
}

// cachedGitDiff wraps gitDiff with a per-server TTL cache. gitDiff fans out
// into 3-12 git invocations depending on the diff size, so caching across an
// SSE tick is the highest-leverage win in this whole file.
func (s *Server) cachedGitDiff(dir string) (uiDiff, []uiDiffFile) {
	if s == nil || s.caches == nil || dir == "" {
		return gitDiff(dir)
	}
	if v, ok := s.caches.gitDiff.get(dir); ok {
		return v.diff, v.files
	}
	diff, files := gitDiff(dir)
	s.caches.gitDiff.set(dir, gitDiffSnapshot{diff: diff, files: files})
	return diff, files
}

func gitBranches(dir, current string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(branch string) {
		branch = strings.TrimSpace(branch)
		if branch == "" || branch == "HEAD" || branch == "origin" || strings.HasSuffix(branch, "/HEAD") || seen[branch] {
			return
		}
		seen[branch] = true
		out = append(out, branch)
	}
	add(current)
	if branches, err := runGit(dir, "branch", "--format=%(refname:short)"); err == nil {
		for _, line := range strings.Split(string(branches), "\n") {
			add(line)
		}
	}
	if branches, err := runGit(dir, "branch", "-r", "--format=%(refname:short)"); err == nil {
		for _, line := range strings.Split(string(branches), "\n") {
			add(line)
		}
	}
	return out
}

func gitDiff(dir string) (uiDiff, []uiDiffFile) {
	var diff uiDiff
	filesByName := map[string]*uiDiffFile{}
	order := []string{}
	addNumstat := func(cached bool) {
		args := []string{"diff", "--numstat"}
		if cached {
			args = append(args, "--cached")
		}
		out, err := runGit(dir, args...)
		if err != nil {
			return
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) < 3 {
				continue
			}
			add, _ := strconv.Atoi(parts[0])
			rem, _ := strconv.Atoi(parts[1])
			name := parts[2]
			f := filesByName[name]
			if f == nil {
				f = &uiDiffFile{Name: name}
				filesByName[name] = f
				order = append(order, name)
			}
			f.Add += add
			f.Rem += rem
			diff.Add += add
			diff.Rem += rem
		}
	}
	addNumstat(false)
	addNumstat(true)
	for _, name := range order {
		f := filesByName[name]
		if len(f.Hunks) == 0 {
			f.Hunks = gitDiffHunks(dir, name, false)
		}
		if len(f.Hunks) == 0 {
			f.Hunks = gitDiffHunks(dir, name, true)
		}
		diff.Files++
	}
	for _, name := range gitUntrackedFiles(dir) {
		if _, ok := filesByName[name]; ok {
			continue
		}
		f := untrackedDiffFile(dir, name)
		filesByName[name] = &f
		order = append(order, name)
		diff.Add += f.Add
		diff.Rem += f.Rem
		diff.Files++
	}
	files := make([]uiDiffFile, 0, len(order))
	for _, name := range order {
		if f := filesByName[name]; f != nil {
			files = append(files, *f)
		}
	}
	return diff, files
}

func gitDiffHunks(dir, file string, cached bool) []uiDiffHunk {
	args := []string{"diff", "--unified=3"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--", file)
	out, err := runGit(dir, args...)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	hunks := []uiDiffHunk{}
	oldLine, newLine := 0, 0
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			if len(hunks) >= 6 {
				break
			}
			match := gitHunkHeaderRe.FindStringSubmatch(line)
			oldLine, newLine = 0, 0
			if len(match) == 3 {
				oldLine, _ = strconv.Atoi(match[1])
				newLine, _ = strconv.Atoi(match[2])
			}
			hunks = append(hunks, uiDiffHunk{Header: line})
			continue
		}
		if len(hunks) == 0 || strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			continue
		}
		if len(hunks[len(hunks)-1].Lines) >= 120 {
			continue
		}
		kind, num := "ctx", ""
		var code string // assigned on every path of the switch below
		switch {
		case strings.HasPrefix(line, "+"):
			kind = "add"
			code = strings.TrimPrefix(line, "+")
			num = strconv.Itoa(newLine)
			newLine++
		case strings.HasPrefix(line, "-"):
			kind = "rem"
			code = strings.TrimPrefix(line, "-")
			num = strconv.Itoa(oldLine)
			oldLine++
		default:
			code = strings.TrimPrefix(line, " ")
			if oldLine > 0 {
				num = strconv.Itoa(oldLine)
			}
			oldLine++
			newLine++
		}
		hunks[len(hunks)-1].Lines = append(hunks[len(hunks)-1].Lines, uiDiffLine{Type: kind, N: num, Code: code})
	}
	return hunks
}

func gitUntrackedFiles(dir string) []string {
	out, err := runGit(dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	if len(files) > 20 {
		files = files[:20]
	}
	return files
}

func untrackedDiffFile(dir, name string) uiDiffFile {
	f := uiDiffFile{Name: name}
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > 128*1024 {
		f.Hunks = []uiDiffHunk{{Header: "@@ untracked file @@", Lines: []uiDiffLine{{Type: "add", Code: "untracked file"}}}}
		return f
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return f
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) > 120 {
		lines = lines[:120]
	}
	h := uiDiffHunk{Header: "@@ untracked file @@"}
	for i, line := range lines {
		if strings.ContainsRune(line, '\x00') {
			h.Lines = []uiDiffLine{{Type: "add", Code: "binary or non-text file"}}
			break
		}
		h.Lines = append(h.Lines, uiDiffLine{Type: "add", N: strconv.Itoa(i + 1), Code: line})
	}
	f.Add = len(h.Lines)
	f.Hunks = []uiDiffHunk{h}
	return f
}

func runGit(dir string, args ...string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("empty workdir")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("workdir unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	return cmd.Output()
}
