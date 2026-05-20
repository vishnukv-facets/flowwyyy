package app

import (
	"flag"
	"flow/internal/flowdb"
	"flow/internal/listfmt"
	"fmt"
	"os"
	"strings"
)

func cmdSearch(args []string) int {
	fs := flagSet("search")
	scopeRaw := fs.String("in", "briefs,updates", "comma-separated scopes: briefs,updates,transcripts,all")
	limit := fs.Int("limit", 20, "maximum number of results")
	format := fs.String("format", "table", "output format: table|json|tsv")
	noColor := fs.Bool("no-color", false, "disable ANSI color even when stdout is a TTY")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: flow search "<query>" [--in briefs,updates,transcripts] [--limit N] [--format table|json|tsv]`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeSearchArgs(args)); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(os.Stderr, "error: search requires a query")
		fs.Usage()
		return 2
	}
	scopes, err := flowdb.ParseSearchScopes(*scopeRaw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	fmtKind, err := listfmt.ParseFormat(*format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	includeTranscripts := flowdb.SearchScopesInclude(scopes, flowdb.SearchScopeTranscript)
	if err := flowdb.SyncSearchDocs(db, root, includeTranscripts); err != nil {
		fmt.Fprintf(os.Stderr, "error: index search docs: %v\n", err)
		return 1
	}
	results, err := flowdb.SearchDocs(db, query, scopes, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: search: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		switch fmtKind {
		case listfmt.FormatJSON:
			return runJSON(os.Stdout, []flowdb.SearchResult{})
		case listfmt.FormatTSV:
			_ = listfmt.RenderTSV(os.Stdout, []string{"type", "slug", "name", "source", "snippet"}, nil)
		default:
			fmt.Println("(no search results)")
		}
		return 0
	}
	switch fmtKind {
	case listfmt.FormatJSON:
		return runJSON(os.Stdout, results)
	case listfmt.FormatTSV:
		rows := make([][]string, len(results))
		for i, r := range results {
			rows[i] = []string{r.Type, r.Slug, r.Name, r.SourcePath, r.Snippet}
		}
		_ = listfmt.RenderTSV(os.Stdout, []string{"type", "slug", "name", "source", "snippet"}, rows)
		return 0
	}

	painter := listfmt.Painter{Enabled: listfmt.ColorEnabled(os.Stdout, *noColor)}
	rows := make([][]string, len(results))
	for i, r := range results {
		rows[i] = []string{
			painter.Wrap(r.Type, listfmt.Cyan),
			r.Slug,
			r.Name,
			listfmt.Truncate(r.Snippet, 120),
		}
	}
	tab := &listfmt.Table{
		Headers: dimHeaders(painter, []string{"TYPE", "SLUG", "NAME", "SNIPPET"}),
		Rows:    rows,
	}
	_ = tab.Render(os.Stdout)
	return 0
}

func normalizeSearchArgs(args []string) []string {
	var flags []string
	var query []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--in" || arg == "-in" || arg == "--limit" || arg == "-limit" || arg == "--format" || arg == "-format":
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case arg == "--no-color" || arg == "-no-color" || arg == "-h" || arg == "--help":
			flags = append(flags, arg)
		case strings.HasPrefix(arg, "--in=") || strings.HasPrefix(arg, "-in=") ||
			strings.HasPrefix(arg, "--limit=") || strings.HasPrefix(arg, "-limit=") ||
			strings.HasPrefix(arg, "--format=") || strings.HasPrefix(arg, "-format="):
			flags = append(flags, arg)
		default:
			query = append(query, arg)
		}
	}
	return append(flags, query...)
}
