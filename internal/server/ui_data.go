package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/user"
	"regexp"
	"sort"
	"strings"
	"time"

	"flow/internal/flowdb"
)

type uiData struct {
	Agents           []uiAgent        `json:"AGENTS"`
	DeadAgent        *uiAgent         `json:"DEAD_AGENT"`
	DoneAgents       []uiAgent        `json:"DONE_AGENTS"`
	Backlog          []uiBacklogTask  `json:"BACKLOG"`
	DoneTasks        []uiBacklogTask  `json:"DONE_TASKS"`
	KBFiles          []uiKBFile       `json:"KB_FILES"`
	MemorySources    []uiMemorySource `json:"AGENT_MEMORY_SOURCES"`
	Workdirs         []uiWorkdir      `json:"WORKDIRS"`
	Playbooks        []uiPlaybook     `json:"PLAYBOOKS_MC"`
	Projects         []uiProject      `json:"PROJECTS_MC"`
	ActivityHeatmap  []uiActivityDay  `json:"ACTIVITY_HEATMAP"`
	TokenSeries      []uiTokenDay     `json:"TOKEN_SERIES"`
	TopTasks         []uiTopTask      `json:"TOP_TASKS,omitempty"`
	ModelMix         []uiModelCount   `json:"MODEL_MIX,omitempty"`
	Stats            uiStats          `json:"STATS"`
	Capabilities     uiCapabilities   `json:"CAPABILITIES"`
	Trash            uiTrash          `json:"TRASH"`
	SampleTranscript []uiTranscript   `json:"SAMPLE_TRANSCRIPT"`
	SampleDiffFiles  []uiDiffFile     `json:"SAMPLE_DIFF_FILES"`
	FlowDB           uiFlowDB         `json:"FLOWDB"`
	User             uiUser           `json:"USER"`
	// FloatingSessions are adhoc Ask Flow terminals registered server-side.
	// The operator console renders one tray chip per entry so they survive
	// navigation and reloads (the window state lives client-side).
	FloatingSessions []floatingSessionInfo `json:"FLOATING_SESSIONS"`
}

// floatingSessionInfo is one adhoc floating (Ask Flow) session as surfaced to
// the tray. Running reflects whether its PTY is currently attached.
type floatingSessionInfo struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	Title      string `json:"title"`
	Running    bool   `json:"running"`
	Waiting    bool   `json:"waiting"`
	WaitingWhy string `json:"waiting_why,omitempty"`
	Created    string `json:"created_at"`
}

// uiUser carries the operator's display name so the dashboard can greet
// them. Derived from the OS account; falls back gracefully when unknown.
type uiUser struct {
	Name     string `json:"name"` // first name, for greetings
	FullName string `json:"full_name"`
	Username string `json:"username"`
}

func currentUIUser() uiUser {
	// FLOW_GREETING_NAME overrides the OS-derived name — useful when the OS
	// account name isn't how you want to be greeted (and for demo captures).
	if override := strings.TrimSpace(os.Getenv("FLOW_GREETING_NAME")); override != "" {
		first := override
		if fields := strings.Fields(override); len(fields) > 0 {
			first = fields[0]
		}
		return uiUser{Name: first, FullName: override, Username: first}
	}
	u, err := user.Current()
	if err != nil || u == nil {
		return uiUser{Name: "there"}
	}
	full := strings.TrimSpace(u.Name)
	username := strings.TrimSpace(u.Username)
	// On some platforms Username is "domain\\user" — keep the last segment.
	if i := strings.LastIndexAny(username, `\/`); i >= 0 {
		username = username[i+1:]
	}
	display := full
	if display == "" {
		display = username
	}
	first := display
	if fields := strings.Fields(display); len(fields) > 0 {
		first = fields[0]
	}
	if first == "" {
		first = "there"
	}
	// macOS often reports the account name in ALL CAPS — soften to Title case
	// so the greeting reads "Vishnu", not "VISHNU".
	if first == strings.ToUpper(first) && first != strings.ToLower(first) {
		r := []rune(strings.ToLower(first))
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		first = string(r)
	}
	return uiUser{Name: first, FullName: full, Username: username}
}

// uiFlowDB carries on-disk stats for flow.db (plus a display-friendly
// path) so the UI sidebar can show how much storage flow is using.
type uiFlowDB struct {
	Path                 string            `json:"path"`
	DisplayPath          string            `json:"display_path"`
	Bytes                int64             `json:"bytes"`
	HumanSize            string            `json:"human_size"`
	Exists               bool              `json:"exists"`
	PageSize             int64             `json:"page_size"`
	PageCount            int64             `json:"page_count"`
	FreePageCount        int64             `json:"free_page_count"`
	UsedBytes            int64             `json:"used_bytes"`
	UsedHumanSize        string            `json:"used_human_size"`
	ReclaimableBytes     int64             `json:"reclaimable_bytes"`
	ReclaimableHumanSize string            `json:"reclaimable_human_size"`
	QuickCheck           string            `json:"quick_check"`
	QuickCheckSource     string            `json:"quick_check_source"`
	QuickCheckCheckedAt  string            `json:"quick_check_checked_at"`
	QuickCheckNote       string            `json:"quick_check_note"`
	CanCompact           bool              `json:"can_compact"`
	Explanation          string            `json:"explanation"`
	Objects              []uiFlowDBObject  `json:"objects"`
	Documents            []uiFlowDBDocStat `json:"documents"`
	Error                string            `json:"error,omitempty"`
}

type uiFlowDBObject struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Bytes     int64   `json:"bytes"`
	HumanSize string  `json:"human_size"`
	Percent   float64 `json:"percent"`
}

type uiFlowDBDocStat struct {
	Scope        string `json:"scope"`
	EntityType   string `json:"entity_type"`
	Count        int    `json:"count"`
	ContentBytes int64  `json:"content_bytes"`
	HumanSize    string `json:"human_size"`
}

const (
	defaultFlowDBQuickCheckTimeout = time.Second
	flowDBQuickCheckCacheTTL       = 30 * time.Minute
	// flowDBDiagCacheTTL bounds how often the expensive flow.db diagnostics
	// (PRAGMA quick_check, dbstat object sizing, search_docs content scan) run.
	// Each is O(database size) — multi-second on a multi-hundred-MB flow.db —
	// so they must never run on the per-tick SSE snapshot path. The numbers
	// move slowly (free pages accrue over hours), so a few minutes of staleness
	// is fine; the compact-db action invalidates explicitly for instant updates.
	flowDBDiagCacheTTL = 5 * time.Minute
	// flowDBDiagQuickCheckTimeout is the integrity-check budget for the
	// background refresh. It runs off the hot path, so it can be generous
	// enough to actually finish on a large database (where the short
	// first-paint budget times out and shows "not checked").
	flowDBDiagQuickCheckTimeout = 2 * time.Minute
)

// flowDBDiag is the cached, expensive portion of uiFlowDB. Computed at most
// once per flowDBDiagCacheTTL and copied onto every snapshot so buildUIData
// never re-scans the whole database on the hot SSE path.
type flowDBDiag struct {
	PageSize            int64
	PageCount           int64
	FreePageCount       int64
	UsedBytes           int64
	ReclaimableBytes    int64
	QuickCheck          string
	QuickCheckSource    string
	QuickCheckCheckedAt string
	QuickCheckNote      string
	Objects             []uiFlowDBObject
	Documents           []uiFlowDBDocStat
	Error               string
}

type uiActivityDay struct {
	Date  string   `json:"date"`
	Count int      `json:"count"`
	Tasks []string `json:"tasks,omitempty"`
}

// uiTokenDay is one day of the token-cost-over-time trend: fresh "work" tokens
// (cache-excluded, same basis as TokensSession) summed across every tracked
// session active that day. Aligned to the same 12-week window as the heatmap.
// Tasks carries the per-task breakdown (top contributors by tokens) so the
// activity bar tooltip can show which task burned how many tokens; TaskCount is
// the total number of tasks that contributed, so the tooltip can render
// "+N more" when the list is truncated.
type uiTokenDay struct {
	Date      string        `json:"date"`
	Tokens    int           `json:"tokens"`
	CostUSD   float64       `json:"cost_usd,omitempty"`
	TaskCount int           `json:"task_count,omitempty"`
	Tasks     []uiTokenTask `json:"tasks,omitempty"`
}

// uiTokenTask is one task's token contribution on a single day. Tokens is the
// cache-excluded "work" count; CostUSD is the estimated FULL billed cost (cache
// reads + creation included — see pricing.go), so the two are not proportional
// (cache dominates the bill but not the work count).
type uiTokenTask struct {
	Name    string  `json:"name"`
	Tokens  int     `json:"tokens"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// uiTopTask is one task's total token + estimated-cost contribution across the
// whole 12-week window — the source for the "Top tasks by cost" leaderboard.
// Unlike uiTokenTask (per-day, truncated to a daily top-5), this is summed over
// every day so the ranking is accurate, and it carries the slug so the row can
// deep-link to the session.
type uiTopTask struct {
	Slug     string  `json:"slug"`
	Name     string  `json:"name"`
	Provider string  `json:"provider,omitempty"`
	Tokens   int     `json:"tokens"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
}

// uiModelCount is how many done tasks actually ran on a given model — taken
// from the session transcript (the model the assistant messages report), NOT
// the task's explicit model pin (which is empty for auto-resolved tasks). The
// client normalizes the raw model id to a tier label for the Composition bar.
type uiModelCount struct {
	Model string `json:"model"`
	Count int    `json:"count"`
}

// uiStats are the at-a-glance Mission Control analytics: how consistently the
// operator has been running agents (activity-day streaks, derived from the same
// 12-week heatmap shown on the dashboard) and how many context tokens each
// provider has in play across all tracked sessions.
type uiStats struct {
	CurrentStreak  int     `json:"current_streak"` // consecutive active days ending today
	LongestStreak  int     `json:"longest_streak"` // longest active-day run in the window
	ActiveDays     int     `json:"active_days"`    // active days within the 12-week window
	TokensTotal    int     `json:"tokens_total"`
	TokensClaude   int     `json:"tokens_claude"`
	TokensCodex    int     `json:"tokens_codex"`
	CostTotal      float64 `json:"cost_total,omitempty"`
	CostClaude     float64 `json:"cost_claude,omitempty"`
	CostCodex      float64 `json:"cost_codex,omitempty"`
	SessionsTotal  int     `json:"sessions_total"`
	SessionsClaude int     `json:"sessions_claude"`
	SessionsCodex  int     `json:"sessions_codex"`
}

type uiTrash struct {
	Tasks     []uiTrashItem `json:"tasks"`
	Projects  []uiTrashItem `json:"projects"`
	Playbooks []uiTrashItem `json:"playbooks"`
	Total     int           `json:"total"`
}

type uiTrashItem struct {
	Kind      string  `json:"kind"`
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	Status    string  `json:"status,omitempty"`
	Priority  string  `json:"priority,omitempty"`
	Project   *string `json:"project,omitempty"`
	WorkDir   string  `json:"work_dir"`
	DeletedAt string  `json:"deleted_at"`
	Archived  bool    `json:"archived"`
}

type uiAgent struct {
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	Project         *string        `json:"project"`
	Kind            string         `json:"kind"`
	PlaybookSlug    *string        `json:"playbook_slug,omitempty"`
	Parent          *TaskSummary   `json:"parent,omitempty"`
	Parents         []TaskSummary  `json:"parents,omitempty"`
	Children        []TaskSummary  `json:"children,omitempty"`
	ForkedFromSlug  *string        `json:"forked_from_slug,omitempty"`
	ForkedFrom      *TaskSummary   `json:"forked_from,omitempty"`
	ForkReason      *string        `json:"fork_reason,omitempty"`
	Forks           []TaskSummary  `json:"forks,omitempty"`
	Branch          string         `json:"branch"`
	Branches        []string       `json:"branches,omitempty"`
	WorkDir         string         `json:"work_dir"`
	Provider        string         `json:"provider"`
	PermissionMode  string         `json:"permission_mode"`
	Priority        string         `json:"priority"`
	Status          string         `json:"status"`
	TaskStatus      string         `json:"task_status"`
	RuntimeStatus   string         `json:"runtime_status"`
	RuntimeEvent    string         `json:"runtime_event,omitempty"`
	RuntimeSource   string         `json:"runtime_source,omitempty"`
	HookHealth      *uiHookHealth  `json:"hook_health,omitempty"`
	SessionID       string         `json:"session_id"`
	StartedMin      int            `json:"started_min"`
	LastActivitySec int            `json:"last_activity_sec"`
	LastAction      string         `json:"last_action"`
	WaitingFor      *uiWaitingFor  `json:"waiting_for,omitempty"`
	Diff            uiDiff         `json:"diff"`
	TokensUsed      int            `json:"tokens_used"`
	TokensMax       int            `json:"tokens_max"`
	TokensSession   int            `json:"tokens_session"`
	CostSession     float64        `json:"cost_session,omitempty"`
	Activity        []int          `json:"activity"`
	Tags            []string       `json:"tags"`
	Summary         string         `json:"summary"`
	NextStep        string         `json:"next_step"`
	Transcript      []uiTranscript `json:"transcript,omitempty"`
	Brief           string         `json:"brief,omitempty"`
	RecentTools     []uiRecentTool `json:"recent_tools,omitempty"`
	DiffFiles       []uiDiffFile   `json:"diff_files,omitempty"`
	BriefPath       string         `json:"brief_path,omitempty"`
	Updates         []FileRef      `json:"updates,omitempty"`
	AuxFiles        []FileRef      `json:"aux_files,omitempty"`
	Terminal        uiTerminal     `json:"terminal"`
	// Monitored reports whether a persistent background monitor is watching
	// this task's inbox (independent of whether the session is live).
	Monitored bool `json:"monitored"`
	// AutoRun fields carry the supervisor lifecycle for `flow do --auto` runs.
	// Display-only reconcile: the server never writes auto_run_status to DB.
	AutoRunStatus   string `json:"auto_run_status,omitempty"`
	AutoRunStarted  string `json:"auto_run_started,omitempty"`
	AutoRunFinished string `json:"auto_run_finished,omitempty"`
	AutoRunLog      string `json:"auto_run_log,omitempty"`
}

type uiWaitingFor struct {
	Kind string `json:"kind"`
	Cmd  string `json:"cmd"`
	Why  string `json:"why"`
}

type uiHookHealth struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Action  string `json:"action,omitempty"`
}

type uiDiff struct {
	Add   int `json:"add"`
	Rem   int `json:"rem"`
	Files int `json:"files"`
}

type uiDiffFile struct {
	Name  string       `json:"name"`
	Add   int          `json:"add"`
	Rem   int          `json:"rem"`
	Hunks []uiDiffHunk `json:"hunks,omitempty"`
}

type uiDiffHunk struct {
	Header string       `json:"header"`
	Lines  []uiDiffLine `json:"lines"`
}

type uiDiffLine struct {
	Type string `json:"type"`
	N    string `json:"n"`
	Code string `json:"code"`
}

type uiRecentTool struct {
	Name string `json:"name"`
	S    string `json:"s"`
}

type uiTerminal struct {
	Banner  []uiTermLine `json:"banner"`
	Feed    []uiTermLine `json:"feed"`
	Appends []uiTermLine `json:"appends"`
	Footer  []uiTermLine `json:"footer"`
	Mode    string       `json:"mode,omitempty"`
	Message string       `json:"message,omitempty"`
}

type uiTermLine struct {
	C    string `json:"c"`
	Text string `json:"text"`
}

type uiBacklogTask struct {
	Slug      string        `json:"slug"`
	Name      string        `json:"name"`
	Project   string        `json:"project"`
	Parent    *TaskSummary  `json:"parent,omitempty"`
	Parents   []TaskSummary `json:"parents,omitempty"`
	Children  []TaskSummary `json:"children,omitempty"`
	Provider  string        `json:"provider"`
	Priority  string        `json:"priority"`
	Due       string        `json:"due,omitempty"`
	Tags      []string      `json:"tags,omitempty"`
	WaitingOn *string       `json:"waiting_on,omitempty"`
	// StartedMin is minutes since the task row was created. Lets the UI sort
	// backlog tasks by age alongside live sessions (uiAgent.StartedMin).
	StartedMin int `json:"started_min"`
}

type uiProject struct {
	Slug     string       `json:"slug"`
	Name     string       `json:"name"`
	Priority string       `json:"priority"`
	Tasks    uiTaskCounts `json:"tasks"`
	WorkDir  string       `json:"work_dir"`
}

type uiTaskCounts struct {
	Total      int `json:"total"`
	InProgress int `json:"in_progress"`
	Backlog    int `json:"backlog"`
	Done       int `json:"done"`
}

type uiPlaybook struct {
	Slug     string          `json:"slug"`
	Name     string          `json:"name"`
	Project  *string         `json:"project"`
	RunsWeek int             `json:"runs_week"`
	LastMin  *int            `json:"last_min"`
	Spark    []int           `json:"spark"`
	Runs     []uiPlaybookRun `json:"runs"`
	WorkDir  string          `json:"work_dir"`
	// Scheduling (nil/false when unscheduled).
	Schedule       *string `json:"schedule"`
	SchedulePaused bool    `json:"schedule_paused"`
	NextFireAt     *string `json:"next_fire_at"`
}

// uiPlaybookRun is one run shown as a hoverable bar in the Active Playbooks
// strip — enough to tell the operator when it ran and how it ended.
type uiPlaybookRun struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

type uiKBFile struct {
	Name    string      `json:"name"`
	Preview string      `json:"preview"`
	Count   int         `json:"count"`
	Entries []uiKBEntry `json:"entries"`
}

type uiKBEntry struct {
	D string `json:"d"`
	T string `json:"t"`
}

type uiMemorySource struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Scope     string `json:"scope"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Path      string `json:"path"`
	Status    string `json:"status"`
	Available bool   `json:"available"`
	Format    string `json:"format,omitempty"`
	MTime     string `json:"mtime,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Content   string `json:"content,omitempty"`
	Error     string `json:"error,omitempty"`
}

type uiWorkdir struct {
	Path      string  `json:"path"`
	Name      string  `json:"name"`
	Remote    *string `json:"remote"`
	UsedMin   int     `json:"used_min"`
	Tasks     int     `json:"tasks"`
	Untouched bool    `json:"untouched"`
}

type uiTranscript struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Input   string `json:"input,omitempty"`
	Summary string `json:"summary,omitempty"`
	Preview string `json:"preview,omitempty"`
	Lines   int    `json:"lines,omitempty"`
	Time    string `json:"time,omitempty"`
}

func (s *Server) handleUIDataJS(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	data, err := s.cachedUIData()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(data)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("window.FLOW_BOOTSTRAP = "))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte(";\n"))
}

func (s *Server) handleUIDataJSON(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	data, err := s.cachedUIData()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, data)
}

// cachedUIData serves a recently-built ui-data snapshot, collapsing concurrent
// rebuilds into one. This is the path every /api/ui-data request takes — the
// raw buildUIData is O(tasks) (graph queries + transcript parse + allocation)
// and was re-run per request, pegging multiple cores under load.
func (s *Server) cachedUIData() (uiData, error) {
	if s.caches == nil || s.caches.uiData == nil {
		return s.buildUIData()
	}
	return s.caches.uiData.load(time.Now(), s.buildUIData)
}

// freshUIData forces a rebuild and refreshes the snapshot. The debounced SSE
// broadcast uses it so a just-mutated state is pushed (and seeds the cache for
// subsequent reads) without waiting out the TTL.
func (s *Server) freshUIData() (uiData, error) {
	data, err := s.buildUIData()
	if s.caches != nil && s.caches.uiData != nil {
		s.caches.uiData.store(time.Now(), data, err)
	}
	return data, err
}

func (s *Server) buildUIData() (uiData, error) {
	live, _ := s.cachedLiveAgentSessions()
	tasks, err := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{Kind: "", IncludeArchived: false})
	if err != nil {
		return uiData{}, err
	}
	taskViews, err := buildTaskViewsWithLive(s.cfg.DB, s.cfg.FlowRoot, tasks, live)
	if err != nil {
		return uiData{}, err
	}

	agents := []uiAgent{}
	backlog := []uiBacklogTask{}
	doneTasks := []uiBacklogTask{}
	doneCandidates := []uiAgent{}
	for _, tv := range taskViews {
		switch tv.Status {
		case "backlog":
			if tv.Kind == "regular" {
				backlog = append(backlog, s.uiBacklog(tv))
			}
		case "done":
			if tv.Kind == "regular" {
				doneTasks = append(doneTasks, s.uiBacklog(tv))
				doneCandidates = append(doneCandidates, s.uiAgent(tv, live))
			}
		default:
			if tv.Kind == "regular" || tv.Kind == "playbook_run" {
				agents = append(agents, s.uiAgent(tv, live))
			}
		}
	}
	sort.SliceStable(agents, func(i, j int) bool {
		ri, rj := uiStatusRank(agents[i].Status), uiStatusRank(agents[j].Status)
		if ri != rj {
			return ri < rj
		}
		pi, pj := priorityRank(agents[i].Priority), priorityRank(agents[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return agents[i].Slug < agents[j].Slug
	})
	sort.SliceStable(backlog, func(i, j int) bool {
		pi, pj := priorityRank(backlog[i].Priority), priorityRank(backlog[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return backlog[i].Slug < backlog[j].Slug
	})
	sort.SliceStable(doneCandidates, func(i, j int) bool {
		return doneCandidates[i].LastActivitySec < doneCandidates[j].LastActivitySec
	})
	sort.SliceStable(doneTasks, func(i, j int) bool {
		return doneTasks[i].Slug < doneTasks[j].Slug
	})

	projects, err := s.uiProjects()
	if err != nil {
		return uiData{}, err
	}
	playbooks, err := s.uiPlaybooks()
	if err != nil {
		return uiData{}, err
	}
	workdirs, err := s.uiWorkdirs()
	if err != nil {
		return uiData{}, err
	}
	kb := s.uiKBFiles()
	transcript := []uiTranscript{{Type: "assistant", Text: "No transcript is available for the selected flow task yet."}}
	diffFiles := []uiDiffFile{}
	if len(agents) > 0 && len(agents[0].Transcript) > 0 {
		transcript = agents[0].Transcript
	}
	if len(agents) > 0 && len(agents[0].DiffFiles) > 0 {
		diffFiles = agents[0].DiffFiles
	}
	if len(diffFiles) == 0 {
		diffFiles = []uiDiffFile{{Name: "No local git diff", Add: 0, Rem: 0}}
	}
	memorySources := s.uiAgentMemorySources(taskViews, projects, playbooks, workdirs)

	var dead *uiAgent
	if len(doneCandidates) > 0 {
		doneCandidates[0].Status = "idle"
		deadAgent := doneCandidates[0]
		trimUIAgentSessionDetails(&deadAgent)
		dead = &deadAgent
		for i := range doneCandidates {
			trimUIAgentSessionDetails(&doneCandidates[i])
		}
	}
	heatmap := buildActivityHeatmap(taskViews, time.Now())
	tokenSeries, topTasks, modelMix := s.buildTokenSeries(taskViews, time.Now())
	return uiData{
		Agents:           agents,
		DeadAgent:        dead,
		DoneAgents:       doneCandidates,
		Backlog:          backlog,
		DoneTasks:        doneTasks,
		KBFiles:          kb,
		MemorySources:    memorySources,
		Workdirs:         workdirs,
		Playbooks:        playbooks,
		Projects:         projects,
		ActivityHeatmap:  heatmap,
		TokenSeries:      tokenSeries,
		TopTasks:         topTasks,
		ModelMix:         modelMix,
		Stats:            buildUIStats(agents, doneCandidates, s.chatStatAgents(), tokenSeries, time.Now()),
		Capabilities:     s.uiCapabilities(),
		Trash:            s.uiTrash(),
		SampleTranscript: transcript,
		SampleDiffFiles:  diffFiles,
		FlowDB:           s.uiFlowDB(),
		User:             currentUIUser(),
		FloatingSessions: s.floatingSessionList(),
	}, nil
}

func trimUIAgentSessionDetails(agent *uiAgent) {
	agent.Transcript = nil
	agent.Brief = ""
	agent.RecentTools = nil
	agent.DiffFiles = nil
	agent.BriefPath = ""
	agent.Updates = nil
	agent.AuxFiles = nil
	agent.Terminal = uiTerminal{}
}

// floatingSessionList returns the adhoc Ask Flow sessions for the tray, or an
// empty slice when the terminal hub isn't wired (keeps the JSON field a stable
// array rather than null).
func (s *Server) floatingSessionList() []floatingSessionInfo {
	if s.terminals == nil {
		return []floatingSessionInfo{}
	}
	return s.terminals.floatingSessions()
}

type taskSessionInsights struct {
	ActivityAt    string
	LastAction    string
	TokensUsed    int // current context-window occupancy (latest turn)
	TokensMax     int
	TokensSession int     // cumulative tokens used this session (the CLI's Σ)
	CostSession   float64 // estimated USD for this session's full billed usage, cache included (all-time)
}

// runtimeStateStaleForRunning returns true when a hook-driven "running"
// status has gone quiet — i.e., no hook events and no transcript activity
// for long enough that the running claim is no longer trustworthy. We
// require BOTH the hook updated_at and the transcript ActivityAt to be old
// before demoting to idle, so a long-running tool call (which holds the
// hook in "running" but pauses transcript writes) isn't mistakenly idled.
//
// Thresholds: hook hasn't ticked in >= 90s AND transcript hasn't moved in
// >= 30s. Cases like user-interrupt-then-walk-away land here quickly;
// genuine tool runs (Bash, web fetch, etc.) update the transcript far
// more often than every 30s.
const (
	runtimeHookStaleAfter       = 90 * time.Second
	runtimeTranscriptStaleAfter = 30 * time.Second
)

var kbEntryRe = regexp.MustCompile(`^-\s*([0-9]{4}-[0-9]{2}-[0-9]{2})\s*(?:[-—])\s*(.*)$`)

var gitHunkHeaderRe = regexp.MustCompile(`^@@ -([0-9]+)(?:,[0-9]+)? \+([0-9]+)(?:,[0-9]+)? @@`)

func uiStatusRank(status string) int {
	switch status {
	case "waiting":
		return 0
	case "running":
		return 1
	case "idle":
		return 2
	case "stale":
		return 3
	default:
		return 4
	}
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func nullStringValue(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncateText(line, 140)
		}
	}
	return ""
}
