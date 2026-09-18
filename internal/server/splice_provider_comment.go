package server

import (
	"fmt"
	"strings"
)

// spliceProviderComment handles provider disable/enable by
// commenting out / uncommenting the entire [[providers]] block.
//
// disabled=true:  every line of the provider's block gets a "#"
// prefix — the block becomes a single commented region, so
// config.Load skips it entirely and the provider drops out of the
// live pool. Comments and nested tables are carried through (they
// just also get commented — that's fine, they stay commented).
// Any [[combo]] block targeting the disabled provider is also
// commented out (it can't resolve offline); it is uncommented
// on re-enable.
//
// disabled=false: uncomment only lines that were commented
// (lines starting with "#") in the provider's block range
// and in any #[[combo]] blocks, remove any "disabled = true"
// lines, and round-trip validate via validateLines.
//
// NOTE on round-trip: disable comments EVERY line in the block
// range (including blank lines and pre-existing comments),
// so enable must only strip "#" from lines that have it.
// validateLines guards the result against TOML parse errors.
func spliceProviderComment(lines []string, name string, disabled bool) ([]string, error) {
	if disabled {
		providerFound := false
		candidate := cloneLines(lines)
		// Comment out the provider block.
		for _, b := range scanBlocks(lines, "[[providers]]") {
			n, ok := blockName(lines, b)
			if !ok || n != name {
				continue
			}
			providerFound = true
			for i := b.start; i < b.end; i++ {
				candidate[i] = "#" + candidate[i]
			}
		}
		if !providerFound {
			return nil, fmt.Errorf("provider %s not found in config", name)
		}
		// Also comment out any [[combo]] blocks that reference
		// the disabled provider — they can't resolve offline.
		for _, b := range scanBlocks(lines, "[[combo]]") {
			_, ok := blockName(lines, b)
			if !ok {
				continue
			}
			for i := b.start; i < b.end; i++ {
				t := strings.TrimSpace(lines[i])
				if strings.HasPrefix(t, "targets") && strings.Contains(t, name+"/") {
					for j := b.start; j < b.end; j++ {
						candidate[j] = "#" + candidate[j]
					}
					break
				}
			}
		}
		if err := validateLines(candidate); err != nil {
			return nil, err
		}
		return candidate, nil
	}

	// Enable: find the commented provider block by name,
	// uncomment all lines in its range, uncomment any
	// #[[combo]] blocks, remove "disabled = true" lines,
	// then round-trip validate.
	candidate := cloneLines(lines)

	// Find the #[[providers]] header matching `name`
	// and determine the full commented range by extending
	// forward through consecutive commented lines.
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t != "#[[providers]]" {
			continue
		}
		// Determine the block range: from i forward
		// through consecutive commented lines.
		end := i + 1
		for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "#") {
			end++
		}
		// Check if this block matches `name`.
		n, ok := commentedBlockName(lines, tomlBlock{i, end})
		if !ok || n != name {
			i = end // skip past this block
			continue
		}
		// Uncomment all lines in [i, end).
		for j := i; j < end; j++ {
			if strings.HasPrefix(strings.TrimSpace(candidate[j]), "#") {
				tt := strings.TrimSpace(candidate[j])
				tt = strings.TrimPrefix(tt, "#")
				tt = strings.TrimPrefix(tt, " ")
				candidate[j] = tt
			}
		}
		i = end // skip past this block
	}

	// Uncomment all #[[combo]] blocks.
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t != "#[[combo]]" {
			continue
		}
		end := i + 1
		for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "#") {
			end++
		}
		for j := i; j < end; j++ {
			if strings.HasPrefix(strings.TrimSpace(candidate[j]), "#") {
				tt := strings.TrimSpace(candidate[j])
				tt = strings.TrimPrefix(tt, "#")
				tt = strings.TrimPrefix(tt, " ")
				candidate[j] = tt
			}
		}
		i = end
	}
	// Remove any "disabled = true" lines left in provider blocks.
	for _, b := range scanBlocks(candidate, "[[providers]]") {
		for i := b.start; i < b.end; i++ {
			if strings.TrimSpace(candidate[i]) == "disabled = true" {
				candidate = append(candidate[:i], candidate[i+1:]...)
				break
			}
		}
	}
	if err := validateLines(candidate); err != nil {
		return nil, err
	}
	return candidate, nil
}

// scanCommentedBlockHeaders returns the line ranges of every
// #[[providers]] / #[[combo]] block (a commented-out TOML table).
// The block extends through nested sub-tables whose headers are
// also commented (#[[providers.accounts]], # [providers.extra_headers]
// — note the space before "[": TOML allows whitespace inside brackets).
func scanCommentedBlockHeaders(lines []string, header string) []tomlBlock {
	root := strings.Trim(header, "[]") + "."
	var out []tomlBlock
	cur := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		inner := strings.TrimSpace(t[1:])
		if !strings.HasPrefix(inner, "[") {
			continue
		}
		switch {
		case inner == header:
			if cur >= 0 {
				out = append(out, tomlBlock{cur, i})
			}
			cur = i
		case strings.HasPrefix(inner, "[["+root) || strings.HasPrefix(inner, "["+root):
			// nested sub-table header (possibly with a space: "# [providers.extra_headers]")
		default:
			if cur >= 0 {
				out = append(out, tomlBlock{cur, i})
				cur = -1
			}
		}
	}
	if cur >= 0 {
		out = append(out, tomlBlock{cur, len(lines)})
	}
	return out
}

// commentedBlockName extracts the provider/combo name from a commented
// block: looks for a "#name = ..." line within the block range.
func commentedBlockName(lines []string, b tomlBlock) (string, bool) {
	for i := b.start + 1; i < b.end; i++ {
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "#") {
			continue
		}
		inner := strings.TrimSpace(t[1:])
		eq := strings.Index(inner, "=")
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(inner[:eq]) != "name" {
			continue
		}
		if v, ok := parseTOMLString(inner[eq+1:]); ok {
			return v, true
		}
	}
	return "", false
}
