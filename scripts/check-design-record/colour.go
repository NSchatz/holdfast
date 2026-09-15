package main

import (
	"math"
	"strconv"
	"strings"
)

// Just enough colour to answer ONE question: is this gradient purple-to-blue. Nothing
// here is a colour library and nothing else in this check reads a colour.

// hueClass is what a colour is for the purposes of the C2 entry.
type hueClass int

const (
	hueOther hueClass = iota
	hueBlue
	huePurple
)

func (h hueClass) String() string {
	switch h {
	case hueBlue:
		return "blue"
	case huePurple:
		return "purple"
	}
	return "other"
}

// classify puts a hue in the blue or the purple band, and refuses to classify a colour
// with no chroma: a white-to-black gradient has a hue of zero and is not blue.
func classify(h, s, l float64) hueClass {
	if s < 0.15 || l < 0.05 || l > 0.95 {
		return hueOther
	}
	switch {
	case h >= 200 && h < 265:
		return hueBlue
	case h >= 265 && h < 320:
		return huePurple
	}
	return hueOther
}

// colourOf reads one colour token and returns its hue, saturation and lightness.
func colourOf(tok string) (h, s, l float64, ok bool) {
	tok = strings.TrimSpace(strings.Trim(strings.TrimSpace(tok), ","))
	low := strings.ToLower(tok)
	if hex, named := namedColours[low]; named {
		low = hex
	}
	switch {
	case strings.HasPrefix(low, "#"):
		r, g, b, ok := parseHex(low)
		if !ok {
			return 0, 0, 0, false
		}
		h, s, l := toHSL(r, g, b)
		return h, s, l, true
	case strings.HasPrefix(low, "rgb("), strings.HasPrefix(low, "rgba("):
		args := numbersIn(low)
		if len(args) < 3 {
			return 0, 0, 0, false
		}
		h, s, l := toHSL(args[0]/255, args[1]/255, args[2]/255)
		return h, s, l, true
	case strings.HasPrefix(low, "hsl("), strings.HasPrefix(low, "hsla("):
		args := numbersIn(low)
		if len(args) < 3 {
			return 0, 0, 0, false
		}
		return math.Mod(args[0], 360), args[1] / 100, args[2] / 100, true
	}
	return 0, 0, 0, false
}

func parseHex(s string) (r, g, b float64, ok bool) {
	s = strings.TrimPrefix(s, "#")
	switch len(s) {
	case 3, 4:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 6, 8:
		s = s[:6]
	default:
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64((v>>16)&0xff) / 255, float64((v>>8)&0xff) / 255, float64(v&0xff) / 255, true
}

func toHSL(r, g, b float64) (h, s, l float64) {
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	if max == min {
		return 0, 0, l
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = math.Mod((g-b)/d+6, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return math.Mod(h*60, 360), s, l
}

// numbersIn pulls the numeric arguments out of a functional colour notation. Percent
// signs and the slash of the modern syntax are separators like any other.
func numbersIn(s string) []float64 {
	open := strings.Index(s, "(")
	if open < 0 {
		return nil
	}
	body := s[open+1:]
	body = strings.TrimSuffix(strings.TrimSpace(body), ")")
	var out []float64
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		if v, err := strconv.ParseFloat(cur.String(), 64); err == nil {
			out = append(out, v)
		}
		cur.Reset()
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if (c >= '0' && c <= '9') || c == '.' || (c == '-' && cur.Len() == 0) {
			cur.WriteByte(c)
			continue
		}
		flush()
	}
	flush()
	return out
}

// namedColours is the purple and blue half of the CSS named colours, plus the greys a
// gradient is most likely to be built from, so a two-stop neutral gradient resolves to
// something rather than to nothing.
var namedColours = map[string]string{
	"purple":          "#800080",
	"rebeccapurple":   "#663399",
	"blueviolet":      "#8a2be2",
	"darkviolet":      "#9400d3",
	"darkorchid":      "#9932cc",
	"mediumorchid":    "#ba55d3",
	"mediumpurple":    "#9370db",
	"violet":          "#ee82ee",
	"orchid":          "#da70d6",
	"magenta":         "#ff00ff",
	"fuchsia":         "#ff00ff",
	"indigo":          "#4b0082",
	"slateblue":       "#6a5acd",
	"darkslateblue":   "#483d8b",
	"mediumslateblue": "#7b68ee",
	"blue":            "#0000ff",
	"mediumblue":      "#0000cd",
	"darkblue":        "#00008b",
	"navy":            "#000080",
	"royalblue":       "#4169e1",
	"dodgerblue":      "#1e90ff",
	"deepskyblue":     "#00bfff",
	"cornflowerblue":  "#6495ed",
	"steelblue":       "#4682b4",
	"skyblue":         "#87ceeb",
	"lightskyblue":    "#87cefa",
	"cyan":            "#00ffff",
	"aqua":            "#00ffff",
	"white":           "#ffffff",
	"black":           "#000000",
	"transparent":     "#00000000",
}
