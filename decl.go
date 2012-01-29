/*
Copyright (c) 2011, 2012 Andrew Wilkins <axwalk@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package main

import (
    "fmt"
    "go/ast"
    "go/token"
    "go/types"
    "reflect"
    "strconv"
    "github.com/axw/gollvm/llvm"
)

func (self *Visitor) VisitFuncProtoDecl(f *ast.FuncDecl) Value {
    fn_type := self.VisitFuncType(f.Type)
    fn_name := f.Name.String()
    var fn llvm.Value
    if self.modulename == "main" && fn_name == "main" {
        fn = llvm.AddFunction(self.module, "main", fn_type.LLVMType())
        fn.SetLinkage(llvm.ExternalLinkage)
    } else {
/* TODO move this to LLVMType()
        if fn_name == "init" {
            // Make init functions anonymous
            fn_name = ""
        } else if f.Recv != nil {
            return_type := llvm.VoidType() //fn_type.ReturnType() TODO
            param_types := make([]llvm.Type, 0) //fn_type.ParamTypes()
            isvararg := fn_type.IsVariadic

            // Add receiver as the first parameter type.
            recv_type := self.GetType(f.Recv.List[0].Type)
            if recv_type != nil {
                param_types = append(param_types, recv_type.LLVMType())
            }

            for _, param := range fn_type.Params {
                param_type := param.Type.(Type)
                param_types = append(param_types, param_type.LLVMType())
            }

            fn_type = llvm.FunctionType(return_type, param_types, isvararg)
        }
        fn = llvm.AddFunction(self.module, fn_name, fn_type.LLVMType())
        //fn.SetFunctionCallConv(llvm.FastCallConv) // XXX
*/
    }
    if f.Name.Obj != nil {
        f.Name.Obj.Data = fn
        f.Name.Obj.Type = fn_type
    }
    return NewLLVMValue(self.builder, fn)
}

func (self *Visitor) VisitFuncDecl(f *ast.FuncDecl) Value {
    name := f.Name.String()
    obj := f.Name.Obj

    var fn Value
    if obj != nil && obj.Data != nil {
        var ok bool
        fn, ok = (obj.Data).(Value)
        if !ok {panic("obj.Data is not nil and is not a llvm.Value")}
    } else {
        fn = self.VisitFuncProtoDecl(f)
    }
    fn_type := obj.Type.(*Func)
    llvm_fn := fn.LLVMValue()

    // Bind receiver, arguments and return values to their identifiers/objects.
    param_i := 0
    if f.Recv != nil {
        param_0 := llvm_fn.Param(0)
        f.Recv.List[0].Names[0].Obj.Data = NewLLVMValue(self.builder, param_0)
        param_i++
    }
    if param_i < len(fn_type.Params) {
        for _, param := range fn_type.Params {
            name := param.Name
            value := llvm_fn.Param(param_i)
            value.SetName(name)
            if name != "_" {param.Data = NewLLVMValue(self.builder, value)}
            param_i++
        }
    }

    entry := llvm.AddBasicBlock(llvm_fn, "entry")
    self.builder.SetInsertPointAtEnd(entry)

    self.functions = append(self.functions, fn)
    if f.Body != nil {self.VisitBlockStmt(f.Body)}
    self.functions = self.functions[0:len(self.functions)-1]

    last_block := llvm_fn.LastBasicBlock()
    lasti := last_block.LastInstruction()
    if lasti.IsNil() || lasti.InstructionOpcode() != llvm.Ret {
        // Assume nil return type, AST should be checked first.
        self.builder.CreateRetVoid()
    }

    // Is it an 'init' function? Then record it.
    if name == "init" {
        self.initfuncs = append(self.initfuncs, fn)
    } else {
        if obj != nil {
            obj.Data = fn
        }
    }
    return fn
}

func (self *Visitor) VisitValueSpec(valspec *ast.ValueSpec, isconst bool) {
    var value_type Type
    if valspec.Type != nil {
        value_type = self.GetType(valspec.Type)
    }

    var iota_obj *ast.Object = types.Universe.Lookup("iota")
    defer func(data interface{}) {
        iota_obj.Data = data
    }(iota_obj.Data)

    for i, name_ := range valspec.Names {
        // We may resolve constants in the process of resolving others.
        obj := name_.Obj
        if _, isvalue := (obj.Data).(Value); isvalue {continue}

        // Set iota if necessary.
        if isconst {
            if iota_, isint := (name_.Obj.Data).(int); isint {
                iota_value := llvm.ConstInt(
                    llvm.Int32Type(), uint64(iota_), false)
                iota_obj.Data = iota_value

                // Con objects with an iota have an embedded ValueSpec
                // in the Decl field. We'll just pull it out and use it
                // for evaluating the expression below.
                valspec, _ = (name_.Obj.Decl).(*ast.ValueSpec)
            }
        }

        // Expression may have side-effects, so compute it regardless of
        // whether it'll be assigned to a name.
        var value Value
        if valspec.Values != nil && i < len(valspec.Values) &&
           valspec.Values[i] != nil {
            value = self.VisitExpr(valspec.Values[i])
        }

        // TODO (from language spec)
        // If the type is absent and the corresponding expression evaluates to
        // an untyped constant, the type of the declared variable is bool, int,
        // float64, or string respectively, depending on whether the value is
        // a boolean, integer, floating-point, or string constant.
        if value_type == nil {
            value_type = value.Type()
        }

        ispackagelevel := len(self.functions) == 0
        name := name_.String()
        if name != "_" {
            if !ispackagelevel {
                // The variable should be allocated on the stack if it's
                // declared inside a function.
                init_ := value
                var llvm_init llvm.Value
                stack_value := self.builder.CreateAlloca(
                    value_type.LLVMType(), name)
                if init_ == nil {
                    // If no initialiser was specified, set it to the
                    // zero value.
                    llvm_init = llvm.ConstNull(value_type.LLVMType())
                } else {
                    llvm_init = init_.Convert(value_type).LLVMValue()
                }
                self.builder.CreateStore(llvm_init, stack_value)
                //setindirect(value) TODO
                value = NewLLVMValue(self.builder, stack_value)
            } else {
                exported := name_.IsExported()

                // If it's a non-string constant, assign it to .
                var constprim bool
                if _, isconstval := value.(ConstValue); isconstval {
                    if basic, isbasic := (value.Type()).(*Basic); isbasic {
                        constprim = basic.Kind != String
                    }
                }

                if isconst && constprim && !exported {
                    // Not exported, and it's a constant. Let's forego creating
                    // the internal constant and just pass around the
                    // llvm.Value.
                    obj.Kind = ast.Con // Change to constant
                    obj.Data = value.Convert(value_type)
                } else {
                    init_ := value
                    global_value := llvm.AddGlobal(
                        self.module, value_type.LLVMType(), name)
                    if init_ != nil {
                        init_ = init_.Convert(value_type)
                        global_value.SetInitializer(init_.LLVMValue())
                    }
                    if isconst {
                        global_value.SetGlobalConstant(true)
                    }
                    if !exported {
                        global_value.SetLinkage(llvm.InternalLinkage)
                    }

                    value = NewLLVMValue(self.builder, global_value)
                    obj.Data = value
    
                    // If it's not an array, we should mark the value as being
                    // "indirect" (i.e. it must be loaded before use).
                    // TODO
                    //if value_type.TypeKind() != llvm.ArrayTypeKind {
                    //    setindirect(value)
                    //}
                }
            }
            obj.Data = value
        }
    }
}

func (self *Visitor) VisitTypeSpec(spec *ast.TypeSpec) {
    obj := spec.Name.Obj
    type_, istype := (obj.Data).(Type)
    if !istype {
        type_ = self.GetType(spec.Type)
        obj.Data = type_
    }
}

func (self *Visitor) VisitImportSpec(spec *ast.ImportSpec) {
    // TODO we will need to create our own Importer.
    path, err := strconv.Unquote(spec.Path.Value)
    if err != nil {panic(err)}
    pkg, err := types.GcImporter(self.imports, path)
    if err != nil {panic(err)}

    // TODO handle spec.Name (local package name), if not nil

    // Insert the package object into the scope.
    self.filescope.Outer.Insert(pkg)
}

func (self *Visitor) VisitGenDecl(decl *ast.GenDecl) {
    switch decl.Tok {
    case token.IMPORT:
        for _, spec := range decl.Specs {
            importspec, _ := spec.(*ast.ImportSpec)
            self.VisitImportSpec(importspec)
        }
    case token.TYPE:
        for _, spec := range decl.Specs {
            typespec, _ := spec.(*ast.TypeSpec)
            self.VisitTypeSpec(typespec)
        }
    case token.CONST:
        for _, spec := range decl.Specs {
            valspec, _ := spec.(*ast.ValueSpec)
            self.VisitValueSpec(valspec, true)
        }
    case token.VAR:
        for _, spec := range decl.Specs {
            valspec, _ := spec.(*ast.ValueSpec)
            self.VisitValueSpec(valspec, false)
        }
    }
}

func (self *Visitor) VisitDecl(decl ast.Decl) Value {
    switch x := decl.(type) {
    case *ast.FuncDecl: return self.VisitFuncDecl(x)
    case *ast.GenDecl: {
        self.VisitGenDecl(x)
        return nil
    }
    }
    panic(fmt.Sprintf("Unhandled decl (%s) at %s\n",
                      reflect.TypeOf(decl),
                      self.fileset.Position(decl.Pos())))
}

// vim: set ft=go :
