package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSlackText bounds the text of one Slack message (Slack's own limit is
// 40000 characters for the text field).
const maxSlackText = 30000

// slackPayload is the body of a Slack incoming webhook call.
type slackPayload struct {
	Text string `json:"text"`
}

// SlackText renders msg as Slack mrkdwn. Every dynamic part is escaped so that
// it cannot inject links or mentions (<!channel>, <http://...>), close the code
// spans it is shown in, or reach the reader with control or bidirectional
// characters.
func SlackText(msg Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Graft superfan digest* for `%s` (%s..%s): %d events, risk *%s*\n",
		slackText(component(msg.Component), false),
		msg.WindowStart.UTC().Format(time.RFC3339), msg.WindowEnd.UTC().Format(time.RFC3339),
		msg.Events, slackText(msg.Risk, false))
	if msg.Summary != "" {
		fmt.Fprintf(&b, "%s\n", slackText(msg.Summary, true))
	}
	for _, f := range msg.Findings {
		fmt.Fprintf(&b, "• *%s* `%s` %s (seq %d, `%s`", slackText(f.Severity, false), slackText(f.Rule, false),
			slackText(f.Summary, false), f.Sequence, slackText(f.Reference, false))
		if f.Actor != "" {
			fmt.Fprintf(&b, ", actor `%s`", slackText(f.Actor, false))
		}
		b.WriteString(")\n")
	}
	if msg.Omitted > 0 {
		fmt.Fprintf(&b, "… %d more findings omitted\n", msg.Omitted)
	}
	text := b.String()
	if len(text) > maxSlackText {
		cut := maxSlackText
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "\n… truncated"
	}
	return text
}

func component(name string) string {
	if name == "" {
		return "unassigned"
	}
	return name
}

// slackText makes untrusted text safe for a Slack message: unprintable
// characters become spaces (newlines are kept when keepNewlines is set), the
// three characters Slack treats specially are escaped, and backticks, which
// mrkdwn cannot escape, become apostrophes so that the text can neither close
// the code span it is shown in nor open one.
func slackText(s string, keepNewlines bool) string {
	return slackEscape(strings.ReplaceAll(printable(s, keepNewlines), "`", "'"))
}

// slackEscape escapes the three characters Slack treats specially.
func slackEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}
