// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build ignore

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cuelang.org/go/cue"
	cueformat "cuelang.org/go/cue/format"
	"cuelang.org/go/cue/load"
)

const prefix = "../pkg/"

const header = `// Code generated by go generate. DO NOT EDIT.

package cue

`

const initFunc = `
func init() {
	initBuiltins(builtinPackages)
}

var _ io.Reader
`

func main() {
	flag.Parse()
	log.SetFlags(log.Lshortfile)
	log.SetOutput(os.Stdout)

	g := generator{
		w:     &bytes.Buffer{},
		decls: &bytes.Buffer{},
		fset:  token.NewFileSet(),
	}

	fmt.Fprintln(g.w, "var builtinPackages = map[string]*builtinPkg{")
	filepath.Walk(prefix, func(dir string, info os.FileInfo, err error) error {
		if err != nil {
			log.Fatal(err)
		}
		if info.Name() == "testdata" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			g.processDir(dir)
		}
		return nil
	})
	fmt.Fprintln(g.w, "}")

	w := &bytes.Buffer{}
	fmt.Fprintln(w, header)

	m := map[string]*ast.ImportSpec{}
	keys := []string{}
	for _, spec := range g.imports {
		if spec.Path.Value == `"cuelang.org/go/cue"` {
			// Don't add this package.
			continue
		}
		if prev, ok := m[spec.Path.Value]; ok {
			if importName(prev) != importName(spec) {
				log.Fatalf("inconsistent name for import %s: %q != %q",
					spec.Path.Value, importName(prev), importName(spec))
			}
			continue
		}
		m[spec.Path.Value] = spec
		keys = append(keys, spec.Path.Value)
	}
	fmt.Fprintln(w, "import (")
	sort.Strings(keys)
	for _, k := range keys {
		printer.Fprint(w, g.fset, m[k])
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, ")")
	fmt.Fprintln(w, initFunc)
	io.Copy(w, g.decls)
	io.Copy(w, g.w)

	b, err := format.Source(w.Bytes())
	if err != nil {
		b = w.Bytes() // write the unformatted source
	}
	// TODO: do this in a more principled way. The best is probably to
	// put all builtins in a separate package.
	b = bytes.Replace(b, []byte("cue."), []byte(""), -1)

	if err := ioutil.WriteFile("builtins.go", b, 0644); err != nil {
		log.Fatal(err)
	}
	if err != nil {
		log.Fatal(err)
	}
}

type generator struct {
	w          *bytes.Buffer
	decls      *bytes.Buffer
	name       string
	fset       *token.FileSet
	defaultPkg string
	first      bool
	iota       int

	imports []*ast.ImportSpec
}

func (g *generator) processDir(dir string) {
	goFiles, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		log.Fatal(err)
	}

	cueFiles, err := filepath.Glob(filepath.Join(dir, "*.cue"))
	if err != nil {
		log.Fatal(err)
	}

	if len(goFiles)+len(cueFiles) == 0 {
		return
	}

	pkg := dir[len(prefix):]
	fmt.Fprintf(g.w, "%q: &builtinPkg{\nnative: []*builtin{{\n", pkg)
	g.first = true
	for _, filename := range goFiles {
		g.processGo(filename)
	}
	fmt.Fprintf(g.w, "}},\n")
	g.processCUE(dir)
	fmt.Fprintf(g.w, "},\n")
}

func (g *generator) sep() {
	if g.first {
		g.first = false
		return
	}
	fmt.Fprintln(g.w, "}, {")
}

func importName(s *ast.ImportSpec) string {
	if s.Name != nil {
		return s.Name.Name
	}
	pkg, err := strconv.Unquote(s.Path.Value)
	if err != nil {
		log.Fatal(err)
	}
	return path.Base(pkg)
}

// processCUE mixes in CUE definitions defined in the package directory.
func (g *generator) processCUE(dir string) {
	instances := cue.Build(load.Instances([]string{dir}, &load.Config{
		StdRoot: "../pkg",
	}))

	if err := instances[0].Err; err != nil {
		if !strings.Contains(err.Error(), "no CUE files") {
			log.Fatal(err)
		}
		return
	}

	n := instances[0].Value().Syntax(cue.Hidden(true), cue.Concrete(false))
	b, err := cueformat.Node(n)
	if err != nil {
		log.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("\n\n"), []byte("\n"))
	// body = strings.ReplaceAll(body, "\t", "")
	// TODO: escape backtick
	fmt.Fprintf(g.w, "cue: `%s`,\n", string(b))
}

func (g *generator) processGo(filename string) {
	if strings.HasSuffix(filename, "_test.go") {
		return
	}
	f, err := parser.ParseFile(g.fset, filename, nil, parser.ParseComments)
	if err != nil {
		log.Fatal(err)
	}
	g.defaultPkg = ""
	g.name = f.Name.Name
	if g.name == "structs" {
		g.name = "struct"
	}

	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.GenDecl:
			switch x.Tok {
			case token.CONST:
				for _, spec := range x.Specs {
					if !ast.IsExported(spec.(*ast.ValueSpec).Names[0].Name) {
						continue
					}
					g.genConst(spec.(*ast.ValueSpec))
				}
			case token.IMPORT:
				for _, s := range x.Specs {
					spec := s.(*ast.ImportSpec)
					g.imports = append(g.imports, spec)
				}
				if g.defaultPkg == "" {
					g.defaultPkg = importName(x.Specs[0].(*ast.ImportSpec))
				}
			case token.VAR:
				for _, spec := range x.Specs {
					if ast.IsExported(spec.(*ast.ValueSpec).Names[0].Name) {
						log.Fatal("gen %s: var declarations not supported", filename)
					}
				}
				printer.Fprint(g.decls, g.fset, x)
				fmt.Fprint(g.decls, "\n\n")
				continue
			case token.TYPE:
				// TODO: support type declarations.
				for _, spec := range x.Specs {
					if ast.IsExported(spec.(*ast.TypeSpec).Name.Name) {
						log.Fatal("gen %s: type declarations not supported", filename)
					}
				}
				continue
			default:
				log.Fatalf("gen %s: unexpected spec of type %s", filename, x.Tok)
			}
		case *ast.FuncDecl:
			g.genFun(x)
		}
	}
}

func (g *generator) genConst(spec *ast.ValueSpec) {
	name := spec.Names[0].Name
	value := ""
	switch v := g.toValue(spec.Values[0]); v.Kind() {
	case constant.Bool, constant.Int, constant.String:
		// TODO: convert octal numbers
		value = v.ExactString()
	case constant.Float:
		var rat big.Rat
		rat.SetString(v.ExactString())
		var float big.Float
		float.SetRat(&rat)
		value = float.Text('g', -1)
	default:
		fmt.Printf("Dropped entry %s.%s (%T: %v)\n", g.defaultPkg, name, v.Kind(), v.ExactString())
		return
	}
	g.sep()
	fmt.Fprintf(g.w, "Name: %q,\n Const: %q,\n", name, value)
}

func (g *generator) toValue(x ast.Expr) constant.Value {
	switch x := x.(type) {
	case *ast.BasicLit:
		return constant.MakeFromLiteral(x.Value, x.Kind, 0)
	case *ast.BinaryExpr:
		return constant.BinaryOp(g.toValue(x.X), x.Op, g.toValue(x.Y))
	case *ast.UnaryExpr:
		return constant.UnaryOp(x.Op, g.toValue(x.X), 0)
	default:
		log.Fatalf("%s: unsupported expression type %T: %#v", g.defaultPkg, x, x)
	}
	return constant.MakeUnknown()
}

func (g *generator) genFun(x *ast.FuncDecl) {
	if x.Body == nil {
		return
	}
	types := []string{}
	if x.Type.Results != nil {
		for _, f := range x.Type.Results.List {
			if len(f.Names) > 0 {
				for range f.Names {
					types = append(types, g.goKind(f.Type))
				}
			} else {
				types = append(types, g.goKind(f.Type))
			}
		}
	}
	if n := len(types); n != 1 && (n != 2 || types[1] != "error") {
		fmt.Printf("Dropped func %s.%s: must have one return value or a value and an error %v\n", g.defaultPkg, x.Name.Name, types)
		return
	}

	if !ast.IsExported(x.Name.Name) || x.Recv != nil {
		if strings.HasPrefix(x.Name.Name, g.name) {
			printer.Fprint(g.decls, g.fset, x)
			fmt.Fprint(g.decls, "\n\n")
		}
		return
	}

	g.sep()
	fmt.Fprintf(g.w, "Name: %q,\n", x.Name.Name)

	args := []string{}
	vals := []string{}
	kind := []string{}
	omitCheck := true
	for _, f := range x.Type.Params.List {
		for _, name := range f.Names {
			typ := g.goKind(f.Type)
			argKind, ground := g.goToCUE(f.Type)
			if !ground {
				omitCheck = false
			}
			vals = append(vals, fmt.Sprintf("c.%s(%d)", typ, len(args)))
			args = append(args, name.Name)
			kind = append(kind, argKind)
		}
	}

	fmt.Fprintf(g.w, "Params: []kind{%s},\n", strings.Join(kind, ", "))
	result, _ := g.goToCUE(x.Type.Results.List[0].Type)
	fmt.Fprintf(g.w, "Result: %s,\n", result)
	argList := strings.Join(args, ", ")
	valList := strings.Join(vals, ", ")
	init := ""
	if len(args) > 0 {
		init = fmt.Sprintf("%s := %s", argList, valList)
	}

	fmt.Fprintf(g.w, "Func: func(c *callCtxt) {")
	defer fmt.Fprintln(g.w, "},")
	fmt.Fprintln(g.w)
	if init != "" {
		fmt.Fprintln(g.w, init)
	}
	if !omitCheck {
		fmt.Fprintln(g.w, "if c.do() {")
		defer fmt.Fprintln(g.w, "}")
	}
	if len(types) == 1 {
		fmt.Fprint(g.w, "c.ret = func() interface{} ")
	} else {
		fmt.Fprint(g.w, "c.ret, c.err = func() (interface{}, error) ")
	}
	printer.Fprint(g.w, g.fset, x.Body)
	fmt.Fprintln(g.w, "()")
}

func (g *generator) goKind(expr ast.Expr) string {
	if star, isStar := expr.(*ast.StarExpr); isStar {
		expr = star.X
	}
	w := &bytes.Buffer{}
	printer.Fprint(w, g.fset, expr)
	switch str := w.String(); str {
	case "big.Int":
		return "bigInt"
	case "big.Float":
		return "bigFloat"
	case "big.Rat":
		return "bigRat"
	case "internal.Decimal":
		return "decimal"
	case "cue.Struct":
		return "structVal"
	case "cue.Value":
		return "value"
	case "cue.List":
		return "list"
	case "[]string":
		return "strList"
	case "[]byte":
		return "bytes"
	case "[]cue.Value":
		return "list"
	case "io.Reader":
		return "reader"
	case "time.Time":
		return "string"
	default:
		return str
	}
}

func (g *generator) goToCUE(expr ast.Expr) (cueKind string, omitCheck bool) {
	// TODO: detect list and structs types for return values.
	omitCheck = true
	switch k := g.goKind(expr); k {
	case "error":
		cueKind += "bottomKind"
	case "bool":
		cueKind += "boolKind"
	case "string", "bytes", "reader":
		cueKind += "stringKind"
	case "int", "int8", "int16", "int32", "rune", "int64",
		"uint", "byte", "uint8", "uint16", "uint32", "uint64",
		"bigInt":
		cueKind += "intKind"
	case "float64", "bigRat", "bigFloat", "decimal":
		cueKind += "numKind"
	case "list":
		cueKind += "listKind"
	case "strList":
		omitCheck = false
		cueKind += "listKind"
	case "structVal":
		cueKind += "structKind"
	case "value":
		// Must use callCtxt.value method for these types and resolve manually.
		cueKind += "topKind" // TODO: can be more precise
	default:
		switch {
		case strings.HasPrefix(k, "[]"):
			cueKind += "listKind"
		case strings.HasPrefix(k, "map["):
			cueKind += "structKind"
		default:
			// log.Println("Unknown type:", k)
			// Must use callCtxt.value method for these types and resolve manually.
			cueKind += "topKind" // TODO: can be more precise
			omitCheck = false
		}
	}
	return cueKind, omitCheck
}
