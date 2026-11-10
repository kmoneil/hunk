package main

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A --measure run's closing table (#163): every number the command printed,
// a row for each number of each kind of line, a column for each side (base,
// then each patch), each cell the median across the rounds, and after each
// patch's cell its ratio to base. Without it a run of 5 rounds over 3
// benchmarks and 3 sides was 45 numbers to read by eye, or a script to group
// them, which the issue's author wrote three times in one session.
//
// The numbers are read from every line a run printed, before its output is
// cut to the --verify-lines tail that is shown, so a bench whose output runs
// past that tail is read whole.
//
// A line's kind is its shape: the line with each number's digits taken out
// and every run of whitespace made one space, so that columns padded to their
// numbers' widths, as Go's are, do not part one benchmark into several. Every
// time a shape is printed, in every round, is a sample of its row on that
// side, so `go test -bench -count 5` gives each cell five per round.
//
// A row is labelled as benchstat labels one, decided 2026-11-10 from four
// layouts: the text before its line's first number, then the number's own
// word, which is its unit, its key, or the field after it when that is no
// number. "BenchmarkParse-8 ns/op" is the row for the 1053 in
// "BenchmarkParse-8  1000000  1053 ns/op", and "BenchmarkParse-8" alone is
// the 1000000's. Rows that would share a label have the number's place in
// its line after it, "(2)"; rows that would share that too, and a row with no
// label at all, are labelled by their shape.

// figure is one number in one line of a run's output.
type figure struct {
	shape string // the line it is in, numbers taken out: its row's key with place
	place int    // its place among the line's numbers, from 1
	label string // its row's label, before a clash with another is settled
	value float64
	text  string // the number as printed, which a median of an odd count is
}

// readFigures is every number in lines, in order.
func readFigures(lines []string) []figure {
	var out []figure
	for _, l := range lines {
		out = append(out, lineFigures(l)...)
	}
	return out
}

// edgePunct is what may stand before or after a number in its field without
// making it part of a word: brackets, quotes, and after it, the punctuation
// that ends a clause. "[1.2345", "(32ns", "ms,", "1.38us)" and "12:" are
// numbers.
const (
	leadPunct  = "([{<\"'"
	trailPunct = ")]}>\"',;:"
	edgePunct  = "([{<)]}>\"',;:" // both, each once
)

// lineFigures reads one line: its shape, and each of its numbers with its
// label.
func lineFigures(line string) []figure {
	fields := strings.Fields(line)
	type found struct {
		i int
		n number
	}
	var nums []found
	isNum := make([]bool, len(fields))
	for i, f := range fields {
		if n, ok := readNumber(f); ok {
			nums = append(nums, found{i, n})
			isNum[i] = true
		}
	}
	if len(nums) == 0 {
		return nil
	}
	shaped := slices.Clone(fields)
	for _, f := range nums {
		shaped[f.i] = f.n.lead + f.n.key + "#" + f.n.unit + f.n.trail
	}
	shape := strings.Join(shaped, " ")
	prefix := strings.Join(fields[:nums[0].i], " ")
	out := make([]figure, 0, len(nums))
	for k, f := range nums {
		word := f.n.unit
		if word == "" {
			word = strings.TrimSuffix(f.n.key, "=")
		}
		if word == "" && f.i+1 < len(fields) && !isNum[f.i+1] {
			word = strings.Trim(fields[f.i+1], edgePunct)
		}
		out = append(out, figure{
			shape: shape, place: k + 1, label: strings.TrimSpace(prefix + " " + word),
			value: f.n.value, text: f.n.digits,
		})
	}
	return out
}

// number is a field read as a number: what stood before and after it, its
// key when the field is key=value, its digits and its unit.
type number struct {
	lead, key, digits, unit, trail string
	value                          float64
}

// readNumber reads field as a number, if it is one: past any leadPunct
// before it, any trailPunct after it and a "key=", digits with a sign, a
// point and an exponent if it has them, and then a unit of letters, % and /
// alone: 1053, -5, .5, 1e6, 3.456s, 35ns, 50%, 4.5GB/s, 1.38µs, n=5. A number
// with more after it is not one, so a version, a date, a time of day, an
// address and a hex number are text: 1.2.3, 2026-11-10, 12:30:45,
// 192.168.0.1, 0x1F, 0xff. Nor is one in a word: arm64, BenchmarkParse-8.
func readNumber(field string) (number, bool) {
	core := strings.TrimLeft(field, leadPunct)
	body := strings.TrimRight(core, trailPunct)
	n := number{lead: field[:len(field)-len(core)], trail: core[len(body):]}
	if i := strings.LastIndexByte(body, '='); i >= 0 {
		n.key, body = body[:i+1], body[i+1:]
	}
	end := numberEnd(body)
	if end == 0 {
		return number{}, false
	}
	n.digits, n.unit = body[:end], body[end:]
	if n.unit != "" && (n.unit[0] == 'x' || n.unit[0] == 'X') && strings.TrimLeft(n.digits, "+-") == "0" {
		return number{}, false
	}
	for _, r := range n.unit {
		if !unicode.IsLetter(r) && r != '%' && r != '/' {
			return number{}, false
		}
	}
	v, err := strconv.ParseFloat(n.digits, 64)
	if err != nil {
		// Out of float64's range: a number, but not one a median of means
		// anything.
		return number{}, false
	}
	n.value = v
	return n, true
}

// numberEnd is the length of the number s begins with, 0 for none: a sign,
// digits with at most one point among them, at least one digit, and an
// exponent where e is followed by digits.
func numberEnd(s string) int {
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	digits := 0
	for i < len(s) && isDigit(s[i]) {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '-' || s[j] == '+') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	return i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// measureTable is the table: its sides' names, base first, and its rows.
type measureTable struct {
	sides []string
	rows  []*measureRow
}

// measureRow is one number of one kind of line, and its samples on each
// side.
type measureRow struct {
	shape   string
	place   int
	label   string
	samples [][]figure // by side
}

// everywhere reports whether every side printed the row's line.
func (r *measureRow) everywhere() bool {
	for _, s := range r.samples {
		if len(s) == 0 {
			return false
		}
	}
	return true
}

// measureTableOf is the table of runs, measureRuns' entries, perRound of
// them a round. Rows on every side come first and then the rest, each in the
// order its line was first printed, so a row only one side has is listed
// apart rather than between two that compare.
func measureTableOf(runs []*eachPatch, perRound int) *measureTable {
	t := &measureTable{}
	for _, ep := range runs[:perRound] {
		t.sides = append(t.sides, ep.name)
	}
	type key struct {
		shape string
		place int
	}
	rows := map[key]*measureRow{}
	for _, ep := range runs {
		for _, f := range ep.figures {
			r := rows[key{f.shape, f.place}]
			if r == nil {
				r = &measureRow{shape: f.shape, place: f.place, label: f.label, samples: make([][]figure, perRound)}
				rows[key{f.shape, f.place}] = r
				t.rows = append(t.rows, r)
			}
			r.samples[ep.side] = append(r.samples[ep.side], f)
		}
	}
	slices.SortStableFunc(t.rows, func(a, b *measureRow) int {
		return boolRank(!a.everywhere()) - boolRank(!b.everywhere())
	})
	settleLabels(t.rows)
	return t
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// settleLabels makes each row's label its own: a clash gets each row's
// place in its line, and one that clashes still, or a row with no label, its
// shape and place, which are its key.
func settleLabels(rows []*measureRow) {
	for _, r := range rows {
		if r.label == "" {
			r.label = fmt.Sprintf("%s (%d)", r.shape, r.place)
		}
	}
	for _, suffix := range []func(r *measureRow) string{
		func(r *measureRow) string { return fmt.Sprintf("%s (%d)", r.label, r.place) },
		func(r *measureRow) string { return fmt.Sprintf("%s (%d)", r.shape, r.place) },
	} {
		seen := map[string]int{}
		for _, r := range rows {
			seen[r.label]++
		}
		for _, r := range rows {
			if seen[r.label] > 1 {
				r.label = suffix(r)
			}
		}
	}
}

// median is the middle of samples by value, as printed where there is one
// middle and the mean of the two where there are two, with a decimal place
// more than the finer of them where the mean needs it.
func median(samples []figure) (float64, string) {
	s := slices.SortedFunc(slices.Values(samples), func(a, b figure) int {
		switch {
		case a.value < b.value:
			return -1
		case a.value > b.value:
			return 1
		}
		return 0
	})
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid].value, s[mid].text
	}
	a, b := s[mid-1], s[mid]
	m := a.value/2 + b.value/2
	d := max(decimals(a.text), decimals(b.text))
	if d < 0 {
		return m, strconv.FormatFloat(m, 'g', -1, 64)
	}
	text := strings.TrimSuffix(strconv.FormatFloat(m, 'f', d+1, 64), "0")
	return m, strings.TrimSuffix(text, ".")
}

// decimals is how many digits after the point text has, and -1 for one with
// an exponent, whose places say nothing about its precision.
func decimals(text string) int {
	if strings.ContainsAny(text, "eE") {
		return -1
	}
	if _, frac, ok := strings.Cut(text, "."); ok {
		return len(frac)
	}
	return 0
}

// cell is a side's median of r, or false where the side printed none.
func (r *measureRow) cell(side int) (float64, string, bool) {
	if len(r.samples[side]) == 0 {
		return 0, "", false
	}
	v, text := median(r.samples[side])
	return v, text, true
}

// ratio is a patch's median over base's, or false where either has none,
// base's is zero, or the quotient is past float64's range, which JSON cannot
// carry: 1e300 over 1e-300.
func (r *measureRow) ratio(side int) (float64, bool) {
	b, _, okBase := r.cell(0)
	p, _, ok := r.cell(side)
	if !okBase || !ok || b == 0 {
		return 0, false
	}
	q := p / b
	return q, !math.IsInf(q, 0)
}

// write prints the table: a header of the sides' names, then a row a line,
// the label, base's cell, and each patch's cell and ratio. Text the command
// printed, in a label, is shown as its output is, a control character by
// name. A cell a side has no sample for, and its ratio, is "-".
func (t *measureTable) write(w io.Writer) {
	header := []string{"", showControls(t.sides[0])}
	for _, s := range t.sides[1:] {
		header = append(header, showControls(s), "")
	}
	grid := [][]string{header}
	for _, r := range t.rows {
		line := []string{shown([]byte(r.label), false)}
		for side := range t.sides {
			text := "-"
			if _, s, ok := r.cell(side); ok {
				text = s
			}
			line = append(line, text)
			if side == 0 {
				continue
			}
			ratio := "-"
			if q, ok := r.ratio(side); ok {
				ratio = fmt.Sprintf("%.2fx", q)
			}
			line = append(line, ratio)
		}
		grid = append(grid, line)
	}
	widths := make([]int, len(header))
	for _, line := range grid {
		for i, c := range line {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	for _, line := range grid {
		var b strings.Builder
		for i, c := range line {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// jsonMeasureTable is the table for --json: the sides, and each row with a
// median, a ratio and a count of samples for each side, null where the side
// has none and base's ratio always.
type jsonMeasureTable struct {
	Sides []string         `json:"sides"`
	Rows  []jsonMeasureRow `json:"rows"`
}

type jsonMeasureRow struct {
	Label   string     `json:"label"`
	Line    string     `json:"line"`
	Number  int        `json:"number"`
	Medians []*float64 `json:"medians"`
	Ratios  []*float64 `json:"ratios"`
	Samples []int      `json:"samples"`
}

func (t *measureTable) json() *jsonMeasureTable {
	jt := &jsonMeasureTable{Sides: t.sides, Rows: []jsonMeasureRow{}}
	for _, r := range t.rows {
		jr := jsonMeasureRow{Label: r.label, Line: r.shape, Number: r.place}
		for side := range t.sides {
			jr.Samples = append(jr.Samples, len(r.samples[side]))
			var m, q *float64
			if v, _, ok := r.cell(side); ok {
				m = &v
			}
			if v, ok := r.ratio(side); ok && side > 0 {
				q = &v
			}
			jr.Medians = append(jr.Medians, m)
			jr.Ratios = append(jr.Ratios, q)
		}
		jt.Rows = append(jt.Rows, jr)
	}
	return jt
}

// tailOf is the last n of lines, all of them for n of 0: what --verify-lines
// keeps of a run's output.
func tailOf(lines []string, n int) []string {
	if n > 0 && len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}
