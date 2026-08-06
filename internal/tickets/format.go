package tickets

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// allowedKeys is the whole frontmatter vocabulary. A key outside this set is
// a typo, not a value to carry forward silently.
var allowedKeys = map[string]bool{
	"id":         true,
	"title":      true,
	"status":     true,
	"branch":     true,
	"pr":         true,
	"depends_on": true,
	"updated":    true,
}

// listKeys is the subset of allowedKeys that may take list form
// ("key:" on its own line, followed by "  - value" lines). Every other key
// must be a plain scalar.
var listKeys = map[string]bool{
	"depends_on": true,
}

const logHeading = "## Log"

// parse decodes one ticket file: frontmatter between two "---" lines, then a
// markdown body. It is strict — an unknown key, a key used in the wrong
// shape (scalar vs list), a missing required field, or a field that fails
// validateShape are all errors, not warnings.
func parse(b []byte) (Ticket, error) {
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Ticket{}, errors.New("missing frontmatter opening \"---\"")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return Ticket{}, errors.New("missing frontmatter closing \"---\"")
	}

	scalars, lists, err := parseFrontmatter(lines[1:end])
	if err != nil {
		return Ticket{}, err
	}

	id, ok := scalars["id"]
	if !ok || id == "" {
		return Ticket{}, errors.New("missing required field \"id\"")
	}
	title, ok := scalars["title"]
	if !ok || strings.TrimSpace(title) == "" {
		return Ticket{}, errors.New("missing required field \"title\"")
	}
	status, ok := scalars["status"]
	if !ok || status == "" {
		return Ticket{}, errors.New("missing required field \"status\"")
	}
	updatedRaw, ok := scalars["updated"]
	if !ok || updatedRaw == "" {
		return Ticket{}, errors.New("missing required field \"updated\"")
	}
	updated, err := time.Parse(time.RFC3339, updatedRaw)
	if err != nil {
		return Ticket{}, fmt.Errorf("bad \"updated\" timestamp %q: %w", updatedRaw, err)
	}

	t := Ticket{
		ID:        id,
		Title:     title,
		Status:    status,
		Branch:    scalars["branch"],
		PR:        scalars["pr"],
		DependsOn: lists["depends_on"],
		Updated:   updated,
	}
	if err := validateShape(t); err != nil {
		return Ticket{}, err
	}

	bodyLines := lines[end+1:]
	if len(bodyLines) > 0 && bodyLines[0] == "" {
		bodyLines = bodyLines[1:]
	}
	t.Body = strings.Join(bodyLines, "\n")

	return t, nil
}

// parseFrontmatter reads the lines strictly between the "---" delimiters
// into scalar and list fields, by key. It is the whole parser: fields here
// are flat enough that a YAML dependency would buy nothing but a bigger
// attack surface for the "unknown key" check.
func parseFrontmatter(lines []string) (scalars map[string]string, lists map[string][]string, err error) {
	scalars = map[string]string{}
	lists = map[string][]string{}
	currentList := ""

	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}

		if strings.HasPrefix(raw, "  - ") {
			if currentList == "" {
				return nil, nil, fmt.Errorf("list item %q has no preceding key", strings.TrimSpace(raw))
			}
			lists[currentList] = append(lists[currentList], strings.TrimSpace(raw[len("  - "):]))
			continue
		}

		key, val, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, nil, fmt.Errorf("malformed frontmatter line %q", raw)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		if !allowedKeys[key] {
			return nil, nil, fmt.Errorf("unknown frontmatter key %q", key)
		}

		if val == "" {
			if !listKeys[key] {
				return nil, nil, fmt.Errorf("key %q must have a value", key)
			}
			currentList = key
			if _, exists := lists[key]; !exists {
				lists[key] = []string{}
			}
			continue
		}

		currentList = ""
		if _, exists := scalars[key]; exists {
			return nil, nil, fmt.Errorf("duplicate frontmatter key %q", key)
		}
		scalars[key] = val
	}

	return scalars, lists, nil
}

// render serializes t back into the exact frontmatter shape parse expects,
// in a fixed field order, so the file a repeated Put writes is a minimal git
// diff rather than a reshuffled one.
func render(t Ticket) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("id: " + t.ID + "\n")
	b.WriteString("title: " + t.Title + "\n")
	b.WriteString("status: " + t.Status + "\n")
	if t.Branch != "" {
		b.WriteString("branch: " + t.Branch + "\n")
	}
	if t.PR != "" {
		b.WriteString("pr: " + t.PR + "\n")
	}
	if len(t.DependsOn) > 0 {
		b.WriteString("depends_on:\n")
		for _, dep := range t.DependsOn {
			b.WriteString("  - " + dep + "\n")
		}
	}
	b.WriteString("updated: " + t.Updated.UTC().Format(time.RFC3339) + "\n")
	b.WriteString("---\n")
	b.WriteString("\n")
	b.WriteString(t.Body)
	return []byte(b.String())
}

// slugify lowercases title and collapses every run of characters outside
// [a-z0-9] into a single '-', for the human-readable half of a ticket's
// filename. The id, not this, is what the store actually keys on.
func slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 60 {
		slug = strings.Trim(slug[:60], "-")
	}
	if slug == "" {
		slug = "ticket"
	}
	return slug
}

// appendLog appends note to the body's "## Log" section, creating the
// section if this is the first note. Callers pass an already-formatted
// RFC3339 timestamp so the whole store stamps notes with the same clock it
// stamps Updated with.
func appendLog(body, note, timestamp string) string {
	entry := fmt.Sprintf("- %s: %s", timestamp, note)

	if !strings.Contains(body, logHeading) {
		body = strings.TrimRight(body, "\n")
		if body != "" {
			body += "\n\n"
		}
		return body + logHeading + "\n\n" + entry + "\n"
	}

	return strings.TrimRight(body, "\n") + "\n" + entry + "\n"
}
