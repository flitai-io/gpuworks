package proxy

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGatewayErrorTableIsComplete: every code has a status (but the
// upstream's own), an OpenAI type, an English template whose placeholders
// are lower_snake params, and console text in en and zh.
func TestGatewayErrorTableIsComplete(t *testing.T) {
	openAITypes := map[string]bool{typeInvalidRequest: true, typeAuthentication: true, typePermission: true, typeNotFound: true, typeServer: true}
	upper := regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
	locales := map[string]map[string]string{}
	for _, locale := range []string{"en", "zh"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "ui", "src", "i18n", "locales", locale, "common.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			APIError map[string]string `json:"apiError"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		locales[locale] = doc.APIError
	}
	for code, row := range gatewayErrors {
		if !upper.MatchString(code) {
			t.Errorf("%s is not UPPER_SNAKE", code)
		}
		if code == ErrorUpstreamRejected {
			if row.status != 0 || row.typ != "" {
				t.Errorf("%s takes the upstream's status and its type from it, got %d %q", code, row.status, row.typ)
			}
		} else {
			if row.status < 400 || row.status > 599 {
				t.Errorf("%s status = %d", code, row.status)
			}
			if !openAITypes[row.typ] {
				t.Errorf("%s type = %q, not an OpenAI error type", code, row.typ)
			}
			if (row.status >= 500) != (row.typ == typeServer) {
				t.Errorf("%s: status %d with type %q", code, row.status, row.typ)
			}
		}
		if strings.TrimSpace(row.template) == "" {
			t.Errorf("%s has no English message", code)
		}
		for locale, texts := range locales {
			if strings.TrimSpace(texts[code]) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
			}
		}
	}
}

// TestGatewayErrorsAreWrittenFromTheTable: every Error* constant is a row of
// the table, every refusal names one of them, writeAPIError is the only
// writer (no http.Error, no writeErr, no hand-written error body), and the
// table is the only thing that knows a status.
func TestGatewayErrorsAreWrittenFromTheTable(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]bool{}
	rows := map[string]bool{}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, file)
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.GenDecl:
				if node.Tok == token.CONST {
					for _, spec := range node.Specs {
						for _, ident := range spec.(*ast.ValueSpec).Names {
							if strings.HasPrefix(ident.Name, "Error") {
								constants[ident.Name] = true
							}
						}
					}
				}
			case *ast.ValueSpec:
				if len(node.Names) == 1 && node.Names[0].Name == "gatewayErrors" {
					for _, elt := range node.Values[0].(*ast.CompositeLit).Elts {
						if key, ok := elt.(*ast.KeyValueExpr).Key.(*ast.Ident); ok {
							rows[key.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	for name := range constants {
		if !rows[name] {
			t.Errorf("%s is not a row of gatewayErrors", name)
		}
	}
	if len(rows) != len(gatewayErrors) {
		t.Errorf("gatewayErrors has %d rows, %d named by constants", len(gatewayErrors), len(rows))
	}
	tableCode := func(arg ast.Expr) bool {
		ident, ok := arg.(*ast.Ident)
		return ok && constants[ident.Name]
	}
	for _, file := range parsed {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				switch fn := node.Fun.(type) {
				case *ast.Ident:
					switch fn.Name {
					case "refuse":
						if !tableCode(node.Args[0]) {
							t.Errorf("%s: refuse takes a code that is not a table constant", fset.Position(node.Pos()))
						}
					case "writeAPIError":
						// Besides constants: a refusal's own code (writeRefusal)
						// and the transport's classification (upstreamErrorCode).
						if !tableCode(node.Args[2]) && !isSelector(node.Args[2], "e", "code") && !isIdent(node.Args[2], "code") {
							t.Errorf("%s: writeAPIError takes a code that is not a table constant", fset.Position(node.Pos()))
						}
					case "writeErr":
						t.Errorf("%s: writeErr bypasses the gateway's error table", fset.Position(node.Pos()))
					}
				case *ast.SelectorExpr:
					if isIdent(fn.X, "http") && fn.Sel.Name == "Error" {
						t.Errorf("%s: http.Error bypasses the gateway's error table", fset.Position(node.Pos()))
					}
				}
			case *ast.BasicLit:
				if node.Kind == token.STRING && strings.Contains(node.Value, `{"error"`) {
					t.Errorf("%s: a hand-written error body", fset.Position(node.Pos()))
				}
			}
			return true
		})
	}
}

func isIdent(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == name
}

func isSelector(e ast.Expr, x, sel string) bool {
	s, ok := e.(*ast.SelectorExpr)
	return ok && isIdent(s.X, x) && s.Sel.Name == sel
}

// TestGatewayErrorWire pins the literal body of representative codes.
func TestGatewayErrorWire(t *testing.T) {
	for _, tc := range []struct {
		code   string
		params map[string]any
		status int
		body   string
	}{
		{ErrorInvalidAPIKey, nil, 401, `{"error":{"message":"invalid or missing API key","type":"authentication_error","code":"INVALID_API_KEY","param":null}}`},
		{ErrorModelAccessDenied, map[string]any{"model": "beta"}, 403, `{"error":{"message":"this API key may not call model beta","type":"permission_error","code":"MODEL_ACCESS_DENIED","param":"model","params":{"model":"beta"}}}`},
		{ErrorModelNotFound, map[string]any{"model": "nope"}, 404, `{"error":{"message":"no deployment serves model nope","type":"not_found_error","code":"MODEL_NOT_FOUND","param":"model","params":{"model":"nope"}}}`},
		{ErrorModelRequired, nil, 400, `{"error":{"message":"request body must specify a model","type":"invalid_request_error","code":"MODEL_REQUIRED","param":"model"}}`},
		{ErrorModelAsleep, map[string]any{"deployment_id": 7}, 503, `{"error":{"message":"model is asleep; wake it first (console, or POST /api/v1/deployments/7/wake; a schedule or a waiting wake may also wake it)","type":"server_error","code":"MODEL_ASLEEP","param":null,"params":{"deployment_id":7}}}`},
		{ErrorModalityUnsupported, map[string]any{"modality": "image_url", "supported": "text"}, 400, `{"error":{"message":"This model does not support image_url input (supported: text). Remove the attachment or select a compatible model.","type":"invalid_request_error","code":"MODEL_INPUT_MODALITY_UNSUPPORTED","param":"messages","params":{"modality":"image_url","supported":"text"}}}`},
		{ErrorUpstreamUnavailable, map[string]any{"detail": "dial: refused"}, 502, `{"error":{"message":"upstream error: dial: refused","type":"server_error","code":"UPSTREAM_UNAVAILABLE","param":null,"params":{"detail":"dial: refused"}}}`},
		{ErrorRouteNotFound, map[string]any{"path": "/v1/chat/completions"}, 404, `{"error":{"message":"/v1/chat/completions is not served on this address","type":"not_found_error","code":"ROUTE_NOT_FOUND","param":null,"params":{"path":"/v1/chat/completions"}}}`},
	} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		writeAPIError(rec, r, tc.code, tc.params)
		if rec.Code != tc.status || rec.Body.String() != tc.body+"\n" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s = %d %s, want %d %s", tc.code, rec.Code, rec.Body.String(), tc.status, tc.body)
		}
		if rec.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s names no request", tc.code)
		}
		var round struct {
			Error apiErrorObject `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &round); err != nil || round.Error.Code != tc.code {
			t.Errorf("%s does not round-trip: %v %+v", tc.code, err, round)
		}
	}
	// An X-Request-ID an earlier step set is kept.
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-ID", "r42")
	writeAPIError(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), ErrorInvalidAPIKey, nil)
	if rec.Header().Get("X-Request-ID") != "r42" {
		t.Fatalf("X-Request-ID = %q, want the access log's r42", rec.Header().Get("X-Request-ID"))
	}
}

// TestUpstreamRejectionTakesTheUpstreamsStatusAndType: an engine's error the
// gateway rewrites keeps the engine's status, typed by it.
func TestUpstreamRejectionTakesTheUpstreamsStatusAndType(t *testing.T) {
	if got := string(apiErrorBody(ErrorUpstreamRejected, map[string]any{"detail": "bad"}, 422)); got != `{"error":{"message":"bad","type":"invalid_request_error","code":"UPSTREAM_REJECTED","param":null,"params":{"detail":"bad"}}}` {
		t.Fatalf("4xx = %s", got)
	}
	if got := string(apiErrorBody(ErrorUpstreamRejected, map[string]any{"detail": "boom"}, 500)); got != `{"error":{"message":"boom","type":"server_error","code":"UPSTREAM_REJECTED","param":null,"params":{"detail":"boom"}}}` {
		t.Fatalf("5xx = %s", got)
	}
}

func TestUpstreamErrorCodeClassifiesTheTransport(t *testing.T) {
	if got := upstreamErrorCode(errInstanceUnavailable); got != ErrorInstanceUnavailable {
		t.Fatalf("instance gone = %s", got)
	}
	if got := upstreamErrorCode(os.ErrDeadlineExceeded); got != ErrorUpstreamUnavailable {
		t.Fatalf("other = %s", got)
	}
}
