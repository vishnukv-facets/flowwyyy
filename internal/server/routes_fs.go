package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (s *Server) handleFSEntries(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	dir, err := expandUIPath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if !info.IsDir() {
		dir = filepath.Dir(dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	out := FSEntriesView{
		Path:        dir,
		DisplayPath: displayUIPath(dir),
		IsGit:       isGitWorkdir(dir),
		Breadcrumbs: fsBreadcrumbs(dir),
		Entries:     []FSEntryView{},
	}
	if parent := filepath.Dir(dir); parent != dir {
		out.Parent = &parent
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			if info, err := os.Stat(child); err == nil {
				isDir = info.IsDir()
			}
		}
		out.Entries = append(out.Entries, FSEntryView{
			Name:        entry.Name(),
			Path:        child,
			DisplayPath: displayUIPath(child),
			IsDir:       isDir,
			IsGit:       isDir && isGitWorkdir(child),
			Hidden:      strings.HasPrefix(entry.Name(), "."),
		})
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	writeJSON(w, out)
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	parent, err := expandUIPath(body.Parent)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	info, err := os.Stat(parent)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if !info.IsDir() {
		writeError(w, fmt.Errorf("parent is not a directory: %s", parent), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, errors.New("directory name is required"), http.StatusBadRequest)
		return
	}
	// Reject anything that escapes the parent: separators, traversals, or
	// hidden quirks like ".." with NULs. A safe directory name is a single
	// path segment.
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		writeError(w, fmt.Errorf("invalid directory name: %q", body.Name), http.StatusBadRequest)
		return
	}
	target := filepath.Join(parent, name)
	if _, err := os.Stat(target); err == nil {
		writeError(w, fmt.Errorf("already exists: %s", displayUIPath(target)), http.StatusConflict)
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, FSEntryView{
		Name:        name,
		Path:        target,
		DisplayPath: displayUIPath(target),
		IsDir:       true,
		IsGit:       false,
		Hidden:      strings.HasPrefix(name, "."),
	})
}

func expandUIPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch {
	case raw == "", raw == "~":
		return home, nil
	case strings.HasPrefix(raw, "~/"):
		return filepath.Clean(filepath.Join(home, strings.TrimPrefix(raw, "~/"))), nil
	case filepath.IsAbs(raw):
		return filepath.Clean(raw), nil
	default:
		return filepath.Abs(raw)
	}
}

func displayUIPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err == nil && rel == "." {
		return "~"
	}
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}

func fsBreadcrumbs(path string) []FSBreadcrumb {
	home, err := os.UserHomeDir()
	if err == nil {
		rel, relErr := filepath.Rel(home, path)
		if relErr == nil && rel == "." {
			return []FSBreadcrumb{{Name: "~", Path: home}}
		}
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			crumbs := []FSBreadcrumb{{Name: "~", Path: home}}
			cursor := home
			for _, part := range strings.Split(rel, string(os.PathSeparator)) {
				if part == "" {
					continue
				}
				cursor = filepath.Join(cursor, part)
				crumbs = append(crumbs, FSBreadcrumb{Name: part, Path: cursor})
			}
			return crumbs
		}
	}

	volume := filepath.VolumeName(path)
	root := string(os.PathSeparator)
	if volume != "" {
		root = volume + string(os.PathSeparator)
	}
	crumbs := []FSBreadcrumb{{Name: root, Path: root}}
	rel := strings.TrimPrefix(path, root)
	cursor := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		cursor = filepath.Join(cursor, part)
		crumbs = append(crumbs, FSBreadcrumb{Name: part, Path: cursor})
	}
	return crumbs
}

func isGitWorkdir(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}
