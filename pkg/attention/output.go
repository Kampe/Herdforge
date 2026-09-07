package attention

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteResult renders complete or partial triage identically, so a failed
// provider read cannot hide actionable findings already collected elsewhere.
func WriteResult(w io.Writer, r Result, asJSON, quiet bool) error {
	var body string
	if asJSON {
		out, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		body = string(out) + "\n"
	} else {
		var b strings.Builder
		fmt.Fprintln(&b, Summary(r))
		if !quiet {
			for _, item := range r.Items {
				fmt.Fprintln(&b, "  "+FormatItem(item))
			}
			for _, item := range r.Candidates {
				fmt.Fprintf(&b, "  %s %s SHA=%s PR=%d %s %s: %s\n", item.Level, item.Task, item.SHA, item.PullRequest, item.URL, item.Status, item.Reason)
				for _, check := range item.Checks {
					fmt.Fprintf(&b, "    %s %s %s %s\n", check.Name, check.Status, check.Conclusion, check.URL)
				}
			}
			if r.Needing > 0 {
				fmt.Fprintln(&b, "\nherd-attention: triage complete. Actions: review/harvest done lanes,")
				fmt.Fprintln(&b, "  unblock blocked lanes, kick idle lanes (herd kick), raise missing")
				fmt.Fprintln(&b, "  lanes (herd standing), reroute provider-death lanes.")
			}
		}
		body = b.String()
	}
	n, err := io.WriteString(w, body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}
