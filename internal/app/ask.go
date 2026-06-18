package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"flow/internal/flowdb"
)

type operatorAskPayload struct {
	TaskSlug string `json:"task_slug"`
	Question string `json:"question"`
}

var postOperatorAskFn = func(taskSlug, question string) (status int, body string, err error) {
	payload, err := json.Marshal(operatorAskPayload{TaskSlug: taskSlug, Question: question})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, flowServerURL("/api/operator/ask"), strings.NewReader(string(payload)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := uiSessionToken(); tok != "" {
		req.Header.Set("X-Flow-Session-Token", tok)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

func cmdAsk(args []string) int {
	if len(args) == 0 || leadingHelpArg(args) {
		printAskUsage()
		return 0
	}
	switch args[0] {
	case "operator":
		return cmdAskOperator(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown ask subcommand %q\n", args[0])
		printAskUsage()
		return 2
	}
}

func printAskUsage() {
	fmt.Println(`flow ask

  flow ask operator [--task <slug>] "<question>"`)
}

func cmdAskOperator(args []string) int {
	fs := flagSet("ask operator")
	taskFlag := fs.String("task", "", "task slug to route the answer back to (default: FLOW_TASK or current session)")
	if handled, rc := parseFlagSet(fs, args); handled {
		return rc
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, "error: ask operator requires a question")
		return 2
	}

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()

	taskSlug := strings.TrimSpace(*taskFlag)
	if taskSlug == "" {
		taskSlug = strings.TrimSpace(os.Getenv("FLOW_TASK"))
	}
	if taskSlug == "" {
		task, err := currentSessionTask(db)
		if err != nil {
			if isNoBindingErr(err) {
				fmt.Fprintln(os.Stderr, "error: no bound task found; pass --task <slug>")
				return 1
			}
			fmt.Fprintf(os.Stderr, "error: resolve current task: %v\n", err)
			return 1
		}
		taskSlug = task.Slug
	}
	if _, err := flowdb.GetTask(db, taskSlug); err != nil {
		fmt.Fprintf(os.Stderr, "error: task %q not found\n", taskSlug)
		return 1
	}

	status, respBody, err := postOperatorAskFn(taskSlug, question)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: ask operator requires the flow UI server: %v\n", err)
		return 1
	}
	if status < 200 || status >= 300 {
		fmt.Fprintf(os.Stderr, "error: %s\n", serverResponseError(respBody, "ask operator failed (server)"))
		return 1
	}
	fmt.Printf("asked operator for %s\n", taskSlug)
	return 0
}

func serverResponseError(body, fallback string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return fallback
	}
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err == nil && strings.TrimSpace(decoded.Error) != "" {
		return strings.TrimSpace(decoded.Error)
	}
	return body
}
