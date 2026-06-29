package server

import (
	"flow/internal/flowdb"
	"path/filepath"
)

func (s *Server) uiTrash() uiTrash {
	var out uiTrash
	tasks, _ := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{Kind: "", IncludeArchived: true, DeletedOnly: true})
	for _, task := range tasks {
		out.Tasks = append(out.Tasks, uiTrashItem{
			Kind:      "task",
			Slug:      task.Slug,
			Name:      task.Name,
			Status:    task.Status,
			Priority:  task.Priority,
			Project:   nullStringPtr(task.ProjectSlug),
			WorkDir:   task.WorkDir,
			DeletedAt: task.DeletedAt.String,
			Archived:  task.ArchivedAt.Valid,
		})
	}
	projects, _ := flowdb.ListProjects(s.cfg.DB, flowdb.ProjectFilter{IncludeArchived: true, DeletedOnly: true})
	for _, project := range projects {
		out.Projects = append(out.Projects, uiTrashItem{
			Kind:      "project",
			Slug:      project.Slug,
			Name:      project.Name,
			Status:    project.Status,
			Priority:  project.Priority,
			WorkDir:   project.WorkDir,
			DeletedAt: project.DeletedAt.String,
			Archived:  project.ArchivedAt.Valid,
		})
	}
	playbooks, _ := flowdb.ListPlaybooks(s.cfg.DB, flowdb.PlaybookFilter{IncludeArchived: true, DeletedOnly: true})
	for _, playbook := range playbooks {
		out.Playbooks = append(out.Playbooks, uiTrashItem{
			Kind:      "playbook",
			Slug:      playbook.Slug,
			Name:      playbook.Name,
			Project:   nullStringPtr(playbook.ProjectSlug),
			WorkDir:   playbook.WorkDir,
			DeletedAt: playbook.DeletedAt.String,
			Archived:  playbook.ArchivedAt.Valid,
		})
	}
	out.Total = len(out.Tasks) + len(out.Projects) + len(out.Playbooks)
	return out
}

func (s *Server) uiProjects() ([]uiProject, error) {
	projects, err := flowdb.ListProjects(s.cfg.DB, flowdb.ProjectFilter{})
	if err != nil {
		return nil, err
	}
	views, err := BuildProjectViews(s.cfg.DB, s.cfg.FlowRoot, projects)
	if err != nil {
		return nil, err
	}
	out := make([]uiProject, 0, len(views))
	for _, p := range views {
		out = append(out, uiProject{
			Slug:     p.Slug,
			Name:     p.Name,
			Priority: p.Priority,
			Tasks: uiTaskCounts{
				Total:      p.TaskCounts.Total,
				InProgress: p.TaskCounts.InProgress,
				Backlog:    p.TaskCounts.Backlog,
				Done:       p.TaskCounts.Done,
			},
			WorkDir: p.WorkDir,
		})
	}
	return out, nil
}

func (s *Server) uiPlaybooks() ([]uiPlaybook, error) {
	playbooks, err := flowdb.ListPlaybooks(s.cfg.DB, flowdb.PlaybookFilter{})
	if err != nil {
		return nil, err
	}
	views, err := BuildPlaybookViews(s.cfg.DB, s.cfg.FlowRoot, playbooks)
	if err != nil {
		return nil, err
	}
	out := make([]uiPlaybook, 0, len(views))
	for _, p := range views {
		var lastMin *int
		if len(p.RecentRuns) > 0 {
			v := minutesSince(p.RecentRuns[0].CreatedAt)
			lastMin = &v
		}
		// RecentRuns is newest-first; the strip reads oldest→newest left-to-right,
		// so take the most recent 16 and reverse them.
		const maxRuns = 16
		recent := p.RecentRuns
		if len(recent) > maxRuns {
			recent = recent[:maxRuns]
		}
		runs := make([]uiPlaybookRun, 0, len(recent))
		for i := len(recent) - 1; i >= 0; i-- {
			r := recent[i]
			runs = append(runs, uiPlaybookRun{Name: r.Name, Status: r.Status, CreatedAt: r.CreatedAt})
		}
		out = append(out, uiPlaybook{
			Slug:               p.Slug,
			Name:               p.Name,
			Project:            p.ProjectSlug,
			RunsWeek:           p.RunCount7d,
			LastMin:            lastMin,
			Spark:              lastSevenFromThirty(p.RunDays30),
			Runs:               runs,
			WorkDir:            p.WorkDir,
			Schedule:           p.Schedule,
			SchedulePaused:     p.SchedulePaused,
			ScheduleHoldReason: p.ScheduleHoldReason,
			ScheduleHoldUntil:  p.ScheduleHoldUntil,
			NextFireAt:         p.NextFireAt,
		})
	}
	return out, nil
}

func (s *Server) uiWorkdirs() ([]uiWorkdir, error) {
	workdirs, err := flowdb.ListWorkdirs(s.cfg.DB)
	if err != nil {
		return nil, err
	}
	views := BuildWorkdirViews(s.cfg.DB, workdirs)
	out := make([]uiWorkdir, 0, len(views))
	for _, w := range views {
		name := filepath.Base(w.Path)
		usedMin := 0
		if w.LastUsedAt != nil {
			usedMin = minutesSince(*w.LastUsedAt)
		}
		out = append(out, uiWorkdir{
			Path:      w.Path,
			Name:      name,
			Remote:    w.GitRemote,
			UsedMin:   usedMin,
			Tasks:     w.TasksUsingThis,
			Untouched: w.Untouched30d,
		})
	}
	return out, nil
}

func (s *Server) uiKBFiles() []uiKBFile {
	out := []uiKBFile{}
	for _, path := range kbFiles(s.cfg.FlowRoot) {
		view := BuildKBFileView(path)
		entries := readKBEntries(path)
		if entries == nil {
			entries = []uiKBEntry{}
		}
		count := view.Entries
		if count == 0 {
			count = len(entries)
		}
		preview := view.Preview
		if preview == "" && len(entries) > 0 {
			preview = entries[len(entries)-1].D + " - " + entries[len(entries)-1].T
		}
		out = append(out, uiKBFile{Name: view.Filename, Preview: preview, Count: count, Entries: entries})
	}
	return out
}
