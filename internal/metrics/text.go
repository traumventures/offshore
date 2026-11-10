package metrics

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
)

// ContentType is the media type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteText writes families in the Prometheus text exposition format.
func WriteText(w io.Writer, fams []Family) error {
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		if f.Help != "" {
			bw.WriteString("# HELP " + f.Name + " " + escapeHelp(f.Help) + "\n")
		}
		typ := f.Type
		if typ != Counter && typ != Gauge && typ != Histogram {
			typ = "untyped"
		}
		bw.WriteString("# TYPE " + f.Name + " " + typ + "\n")
		for _, s := range f.Samples {
			if f.Type == Histogram {
				for _, b := range s.Buckets {
					line(bw, f.Name+"_bucket", s.Labels, &Label{"le", formatFloat(b.UpperBound)}, float64(b.Count))
				}
				line(bw, f.Name+"_bucket", s.Labels, &Label{"le", "+Inf"}, float64(s.Count))
				line(bw, f.Name+"_sum", s.Labels, nil, s.Sum)
				line(bw, f.Name+"_count", s.Labels, nil, float64(s.Count))
				continue
			}
			line(bw, f.Name, s.Labels, nil, s.Value)
		}
	}
	return bw.Flush()
}

func line(w *bufio.Writer, name string, labels []Label, extra *Label, v float64) {
	w.WriteString(name)
	if len(labels) > 0 || extra != nil {
		w.WriteByte('{')
		first := true
		write := func(l Label) {
			if !first {
				w.WriteByte(',')
			}
			first = false
			w.WriteString(l.Name + `="` + escapeValue(l.Value) + `"`)
		}
		for _, l := range labels {
			write(l)
		}
		if extra != nil {
			write(*extra)
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(formatFloat(v))
	w.WriteByte('\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	valueEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeValue(s string) string { return valueEscaper.Replace(s) }
