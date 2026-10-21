package rules

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zricethezav/gitleaks/v8/detect"
)

type Span struct {
	Start, End int
	RuleIDs    []string
}

const MaxScanMatches = 32768

func (b *Bound) scan(ctx context.Context, text string) ([]Span, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("scan_canceled")
	}
	if !utf8.ValidString(text) {
		return nil, errors.New("scan_invalid_utf8")
	}
	opaque := encodedMedia(text)
	if len(opaque) > MaxScanMatches {
		return nil, errors.New("scan_media_limit")
	}
	remaining := MaxScanMatches
	var spans []Span
	start := 0
	// Scan separate text fragments so even a broad or multiline rule cannot
	// cover the payload or join secrets on opposite sides of an image.
	for i := 0; i <= len(opaque); i++ {
		end := len(text)
		if i < len(opaque) {
			end = opaque[i].Start
		}
		if start < end {
			found, err := b.scanText(ctx, text[start:end], &remaining)
			if err != nil {
				return nil, err
			}
			for _, span := range found {
				span.Start += start
				span.End += start
				spans = append(spans, span)
			}
		}
		if i < len(opaque) {
			start = opaque[i].End
		}
	}
	if ctx.Err() != nil {
		return nil, errors.New("scan_canceled")
	}
	return spans, nil
}

func (b *Bound) scanText(ctx context.Context, text string, remaining *int) ([]Span, error) {
	// The library returns a complete finding slice. Bound that allocation before
	// entering it, without dropping any part of a scan that was accepted.
	lower := strings.ToLower(text)
	for _, r := range b.rules {
		if ctx.Err() != nil {
			return nil, errors.New("scan_canceled")
		}
		if r.Regex == nil || r.Path != nil {
			continue
		}
		if len(r.Keywords) > 0 {
			matched := false
			for _, k := range r.Keywords {
				if strings.Contains(lower, strings.ToLower(k)) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		*remaining -= len(r.Regex.FindAllStringIndex(text, *remaining+1))
		if *remaining < 0 {
			return nil, errors.New("scan_match_limit")
		}
	}
	findings := b.detector.DetectContext(ctx, detect.Fragment{Raw: text})
	if err := ctx.Err(); err != nil {
		return nil, errors.New("scan_canceled")
	}
	// Gitleaks columns are byte offsets from the preceding newline byte, not
	// the first byte after it. Resolve the full regex match before selecting a
	// capture; never search for the nearest occurrence of Finding.Secret.
	newlines := []int{}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			newlines = append(newlines, i)
		}
	}
	matches := make(map[string]map[int][]int)
	var spans []Span
	for _, f := range findings {
		if ctx.Err() != nil {
			return nil, errors.New("scan_canceled")
		}
		r, ok := b.rules[f.RuleID]
		if !ok || r.Regex == nil {
			return nil, errors.New("finding_rule_missing")
		}
		byStart := matches[f.RuleID]
		if byStart == nil {
			byStart = make(map[int][]int)
			for _, idx := range r.Regex.FindAllStringSubmatchIndex(text, -1) {
				byStart[idx[0]] = idx
			}
			matches[f.RuleID] = byStart
		}
		start := f.StartColumn - 1
		if f.StartLine > 0 {
			if f.StartLine > len(newlines) {
				return nil, errors.New("finding_location_invalid")
			}
			start += newlines[f.StartLine-1]
		}
		idx, ok := byStart[start]
		if !ok {
			return nil, errors.New("finding_location_invalid")
		}
		if strings.Trim(text[idx[0]:idx[1]], "\n") != f.Match {
			return nil, errors.New("finding_match_mismatch")
		}
		a, z := idx[0], idx[1]
		if r.SecretGroup > 0 {
			g := r.SecretGroup * 2
			if g+1 >= len(idx) || idx[g] < 0 {
				return nil, errors.New("finding_capture_missing")
			}
			a, z = idx[g], idx[g+1]
		} else if len(idx) > 2 {
			for g := 2; g+1 < len(idx); g += 2 {
				if idx[g] >= 0 && idx[g+1] > idx[g] {
					a, z = idx[g], idx[g+1]
					break
				}
			}
		} else {
			for a < z && text[a] == '\n' {
				a++
			}
			for z > a && text[z-1] == '\n' {
				z--
			}
		}
		if a < 0 || z <= a || z > len(text) || text[a:z] != f.Secret {
			return nil, errors.New("finding_secret_mismatch")
		}
		spans = append(spans, Span{Start: a, End: z, RuleIDs: []string{b.originals[f.RuleID]}})
	}
	return Merge(spans), nil
}

// Merge covers the union of overlapping ranges, including crossing matches.
// Adjacent ranges remain separate secrets. Rule metadata order is deterministic.
func Merge(spans []Span) []Span {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End > spans[j].End
	})
	var merged []Span
	for _, s := range spans {
		if len(merged) == 0 || s.Start >= merged[len(merged)-1].End {
			merged = append(merged, s)
			continue
		}
		last := &merged[len(merged)-1]
		if s.End > last.End {
			last.End = s.End
		}
		last.RuleIDs = append(last.RuleIDs, s.RuleIDs...)
	}
	for i := range merged {
		ids := merged[i].RuleIDs
		sort.Strings(ids)
		out := ids[:0]
		for _, id := range ids {
			if len(out) == 0 || out[len(out)-1] != id {
				out = append(out, id)
			}
		}
		merged[i].RuleIDs = out
	}
	return merged
}
