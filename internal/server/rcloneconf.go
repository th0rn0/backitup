package server

import "strings"

// rclone.conf has two kinds of content. The sections backitup generates from
// the `remotes` table, and everything else — sections an operator wrote by
// hand, comments, blank lines. Regenerating the whole file from the database
// (what backitup used to do) destroys the second kind on every startup, create,
// edit and delete: an operator who hand-wrote a [crypt] section lost it the
// next time anybody touched the UI.
//
// So the writer owns only what it generates:
//
//   - sections are owned by NAME — a section is backitup's to rewrite or delete
//     exactly when a row in `remotes` has the same name;
//   - generated sections are appended in one block delimited by the markers
//     below, which is also how the writer finds last run's block again;
//   - every other line of the file is preserved byte for byte, in place.
//
// Ownership by name (rather than "everything after the marker") also migrates a
// file written by an older backitup, which has no markers: those sections match
// remote names, so they are replaced rather than duplicated — two sections with
// one name would make rclone refuse the file.
const (
	rcloneManagedBegin = "# >>> backitup managed remotes — generated from the database on every change, do not edit this block"
	rcloneManagedEnd   = "# <<< backitup managed remotes"
)

// mergeRcloneConfig builds the new rclone.conf from the file's current contents
// and the sections to generate. generatedSections are the `[name]` blocks for
// the remotes in the database; their names are the names backitup owns.
//
// Content backitup does not own is returned verbatim and in its original order,
// followed by the generated block. Passing no sections removes the block and
// keeps everything the operator wrote.
func mergeRcloneConfig(existing string, generatedSections []string) string {
	owned := make(map[string]bool, len(generatedSections))
	for _, s := range generatedSections {
		if name, ok := sectionName(firstLine(s)); ok {
			owned[name] = true
		}
	}

	preserved := stripOwnedSections(existing, owned)

	var sb strings.Builder
	if preserved != "" {
		sb.WriteString(preserved)
		if len(generatedSections) > 0 {
			sb.WriteString("\n\n")
		} else {
			// Nothing to generate: the file is the operator's content alone,
			// still terminated the way a text file should be.
			sb.WriteString("\n")
		}
	}
	if len(generatedSections) > 0 {
		sb.WriteString(rcloneManagedBegin)
		sb.WriteString("\n\n")
		for _, s := range generatedSections {
			sb.WriteString(strings.TrimRight(s, "\n"))
			sb.WriteString("\n\n")
		}
		sb.WriteString(rcloneManagedEnd)
		sb.WriteString("\n")
	}
	return sb.String()
}

// stripOwnedSections returns everything in existing that backitup must keep:
// the previous managed block removed, and any section whose name is in owned
// removed (the migration case above, and self-healing if one is edited in
// place). Trailing blank lines are trimmed so repeated writes are idempotent.
func stripOwnedSections(existing string, owned map[string]bool) string {
	if existing == "" {
		return ""
	}
	var out []string
	inBlock := false
	dropping := false // inside a section backitup owns
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if trimmed == rcloneManagedEnd {
				inBlock = false
			}
			continue
		}
		if trimmed == rcloneManagedBegin {
			inBlock = true
			continue
		}
		if name, ok := sectionName(trimmed); ok {
			// A section header either starts a section backitup owns (drop it
			// and its body) or ends the previous one (keep it).
			dropping = owned[name]
		}
		if dropping {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// sectionName reports whether line is an rclone.conf section header — "[name]"
// — and returns the name. rclone section names are matched literally.
func sectionName(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 3 || line[0] != '[' || line[len(line)-1] != ']' {
		return "", false
	}
	name := strings.TrimSpace(line[1 : len(line)-1])
	if name == "" || strings.ContainsAny(name, "[]") {
		return "", false
	}
	return name, true
}

// sectionsReferencing returns the names of the sections in existing whose
// `remote` setting points at name — a [crypt] that wraps it, whether backitup
// generated that section or an operator wrote it by hand.
//
// It answers a question the `remotes` table cannot: deleting the remote such a
// section references leaves a config rclone cannot resolve, even when the
// wrapping section is not backitup's at all. Anything after the colon
// (`remote = provider:encrypted`) is the path inside the remote, so it counts
// too. Commented-out lines are ignored — they are text, not settings.
func sectionsReferencing(existing, name string) []string {
	if name == "" {
		return nil
	}
	var refs []string
	seen := make(map[string]bool)
	current := ""
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if n, ok := sectionName(trimmed); ok {
			current = n
			continue
		}
		key, val, ok := strings.Cut(trimmed, "=")
		if !ok || current == "" || !strings.EqualFold(strings.TrimSpace(key), "remote") {
			continue
		}
		val = strings.TrimSpace(val)
		if val == name || strings.HasPrefix(val, name+":") {
			if !seen[current] {
				seen[current] = true
				refs = append(refs, current)
			}
		}
	}
	return refs
}

// firstLine returns s up to (not including) the first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
