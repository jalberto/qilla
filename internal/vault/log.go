package vault

import (
	"strings"
)

// LogHeading is the daily note's audit-trail section.
const LogHeading = "## 📓 Log"

// AppendToSection inserts line at the end of the section whose heading matches
// want, leaving every other byte of the note untouched. When the section is
// missing it is created at the end of the note with heading as its title.
func AppendToSection(note, want, heading, line string) string {
	lines := Lines(note)
	hs := Headings(lines)
	for _, h := range hs {
		if Norm(h.Text) != want {
			continue
		}
		// last non-empty line of the section body, sub-headings excluded
		at := h.Start + 1
		for i := h.Start + 1; i < h.End; i++ {
			if strings.TrimSpace(lines[i]) != "" {
				at = i + 1
			}
		}
		out := append([]string{}, lines[:at]...)
		out = append(out, line)
		out = append(out, lines[at:]...)
		return strings.Join(out, "\n") + "\n"
	}
	body := strings.TrimRight(note, "\n")
	if body != "" {
		body += "\n\n"
	}
	return body + heading + "\n" + line + "\n"
}
