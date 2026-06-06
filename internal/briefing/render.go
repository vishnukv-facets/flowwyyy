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
	out.WriteString("\n## Needs action\n")
	renderItems(&out, b.NeedsAction, true)
	out.WriteString("\n## FYI\n")
	renderItems(&out, b.FYI, false)
	return out.String()
}

func renderItems(out *strings.Builder, items []Item, action bool) {
	if len(items) == 0 {
		if action {
			out.WriteString("- Nothing needs action.\n")
		} else {
			out.WriteString("- No FYI items in this window.\n")
		}
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
