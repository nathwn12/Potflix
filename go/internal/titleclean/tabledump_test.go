package titleclean

import (
	"fmt"
	"os"
	"testing"
)

// Temporary verification helper: emits my JDK-11 tables in the reviewer's dump
// format so they can be diffed against the Java ground-truth dumps.
func TestTableDump(t *testing.T) {
	if os.Getenv("TABLEDUMP") == "" {
		t.Skip("set TABLEDUMP=1")
	}
	fw, _ := os.Create("td.isword.tsv")
	fm, _ := os.Create("td.mn.tsv")
	fu, _ := os.Create("td.upper.tsv")
	fe, _ := os.Create("td.exp.tsv")
	defer fw.Close()
	defer fm.Close()
	defer fu.Close()
	defer fe.Close()
	for cp := 0; cp <= 0xFFFF; cp++ {
		b := 0
		if jdkIsLetterOrDigit(rune(cp)) {
			b = 1
		}
		fmt.Fprintf(fw, "%04X\t%d\n", cp, b)
		m := 0
		if jdkIsMn(rune(cp)) {
			m = 1
		}
		fmt.Fprintf(fm, "%04X\t%d\n", cp, m)
		if cp >= 0xD800 && cp <= 0xDFFF {
			continue
		}
		var s string
		if exp, ok := jdkFullUpper[rune(cp)]; ok {
			s = exp
		} else {
			s = string(jdkUpperSimple(rune(cp)))
		}
		fmt.Fprintf(fu, "%04X\t%X\n", cp, []byte(s))
	}
	for cp := 0; cp <= 0xFFFF; cp++ {
		if s, ok := jdkFullUpper[rune(cp)]; ok {
			fmt.Fprintf(fe, "%04X\t%X\n", cp, []byte(s))
		}
	}
}
