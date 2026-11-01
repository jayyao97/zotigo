package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

var (
	usageStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	usageDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	usageWarnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	usageDangerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
)

// renderUsageStatus builds the one-line context/usage readout shown above
// the prompt. Context occupancy comes from the session service, which owns
// the authoritative estimate; cumulative output comes from completed turns.
// Returns "" before anything is known so the prompt has no empty line.
func renderUsageStatus(contextUsage *zotigodclient.ContextUsage, last *protocol.Usage, total protocol.Usage) string {
	var parts []string
	if contextUsage != nil && contextUsage.Status == "available" && contextUsage.Tokens > 0 {
		ctxPart := fmt.Sprintf("ctx %s", formatTokens(contextUsage.Tokens))
		if contextUsage.Window > 0 {
			pct := float64(contextUsage.Tokens) / float64(contextUsage.Window) * 100
			pctStr := fmt.Sprintf("%.0f%%", pct)
			switch {
			case pct >= 90:
				pctStr = usageDangerStyle.Render(pctStr)
			case pct >= 70:
				pctStr = usageWarnStyle.Render(pctStr)
			}
			ctxPart = fmt.Sprintf("ctx %s/%s (%s)", formatTokens(contextUsage.Tokens), formatTokens(contextUsage.Window), pctStr)
		}
		parts = append(parts, ctxPart)
	}
	if last != nil && last.CacheReadInputTokens > 0 {
		parts = append(parts, "cache "+formatTokens(last.CacheReadInputTokens))
	}
	if total.OutputTokens > 0 {
		parts = append(parts, "session out "+formatTokens(total.OutputTokens))
	}
	if len(parts) == 0 {
		return ""
	}
	return usageStyle.Render("◇ " + strings.Join(parts, " · "))
}

// renderUsageSummary builds the block printed when the user quits.
// Returns "" when this TUI saw no completed turn.
func renderUsageSummary(session protocol.Usage, turns int, profile string) string {
	if turns == 0 || session.TotalTokens == 0 {
		return ""
	}
	header := usageDimStyle.Render(fmt.Sprintf("── Session usage (%s, %d turns) ──", profile, turns))
	rows := []string{
		header,
		fmt.Sprintf("  %s %s", padLabel("Input (new)"), formatTokens(session.InputTokens)),
	}
	if session.CacheCreationInputTokens > 0 {
		rows = append(rows, fmt.Sprintf("  %s %s", padLabel("Cache create"), formatTokens(session.CacheCreationInputTokens)))
	}
	if session.CacheReadInputTokens > 0 {
		rows = append(rows, fmt.Sprintf("  %s %s", padLabel("Cache read"), formatTokens(session.CacheReadInputTokens)))
	}
	rows = append(rows,
		fmt.Sprintf("  %s %s", padLabel("Output"), formatTokens(session.OutputTokens)),
		fmt.Sprintf("  %s %s", padLabel("Total"), formatTokens(session.TotalTokens)),
	)
	return strings.Join(rows, "\n")
}

func padLabel(s string) string {
	const w = 14
	if len(s) >= w {
		return s + ":"
	}
	return s + ":" + strings.Repeat(" ", w-len(s))
}

// formatTokens renders a token count compactly: 12,345 → "12.3k", 1,234,567 → "1.23M".
// Keeps small counts as raw integers with thousands separators so a
// 200-token turn is still recognisable at a glance.
func formatTokens(n int) string {
	if n < 0 {
		return "-" + formatTokens(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 10_000 {
		return formatNumberWithCommas(n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
}

func formatNumberWithCommas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
