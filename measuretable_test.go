package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The closing table of a --measure run (#163). readNumber and lineFigures
// are what is a number and what a row is called; median, the table and its
// two renderings are what a cell says; the CLI rows are the issue's own repro
// and the cases the table has to leave out or keep apart.

func TestReadNumber(t *testing.T) {
	for _, c := range []struct {
		field                          string
		lead, key, digits, unit, trail string
		value                          float64
	}{
		{"1053", "", "", "1053", "", "", 1053},
		{"-5", "", "", "-5", "", "", -5},
		{"+5", "", "", "+5", "", "", 5},
		{".5", "", "", ".5", "", "", 0.5},
		{"5.", "", "", "5.", "", "", 5},
		{"0", "", "", "0", "", "", 0},
		{"1e6", "", "", "1e6", "", "", 1e6},
		{"1E-3", "", "", "1E-3", "", "", 1e-3},
		{"2.5e+2ms", "", "", "2.5e+2", "ms", "", 250},
		{"3e", "", "", "3", "e", "", 3},
		{"3.456s", "", "", "3.456", "s", "", 3.456},
		{"35ns", "", "", "35", "ns", "", 35},
		{"50%", "", "", "50", "%", "", 50},
		{"4.5GB/s", "", "", "4.5", "GB/s", "", 4.5},
		{"1.38µs", "", "", "1.38", "µs", "", 1.38},
		{"n=5", "", "n=", "5", "", "", 5},
		{"a=b=7", "", "a=b=", "7", "", "", 7},
		{"[1.2345", "[", "", "1.2345", "", "", 1.2345},
		{"(32ns", "(", "", "32", "ns", "", 32},
		{"1.38us)", "", "", "1.38", "us", ")", 1.38},
		{"(0.00s)", "(", "", "0.00", "s", ")", 0},
		{"12:", "", "", "12", "", ":", 12},
		{"100,", "", "", "100", "", ",", 100},
		{`"7"`, `"`, "", "7", "", `"`, 7},
	} {
		t.Run(c.field, func(t *testing.T) {
			n, ok := readNumber(c.field)
			if !ok {
				t.Fatalf("readNumber(%q) is no number", c.field)
			}
			want := number{c.lead, c.key, c.digits, c.unit, c.trail, c.value}
			if n != want {
				t.Errorf("readNumber(%q) = %+v, want %+v", c.field, n, want)
			}
		})
	}
	for _, field := range []string{
		"arm64", "BenchmarkParse-8", "M1", "1.2.3", "2026-11-10", "12:30:45", "192.168.0.1",
		"0x1F", "0xff", "0Xff", "-0xff", "-", "+", ".", "e5", "ns/op", "1,234", "5x5", "1e999",
		"x=", "±", "", "(", "v1.2", "Inf", "NaN", "+Inf",
	} {
		t.Run("not "+field, func(t *testing.T) {
			if n, ok := readNumber(field); ok {
				t.Errorf("readNumber(%q) = %+v, want no number", field, n)
			}
		})
	}
}

func TestLineFigures(t *testing.T) {
	type fig struct {
		label, text string
	}
	for _, c := range []struct {
		name, line, shape string
		figs              []fig
	}{
		{"the issue's line", "BenchShort 100 ns/op", "BenchShort # ns/op", []fig{{"BenchShort ns/op", "100"}}},
		{
			"go's bench line, padded with tabs and spaces",
			"BenchmarkParse-8   \t 1000000\t  1053 ns/op\t 48 B/op\t 2 allocs/op",
			"BenchmarkParse-8 # # ns/op # B/op # allocs/op",
			[]fig{{"BenchmarkParse-8", "1000000"}, {"BenchmarkParse-8 ns/op", "1053"}, {"BenchmarkParse-8 B/op", "48"}, {"BenchmarkParse-8 allocs/op", "2"}},
		},
		{
			"the same line padded otherwise is the same shape",
			"BenchmarkParse-8 900000 1210 ns/op 48 B/op 2 allocs/op",
			"BenchmarkParse-8 # # ns/op # B/op # allocs/op",
			[]fig{{"BenchmarkParse-8", "900000"}, {"BenchmarkParse-8 ns/op", "1210"}, {"BenchmarkParse-8 B/op", "48"}, {"BenchmarkParse-8 allocs/op", "2"}},
		},
		{"go's ok line: the unit is the number's own", "ok  \texample.com/x\t3.456s", "ok example.com/x #s", []fig{{"ok example.com/x s", "3.456"}}},
		{
			"criterion: three numbers with one word",
			"time:   [1.2345 µs 1.2400 µs 1.2456 µs]",
			"time: [# µs # µs # µs]",
			[]fig{{"time: µs", "1.2345"}, {"time: µs", "1.2400"}, {"time: µs", "1.2456"}},
		},
		{"key=value: the key is the word", "n=5 ops=1000", "n=# ops=#", []fig{{"n", "5"}, {"ops", "1000"}}},
		{"a word in brackets after the number", "took 5 (ms)", "took # (ms)", []fig{{"took ms", "5"}}},
		{"a line that starts with a number", "42 widgets", "# widgets", []fig{{"widgets", "42"}}},
		{"a number alone has no label", "  42  ", "#", []fig{{"", "42"}}},
		{"a number then a number has no word", "x 1 2", "x # #", []fig{{"x", "1"}, {"x", "2"}}},
		{"a number in a word is text", "goarch: arm64", "", nil},
		{"no number", "PASS", "", nil},
		{"empty", "", "", nil},
		{"whitespace", " \t ", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := lineFigures(c.line)
			if len(got) != len(c.figs) {
				t.Fatalf("lineFigures(%q) = %+v, want %d figures", c.line, got, len(c.figs))
			}
			for i, f := range got {
				if f.shape != c.shape || f.place != i+1 || f.label != c.figs[i].label || f.text != c.figs[i].text {
					t.Errorf("figure %d = {shape %q, place %d, label %q, text %q}, want {%q, %d, %q, %q}",
						i, f.shape, f.place, f.label, f.text, c.shape, i+1, c.figs[i].label, c.figs[i].text)
				}
			}
		})
	}
}

// figs is samples of one row, from their texts.
func figs(texts ...string) []figure {
	var out []figure
	for _, s := range texts {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			panic(err)
		}
		out = append(out, figure{value: v, text: s})
	}
	return out
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		samples []string
		want    string
		value   float64
	}{
		{[]string{"7"}, "7", 7},
		{[]string{"3", "1", "2"}, "2", 2},
		{[]string{"45.60", "45.1", "46"}, "45.60", 45.6},
		{[]string{"100", "100"}, "100", 100},
		{[]string{"1060", "1053"}, "1056.5", 1056.5},
		{[]string{"45.6", "45.7"}, "45.65", 45.65},
		{[]string{"2.0", "2.0"}, "2.0", 2},
		{[]string{"1", "2", "3", "4"}, "2.5", 2.5},
		{[]string{"1.5", "2"}, "1.75", 1.75},
		{[]string{"-1", "1"}, "0", 0},
		{[]string{"1e6", "2e6"}, "1.5e+06", 1.5e6},
		{[]string{"1.7976931348623157e308", "1.7976931348623157e308"}, "1.7976931348623157e+308", math.MaxFloat64},
	} {
		t.Run(strings.Join(c.samples, ","), func(t *testing.T) {
			v, text := median(figs(c.samples...))
			if text != c.want || math.Abs(v-c.value) > 1e-9*math.Abs(c.value) {
				t.Errorf("median = %v %q, want %v %q", v, text, c.value, c.want)
			}
		})
	}
}

// measuredRuns is runs for measureTableOf: outputs[round][side] is what that
// run printed, a nil one a run that did not finish.
func measuredRuns(names []string, outputs ...[][]string) []*eachPatch {
	var runs []*eachPatch
	for r, round := range outputs {
		for side, out := range round {
			ep := &eachPatch{name: names[side], base: side == 0, round: r + 1, rounds: len(outputs), side: side}
			ep.figures = readFigures(out)
			runs = append(runs, ep)
		}
	}
	return runs
}

func TestMeasureTable(t *testing.T) {
	render := func(tab *measureTable) string {
		var b bytes.Buffer
		tab.write(&b)
		return b.String()
	}

	t.Run("the issue's two rounds", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "a.hunk"},
			[][]string{{"BenchShort 100 ns/op", "BenchLong 150 ns/op"}, {"BenchShort 200 ns/op", "BenchLong 250 ns/op"}},
			[][]string{{"BenchShort 110 ns/op", "BenchLong 140 ns/op"}, {"BenchShort 220 ns/op", "BenchLong 260 ns/op"}},
		)
		want := "" +
			"                  base  a.hunk\n" +
			"BenchShort ns/op  105   210     2.00x\n" +
			"BenchLong ns/op   145   255     1.76x\n"
		if got := render(measureTableOf(runs, 2)); got != want {
			t.Errorf("table:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("a row not on every side comes after, its missing cells and ratios a dash", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "a.hunk", "b.hunk"},
			[][]string{{"only base 4", "both 10"}, {"both 20", "only a 7"}, {"both 5"}},
		)
		want := "" +
			"           base  a.hunk         b.hunk\n" +
			"both       10    20      2.00x  5       0.50x\n" +
			"only base  4     -       -      -       -\n" +
			"only a     -     7       -      -       -\n"
		if got := render(measureTableOf(runs, 3)); got != want {
			t.Errorf("table:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("a zero base has no ratio, nor one past float64", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "p"},
			[][]string{{"z 0", "zz 0", "huge 1e-300"}, {"z 5", "zz 0", "huge 1e300"}},
		)
		want := "" +
			"      base    p\n" +
			"z     0       5      -\n" +
			"zz    0       0      -\n" +
			"huge  1e-300  1e300  -\n"
		tab := measureTableOf(runs, 2)
		if got := render(tab); got != want {
			t.Errorf("table:\n%s\nwant:\n%s", got, want)
		}
		if _, err := json.Marshal(tab.json()); err != nil {
			t.Errorf("--json cannot carry the table: %v", err)
		}
	})

	t.Run("labels that clash take their place, then their shape", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "p"},
			[][]string{
				{"time: [1 µs 2 µs 3 µs]", "time: 4 µs", "time: x 5 µs", "6"},
				{"time: [1 µs 2 µs 3 µs]", "time: 4 µs", "time: x 5 µs", "6"},
			},
		)
		var labels []string
		for _, r := range measureTableOf(runs, 2).rows {
			labels = append(labels, r.label)
		}
		want := []string{
			"time: [# µs # µs # µs] (1)", "time: µs (2)", "time: µs (3)",
			"time: # µs (1)", "time: x µs", "# (1)",
		}
		if !slices.Equal(labels, want) {
			t.Errorf("labels = %q\nwant     %q", labels, want)
		}
	})

	t.Run("every printing of a shape is a sample, as go test -count gives", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "p"},
			[][]string{{"B 1 ns/op", "B 2 ns/op", "B 9 ns/op"}, {"B 4 ns/op"}},
			[][]string{{"B 3 ns/op"}, {"B 6 ns/op", "B 5 ns/op"}},
		)
		tab := measureTableOf(runs, 2)
		jt := tab.json()
		if len(jt.Rows) != 1 || !slices.Equal(jt.Rows[0].Samples, []int{4, 3}) {
			t.Fatalf("rows = %+v", jt.Rows)
		}
		if m := jt.Rows[0].Medians; *m[0] != 2.5 || *m[1] != 5 {
			t.Errorf("medians = %v %v, want 2.5 5", *m[0], *m[1])
		}
		if r := jt.Rows[0].Ratios; r[0] != nil || *r[1] != 2 {
			t.Errorf("ratios = %v", r)
		}
	})

	t.Run("json: a null where a side has none, and for base's ratio", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "p"}, [][]string{{"a 2", "b 1"}, {"a 3"}})
		out, err := json.Marshal(measureTableOf(runs, 2).json())
		must(t, err)
		want := `{"sides":["base","p"],"rows":[` +
			`{"label":"a","line":"a #","number":1,"medians":[2,3],"ratios":[null,1.5],"samples":[1,1]},` +
			`{"label":"b","line":"b #","number":1,"medians":[1,null],"ratios":[null,null],"samples":[1,0]}]}`
		if string(out) != want {
			t.Errorf("json = %s\nwant   %s", out, want)
		}
	})

	t.Run("a control character in a label or a name is shown by name", func(t *testing.T) {
		runs := measuredRuns([]string{"base", "p\x1b"}, [][]string{{"a\x07b 1"}, {"a\x07b 2"}})
		got := render(measureTableOf(runs, 2))
		if strings.ContainsAny(got, "\x07\x1b") || !strings.Contains(got, "a<U+0007>b") || !strings.Contains(got, "p<U+001B>") {
			t.Errorf("table = %q", got)
		}
	})
}

// measureWords is a --measure command that prints the issue's two bench
// lines, each number made from the digit in f.txt.
const measureWords = `v=$(tr -dc 0-9 < f.txt); echo "BenchShort ${v}00 ns/op"; echo "BenchLong ${v}50 ns/op"`

func measureTree(t *testing.T) (root, a, b string) {
	t.Helper()
	root = cliTree(t, map[string]string{"f.txt": "n = 1\n"})
	dir := t.TempDir()
	a = writePatch(t, dir, "a.hunk", "@@ file f.txt\n@@ old\nn = 1\n@@ new\nn = 2\n")
	b = writePatch(t, dir, "b.hunk", "@@ file f.txt\n@@ old\nn = 1\n@@ new\nn = 3\n")
	return root, a, b
}

func TestMeasureEndsWithTheTable(t *testing.T) {
	t.Run("the issue's repro", func(t *testing.T) {
		root, a, _ := measureTree(t)
		code, out, errOut := runCLI(t, root, []string{"--measure", "--rounds", "2", "--try", measureWords, "--each", a}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		table, summary, _ := strings.Cut(out, "tried ")
		want := "" +
			"                  base  " + a + "\n" +
			"BenchShort ns/op  100   200" + strings.Repeat(" ", len(a)-1) + "2.00x\n" +
			"BenchLong ns/op   150   250" + strings.Repeat(" ", len(a)-1) + "1.67x\n"
		if table != want {
			t.Errorf("table:\n%s\nwant:\n%s", table, want)
		}
		if !strings.HasPrefix(summary, "base and 1 patch in 2 rounds and put back every file; every command exited 0") {
			t.Errorf("summary = %q", summary)
		}
	})

	t.Run("read from every line, not only the tail that is shown", func(t *testing.T) {
		root, a, _ := measureTree(t)
		command := measureWords + `; i=0; while [ $i -lt 50 ]; do echo "filler $i"; i=$((i+1)); done`
		code, out, errOut := runCLI(t, root, []string{"--measure", "--verify-lines", "2", "--try", command, "--each", a}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(out, "BenchShort ns/op  100") || !strings.Contains(out, "filler            24.5") {
			t.Errorf("stdout = %q", out)
		}
		if strings.Contains(errOut, "BenchShort 100") || !strings.Contains(errOut, "filler 48\nfiller 49\n(last 2 of 52 lines)") {
			t.Errorf("stderr shows more than the tail: %q", errOut)
		}
	})

	t.Run("a run the timeout ended is left out", func(t *testing.T) {
		root, a, b := measureTree(t)
		command := measureWords + `; grep -q 3 f.txt && sleep 5; exit 0`
		code, out, errOut := runCLI(t, root, []string{"--measure", "--timeout", "300ms", "--try", command, "--each", a, b}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if out != "" {
			t.Errorf("stdout = %q", out)
		}
		lines := strings.Split(errOut, "\n")
		var shortRow string
		for _, l := range lines {
			if strings.HasPrefix(l, "BenchShort ns/op") {
				shortRow = l
			}
		}
		if f := strings.Fields(shortRow); !slices.Equal(f, []string{"BenchShort", "ns/op", "100", "200", "2.00x", "-", "-"}) {
			t.Errorf("BenchShort's row = %q in %q", shortRow, errOut)
		}
	})

	t.Run("--json carries it", func(t *testing.T) {
		root, a, _ := measureTree(t)
		code, out, errOut := runCLI(t, root, []string{"--measure", "--json", "--try", measureWords, "--each", a}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		var r struct {
			Each struct {
				Table *jsonMeasureTable `json:"table"`
			} `json:"each"`
		}
		must(t, json.Unmarshal([]byte(out), &r))
		tab := r.Each.Table
		if tab == nil || !slices.Equal(tab.Sides, []string{"base", a}) || len(tab.Rows) != 2 ||
			tab.Rows[0].Label != "BenchShort ns/op" || *tab.Rows[0].Medians[1] != 200 || *tab.Rows[0].Ratios[1] != 2 {
			t.Errorf("table = %+v", tab)
		}
	})

	t.Run("none under --quiet, which prints nothing on success", func(t *testing.T) {
		root, a, _ := measureTree(t)
		code, out, errOut := runCLI(t, root, []string{"--measure", "--quiet", "--try", measureWords, "--each", a}, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("none for output with no number", func(t *testing.T) {
		root, a, _ := measureTree(t)
		code, out, errOut := runCLI(t, root, []string{"--measure", "--try", "echo PASS", "--each", a}, "")
		if code != exitOK || !strings.HasPrefix(out, "tried base and 1 patch") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("none for a run that stopped, in the text or in --json", func(t *testing.T) {
		needsPOSIXPerms(t)
		root, a, b := measureTree(t)
		// The command rewrites f.txt under a.hunk, which hunk will not
		// overwrite: the run stops there with a file not put back.
		command := measureWords + `; grep -q 2 f.txt && echo changed > f.txt; exit 0`
		code, out, errOut := runCLI(t, root, []string{"--measure", "--try", command, "--each", a, b}, "")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if strings.Contains(out+errOut, "BenchShort ns/op") {
			t.Errorf("a stopped run printed a table: %q", out+errOut)
		}
		must(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("n = 1\n"), 0o644))
		code, out, _ = runCLI(t, root, []string{"--measure", "--json", "--try", command, "--each", a, b}, "")
		if code != exitRollbackFailed || strings.Contains(out, `"table"`) {
			t.Errorf("exit %d, --json = %s", code, out)
		}
	})
}

// FuzzLineFigures: every line is read without a panic, as a run's output is
// whatever the command printed; its figures share one shape, are numbered
// from 1, each worth what its text says; and whitespace between fields
// changes nothing.
func FuzzLineFigures(f *testing.F) {
	for _, s := range []string{
		"BenchShort 100 ns/op", "BenchmarkParse-8   \t 1000000\t  1053 ns/op\t 48 B/op\t 2 allocs/op",
		"time:   [1.2345 µs 1.2400 µs 1.2456 µs]", "n=5 ops=1000", "ok  \texample.com/x\t3.456s",
		"goarch: arm64", "(0.00s)", "1e999 1e-999 -0 +.5 5.", "\xff\xfe 3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		got := lineFigures(line)
		for i, fg := range got {
			if fg.shape != got[0].shape || fg.place != i+1 {
				t.Fatalf("figure %d = %+v beside %+v", i, fg, got[0])
			}
			v, err := strconv.ParseFloat(fg.text, 64)
			if err != nil || v != fg.value || math.IsInf(v, 0) || math.IsNaN(v) {
				t.Fatalf("figure %d's text %q is worth %v (%v), not %v", i, fg.text, v, err, fg.value)
			}
			if strings.Contains(fg.shape, "  ") || strings.ContainsAny(fg.shape, "\t\n") {
				t.Fatalf("shape %q has whitespace beyond single spaces", fg.shape)
			}
		}
		again := lineFigures(strings.Join(strings.Fields(line), "\t  "))
		if len(again) != len(got) {
			t.Fatalf("respaced, %d figures, not %d", len(again), len(got))
		}
		for i := range got {
			if again[i] != got[i] {
				t.Fatalf("respaced, figure %d is %+v, not %+v", i, again[i], got[i])
			}
		}
	})
}

// FuzzMeasureTable: a table of any two runs' output renders, and --json can
// carry it, whatever the numbers.
func FuzzMeasureTable(f *testing.F) {
	f.Add("a 1\nb 2", "a 3\nb 0")
	f.Add("z 0\nhuge 1e-300", "z 5\nhuge 1e300")
	f.Add("time: [1 µs 2 µs]", "6\n6")
	f.Fuzz(func(t *testing.T, base, patch string) {
		runs := measuredRuns([]string{"base", "p"}, [][]string{strings.Split(base, "\n"), strings.Split(patch, "\n")})
		tab := measureTableOf(runs, 2)
		var b bytes.Buffer
		tab.write(&b)
		if _, err := json.Marshal(tab.json()); err != nil {
			t.Fatalf("--json cannot carry the table: %v", err)
		}
		if got := strings.Count(b.String(), "\n"); got != len(tab.rows)+1 {
			t.Fatalf("%d lines for %d rows:\n%s", got, len(tab.rows), b.String())
		}
	})
}
