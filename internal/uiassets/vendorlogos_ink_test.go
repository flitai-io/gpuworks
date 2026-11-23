package uiassets

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every vendor mark declares its ink tone in VENDOR_LOGO_INK, and the console
// draws marks only through VendorMarkImage, which styles light artwork for
// the current surface and gives dark ink a light plate on dark surfaces.
// These tests check the source artwork and shared renderer. The template
// catalog's browser regression checks the renderer's computed theme styles.

var (
	stylesCSS = filepath.Join(uiRoot, "src", "styles.css")
	uiSrcDir  = filepath.Join(uiRoot, "src")
)

// The files that may name VENDOR_ASSETS or a /vendor-logos/ path.
var vendorMarkDrawers = map[string]bool{
	"src/components/vendorAssets.ts": true,
	"src/components/VendorLogo.tsx":  true,
}

const (
	// WCAG 2 asks 3:1 for graphical objects and large text. A wordmark is
	// legible when most of its ink clears that. The shipped marks measure
	// 58-100% of ink weight at 3:1 or more on their surface: the colour
	// accents keep the lowest two there (Kunlunxin's blue symbol, about
	// roughly 3:1 on a dark surface; NVIDIA's green eye, 2.4:1 on white, weighted
	// by its large bounding box), while their wordmarks read at 13.8:1 and
	// 21:1. Half is the line where the wordmark, not the accent, carries it.
	inkMinShareAt3 = 0.50
	// Below 1.5:1 a colour all but disappears into its surface. No painted
	// SVG colour may fall below it; a PNG may have this share of its ink
	// there (shipped PNGs measure 0%), which leaves room for antialiasing.
	inkFaintContrast    = 1.5
	inkMaxFaintPNGShare = 0.02
	// A PNG pixel counts as ink from this alpha (out of 255): fainter edge
	// pixels are antialiasing, mostly the surface showing through.
	inkMinAlpha = 128
)

const fixDrawer = "fix: draw the mark with VendorMarkImage from ui/src/components/VendorLogo.tsx, which styles the declared ink for the active theme"

type rgb [3]float64 // sRGB channels, 0-1

// vendorPlates are the light and dark reference colours from ui/src/styles.css.
type vendorPlates struct{ light, dark rgb }

// TestVendorLogoInkIsDeclared: every mapped mark declares its ink tone, and
// every declaration names a mapped mark.
func TestVendorLogoInkIsDeclared(t *testing.T) {
	src, err := os.ReadFile(vendorAssetsTS)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := vendorAssetRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	ink, err := tsStringRecord(src, "VENDOR_LOGO_INK")
	if err != nil {
		t.Fatalf("%v; fix: add `export const VENDOR_LOGO_INK: Record<string, \"dark\" | \"light\">` with one entry per VENDOR_ASSETS key", err)
	}
	for _, p := range vendorInkDeclarationProblems(refs, ink) {
		t.Error(p)
	}
}

func vendorInkDeclarationProblems(refs, ink map[string]string) []string {
	var problems []string
	for _, k := range sortedKeys(refs) {
		if _, ok := ink[k]; !ok {
			problems = append(problems, fmt.Sprintf("ui/src/components/vendorAssets.ts: VENDOR_ASSETS.%s has no VENDOR_LOGO_INK entry, so the console cannot select a theme treatment; fix: add %s: \"dark\" (dark artwork) or \"light\" (white or pale artwork) to VENDOR_LOGO_INK", k, k))
		}
	}
	for _, k := range sortedKeys(ink) {
		if _, ok := refs[k]; !ok {
			problems = append(problems, fmt.Sprintf("ui/src/components/vendorAssets.ts: VENDOR_LOGO_INK.%s names no mark in VENDOR_ASSETS; fix: delete the entry or map the mark", k))
		} else if v := ink[k]; v != "dark" && v != "light" {
			problems = append(problems, fmt.Sprintf("ui/src/components/vendorAssets.ts: VENDOR_LOGO_INK.%s = %q; fix: use \"dark\" or \"light\"", k, v))
		}
	}
	return problems
}

// TestVendorMarksAreDrawnThroughTheComponent: a mark drawn as a bare <img>
// bypasses theme styling. Only the component files may name the map
// or the directory.
func TestVendorMarksAreDrawnThroughTheComponent(t *testing.T) {
	files := map[string]string{}
	err := filepath.WalkDir(uiSrcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := path.Ext(p)
		if ext != ".ts" && ext != ".tsx" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(uiRoot, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no .ts/.tsx files under %s", uiSrcDir)
	}
	for _, p := range vendorMarkDrawerProblems(files) {
		t.Error(p)
	}
}

var vendorAssetsRE = regexp.MustCompile(`\bVENDOR_ASSETS\b`)

func vendorMarkDrawerProblems(files map[string]string) []string {
	var problems []string
	for _, name := range sortedKeys(files) {
		if vendorMarkDrawers[name] || strings.HasSuffix(name, ".test.ts") || strings.HasSuffix(name, ".test.tsx") || strings.Contains(name, "/__tests__/") {
			continue
		}
		src := files[name]
		if vendorAssetsRE.MatchString(src) {
			problems = append(problems, fmt.Sprintf("ui/%s: uses VENDOR_ASSETS directly, so the mark is drawn without theme styling; %s", name, fixDrawer))
		}
		if strings.Contains(src, "/vendor-logos/") {
			problems = append(problems, fmt.Sprintf("ui/%s: names a /vendor-logos/ file directly, so the mark is drawn without theme styling; %s", name, fixDrawer))
		}
	}
	return problems
}

// TestVendorLogoInkReadsOnItsSurfaces measures unfiltered ink where it is
// displayed and repeats the check with the light-theme filter for pale marks.
func TestVendorLogoInkReadsOnItsSurfaces(t *testing.T) {
	css, err := os.ReadFile(stylesCSS)
	if err != nil {
		t.Fatal(err)
	}
	theme, err := os.ReadFile(filepath.Join(uiSrcDir, "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	plates, err := parseVendorPlates(css, theme)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(vendorAssetsTS)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := vendorAssetRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	ink, err := tsStringRecord(src, "VENDOR_LOGO_INK")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range sortedKeys(refs) {
		tone, ok := ink[k]
		if !ok {
			continue // TestVendorLogoInkIsDeclared reports it
		}
		name := strings.TrimPrefix(refs[k], "/")
		data, err := os.ReadFile(filepath.Join(uiRoot, "public", filepath.FromSlash(name)))
		if err != nil {
			continue // TestVendorAssetsMatchLogoFiles reports it
		}
		m, problems := checkVendorInk("ui/public/"+name, data, tone, plates)
		t.Logf("%s: declared %s ink, measured %s (%.0f%% of ink weight is dark); on %s: %.0f%% of ink at >= 3:1, weighted median %.2f:1, lowest %.2f:1, %.1f%% below %.1f:1",
			k, tone, m.tone(), 100*m.darkShare, m.surface, 100*m.shareAt3, m.median, m.lowest, 100*m.faintShare, inkFaintContrast)
		for _, p := range problems {
			t.Error(p)
		}
	}
}

var (
	lightPlateRE  = regexp.MustCompile(`--vendor-logo-light\s*:\s*(#[0-9a-fA-F]{3,6})\s*;`)
	darkSurfaceRE = regexp.MustCompile(`(?s)\.dark\s*\{[^}]*?--surface-raised\s*:\s*(#[0-9a-fA-F]{3,6})\s*;`)
)

func parseVendorPlates(css, theme []byte) (vendorPlates, error) {
	var p vendorPlates
	for _, ref := range []struct {
		match  []string
		name   string
		colour *rgb
	}{
		{lightPlateRE.FindStringSubmatch(string(css)), "--vendor-logo-light", &p.light},
		{darkSurfaceRE.FindStringSubmatch(string(theme)), ".dark --surface-raised", &p.dark},
	} {
		if len(ref.match) == 2 {
			if c, alpha, ok := parseColour(ref.match[1]); ok && alpha == 1 {
				*ref.colour = c
				continue
			}
		}
		return p, fmt.Errorf("no opaque hex colour for %s; fix: restore the token or update the vendor ink reference surface", ref.name)
	}
	return p, nil
}

// inkSample is one ink colour and how much of the mark it paints.
type inkSample struct {
	colour rgb
	alpha  float64 // coverage over the surface, 0-1
	weight float64
	label  string // the SVG fill as written; empty for PNG pixels
}

// inkMeasure is a mark's ink measured against the surface its tone needs.
type inkMeasure struct {
	darkShare  float64 // share of ink weight that reads better on the light plate
	surface    string  // the plate token measured against
	shareAt3   float64 // share of ink weight at 3:1 or more
	faintShare float64 // share of ink weight below inkFaintContrast
	median     float64 // weighted median contrast
	lowest     float64 // lowest contrast of any ink colour
	lowestAt   string  // that colour
}

func (m inkMeasure) tone() string {
	if m.darkShare >= 0.5 {
		return "dark"
	}
	return "light"
}

// checkVendorInk measures a mark's ink and returns one self-contained
// message per broken rule.
func checkVendorInk(name string, data []byte, declared string, plates vendorPlates) (inkMeasure, []string) {
	var (
		samples  []inkSample
		problems []string
	)
	isPNG := strings.EqualFold(path.Ext(name), ".png")
	if isPNG {
		var err error
		if samples, err = pngInk(data); err != nil {
			return inkMeasure{}, []string{fmt.Sprintf("%s: %v; fix: re-export it as a real PNG with an alpha channel", name, err)}
		}
	} else {
		samples, problems = svgInk(name, data)
	}
	var total float64
	for _, s := range samples {
		total += s.weight
	}
	if total == 0 {
		return inkMeasure{}, append(problems, fmt.Sprintf("%s: no ink the check can measure (no opaque pixels, or no filled shapes outside <defs>); fix: ship the mark as filled paths or a PNG with opaque artwork", name))
	}
	var m inkMeasure
	for _, s := range samples {
		if contrast(over(s, plates.light), plates.light) > contrast(over(s, plates.dark), plates.dark) {
			m.darkShare += s.weight / total
		}
	}
	surface, label := plates.light, "--vendor-logo-light"
	if declared == "light" {
		surface, label = plates.dark, ".dark --surface-raised"
	}
	darkShare := m.darkShare
	m = scoreVendorInk(samples, total, surface, label, false)
	m.darkShare = darkShare
	if got := m.tone(); got != declared {
		problems = append(problems, fmt.Sprintf("%s: declared %q ink in VENDOR_LOGO_INK, but %.0f%% of its ink weight is %s, so it gets the wrong theme treatment; fix: declare it %q in VENDOR_LOGO_INK in ui/src/components/vendorAssets.ts, or re-export the mark in %s ink", name, declared, 100*max(m.darkShare, 1-m.darkShare), got, got, declared))
	}
	problems = append(problems, vendorContrastProblems(name, m, isPNG)...)
	if declared == "light" {
		filtered := scoreVendorInk(samples, total, plates.light, "--vendor-logo-light after the light-theme filter", true)
		problems = append(problems, vendorContrastProblems(name, filtered, isPNG)...)
	}
	return m, problems
}

// vendorLightThemeInk mirrors invert(1) hue-rotate(180deg) brightness(0.8).
// The browser regression asserts this filter on the real component.
// Filter Effects 1 defines the 180-degree hue matrix as twice the luma
// projection minus the identity. CSS filter functions operate in sRGB.
func vendorLightThemeInk(c rgb) rgb {
	inverted := rgb{1 - c[0], 1 - c[1], 1 - c[2]}
	luma := 0.213*inverted[0] + 0.715*inverted[1] + 0.072*inverted[2]
	for i, channel := range inverted {
		c[i] = 0.8 * max(0, min(1, 2*luma-channel))
	}
	return c
}

func scoreVendorInk(samples []inkSample, total float64, surface rgb, label string, filtered bool) inkMeasure {
	m := inkMeasure{surface: label}
	type scored struct{ c, w float64 }
	var all []scored
	m.lowest = math.Inf(1)
	for _, s := range samples {
		if filtered {
			s.colour = vendorLightThemeInk(s.colour)
		}
		c := contrast(over(s, surface), surface)
		all = append(all, scored{c, s.weight})
		if c >= 3 {
			m.shareAt3 += s.weight / total
		}
		if c < inkFaintContrast {
			m.faintShare += s.weight / total
		}
		if c < m.lowest {
			m.lowest, m.lowestAt = c, s.label
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].c < all[j].c })
	for acc, i := 0.0, 0; i < len(all); i++ {
		if acc += all[i].w; acc >= total/2 {
			m.median = all[i].c
			break
		}
	}
	return m
}

func vendorContrastProblems(name string, m inkMeasure, isPNG bool) []string {
	var problems []string
	if m.shareAt3 < inkMinShareAt3 {
		problems = append(problems, fmt.Sprintf("%s: only %.0f%% of its ink weight reaches 3:1 contrast on %s (weighted median %.2f:1), at least %.0f%% must; fix: re-export the mark in stronger ink", name, 100*m.shareAt3, m.surface, m.median, 100*inkMinShareAt3))
	}
	if isPNG && m.faintShare > inkMaxFaintPNGShare {
		problems = append(problems, fmt.Sprintf("%s: %.1f%% of its ink weight is below %.1f:1 contrast on %s and all but disappears, at most %.0f%% may be; fix: re-export the faint parts of the mark in a stronger colour", name, 100*m.faintShare, inkFaintContrast, m.surface, 100*inkMaxFaintPNGShare))
	}
	if !isPNG && m.lowest < inkFaintContrast {
		problems = append(problems, fmt.Sprintf("%s: fill %q is %.2f:1 on %s and all but disappears, every painted colour must reach %.1f:1; fix: repaint that part of the mark in a stronger colour", name, m.lowestAt, m.lowest, m.surface, inkFaintContrast))
	}
	return problems
}

// pngInk is every pixel at inkMinAlpha or more, weighted by its alpha.
func pngInk(data []byte) ([]inkSample, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not a decodable PNG (%v)", err)
	}
	counts := map[[4]uint8]float64{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA() // premultiplied, 16-bit
			if a>>8 < inkMinAlpha {
				continue
			}
			un := func(c uint32) uint8 { return uint8(c * 0xff / a) }
			counts[[4]uint8{un(r), un(g), un(bl), uint8(a >> 8)}]++
		}
	}
	samples := make([]inkSample, 0, len(counts))
	for k, n := range counts {
		a := float64(k[3]) / 255
		samples = append(samples, inkSample{colour: rgb{float64(k[0]) / 255, float64(k[1]) / 255, float64(k[2]) / 255}, alpha: a, weight: n * a})
	}
	return samples, nil
}

// svgInk is every drawn, filled shape's colour, weighted by the area of its
// bounding box (pathBounds for paths). Fills set by <style> classes,
// transforms, strokes and <use> references are not evaluated.
func svgInk(name string, data []byte) ([]inkSample, []string) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		tree     svgPaintTree
		viewBox  *svgBox
		samples  []inkSample
		problems []string
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, []string{fmt.Sprintf("%s: not well-formed XML (%v); fix: re-export the SVG", name, err)}
		}
		switch el := tok.(type) {
		case xml.StartElement:
			attrs := attrMap(el)
			tag := el.Name.Local
			paint, drawn := tree.enter(tag, attrs, parseStyle(attrs["style"]))
			if tree.depth == 1 {
				if vb, ok := parseViewBox(attrs["viewBox"]); ok {
					viewBox = &vb
				}
			}
			if viewBox == nil || !drawn || !painted(paint) {
				continue
			}
			var (
				box svgBox
				ok  bool
			)
			switch tag {
			case "path":
				box, ok = pathBounds(attrs["d"])
			case "rect", "circle", "ellipse", "polygon", "polyline":
				box, ok = shapeBox(tag, attrs, *viewBox)
			case "text", "use":
				problems = append(problems, fmt.Sprintf("%s: a filled <%s> whose painted area the check cannot measure; fix: convert it to outlined paths", name, tag))
				continue
			default:
				continue
			}
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: a filled <%s> whose bounds the check cannot read; fix: re-export the shape as plain path data", name, tag))
				continue
			}
			c, alpha, ok := parseColour(paint.fill)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: fill %q is not a colour the check can read (hex, rgb() or a basic name; gradients and currentColor depend on context); fix: paint the mark in solid hex colours", name, paint.fill))
				continue
			}
			samples = append(samples, inkSample{colour: c, alpha: min(1, alpha*paint.opacity), weight: (box.x1 - box.x0) * (box.y1 - box.y0), label: paint.fill})
		case xml.EndElement:
			tree.leave()
		}
	}
	if viewBox == nil {
		problems = append(problems, fmt.Sprintf("%s: root <svg> has no valid viewBox, so its ink cannot be measured; fix: add viewBox=\"0 0 <width> <height>\"", name))
	}
	return samples, problems
}

var namedColours = map[string]string{
	"black": "#000", "white": "#fff", "red": "#f00", "green": "#008000", "blue": "#00f",
	"gray": "#808080", "grey": "#808080", "silver": "#c0c0c0", "yellow": "#ff0", "orange": "#ffa500",
}

var rgbFuncRE = regexp.MustCompile(`^rgba?\(\s*([\d.]+%?)[\s,]+([\d.]+%?)[\s,]+([\d.]+%?)(?:[\s,/]+([\d.]+%?))?\s*\)$`)

// parseColour reads #rgb, #rgba, #rrggbb, #rrggbbaa, rgb()/rgba() and a few
// colour names into sRGB and alpha.
func parseColour(s string) (rgb, float64, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, ok := namedColours[s]; ok {
		s = n
	}
	if m := rgbFuncRE.FindStringSubmatch(s); m != nil {
		var c rgb
		for i := 0; i < 3; i++ {
			v, ok := channel(m[i+1], 255)
			if !ok {
				return rgb{}, 0, false
			}
			c[i] = v
		}
		alpha := 1.0
		if m[4] != "" {
			v, ok := channel(m[4], 1)
			if !ok {
				return rgb{}, 0, false
			}
			alpha = v
		}
		return c, alpha, true
	}
	if !strings.HasPrefix(s, "#") {
		return rgb{}, 0, false
	}
	h := s[1:]
	if len(h) == 3 || len(h) == 4 {
		var long strings.Builder
		for _, r := range h {
			long.WriteRune(r)
			long.WriteRune(r)
		}
		h = long.String()
	}
	if len(h) != 6 && len(h) != 8 {
		return rgb{}, 0, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return rgb{}, 0, false
	}
	alpha := 1.0
	if len(h) == 8 {
		alpha = float64(v&0xff) / 255
		v >>= 8
	}
	return rgb{float64(v>>16) / 255, float64(v>>8&0xff) / 255, float64(v&0xff) / 255}, alpha, true
}

// channel reads a number or percentage as a share of full.
func channel(s string, full float64) (float64, bool) {
	pct := strings.HasSuffix(s, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return 0, false
	}
	if pct {
		return min(1, f/100), true
	}
	return min(1, f/full), true
}

// over composites a sample on a surface the way a browser does, in sRGB.
func over(s inkSample, surface rgb) rgb {
	var out rgb
	for i := range out {
		out[i] = s.alpha*s.colour[i] + (1-s.alpha)*surface[i]
	}
	return out
}

// luminance is WCAG 2 relative luminance.
func luminance(c rgb) float64 {
	lin := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}

// contrast is the WCAG 2 contrast ratio, 1 to 21.
func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
