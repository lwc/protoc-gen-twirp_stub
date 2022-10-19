// Copyright 2018 Twitch Interactive, Inc.  All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the License is
// located at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed on
// an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"strconv"

	"google.golang.org/protobuf/proto"
	descriptor "google.golang.org/protobuf/types/descriptorpb"
	plugin "google.golang.org/protobuf/types/pluginpb"
	"github.com/lwc/protoc-gen-twirp_stub/gen"
	"github.com/lwc/protoc-gen-twirp_stub/gen/stringutils"
	"github.com/lwc/protoc-gen-twirp_stub/gen/typemap"
	"github.com/pkg/errors"
)

type twirp struct {
	filesHandled   int
	currentPackage string // Go name of current package we're working on

	reg *typemap.Registry

	// Map to record whether we've built each package
	pkgs          map[string]string
	pkgNamesInUse map[string]bool

	importPrefix string            // String to prefix to imported package file names.
	importMap    map[string]string // Mapping from .proto file name to import path.

	// Package output:
	sourceRelativePaths bool // instruction on where to write output files

	// Package naming:
	genPkgName          string // Name of the package that we're generating
	fileToGoPackageName map[*descriptor.FileDescriptorProto]string

	// List of files that were inputs to the generator. We need to hold this in
	// the struct so we can write a header for the file that lists its inputs.
	genFiles []*descriptor.FileDescriptorProto

	// Output buffer that holds the bytes we want to write out for a single file.
	// Gets reset after working on a file.
	output *bytes.Buffer
}

func newGenerator() *twirp {
	t := &twirp{
		pkgs:                make(map[string]string),
		pkgNamesInUse:       make(map[string]bool),
		importMap:           make(map[string]string),
		fileToGoPackageName: make(map[*descriptor.FileDescriptorProto]string),
		output:              bytes.NewBuffer(nil),
	}

	return t
}

func (t *twirp) Generate(in *plugin.CodeGeneratorRequest) *plugin.CodeGeneratorResponse {
	params, err := parseCommandLineParams(in.GetParameter())
	if err != nil {
		gen.Fail("could not parse parameters passed to --twirp_out", err.Error())
	}
	t.importPrefix = params.importPrefix
	t.importMap = params.importMap

	t.genFiles = gen.FilesToGenerate(in)

	t.sourceRelativePaths = params.paths == "source_relative"

	// Collect information on types.
	t.reg = typemap.New(in.ProtoFile)

	// Register names of packages that we import.
	t.registerPackageName("context")

	// Time to figure out package names of objects defined in protobuf. First,
	// we'll figure out the name for the package we're generating.
	genPkgName, err := deduceGenPkgName(t.genFiles)
	if err != nil {
		gen.Fail(err.Error())
	}
	t.genPkgName = genPkgName

	// Next, we need to pick names for all the files that are dependencies.
	for _, f := range in.ProtoFile {
		if fileDescSliceContains(t.genFiles, f) {
			// This is a file we are generating. It gets the shared package name.
			t.fileToGoPackageName[f] = t.genPkgName
		} else {
			// This is a dependency. Use its package name.
			name := f.GetPackage()
			if name == "" {
				name = stringutils.BaseName(f.GetName())
			}
			name = stringutils.CleanIdentifier(name)
			alias := t.registerPackageName(name)
			t.fileToGoPackageName[f] = alias
		}
	}
	// Showtime! Generate the response.
	resp := new(plugin.CodeGeneratorResponse)
	for _, f := range t.genFiles {
		respFile := t.generate(f)
		if respFile != nil {
			resp.File = append(resp.File, respFile)
		}
	}
	return resp
}

func (t *twirp) registerPackageName(name string) (alias string) {
	alias = name
	i := 1
	for t.pkgNamesInUse[alias] {
		alias = name + strconv.Itoa(i)
		i++
	}
	t.pkgNamesInUse[alias] = true
	t.pkgs[name] = alias
	return alias
}

// deduceGenPkgName figures out the go package name to use for generated code.
// Will try to use the explicit go_package setting in a file (if set, must be
// consistent in all files). If no files have go_package set, then use the
// protobuf package name (must be consistent in all files)
func deduceGenPkgName(genFiles []*descriptor.FileDescriptorProto) (string, error) {
	var genPkgName string
	for _, f := range genFiles {
		name, explicit := goPackageName(f)
		if explicit {
			name = stringutils.CleanIdentifier(name)
			if genPkgName != "" && genPkgName != name {
				// Make sure they're all set consistently.
				return "", errors.Errorf("files have conflicting go_package settings, must be the same: %q and %q", genPkgName, name)
			}
			genPkgName = name
		}
	}
	if genPkgName != "" {
		return genPkgName, nil
	}

	// If there is no explicit setting, then check the implicit package name
	// (derived from the protobuf package name) of the files and make sure it's
	// consistent.
	for _, f := range genFiles {
		name, _ := goPackageName(f)
		name = stringutils.CleanIdentifier(name)
		if genPkgName != "" && genPkgName != name {
			return "", errors.Errorf("files have conflicting package names, must be the same or overridden with go_package: %q and %q", genPkgName, name)
		}
		genPkgName = name
	}

	// All the files have the same name, so we're good.
	return genPkgName, nil
}

func (t *twirp) generate(file *descriptor.FileDescriptorProto) *plugin.CodeGeneratorResponse_File {
	resp := new(plugin.CodeGeneratorResponse_File)
	if len(file.Service) == 0 {
		return nil
	}

	t.generateFileHeader(file)
	t.generateImports(file)

	for _, service := range file.Service {
		t.generateStub(file, service)
		t.generateNoop(file, service)
	}

	resp.Name = proto.String(t.goFileName(file))
	resp.Content = proto.String(t.formattedOutput())
	t.output.Reset()

	return resp
}

func (t *twirp) generateFileHeader(file *descriptor.FileDescriptorProto) {
	t.P("// Code generated by protoc-gen-twirp_stub ", gen.Version, ", DO NOT EDIT.")
	t.P("// source: ", file.GetName())
	t.P()
	t.P(`package `, t.genPkgName)
	t.P()
}

func (t *twirp) generateImports(file *descriptor.FileDescriptorProto) {
	if len(file.Service) == 0 {
		return
	}
	t.P(`import `, t.pkgs["context"], ` "context"`)

	// It's legal to import a message and use it as an input or output for a
	// method. Make sure to import the package of any such message. First, dedupe
	// them.
	deps := make(map[string]string) // Map of package name to quoted import path.
	ourImportPath := path.Dir(t.goFileName(file))
	for _, s := range file.Service {
		for _, m := range s.Method {
			defs := []*typemap.MessageDefinition{
				t.reg.MethodInputDefinition(m),
				t.reg.MethodOutputDefinition(m),
			}
			for _, def := range defs {
				// By default, import path is the dirname of the Go filename.
				importPath := path.Dir(t.goFileName(def.File))
				if importPath == ourImportPath {
					continue
				}

				importPathOpt, _ := parseGoPackageOption(def.File.GetOptions().GetGoPackage())
				if importPathOpt != "" {
					importPath = importPathOpt
				}

				if substitution, ok := t.importMap[def.File.GetName()]; ok {
					importPath = substitution
				}
				importPath = t.importPrefix + importPath
				pkg := t.goPackageName(def.File)
				deps[pkg] = strconv.Quote(importPath)

			}
		}
	}
	for pkg, importPath := range deps {
		t.P(`import `, pkg, ` `, importPath)
	}
	if len(deps) > 0 {
		t.P()
	}
}

// P forwards to g.gen.P, which prints output.
func (t *twirp) P(args ...string) {
	for _, v := range args {
		t.output.WriteString(v)
	}
	t.output.WriteByte('\n')
}

func (t *twirp) generateStub(file *descriptor.FileDescriptorProto, service *descriptor.ServiceDescriptorProto) {
	structName := serviceName(service) + "Stub"

	t.P(`type `, structName, ` struct {`)
	for _, method := range service.Method {
		methName := "On" + methodName(method)
		inputType := t.goTypeName(method.GetInputType())
		outputType := t.goTypeName(method.GetOutputType())

		t.P(methName, ` func(`, t.pkgs["context"], `.Context, *`, inputType, `) (*`, outputType, `, error)`)
	}
	t.P(`}`)

	for _, method := range service.Method {
		methName := methodName(method)
		inputType := t.goTypeName(method.GetInputType())
		outputType := t.goTypeName(method.GetOutputType())

		t.P(`func (s *`, structName, `) `, methName, `(ctx `, t.pkgs["context"], `.Context, in *`, inputType, `) (*`, outputType, `, error) {`)
		t.P(`  return s.On`, methName, `(ctx, in)`)
		t.P(`}`)
		t.P()
	}
}

func (t *twirp) generateNoop(file *descriptor.FileDescriptorProto, service *descriptor.ServiceDescriptorProto) {
	structName := "Noop" + serviceName(service)

	t.P(`type `, structName, ` struct {}`)
	t.P()

	for _, method := range service.Method {
		methName := methodName(method)
		inputType := t.goTypeName(method.GetInputType())
		outputType := t.goTypeName(method.GetOutputType())

		t.P(`func (s *`, structName, `) `, methName, `(ctx `, t.pkgs["context"], `.Context, in *`, inputType, `) (*`, outputType, `, error) {`)
		t.P(`  return &`, outputType, `{}, nil`)
		t.P(`}`)
		t.P()
	}
}

// Given a protobuf name for a Message, return the Go name we will use for that
// type, including its package prefix.
func (t *twirp) goTypeName(protoName string) string {
	def := t.reg.MessageDefinition(protoName)
	if def == nil {
		gen.Fail("could not find message for", protoName)
	}

	var prefix string
	if pkg := t.goPackageName(def.File); pkg != t.genPkgName {
		prefix = pkg + "."
	}

	var name string
	for _, parent := range def.Lineage() {
		name += parent.Descriptor.GetName() + "_"
	}
	name += def.Descriptor.GetName()
	return prefix + name
}

func (t *twirp) goPackageName(file *descriptor.FileDescriptorProto) string {
	return t.fileToGoPackageName[file]
}

func (t *twirp) formattedOutput() string {
	// Reformat generated code.
	fset := token.NewFileSet()
	raw := t.output.Bytes()
	ast, err := parser.ParseFile(fset, "", raw, parser.ParseComments)
	if err != nil {
		// Print out the bad code with line numbers.
		// This should never happen in practice, but it can while changing generated code,
		// so consider this a debugging aid.
		var src bytes.Buffer
		s := bufio.NewScanner(bytes.NewReader(raw))
		for line := 1; s.Scan(); line++ {
			fmt.Fprintf(&src, "%5d\t%s\n", line, s.Bytes())
		}
		gen.Fail("bad Go source code was generated:", err.Error(), "\n"+src.String())
	}

	out := bytes.NewBuffer(nil)
	err = (&printer.Config{Mode: printer.TabIndent | printer.UseSpaces, Tabwidth: 8}).Fprint(out, fset, ast)
	if err != nil {
		gen.Fail("generated Go source code could not be reformatted:", err.Error())
	}

	return out.String()
}

func serviceName(service *descriptor.ServiceDescriptorProto) string {
	return stringutils.CamelCase(service.GetName())
}

func methodName(method *descriptor.MethodDescriptorProto) string {
	return stringutils.CamelCase(method.GetName())
}

func fileDescSliceContains(slice []*descriptor.FileDescriptorProto, f *descriptor.FileDescriptorProto) bool {
	for _, sf := range slice {
		if f == sf {
			return true
		}
	}
	return false
}
