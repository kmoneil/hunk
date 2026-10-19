package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The skill's frontmatter is YAML to everything that loads it, and until
// 2026-10-19 (#102) nothing here read it that way. Five tests read SKILL.md as
// text; its description was a plain scalar holding "invocation: exit 2", which
// no YAML loader parses, and Claude Code loads a skill whose frontmatter does
// not parse with no fields set. So the sentence the skill is chosen by was
// replaced by the body's first line, and nothing said so.
//
// The standard library has no YAML parser (§8), so frontmatter reads the
// subset the file needs and refuses the rest rather than guess at it:
// "key: value" with a plain scalar, and "key: >-" with a folded block. A plain
// scalar is held to YAML's rules for one: no ": ", which starts a mapping
// (#102); no " #", which starts a comment and drops the rest of the value
// without an error; no indicator character first.
//
// Every row of frontmatterRows was put through Ruby's YAML (Psych) when this
// was written. Each it takes, Psych parses to the same value. Each Psych
// refuses, it refuses, the three mappings at Psych's line and column. The rest
// it refuses and YAML does not: a comment, a second key, an empty value, which
// YAML truncates, overwrites or reads as null without a word; and shapes the
// file has no use for, which a reader this small would only be guessing at.
func frontmatter(src string) (map[string]string, error) {
	lines := strings.Split(src, "\n")
	if lines[0] != "---" {
		return nil, fmt.Errorf("line 1: the file does not open with ---, so a loader reads it as having no frontmatter")
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end == 0 {
		return nil, fmt.Errorf("line 1: no --- closes the frontmatter")
	}
	at := func(i, col int, format string, a ...any) error {
		return fmt.Errorf("line %d, column %d: %s", i+1, col, fmt.Sprintf(format, a...))
	}
	fields := map[string]string{}
	for i := 1; i < end; i++ {
		line := lines[i]
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, at(i, 1, "an indented line outside a block, which this reader does not take")
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok || (rest != "" && rest[0] != ' ') {
			return nil, at(i, 1, "%q is not a key and a value", line)
		}
		for j, r := range key {
			if (r < 'a' || r > 'z') && (j == 0 || (r < '0' || r > '9') && r != '-' && r != '_') {
				return nil, at(i, j+1, "%q is not a key this reader takes", key)
			}
		}
		if _, dup := fields[key]; dup {
			return nil, at(i, 1, "%s is given twice", key)
		}
		col := len(key) + 2 + len(rest) - len(strings.TrimLeft(rest, " "))
		value := strings.TrimRight(strings.TrimLeft(rest, " "), " ")
		switch {
		case value == ">-":
			var folded []string
			indent := 0
			for i+1 < end {
				// A blank line ends the block unless an indented one follows.
				k := i + 1
				for k < end && lines[k] == "" {
					k++
				}
				if k == end || lines[k][0] != ' ' && lines[k][0] != '\t' {
					break
				}
				i++
				body := strings.TrimLeft(lines[i], " ")
				n := len(lines[i]) - len(body)
				switch {
				case body == "":
					return nil, at(i, 1, "a blank line inside %s's block, which this reader does not take", key)
				case n == 0:
					return nil, at(i, 1, "a tab indents %s's block, and YAML indents with spaces only", key)
				case indent == 0:
					indent = n
				case n != indent:
					return nil, at(i, n+1, "%s's block changes its indentation, which this reader does not take", key)
				}
				folded = append(folded, body)
			}
			if len(folded) == 0 {
				return nil, at(i, col, "%s's block is empty", key)
			}
			value = strings.Join(folded, " ")
		case value == "":
			return nil, at(i, col, "%s has no value, which YAML reads as null", key)
		case strings.ContainsRune(`'"|>`, rune(value[0])):
			return nil, at(i, col, "%s is a quoted or block scalar this reader does not take; it takes plain ones and >-", key)
		case strings.ContainsRune(",[]{}#&*!%@`", rune(value[0])),
			strings.ContainsRune("-?:", rune(value[0])) && (len(value) == 1 || value[1] == ' '):
			return nil, at(i, col, "%s's value starts with %q, which YAML reads as an indicator, not text", key, value[0])
		default:
			// A blank is a space or a tab, to YAML as to a shell.
			blank := func(j int) bool { return value[j] == ' ' || value[j] == '\t' }
			for j := range len(value) {
				switch {
				case value[j] == ':' && (j+1 == len(value) || blank(j+1)):
					return nil, at(i, col+j, "%s's value holds \": \" unquoted, which YAML reads as a mapping; fold it with >-", key)
				case value[j] == '#' && j > 0 && blank(j-1):
					return nil, at(i, col+j, "%s's value holds \" #\" unquoted, which YAML reads as a comment, dropping the rest; fold it with >-", key)
				}
			}
		}
		fields[key] = value
	}
	return fields, nil
}

// skillFields is what the Agent Skills format asks of the two fields it
// requires (agentskills.io/specification, read 2026-10-19): a name of 1 to 64
// lowercase letters, digits and single hyphens, neither first nor last, that is
// its directory's name; and a description of 1 to 1024 characters.
func skillFields(dir string, f map[string]string) []string {
	var bad []string
	name, ok := f["name"]
	switch {
	case !ok:
		bad = append(bad, "no name")
	case name != dir:
		bad = append(bad, fmt.Sprintf("name %q is not its directory's, %q", name, dir))
	case len(name) > 64:
		bad = append(bad, fmt.Sprintf("name is %d characters, over 64", len(name)))
	case strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--"):
		bad = append(bad, fmt.Sprintf("name %q has a hyphen first, last or doubled", name))
	case strings.IndexFunc(name, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' }) >= 0:
		bad = append(bad, fmt.Sprintf("name %q holds a character other than a-z, 0-9 and -", name))
	}
	switch d, ok := f["description"]; {
	case !ok:
		bad = append(bad, "no description")
	case d == "":
		bad = append(bad, "description is empty")
	case utf8.RuneCountInString(d) > 1024:
		bad = append(bad, fmt.Sprintf("description is %d characters, over 1024", utf8.RuneCountInString(d)))
	}
	return bad
}

func TestTheSkillFrontmatterIsYAML(t *testing.T) {
	dir := filepath.Join(".agents", "skills", "hunk")
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	f, err := frontmatter(string(b))
	if err != nil {
		t.Fatalf("SKILL.md's frontmatter does not parse as YAML, so a loader drops every field of it (#102): %v", err)
	}
	for _, p := range skillFields(filepath.Base(dir), f) {
		t.Errorf("SKILL.md: %s", p)
	}
}

// frontmatterRows is every shape frontmatter takes or refuses. err is a
// substring of the refusal, with its line and column, and "" a frontmatter
// that parses to want.
var frontmatterRows = []struct {
	name, src string
	want      map[string]string
	err       string
}{
	{"two plain scalars", "---\nname: hunk\ndescription: Edit files.\n---\nbody\n", map[string]string{"name": "hunk", "description": "Edit files."}, ""},
	{"a folded block", "---\nname: hunk\ndescription: >-\n  Edit files\n  with hunk.\n---\n", map[string]string{"name": "hunk", "description": "Edit files with hunk."}, ""},
	{"a folded block keeps a colon and a hash", "---\nd: >-\n  invocation: exit 2 #1\n---\n", map[string]string{"d": "invocation: exit 2 #1"}, ""},
	{"a folded block's lines may start with what a plain scalar may not", "---\nd: >-\n  a\n  <<'PY'\n  `hunk`\n---\n", map[string]string{"d": "a <<'PY' `hunk`"}, ""},
	{"a folded block at the end of the frontmatter", "---\nd: >-\n  x\n---", map[string]string{"d": "x"}, ""},
	{"a colon with no space after it", "---\nd: see http://x and a:b\n---\n", map[string]string{"d": "see http://x and a:b"}, ""},
	{"a hash with no space before it", "---\nd: issue#102\n---\n", map[string]string{"d": "issue#102"}, ""},
	{"a hyphen that starts a word", "---\nd: -f FILE\n---\n", map[string]string{"d": "-f FILE"}, ""},
	{"blank lines, comments and trailing blanks", "---\n# a comment\nname: hunk  \n\nd: x\n---\n", map[string]string{"name": "hunk", "d": "x"}, ""},
	{"a key with a digit, a hyphen and an underscore", "---\nallowed-tools: Read\nwhen_to_use: x\nk2: y\n---\n", map[string]string{"allowed-tools": "Read", "when_to_use": "x", "k2": "y"}, ""},

	{"a blank line after a block", "---\nd: >-\n  x\n\n\nname: hunk\n---\n", map[string]string{"d": "x", "name": "hunk"}, ""},

	{"#102: a colon and a space in a plain scalar", "---\nname: hunk\ndescription: Read this first: exit 2 is the tool working\n---\n", nil, `line 3, column 29: description's value holds ": "`},
	{"a colon and a tab", "---\nd: a:\tb\n---\n", nil, `line 2, column 5: d's value holds ": "`},
	{"a colon that ends a plain scalar", "---\nd: ends with a colon:\n---\n", nil, `line 2, column 21: d's value holds ": "`},
	{"a space and a hash in a plain scalar", "---\nd: exit 2 #the tool\n---\n", nil, `line 2, column 11: d's value holds " #"`},
	{"a tab and a hash", "---\nd: x\t#y\n---\n", nil, `line 2, column 6: d's value holds " #"`},
	{"a hash first, which leaves no value", "---\nd: #x\n---\n", nil, "line 2, column 4: d's value starts with '#'"},
	{"an indicator first", "---\nd: `hunk` edits files\n---\n", nil, "line 2, column 4: d's value starts with '`'"},
	{"a block-sequence entry first", "---\nd: - x\n---\n", nil, "line 2, column 4: d's value starts with '-'"},
	{"a quoted scalar", "---\nd: \"x\"\n---\n", nil, "line 2, column 4: d is a quoted or block scalar"},
	{"a literal block", "---\nd: |\n  x\n---\n", nil, "line 2, column 4: d is a quoted or block scalar"},
	{"no value", "---\nd:\n---\n", nil, "line 2, column 3: d has no value"},
	{"an empty block", "---\nd: >-\n---\n", nil, "line 2, column 4: d's block is empty"},
	{"a blank line in a block", "---\nd: >-\n  a\n\n  b\n---\n", nil, "line 4, column 1: a blank line inside d's block"},
	{"a line of spaces in a block", "---\nd: >-\n  a\n  \n  b\n---\n", nil, "line 4, column 1: a blank line inside d's block"},
	{"a block that changes its indentation", "---\nd: >-\n  a\n    b\n---\n", nil, "line 4, column 5: d's block changes its indentation"},
	{"a tab that indents a block", "---\nd: >-\n\tx\n---\n", nil, "line 3, column 1: a tab indents d's block"},
	{"an indented line outside a block", "---\nd: x\n  y\n---\n", nil, "line 3, column 1: an indented line outside a block"},
	{"a line that is not a key", "---\njust text\n---\n", nil, `line 2, column 1: "just text" is not a key and a value`},
	{"a colon glued to its value", "---\nd:x\n---\n", nil, `line 2, column 1: "d:x" is not a key and a value`},
	{"a key with a capital", "---\nName: x\n---\n", nil, `line 2, column 1: "Name" is not a key`},
	{"a key that starts with a digit", "---\n2d: x\n---\n", nil, `line 2, column 1: "2d" is not a key`},
	{"a key with a space", "---\nd d: x\n---\n", nil, `line 2, column 2: "d d" is not a key`},
	{"a key given twice", "---\nd: x\nd: y\n---\n", nil, "line 3, column 1: d is given twice"},
	{"no opening ---", "name: hunk\n---\n", nil, "line 1: the file does not open with ---"},
	{"an opening --- after a blank line", "\n---\nname: hunk\n---\n", nil, "line 1: the file does not open with ---"},
	{"no closing ---", "---\nname: hunk\n", nil, "line 1: no --- closes the frontmatter"},
}

func TestFrontmatterReadsAsYAMLDoes(t *testing.T) {
	for _, tc := range frontmatterRows {
		t.Run(tc.name, func(t *testing.T) {
			got, err := frontmatter(tc.src)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one holding %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSkillFieldsFollowTheFormat(t *testing.T) {
	long := strings.Repeat("é", 1025)
	for _, tc := range []struct {
		name, dir string
		fields    map[string]string
		want      string
	}{
		{"the skill as it ships", "hunk", map[string]string{"name": "hunk", "description": "Edit files."}, ""},
		{"a hyphen inside the name", "a-b2", map[string]string{"name": "a-b2", "description": "x"}, ""},
		{"a description of 1024 characters, counted as characters", "hunk", map[string]string{"name": "hunk", "description": long[2:]}, ""},
		{"no name", "hunk", map[string]string{"description": "x"}, "no name"},
		{"a name that is not the directory's", "hunk", map[string]string{"name": "hunks", "description": "x"}, `name "hunks" is not its directory's, "hunk"`},
		{"a name over 64", strings.Repeat("a", 65), map[string]string{"name": strings.Repeat("a", 65), "description": "x"}, "name is 65 characters, over 64"},
		{"a hyphen first", "-a", map[string]string{"name": "-a", "description": "x"}, "has a hyphen first, last or doubled"},
		{"a hyphen last", "a-", map[string]string{"name": "a-", "description": "x"}, "has a hyphen first, last or doubled"},
		{"a doubled hyphen", "a--b", map[string]string{"name": "a--b", "description": "x"}, "has a hyphen first, last or doubled"},
		{"a capital", "Hunk", map[string]string{"name": "Hunk", "description": "x"}, "holds a character other than a-z, 0-9 and -"},
		{"an underscore", "a_b", map[string]string{"name": "a_b", "description": "x"}, "holds a character other than a-z, 0-9 and -"},
		{"no description", "hunk", map[string]string{"name": "hunk"}, "no description"},
		{"an empty description", "hunk", map[string]string{"name": "hunk", "description": ""}, "description is empty"},
		{"a description over 1024", "hunk", map[string]string{"name": "hunk", "description": long}, "description is 1025 characters, over 1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(skillFields(tc.dir, tc.fields), "; ")
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("skillFields = %q, want %q", got, tc.want)
			}
		})
	}
}
