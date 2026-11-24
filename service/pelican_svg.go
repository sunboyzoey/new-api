package service

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const MaxPelicanSVGBytes = 60 * 1024

// SVG accessible names and descriptions reference space-separated local IDs.
var pelicanARIAReferences = regexp.MustCompile(`^[\p{L}_][\p{L}\p{M}\p{N}_.-]*([ \t\r\n]+[\p{L}_][\p{L}\p{M}\p{N}_.-]*)*$`)

// Both forms of SMIL animation target their parent shape implicitly.
const pelicanNumber = `([-+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][-+]?[0-9]+)?)`
const pelicanSeparator = `(?:\s*,\s*|\s+)`

var pelicanRotation = regexp.MustCompile(`^\s*` + pelicanNumber + `(?:` + pelicanSeparator + pelicanNumber + pelicanSeparator + pelicanNumber + `)?\s*$`)
var pelicanDuration = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?|\.[0-9]+)(ms|s)$`)

func parsePelicanDuration(value string) (float64, string) {
	parts := pelicanDuration.FindStringSubmatch(value)
	if parts == nil {
		return 0, ""
	}
	n, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, ""
	}
	return n, parts[2]
}

func validPelicanWheelRotation(attrs []xml.Attr) bool {
	var from, to, values, duration, repeat, property, kind string
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		name, value := attr.Name.Local, strings.TrimSpace(attr.Value)
		if attr.Name.Space != "" || seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "attributeName":
			property = value
		case "type":
			kind = value
		case "from":
			from = value
		case "to":
			to = value
		case "values":
			values = value
		case "dur":
			duration = value
		case "repeatCount":
			repeat = value
		case "attributeType":
			if value != "XML" && value != "auto" {
				return false
			}
		case "begin":
			if value != "0" && value != "0s" && value != "0ms" {
				return false
			}
		case "calcMode":
			if value != "linear" {
				return false
			}
		case "additive":
			if value != "sum" && value != "replace" {
				return false
			}
		default:
			return false
		}
	}
	clock, _ := parsePelicanDuration(duration)
	if property != "transform" || kind != "rotate" || repeat != "indefinite" || clock <= 0 {
		return false
	}
	frames := []string{from, to}
	if seen["values"] {
		if seen["from"] || seen["to"] {
			return false
		}
		frames = strings.Split(values, ";")
	}
	var first [3]float64
	hasMotion := false
	for i, frame := range frames {
		parts := pelicanRotation.FindStringSubmatch(frame)
		if parts == nil {
			return false
		}
		var rotation [3]float64
		for j, part := range parts[1:] {
			if part == "" {
				continue
			}
			n, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return false
			}
			rotation[j] = n
		}
		if i == 0 {
			first = rotation
		} else if rotation[0] != first[0] {
			hasMotion = true
		}
	}
	return hasMotion
}

// Morph frames share a single subpath of explicit absolute commands. Match
// each command's arity before checking coordinate bounds and frame agreement.
const pelicanPoint = pelicanNumber + pelicanSeparator + pelicanNumber
const pelicanLine = `L\s*` + pelicanPoint
const pelicanQuadratic = `Q\s*` + pelicanPoint + pelicanSeparator + pelicanPoint
const pelicanCubic = `C\s*` + pelicanPoint + pelicanSeparator + pelicanPoint + pelicanSeparator + pelicanPoint

var pelicanPathFrame = regexp.MustCompile(`^\s*M\s*` + pelicanPoint + `(?:\s*(?:` + pelicanLine + `|` + pelicanQuadratic + `|` + pelicanCubic + `))+\s*Z?\s*$`)
var pelicanPathToken = regexp.MustCompile(`[MLQCZ]|` + pelicanNumber)

func validPelicanPathFrames(values string) bool {
	if count := strings.Count(values, ";") + 1; count < 2 || count > 32 {
		return false
	}
	var signature string
	for frame := range strings.SplitSeq(values, ";") {
		if len(frame) > 1024 || !pelicanPathFrame.MatchString(frame) {
			return false
		}
		var commands strings.Builder
		for _, token := range pelicanPathToken.FindAllString(frame, -1) {
			if token[0] >= 'A' && token[0] <= 'Z' {
				commands.WriteByte(token[0])
				continue
			}
			n, err := strconv.ParseFloat(token, 64)
			if err != nil || math.Abs(n) > 1e6 {
				return false
			}
		}
		if commands.Len() > 32 || signature != "" && commands.String() != signature {
			return false
		}
		signature = commands.String()
	}
	return true
}

func validPelicanPathAnimation(attrs []xml.Attr) bool {
	var values string
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		name, value := attr.Name.Local, strings.TrimSpace(attr.Value)
		if attr.Name.Space != "" || seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "attributeName":
			if value != "d" {
				return false
			}
		case "values":
			values = value
		case "dur":
			n, unit := parsePelicanDuration(value)
			if unit == "ms" {
				n /= 1000
			}
			if n < 0.1 || n > 60 {
				return false
			}
		case "repeatCount":
			if value != "indefinite" {
				return false
			}
		case "calcMode":
			if value != "linear" {
				return false
			}
		default:
			return false
		}
	}
	return seen["attributeName"] && seen["dur"] && seen["repeatCount"] && validPelicanPathFrames(values)
}

// Generated documents use a passive HTML shell around one animated drawing.
// Never copy model-provided HTML into the public preview.
var pelicanHTMLStart = regexp.MustCompile(`(?i)^<!doctype\s+html\s*>\s*<html(?:\s+lang=(?:"[a-z0-9-]+"|'[a-z0-9-]+'))?\s*>\s*(?:<head>\s*(?:<meta\s+charset=(?:"utf-8"|'utf-8'|utf-8)\s*/?>\s*)?(?:<title>[^<>]*</title>\s*)?</head>\s*)?<body>\s*`)
var pelicanHTMLEnd = regexp.MustCompile(`(?i)\s*</body>\s*</html>$`)

func PelicanDrawingFromHTML(output string) (string, error) {
	if len(output) > MaxPelicanSVGBytes {
		return "", errors.New("output_too_large")
	}
	document := strings.TrimSpace(output)
	if fenced, ok := strings.CutPrefix(document, "```html"); ok {
		unfenced, ok := strings.CutSuffix(fenced, "```")
		if !ok {
			return "", errors.New("invalid_html")
		}
		document = strings.TrimSpace(unfenced)
	}
	start, end := pelicanHTMLStart.FindStringIndex(document), pelicanHTMLEnd.FindStringIndex(document)
	if start == nil || end == nil || start[1] >= end[0] {
		return "", errors.New("invalid_html")
	}
	body := document[start[1]:end[0]]
	if !strings.HasPrefix(body, "<svg") || !strings.HasSuffix(body, "</svg>") {
		return "", errors.New("invalid_html")
	}
	svg, err := ValidateAnimatedPelicanSVG(body)
	if err != nil {
		switch err.Error() {
		case "unsafe_svg":
			return "", errors.New("unsafe_html")
		case "invalid_svg":
			return "", errors.New("invalid_html")
		}
	}
	return svg, err
}

// PelicanHTMLPreview rechecks persisted drawings and isolates SVG's XML parser
// from the HTML parser. Declarative animation works in an image with no scripts.
func PelicanHTMLPreview(drawing string) (string, error) {
	svg, err := ValidateAnimatedPelicanSVG(drawing)
	if err != nil {
		return "", err
	}
	const head = `<!DOCTYPE html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"><style>html,body,img{width:100%;height:100%;margin:0}img{display:block;object-fit:contain}</style></head><body><img alt="" src="data:image/svg+xml;base64,`
	return head + base64.StdEncoding.EncodeToString([]byte(svg)) + `"></body></html>`, nil
}

// ValidatePelicanSVG accepts historical still drawings and declarative wheel animations.
func ValidatePelicanSVG(output string) (string, error) {
	return validatePelicanSVG(output, false)
}

func ValidateAnimatedPelicanSVG(output string) (string, error) {
	return validatePelicanSVG(output, true)
}

func validatePelicanSVG(output string, requireMotion bool) (string, error) {
	if len(output) > MaxPelicanSVGBytes {
		return "", errors.New("output_too_large")
	}
	start := strings.Index(output, "<svg")
	end := strings.LastIndex(output, "</svg>")
	if start < 0 || end < start {
		return "", errors.New("invalid_svg")
	}
	svg := output[start : end+6]
	elementsAllowed := " svg g defs path rect circle ellipse line polyline polygon text tspan title desc linearGradient radialGradient stop clipPath mask use symbol "
	attributesAllowed := " xmlns id viewBox width height x y x1 y1 x2 y2 cx cy r rx ry d points fill fill-rule fill-opacity stroke stroke-width stroke-linecap stroke-linejoin stroke-dasharray stroke-dashoffset stroke-opacity opacity transform font-family font-size font-weight text-anchor dominant-baseline dx dy offset stop-color stop-opacity gradientUnits gradientTransform spreadMethod clip-path clip-rule mask href preserveAspectRatio role aria-label aria-labelledby aria-describedby version "
	decoder := xml.NewDecoder(strings.NewReader(svg))
	elements := 0
	hasNamespace := false
	hasMotion := false
	pathAnimations := 0
	var parents []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("invalid_svg")
		}
		switch node := token.(type) {
		case xml.StartElement:
			depth := len(parents)
			elements++
			if elements > 20000 || depth > 64 {
				return "", errors.New("output_too_large")
			}
			if depth == 0 && (elements != 1 || node.Name.Local != "svg") {
				return "", errors.New("invalid_svg")
			}
			if node.Name.Space != "" && node.Name.Space != "http://www.w3.org/2000/svg" {
				return "", errors.New("unsafe_svg")
			}
			if depth > 0 && (parents[depth-1] == "animateTransform" || parents[depth-1] == "animate") {
				return "", errors.New("unsafe_svg")
			}
			if node.Name.Local == "animate" {
				pathAnimations++
				if pathAnimations > 16 || depth == 0 || parents[depth-1] != "path" || !validPelicanPathAnimation(node.Attr) {
					return "", errors.New("unsafe_svg")
				}
				parents = append(parents, node.Name.Local)
				continue
			}
			if node.Name.Local == "animateTransform" {
				if depth == 0 || !strings.Contains(" g path rect circle ellipse line polyline polygon ", " "+parents[depth-1]+" ") || !validPelicanWheelRotation(node.Attr) {
					return "", errors.New("unsafe_svg")
				}
				hasMotion = true
				parents = append(parents, node.Name.Local)
				continue
			}
			if !strings.Contains(elementsAllowed, " "+node.Name.Local+" ") {
				return "", errors.New("unsafe_svg")
			}
			for _, attr := range node.Attr {
				if attr.Name.Space == "xmlns" && attr.Name.Local == "xlink" && attr.Value == "http://www.w3.org/1999/xlink" {
					continue
				}
				if attr.Name.Space != "" && attr.Name.Space != "http://www.w3.org/1999/xlink" {
					return "", errors.New("unsafe_svg")
				}
				if !strings.Contains(attributesAllowed, " "+attr.Name.Local+" ") {
					return "", errors.New("unsafe_svg")
				}
				value := strings.TrimSpace(attr.Value)
				if attr.Name.Local == "aria-labelledby" || attr.Name.Local == "aria-describedby" {
					if attr.Name.Space != "" || !pelicanARIAReferences.MatchString(value) {
						return "", errors.New("unsafe_svg")
					}
				}
				if depth == 0 && attr.Name.Local == "xmlns" {
					hasNamespace = true
				}
				if attr.Name.Local == "xmlns" && value != "http://www.w3.org/2000/svg" {
					return "", errors.New("unsafe_svg")
				}
				if attr.Name.Local == "href" && !strings.HasPrefix(value, "#") {
					return "", errors.New("unsafe_svg")
				}
				lower := strings.ToLower(value)
				if strings.ContainsRune(value, 92) || strings.Contains(lower, "/*") || strings.Contains(lower, ":") && attr.Name.Local != "xmlns" {
					return "", errors.New("unsafe_svg")
				}
				if strings.Contains(lower, "url") && !(strings.HasPrefix(value, "url(#") && strings.HasSuffix(value, ")") && strings.Count(value, "(") == 1) {
					return "", errors.New("unsafe_svg")
				}
			}
			parents = append(parents, node.Name.Local)
		case xml.EndElement:
			parents = parents[:len(parents)-1]
		case xml.Directive, xml.ProcInst:
			return "", errors.New("unsafe_svg")
		}
	}
	if len(parents) != 0 || elements < 2 {
		return "", errors.New("invalid_svg")
	}
	if requireMotion && !hasMotion {
		return "", errors.New("missing_animation")
	}
	if !hasNamespace {
		svg = strings.Replace(svg, "<svg", "<svg xmlns=\"http://www.w3.org/2000/svg\"", 1)
	}

	return svg, nil
}
