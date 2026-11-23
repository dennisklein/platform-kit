package change

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dennisklein/platform-kit/internal/gitx"
)

// Trailer keys of platform-kit.
const (
	TrailerChange     = "Pk-Change"
	TrailerTitle      = "Pk-Title"
	TrailerIntent     = "Pk-Intent"
	TrailerLabRun     = "Pk-Lab-Run"
	TrailerAuthor     = "Pk-Author"
	TrailerAgent      = "Pk-Agent"
	TrailerOnBehalfOf = "Pk-On-Behalf-Of"
	TrailerWorkItem   = "Pk-Work-Item"
	TrailerTarget     = "Pk-Target"
	TrailerParent     = "Pk-Parent"
	TrailerBreakGlass = "Pk-Break-Glass"
	TrailerApprovedBy = "Approved-by"
	TrailerReviewedIn = "Reviewed-in"
)

// ParseMeta parses a metadata file.
func ParseMeta(b []byte) (Meta, error) {
	var m Meta
	if err := yaml.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("change metadata: %w", err)
	}
	if m.ID == "" {
		return m, fmt.Errorf("change metadata: no id")
	}
	if m.Target == "" {
		m.Target = "main"
	}
	return m, nil
}

// MarshalMeta renders a metadata file.
func MarshalMeta(m Meta) []byte {
	m.APIVersion, m.Kind = MetaAPIVersion, MetaKind
	if m.Target == "" {
		m.Target = "main"
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(m)
	_ = enc.Close()
	return []byte(b.String())
}

// FormatLabRun is the Pk-Lab-Run trailer value: "<run> <result> [<report>]".
func FormatLabRun(r LabRunRef) string {
	s := r.Run + " " + string(r.Result)
	if r.Report != "" {
		s += " " + r.Report
	}
	return s
}

// ParseLabRun parses a Pk-Lab-Run trailer value.
func ParseLabRun(v string) (LabRunRef, bool) {
	f := strings.Fields(v)
	if len(f) < 2 || !strings.Contains(f[0], "/run-") {
		return LabRunRef{}, false
	}
	r := LabRunRef{Run: f[0], Result: LabResult(f[1])}
	if len(f) > 2 {
		r.Report = f[2]
	}
	return r, true
}

// MetaTrailers are the trailers that carry the metadata after ready.
func MetaTrailers(m Meta) []gitx.Trailer {
	ts := []gitx.Trailer{{Key: TrailerChange, Value: m.ID}, {Key: TrailerTitle, Value: oneLine(m.Title)}}
	if m.Intent != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerIntent, Value: oneLine(m.Intent)})
	}
	for _, r := range m.LabRuns {
		if r.Result == LabRunning {
			continue
		}
		ts = append(ts, gitx.Trailer{Key: TrailerLabRun, Value: FormatLabRun(r)})
	}
	if m.Author != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerAuthor, Value: m.Author})
	}
	if m.Agent != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerAgent, Value: m.Agent})
	}
	for _, w := range m.WorkItems {
		ts = append(ts, gitx.Trailer{Key: TrailerWorkItem, Value: w})
	}
	if m.Target != "" && m.Target != "main" {
		ts = append(ts, gitx.Trailer{Key: TrailerTarget, Value: m.Target})
	}
	if m.Parent != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerParent, Value: m.Parent})
	}
	return ts
}

// Description is the merge request description: the intent, a blank line
// and the trailer block.
func Description(m Meta) string {
	intent := strings.TrimSpace(m.Intent)
	if intent == "" {
		intent = m.Title
	}
	return intent + "\n\n" + gitx.FormatTrailers(MetaTrailers(m)) + "\n"
}

// MetaFromTrailers rebuilds metadata from trailers (MR description or merge commit).
func MetaFromTrailers(ts []gitx.Trailer) (Meta, bool) {
	m := Meta{APIVersion: MetaAPIVersion, Kind: MetaKind, Target: "main"}
	for _, t := range ts {
		switch t.Key {
		case TrailerChange:
			m.ID = t.Value
		case TrailerTitle:
			m.Title = t.Value
		case TrailerIntent:
			m.Intent = t.Value
		case TrailerLabRun:
			if r, ok := ParseLabRun(t.Value); ok {
				m.LabRuns = append(m.LabRuns, r)
			}
		case TrailerAuthor:
			m.Author = t.Value
		case TrailerAgent:
			m.Agent = t.Value
		case TrailerWorkItem:
			for _, w := range strings.Split(t.Value, ",") {
				if w = strings.TrimSpace(w); w != "" {
					m.WorkItems = append(m.WorkItems, w)
				}
			}
		case TrailerTarget:
			m.Target = t.Value
		case TrailerParent:
			m.Parent = t.Value
		}
	}
	return m, ValidID(m.ID)
}

// MetaFromDescription parses an MR description.
func MetaFromDescription(body string) (Meta, bool) {
	m, ok := MetaFromTrailers(gitx.ParseTrailers(body))
	if !ok {
		return m, false
	}
	if paras := strings.SplitN(strings.TrimSpace(body), "\n\n", 2); len(paras) == 2 && m.Intent == "" {
		m.Intent = strings.TrimSpace(paras[0])
	}
	return m, true
}

// MergeInfo is what goes into a merge commit besides the metadata.
type MergeInfo struct {
	MR         int
	ApprovedBy []string
	// LabRun is the run that covers the head (nil when the policy needs none).
	LabRun *LabRunRef
	Extra  []gitx.Trailer
}

// MergeMessage returns the title and message of the merge commit.
func MergeMessage(m Meta, info MergeInfo) (title, message string) {
	title = fmt.Sprintf("Merge change %s: %s", m.ID, oneLine(m.Title))
	if info.MR > 0 {
		title += fmt.Sprintf(" (!%d)", info.MR)
	}
	ts := []gitx.Trailer{{Key: TrailerChange, Value: m.ID}}
	if m.Intent != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerIntent, Value: oneLine(m.Intent)})
	}
	if info.LabRun != nil {
		ts = append(ts, gitx.Trailer{Key: TrailerLabRun, Value: FormatLabRun(*info.LabRun)})
	}
	if m.Author != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerAuthor, Value: m.Author})
	}
	if m.Agent != "" {
		ts = append(ts, gitx.Trailer{Key: TrailerAgent, Value: m.Agent})
	}
	for _, w := range m.WorkItems {
		ts = append(ts, gitx.Trailer{Key: TrailerWorkItem, Value: w})
	}
	ts = append(ts, info.Extra...)
	if len(info.ApprovedBy) > 0 {
		ts = append(ts, gitx.Trailer{Key: TrailerApprovedBy, Value: strings.Join(info.ApprovedBy, ", ")})
	}
	if info.MR > 0 {
		ts = append(ts, gitx.Trailer{Key: TrailerReviewedIn, Value: fmt.Sprintf("!%d", info.MR)})
	}
	intent := strings.TrimSpace(m.Intent)
	if intent == "" {
		intent = m.Title
	}
	return title, intent + "\n\n" + gitx.FormatTrailers(ts)
}

// Trailer returns the first value of key.
func Trailer(ts []gitx.Trailer, key string) string {
	for _, t := range ts {
		if t.Key == key {
			return t.Value
		}
	}
	return ""
}

// TrailerList returns all values of key; comma-separated values are split.
func TrailerList(ts []gitx.Trailer, key string) []string {
	var out []string
	for _, t := range ts {
		if t.Key != key {
			continue
		}
		for _, v := range strings.Split(t.Value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// TitleFromMergeSubject returns id and title from "Merge change <id>: <title> (!N)".
func TitleFromMergeSubject(subject string) (id, title string, mr int) {
	rest, ok := strings.CutPrefix(subject, "Merge change ")
	if !ok {
		return "", "", 0
	}
	id, title, ok = strings.Cut(rest, ": ")
	if !ok {
		return "", "", 0
	}
	if i := strings.LastIndex(title, " (!"); i >= 0 && strings.HasSuffix(title, ")") {
		fmt.Sscanf(title[i+3:len(title)-1], "%d", &mr)
		title = title[:i]
	}
	return id, title, mr
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
