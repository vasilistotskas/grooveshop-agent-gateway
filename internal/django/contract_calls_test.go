package django

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// observedCall is one Django call as the client source spells it.
type observedCall struct {
	method string
	path   string
	body   string // Go type of request.body, "" when absent
	out    string // Go type decoded into, "" when absent
	// query maps each query parameter set to whether every call sends it.
	query map[string]bool
}

// observeClientCalls reads this package's source and returns, per Client
// method, the Django call it makes through c.get or c.send.
func observeClientCalls(t *testing.T) map[string]observedCall {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	var methods []*ast.FuncDecl
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, dup := funcs[fn.Name.Name]; !dup {
				funcs[fn.Name.Name] = fn
			} else {
				funcs[fn.Name.Name] = nil // ambiguous: never followed
			}
			if receiverType(fn) == "Client" {
				methods = append(methods, fn)
			}
		}
	}

	out := map[string]observedCall{}
	for _, fn := range methods {
		o := callObserver{t: t, fn: fn, funcs: funcs}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if c, ok := o.djangoCall(call); ok {
				if _, dup := out[fn.Name.Name]; dup {
					t.Fatalf("Client.%s makes more than one Django call",
						fn.Name.Name)
				}
				out[fn.Name.Name] = c
			}
			return true
		})
	}
	return out
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

type callObserver struct {
	t     *testing.T
	fn    *ast.FuncDecl
	funcs map[string]*ast.FuncDecl
}

func (o callObserver) fatalf(format string, args ...any) {
	o.t.Helper()
	o.t.Fatalf("Client.%s: "+format, append([]any{o.fn.Name.Name}, args...)...)
}

// djangoCall recognises recv.get(ctx, request{…}) and
// recv.send(ctx, http.MethodX, request{…}).
func (o callObserver) djangoCall(call *ast.CallExpr) (observedCall, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return observedCall{}, false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != o.fn.Recv.List[0].Names[0].Name {
		return observedCall{}, false
	}
	var c observedCall
	var lit ast.Expr
	switch sel.Sel.Name {
	case "get":
		c.method = "GET"
		lit = call.Args[1]
	case "send":
		m, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(m.Sel.Name, "Method") {
			o.fatalf("send needs an http.MethodX constant")
		}
		c.method = strings.ToUpper(strings.TrimPrefix(m.Sel.Name, "Method"))
		lit = call.Args[2]
	default:
		return observedCall{}, false
	}

	req, ok := lit.(*ast.CompositeLit)
	if !ok || types.ExprString(req.Type) != "request" {
		o.fatalf("pass the request as a request{…} literal")
	}
	c.query = map[string]bool{}
	for _, e := range req.Elts {
		kv := e.(*ast.KeyValueExpr)
		switch kv.Key.(*ast.Ident).Name {
		case "path":
			c.path = o.stringLit(kv.Value, "request.path")
		case "body":
			c.body = o.typeOf(kv.Value)
		case "out":
			c.out = o.typeOf(kv.Value)
		case "query":
			o.queryParams(kv.Value, c.query)
		}
	}
	return c, true
}

func (o callObserver) stringLit(e ast.Expr, what string) string {
	lit, ok := e.(*ast.BasicLit)
	if ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s
		}
	}
	o.fatalf("%s must be a string literal (a templated path, "+
		"with the concrete one in rawPath)", what)
	return ""
}

// typeOf resolves the Go type of a body or &out expression: a composite
// literal, a parameter, a `var x T` or an `x := T{…}`.
func (o callObserver) typeOf(e ast.Expr) string {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	switch e := e.(type) {
	case *ast.CompositeLit:
		return types.ExprString(e.Type)
	case *ast.Ident:
		for _, p := range o.fn.Type.Params.List {
			for _, n := range p.Names {
				if n.Name == e.Name {
					return types.ExprString(p.Type)
				}
			}
		}
		var found string
		ast.Inspect(o.fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for _, id := range n.Names {
					if id.Name == e.Name && n.Type != nil {
						found = types.ExprString(n.Type)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || id.Name != e.Name || i >= len(n.Rhs) {
						continue
					}
					if cl, ok := n.Rhs[i].(*ast.CompositeLit); ok {
						found = types.ExprString(cl.Type)
					}
				}
			}
			return found == ""
		})
		if found != "" {
			return found
		}
	}
	o.fatalf("cannot resolve the type of %s", types.ExprString(e))
	return ""
}

// queryParams collects the keys of the url.Values a call sends: a literal,
// a local built in the method, or one returned by a helper method. A key
// set at the top level of that body is sent every time; one set inside a
// branch or loop is conditional.
func (o callObserver) queryParams(e ast.Expr, into map[string]bool) {
	switch e := e.(type) {
	case *ast.CompositeLit:
		o.valuesKeys(e, true, into)
	case *ast.Ident:
		o.scanBody(o.fn.Body, into)
	case *ast.CallExpr:
		var helper *ast.FuncDecl
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			helper = o.funcs[sel.Sel.Name]
		}
		if helper == nil {
			o.fatalf("cannot follow query helper %s", types.ExprString(e))
		}
		o.scanBody(helper.Body, into)
	default:
		o.fatalf("cannot read query %s", types.ExprString(e))
	}
}

func (o callObserver) scanBody(body *ast.BlockStmt, into map[string]bool) {
	for _, stmt := range body.List {
		always := true
		switch stmt.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BlockStmt:
			always = false
		}
		ast.Inspect(stmt, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if types.ExprString(n.Type) == "url.Values" {
					o.valuesKeys(n, always, into)
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Set" && len(n.Args) == 2 {
					if _, isIdent := sel.X.(*ast.Ident); isIdent {
						key := o.stringLit(n.Args[0], "a query key")
						into[key] = into[key] || always
					}
				}
			}
			return true
		})
	}
}

func (o callObserver) valuesKeys(
	lit *ast.CompositeLit, always bool, into map[string]bool,
) {
	for _, e := range lit.Elts {
		key := o.stringLit(e.(*ast.KeyValueExpr).Key, "a query key")
		into[key] = into[key] || always
	}
}
