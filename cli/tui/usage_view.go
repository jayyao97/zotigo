package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

var (
	usageStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	usageDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	usageWarnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	usageDangerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
)

// renderUsageStatus builds the one-line "context: ... · session in/out:
// ..." readout shown above the prompt. Returns "" when no usage is
// available yet (fresh session before the first turn) so the prompt
// doesn't get an empty line above it.
func renderUsageStatus(ag *agent.Agent) string {
	if ag == nil {
		return ""
	}
	snap := ag.Snapshot()
	if len(snap.History) == 0 {
		return ""
	}

	last, hasLast := protocol.LastTurnUsage(snap.History)
	session := protocol.SessionUsage(snap.History)
	if !hasLast && session.TotalTokens == 0 {
		return ""
	}

	desc := ag.Describe()
	// Profile config wins; otherwise assume the conservative default.
	// Users on smaller models (gpt-4 8k, local llama) should set
	// `context_window` in their profile rather than relying on this.
	ctxLimit := desc.ContextWindow
	if ctxLimit == 0 {
		ctxLimit = config.DefaultContextWindow
	}

	var parts []string

	// "ctx" and "cache" describe the LAST TURN — they answer
	// "how full is the prompt I'm about to send" and "how much of
	// it just hit the cache". Session-cumulative input would
	// double-count history (each turn re-bills the whole prompt),
	// so we deliberately leave it off the status line. Cumulative
	// output is fine to show because each turn's output is unique.
	if hasLast {
		used := last.TotalInput()
		ctxPart := fmt.Sprintf("ctx %s", formatTokens(used))
		if ctxLimit > 0 {
			pct := float64(used) / float64(ctxLimit) * 100
			pctStr := fmt.Sprintf("%.0f%%", pct)
			switch {
			case pct >= 90:
				pctStr = usageDangerStyle.Render(pctStr)
			case pct >= 70:
				pctStr = usageWarnStyle.Render(pctStr)
			}
			ctxPart = fmt.Sprintf("ctx %s/%s (%s)",
				formatTokens(used), formatTokens(ctxLimit), pctStr)
		}
		parts = append(parts, ctxPart)
		if last.CacheReadInputTokens > 0 {
			parts = append(parts, "cache "+formatTokens(last.CacheReadInputTokens))
		}
	}

	if session.OutputTokens > 0 {
		parts = append(parts, "session out "+formatTokens(session.OutputTokens))
	}

	return usageStyle.Render("◇ " + strings.Join(parts, " · "))
}

// renderUsageSummary builds the multi-line block printed when the user
// quits the session. Returns "" when the session never produced a turn
// — printing "0 tokens" would just be noise.
func renderUsageSummary(ag *agent.Agent) string {
	if ag == nil {
		return ""
	}
	snap := ag.Snapshot()
	session := protocol.SessionUsage(snap.History)
	turns := protocol.CountAssistantTurns(snap.History)
	if turns == 0 || session.TotalTokens == 0 {
		return ""
	}

	desc := ag.Describe()
	header := usageDimStyle.Render(fmt.Sprintf("── Session usage (%s/%s, %d turns) ──", desc.Provider, desc.Model, turns))

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
