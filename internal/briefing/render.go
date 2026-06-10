package briefing

import (
	"fmt"
	"strings"
)

// RenderMarkdown renders a briefing as copyable Markdown for the CLI and any
// future share/export surface.
func RenderMarkdown(b Briefing) string {
	var out strings.Builder
	out.WriteString("# Flow briefing\n")
	if b.WindowStart != "" || b.WindowEnd != "" {
		fmt.Fprintf(&out, "_Window: %s → %s_\n", dash(b.WindowStart), dash(b.WindowEnd))
	}
	if b.GeneratedAt != "" {
		fmt.Fprintf(&out, "_Generated: %s_\n", b.GeneratedAt)
	}
	renderSection(&out, "Needs you", b.NeedsYou, true, "Nothing needs you right now.")
	renderSection(&out, "Since you last looked", b.Overnight, false, "Nothing changed in this window.")
	renderSection(&out, "Pick up next", b.NextUp, true, "No startable or resumable work.")
	return out.String()
}

func renderSection(out *strings.Builder, title string, items []Item, action bool, empty string) {
	fmt.Fprintf(out, "\n## %s\n", title)
	renderItems(out, items, action, empty)
}

func renderItems(out *strings.Builder, items []Item, action bool, empty string) {
	if len(items) == 0 {
		fmt.Fprintf(out, "- %s\n", empty)
		return
	}
	lastGroup := ""
	for _, item := range items {
		group := itemGroup(item, action)
		if group != lastGroup {
			fmt.Fprintf(out, "### %s\n", group)
			lastGroup = group
		}
		fmt.Fprintf(out, "- [%s] %s", item.Kind, item.Title)
		if item.Action != "" {
			fmt.Fprintf(out, " · action: %s", item.Action)
		}
		if item.Detail != "" {
			fmt.Fprintf(out, " · %s", item.Detail)
		}
		out.WriteByte('\n')
		if len(item.Links) > 0 {
			fmt.Fprintf(out, "  links: %s\n", renderLinks(item.Links))
		}
	}
}

func itemGroup(item Item, action bool) string {
	parts := []string{}
	if item.Project != "" {
		parts = append(parts, item.Project)
	}
	if action && item.Source != "" {
		parts = append(parts, item.Source)
	}
	if action && item.Urgency != "" {
		parts = append(parts, item.Urgency)
	}
	if len(parts) == 0 {
		return "general"
	}
	return strings.Join(parts, " · ")
}

func renderLinks(links []Link) string {
	parts := make([]string, 0, len(links))
	for _, link := range links {
		target := link.Target
		if target == "" {
			target = link.URL
		}
		if link.Label != "" {
			parts = append(parts, fmt.Sprintf("%s:%s", link.Kind, link.Label))
		} else {
			parts = append(parts, fmt.Sprintf("%s:%s", link.Kind, target))
		}
	}
	return strings.Join(parts, " · ")
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
