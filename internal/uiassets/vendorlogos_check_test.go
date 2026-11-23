package uiassets

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// transparentMark is a 40x10 canvas, fully transparent apart from a dark bar.
func transparentMark() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 10))
	for y := 3; y < 7; y++ {
		for x := 4; x < 36; x++ {
			img.SetNRGBA(x, y, color.NRGBA{0x10, 0x10, 0x10, 0xff})
		}
	}
	return img
}

func filled(img *image.NRGBA, c color.NRGBA) *image.NRGBA {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// goodSVG is a wordmark cropped tight to its viewBox: its curved path spans
// the whole canvas, which must not read as a plate.
const goodSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><title>X</title><g fill="#1d1d1f"><path d="M0 0H100V20C50 10 50 10 0 20Z"/><path d="M0 0L100 20H0V0Z"/><rect x="10" y="5" width="20" height="10"/></g></svg>`

// TestVendorLogoCheckRejectsOpaqueArtwork: each fixture breaks exactly the
// rule its message must name, and the message says how to fix it.
func TestVendorLogoCheckRejectsOpaqueArtwork(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, transparentMark(), nil); err != nil {
		t.Fatal(err)
	}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	opaqueRGBA := image.NewRGBA(image.Rect(0, 0, 40, 10))
	for i := range opaqueRGBA.Pix {
		opaqueRGBA.Pix[i] = 0xff
	}
	gray := image.NewGray(image.Rect(0, 0, 40, 10))
	padded := transparentMark()
	for x := 0; x < 40; x++ { // a plate along top and bottom edges; corners stay clear
		for _, y := range []int{0, 9} {
			if x > 0 && x < 39 {
				padded.SetNRGBA(x, y, white)
			}
		}
	}
	for y := 1; y < 9; y++ {
		padded.SetNRGBA(0, y, white)
		padded.SetNRGBA(39, y, white)
	}
	// a white plate inside a 1-px fully transparent rim: corners and border
	// pass, only the opaque share catches it
	boxed := image.NewNRGBA(image.Rect(0, 0, 40, 10))
	for y := 1; y < 9; y++ {
		for x := 1; x < 39; x++ {
			boxed.SetNRGBA(x, y, white)
		}
	}
	// the same box exported at alpha 254/255 instead of 255
	nearlyOpaque := image.NewNRGBA(boxed.Bounds())
	copy(nearlyOpaque.Pix, boxed.Pix)
	for i := 3; i < len(nearlyOpaque.Pix); i += 4 {
		if nearlyOpaque.Pix[i] == 0xff {
			nearlyOpaque.Pix[i] = 0xfe
		}
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 40, 10))

	cases := []struct {
		name, file string
		data       []byte
		want       []string
	}{
		{"jpeg", "x.jpg", jpg.Bytes(), []string{"x.jpg", `".jpg" is not allowed`, "fix:"}},
		{"gif", "x.gif", []byte("GIF89a"), []string{`".gif" is not allowed`}},
		{"webp", "x.webp", []byte("RIFF....WEBP"), []string{`".webp" is not allowed`}},
		{"png named as png but not png", "x.png", jpg.Bytes(), []string{"not a decodable PNG"}},
		{"opaque white png", "white.png", encodePNG(t, filled(image.NewNRGBA(image.Rect(0, 0, 40, 10)), white)), []string{"white.png", "corner pixel (0,0)", "1-px border", "100% of pixels are opaque", "transparent background"}},
		{"png without alpha", "gray.png", encodePNG(t, gray), []string{"no alpha channel"}},
		{"rgba png fully opaque", "rgba.png", encodePNG(t, opaqueRGBA), []string{"corner pixel"}},
		{"padding plate", "plate.png", encodePNG(t, padded), []string{"plate.png", "of the 1-px border is fully transparent"}},
		{"filled box with clear rim", "box.png", encodePNG(t, boxed), []string{"box.png", "are opaque (alpha 240/255 or more), it must be under 60%"}},
		{"nearly opaque box with clear rim", "box254.png", encodePNG(t, nearlyOpaque), []string{"box254.png", "are opaque"}},
		{"16-bit png without alpha", "gray16.png", encodePNG(t, gray16), []string{"no alpha channel"}},
		{"corner alpha reads in 0-255", "corner.png", encodePNG(t, filled(image.NewNRGBA(image.Rect(0, 0, 40, 10)), color.NRGBA{0, 0, 0, 0x80})), []string{"corner pixel (0,0) has alpha 128/255"}},
		{"svg full-size white rect", "r.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><rect width="100%" height="100%" fill="#fff"/><path d="M1 1h5"/></svg>`), []string{"r.svg", "<rect>", `"#fff"`, "background plate", "remove the background"}},
		{"svg rect with default black fill", "b.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><rect x="-5" y="0" width="110" height="20"/></svg>`), []string{`"black"`, "covers 100%"}},
		{"svg rect inheriting fill from group", "g.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><g style="fill:white"><rect width="95" height="20"/></g></svg>`), []string{`"white"`, "covers 95%"}},
		{"svg absolute rectangle path", "pa.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#fff" d="M0 0H100V20H0Z"/><path d="M10 5c1 1 2 2 3 3"/></svg>`), []string{"pa.svg", "<path>", `"#fff"`, "covers 100%"}},
		{"svg relative rectangle path", "pr.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#fff" d="m0,0h100v20h-100z"/></svg>`), []string{"<path>", "covers 100%"}},
		{"svg rectangle path with lineto and implicit repeats", "pl.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#eee" d="M0 0 100 0 L100 20 0 20z"/></svg>`), []string{"<path>"}},
		{"svg plate with knocked-out letters", "pk.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill-rule="evenodd" d="M0 0h100v20H0zM10 5c1 1 2 2 3 3z"/></svg>`), []string{"<path>", `"black"`}},
		{"svg foreignObject", "fo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><foreignObject width="10" height="10"><div/></foreignObject></svg>`), []string{"<foreignObject>"}},
		{"svg circle plate", "c.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><circle cx="10" cy="10" r="10" fill="red"/></svg>`), []string{"<circle>"}},
		{"svg polygon plate", "p.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><polygon points="0,0 10,0 10,10 0,10" fill="#eee"/></svg>`), []string{"<polygon>"}},
		{"svg style background on root", "s.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" style="background-color: #fff"><path d="M1 1h5"/></svg>`), []string{"s.svg", "<svg> sets a CSS background-color"}},
		{"svg style background on child", "s2.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><g style="background:#000"><path d="M1 1h5"/></g></svg>`), []string{"<g> sets a CSS background"}},
		{"svg style block background", "s3.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><style>svg{background:#fff}</style><path d="M1 1h5"/></svg>`), []string{"<style> block sets a CSS background"}},
		{"svg embedded raster", "i.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><image href="data:image/png;base64,AAAA" width="5" height="5"/></svg>`), []string{"embeds an <image>"}},
		{"svg without viewBox", "v.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><path d="M1 1h5"/></svg>`), []string{"v.svg", "no valid viewBox", `viewBox="0 0`}},
		{"not svg", "n.svg", []byte(`<html/>`), []string{"root element is <html>"}},
		{"broken xml", "x.svg", []byte(`<svg viewBox="0 0 1 1"><path></svg>`), []string{"not well-formed XML"}},
		{"empty svg", "e.svg", nil, []string{"no root <svg>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkVendorLogo(tc.file, tc.data)
			if len(problems) == 0 {
				t.Fatalf("%s passed, want rejection", tc.file)
			}
			joined := strings.Join(problems, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems for %s lack %q:\n%s", tc.file, w, joined)
				}
			}
			for _, p := range problems {
				if !strings.HasPrefix(p, tc.file+": ") || !strings.Contains(p, "fix:") {
					t.Errorf("message must start with the file and say how to fix: %q", p)
				}
			}
		})
	}
}

func TestVendorLogoCheckAcceptsTransparentArtwork(t *testing.T) {
	palette := image.NewPaletted(image.Rect(0, 0, 40, 10), color.Palette{color.NRGBA{}, color.NRGBA{0, 0, 0, 0xff}})
	palette.SetColorIndex(20, 5, 1)
	cases := map[string][]byte{
		"mark.png":    encodePNG(t, transparentMark()),
		"palette.png": encodePNG(t, palette),
		"mark.svg":    []byte(goodSVG),
		"none.svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="none" stroke="#000"/><rect width="10" height="10" fill="#fff" fill-opacity="0"/><rect width="10" height="10" fill="rgba(255,255,255,0)"/><rect width="10" height="10" fill="#ffffff00"/><circle cx="5" cy="5" r="2"/></svg>`),
		"upper.SVG":   []byte(goodSVG),
		"clip.svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><defs><clipPath id="c"><rect width="10" height="10"/></clipPath><linearGradient id="g"><stop/></linearGradient></defs><mask id="m"><rect width="10" height="10" fill="#fff"/></mask><symbol id="s"><path d="M0 0h10v10H0z"/></symbol><g clip-path="url(#c)"><circle cx="5" cy="5" r="2"/></g><rect width="10" height="10" fill="none"/></svg>`),
	}
	for name, data := range cases {
		if p := checkVendorLogo(name, data); len(p) != 0 {
			t.Errorf("%s rejected:\n%s", name, strings.Join(p, "\n"))
		}
	}
}

func TestVendorAssetMapProblems(t *testing.T) {
	src := []byte(`export const VENDOR_ASSETS: Record<string, string> = {
  nvidia: "/vendor-logos/nvidia.svg",
  // comment
  amd: "/vendor-logos/missing.svg",
  odd: "/elsewhere/odd.svg",
};

export const VENDOR_LOGO_HEIGHT: Record<string, number> = {
  nvidia: 15,
};
`)
	refs, err := vendorAssetRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs["nvidia"] != "/vendor-logos/nvidia.svg" {
		t.Fatalf("refs = %v", refs)
	}
	got := strings.Join(vendorAssetProblems(refs, map[string]bool{"nvidia.svg": true, "orphan.png": true}), "\n")
	for _, w := range []string{
		"VENDOR_ASSETS.amd points at /vendor-logos/missing.svg, which does not exist",
		`VENDOR_ASSETS.odd = "/elsewhere/odd.svg" is not a file directly under /vendor-logos/`,
		"ui/public/vendor-logos/orphan.png: not referenced by VENDOR_ASSETS",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("problems lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "nvidia") {
		t.Errorf("a mapped, present file was reported:\n%s", got)
	}
	if _, err := vendorAssetRefs([]byte("export const OTHER = {};")); err == nil {
		t.Error("source without VENDOR_ASSETS accepted")
	}
}

// bar is a 40x10 transparent canvas with a 32x4 bar of c.
func bar(c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 10))
	for y := 3; y < 7; y++ {
		for x := 4; x < 36; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

var testPlates = vendorPlates{light: rgb{1, 1, 1}, dark: rgb{0x29 / 255.0, 0x29 / 255.0, 0x29 / 255.0}}

// TestVendorInkCheckRejectsIllegibleMarks: each fixture breaks the rule its
// message must name, and the message says how to fix it.
func TestVendorInkCheckRejectsIllegibleMarks(t *testing.T) {
	faint := bar(color.NRGBA{0, 0, 0, 0xff})
	for y := 0; y < 3; y++ { // a pale band above the black bar: 43% of the ink
		for x := 4; x < 36; x++ {
			faint.SetNRGBA(x, y, color.NRGBA{0xf2, 0xf2, 0xf2, 0xff})
		}
	}
	cases := []struct {
		name, file, tone string
		data             []byte
		want             []string
	}{
		{"dark mark declared light", "dark.png", "light", encodePNG(t, bar(color.NRGBA{0x10, 0x10, 0x10, 0xff})), []string{`declared "light" ink`, "100% of its ink weight is dark", `declare it "dark"`, "on .dark --surface-raised"}},
		{"light mark declared dark", "light.png", "dark", encodePNG(t, bar(color.NRGBA{0xff, 0xff, 0xff, 0xff})), []string{`declared "dark" ink`, "is light", `declare it "light"`, "below 1.5:1 contrast on --vendor-logo-light"}},
		{"low-contrast grey mark", "grey.png", "dark", encodePNG(t, bar(color.NRGBA{0xa0, 0xa0, 0xa0, 0xff})), []string{"only 0% of its ink weight reaches 3:1 contrast on --vendor-logo-light", "weighted median 2.6"}},
		{"faint share of a png", "faint.png", "dark", encodePNG(t, faint), []string{"of its ink weight is below 1.5:1", "at most 2% may be"}},
		{"near-white svg declared dark", "pale.svg", "dark", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 20"><path fill="#f4f4f4" d="M0 0C50 5 50 5 100 0V20H0Z"/><path fill="#000" d="M10 5l5 5l-5 5z"/></svg>`), []string{`declared "dark" ink`, `fill "#f4f4f4" is 1.10:1`, "every painted colour must reach 1.5:1"}},
		{"pale svg with a faint filtered accent", "accent.svg", "light", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect x="0" y="0" width="3" height="10" fill="white"/><rect x="3" y="0" width="7" height="10" fill="#00e000" opacity=".5"/></svg>`), []string{"only 30% of its ink weight reaches 3:1 contrast on --vendor-logo-light after the light-theme filter"}},
		{"svg gradient fill", "grad.svg", "dark", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path fill="url(#g)" d="M0 0L9 9H0z"/><path d="M1 1l3 3l-3 3z"/></svg>`), []string{`fill "url(#g)" is not a colour the check can read`}},
		{"svg text", "text.svg", "dark", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><text>X</text><path d="M1 1l3 3l-3 3z"/></svg>`), []string{"a filled <text>", "outlined paths"}},
		{"svg with nothing drawn", "empty.svg", "dark", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><defs><path d="M0 0L9 9H0z"/></defs></svg>`), []string{"no ink the check can measure"}},
		{"transparent png", "clear.png", "dark", encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 4, 4))), []string{"no ink the check can measure"}},
		{"broken png", "broken.png", "dark", []byte("not a png"), []string{"not a decodable PNG"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := checkVendorInk(tc.file, tc.data, tc.tone, testPlates)
			if len(problems) == 0 {
				t.Fatalf("%s passed, want rejection", tc.file)
			}
			joined := strings.Join(problems, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems for %s lack %q:\n%s", tc.file, w, joined)
				}
			}
			for _, p := range problems {
				if !strings.HasPrefix(p, tc.file+": ") || !strings.Contains(p, "fix:") {
					t.Errorf("message must start with the file and say how to fix: %q", p)
				}
			}
		})
	}
}

func TestVendorInkCheckAcceptsLegibleMarks(t *testing.T) {
	// NVIDIA's shape: black wordmark plus a green accent under 3:1 on white.
	mixed := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 160 30"><path fill="#000" fill-rule="evenodd" d="M40 5h110v20H40z"/><g fill="#76b900"><path d="M0 0c10 5 20 5 30 0v30H0z"/></g><rect x="150" y="0" width="5" height="5" fill="#000" opacity=".9"/></svg>`
	whiteOnDark := encodePNG(t, bar(color.NRGBA{0xff, 0xff, 0xff, 0xff}))
	cases := []struct {
		file, tone string
		data       []byte
		dark       bool
	}{
		{"mixed.svg", "dark", []byte(mixed), true},
		{"black.png", "dark", encodePNG(t, transparentMark()), true},
		{"white.png", "light", whiteOnDark, false},
		{"named.svg", "light", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><g style="fill:white"><circle cx="5" cy="5" r="4"/></g></svg>`), false},
	}
	for _, tc := range cases {
		m, problems := checkVendorInk(tc.file, tc.data, tc.tone, testPlates)
		if len(problems) != 0 {
			t.Errorf("%s rejected:\n%s", tc.file, strings.Join(problems, "\n"))
		}
		if (m.tone() == "dark") != tc.dark {
			t.Errorf("%s measured %s ink", tc.file, m.tone())
		}
	}
	m, _ := checkVendorInk("mixed.svg", []byte(mixed), "dark", testPlates)
	// the black path is 110x20, the green one 30x30, the small rect 5x5
	if want := (2200.0 + 25) / (2200 + 900 + 25); m.shareAt3 < want-1e-9 || m.shareAt3 > want+1e-9 || m.lowestAt != "#76b900" {
		t.Errorf("mixed.svg: shareAt3 = %v (want %v), lowest at %q", m.shareAt3, want, m.lowestAt)
	}
}

func TestVendorInkDeclarationProblems(t *testing.T) {
	refs := map[string]string{"nvidia": "/vendor-logos/nvidia.svg", "amd": "/vendor-logos/amd.svg", "odd": "/vendor-logos/odd.svg"}
	ink := map[string]string{"nvidia": "dark", "odd": "grey", "gone": "dark"}
	got := strings.Join(vendorInkDeclarationProblems(refs, ink), "\n")
	for _, w := range []string{
		"VENDOR_ASSETS.amd has no VENDOR_LOGO_INK entry",
		"VENDOR_LOGO_INK.gone names no mark in VENDOR_ASSETS",
		`VENDOR_LOGO_INK.odd = "grey"; fix: use "dark" or "light"`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("problems lack %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "nvidia") {
		t.Errorf("a declared mark was reported:\n%s", got)
	}
	src := []byte("export const VENDOR_LOGO_INK: Record<string, \"dark\" | \"light\"> = {\n  nvidia: \"dark\",\n  kunlunxin: \"light\",\n};\n")
	if got, err := tsStringRecord(src, "VENDOR_LOGO_INK"); err != nil || got["kunlunxin"] != "light" || len(got) != 2 {
		t.Errorf("tsStringRecord = %v, %v", got, err)
	}
}

func TestVendorMarkDrawerProblems(t *testing.T) {
	files := map[string]string{
		"src/components/vendorAssets.ts":   `export const VENDOR_ASSETS = { nvidia: "/vendor-logos/nvidia.svg" };`,
		"src/components/VendorLogo.tsx":    `import { VENDOR_ASSETS } from "./vendorAssets";`,
		"src/pages/dashboard/Overview.tsx": `import { VENDOR_ASSETS } from "../../components/vendorAssets";`,
		"src/pages/Raw.tsx":                `<img src="/vendor-logos/amd.svg" />`,
		"src/pages/Height.tsx":             `import { VENDOR_LOGO_HEIGHT } from "../components/vendorAssets";`,
		"src/pages/Overview.test.tsx":      `VENDOR_ASSETS`,
		"src/pages/__tests__/fixture.ts":   `"/vendor-logos/x.svg"`,
		"src/pages/NotTheMap.tsx":          `const MY_VENDOR_ASSETS_X = 1;`,
	}
	problems := vendorMarkDrawerProblems(files)
	got := strings.Join(problems, "\n")
	for _, w := range []string{
		"ui/src/pages/dashboard/Overview.tsx: uses VENDOR_ASSETS directly",
		"ui/src/pages/Raw.tsx: names a /vendor-logos/ file directly",
		"VendorMarkImage",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("problems lack %q:\n%s", w, got)
		}
	}
	if len(problems) != 2 {
		t.Errorf("want exactly 2 problems, got:\n%s", got)
	}
	for _, p := range problems {
		if !strings.Contains(p, "fix:") {
			t.Errorf("message lacks a fix: %q", p)
		}
	}
}

func TestParseVendorPlates(t *testing.T) {
	css := []byte(":root { --vendor-logo-light: #fff; }")
	theme := []byte(":root { --surface-raised: #eee; } .dark { --surface-raised: #292929; }")
	p, err := parseVendorPlates(css, theme)
	if err != nil || p != testPlates {
		t.Fatalf("surface references = %v, %v", p, err)
	}
	_, err = parseVendorPlates(css, []byte(".dark { --surface-raised: var(--x); }"))
	if err == nil || !strings.Contains(err.Error(), "no opaque hex colour for .dark --surface-raised") {
		t.Fatalf("missing dark reference surface: %v", err)
	}
}

func TestColourAndContrast(t *testing.T) {
	for in, want := range map[string]rgb{
		"#fff": {1, 1, 1}, "black": {0, 0, 0}, "#FF0000": {1, 0, 0},
		"rgb(0, 255, 0)": {0, 1, 0}, "rgba(0%,0%,100%,.5)": {0, 0, 1}, "#00ff0080": {0, 1, 0},
	} {
		if got, _, ok := parseColour(in); !ok || got != want {
			t.Errorf("parseColour(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if _, a, _ := parseColour("#00ff0080"); a < 0.50 || a > 0.51 {
		t.Errorf("alpha of #00ff0080 = %v", a)
	}
	for _, bad := range []string{"currentColor", "url(#g)", "#12", "hsl(0,0%,0%)"} {
		if _, _, ok := parseColour(bad); ok {
			t.Errorf("parseColour(%q) accepted", bad)
		}
	}
	if c := contrast(rgb{1, 1, 1}, rgb{0, 0, 0}); c < 20.99 || c > 21.01 {
		t.Errorf("white on black = %v, want 21", c)
	}
	if c := contrast(rgb{0x76 / 255.0, 0xb9 / 255.0, 0}, rgb{1, 1, 1}); c < 2.40 || c > 2.43 {
		t.Errorf("#76b900 on white = %v, want about 2.41", c)
	}
	half := over(inkSample{colour: rgb{0, 0, 0}, alpha: 0.5}, rgb{1, 1, 1})
	if half != (rgb{0.5, 0.5, 0.5}) {
		t.Errorf("half black over white = %v", half)
	}
}

func TestPathBounds(t *testing.T) {
	b, ok := pathBounds("M10 10c5 -20 15 -20 20 0l5 5H2V30z m1 1h1")
	if !ok || b != (svgBox{2, 10, 35, 30}) {
		t.Errorf("bounds = %v, %v", b, ok)
	}
	if _, ok := pathBounds("M0 0 X 5"); ok {
		t.Error("an unknown command was followed")
	}
	if _, ok := pathBounds("M0 0h10"); ok {
		t.Error("a zero-height path has bounds")
	}
}
