package compiler

// This file transforms interface-related instructions (*ssa.MakeInterface,
// *ssa.TypeAssert, calls on interface types) to an intermediate IR form, to be
// lowered to the final form by the interface lowering pass. See
// interface-lowering.go for more details.

import (
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ssa"
	"tinygo.org/x/go-llvm"
)

var basicTypes = [...]uint8{
	types.Bool:          1,
	types.Int:           2,
	types.Int8:          3,
	types.Int16:         4,
	types.Int32:         5,
	types.Int64:         6,
	types.Uint:          7,
	types.Uint8:         8,
	types.Uint16:        9,
	types.Uint32:        10,
	types.Uint64:        11,
	types.Uintptr:       12,
	types.Float32:       13,
	types.Float64:       14,
	types.Complex64:     15,
	types.Complex128:    16,
	types.String:        17,
	types.UnsafePointer: 18,
}

// createMakeInterface emits the LLVM IR for the *ssa.MakeInterface instruction.
// It tries to put the type in the interface value, but if that's not possible,
// it will do an allocation of the right size and put that in the interface
// value field.
//
// An interface value is a {typecode, value} tuple named runtime._interface.
func (b *builder) createMakeInterface(val llvm.Value, typ types.Type, pos token.Pos) llvm.Value {
	itfValue := b.emitPointerPack([]llvm.Value{val})
	itfType := b.getTypeCode(typ)
	itf := llvm.Undef(b.getLLVMRuntimeType("_interface"))
	itf = b.CreateInsertValue(itf, itfType, 0, "")
	itf = b.CreateInsertValue(itf, itfValue, 1, "")
	return itf
}

// extractValueFromInterface extract the value from an interface value
// (runtime._interface) under the assumption that it is of the type given in
// llvmType. The behavior is undefied if the interface is nil or llvmType
// doesn't match the underlying type of the interface.
func (b *builder) extractValueFromInterface(itf llvm.Value, llvmType llvm.Type) llvm.Value {
	valuePtr := b.CreateExtractValue(itf, 1, "typeassert.value.ptr")
	return b.emitPointerUnpack(valuePtr, []llvm.Type{llvmType})[0]
}

// getTypeCode returns a reference to a type code.
// It returns a pointer to an external global which should be replaced with the
// real type in the interface lowering pass.
func (c *compilerContext) getTypeCode(typ types.Type) llvm.Value {
	ms := c.program.MethodSets.MethodSet(typ)
	hasMethodSet := ms.Len() != 0
	if _, ok := typ.Underlying().(*types.Interface); ok {
		hasMethodSet = false
	}
	globalName := "reflect/types.type:" + getTypeCodeName(typ)
	global := c.mod.NamedGlobal(globalName)
	if global.IsNil() {
		var typeFields []llvm.Value
		// Field values depend on the type:
		// basic:
		//   kind, ptrTo
		// named:
		//   kind, ptrTo, underlying
		// chan, slice:
		//   kind, ptrTo, elementType
		// pointer:
		//   kind, elementType
		// array:
		//   kind, ptrTo, elementType, length
		// map:
		//   kind, ptrTo, elemType, keyType
		// struct:
		//   kind, numFields, ptrTo, fields...
		// interface:
		//   kind, ptrTo, numMethods, methods...
		// signature:
		//   kind, [todo]
		typeFieldTypes := []llvm.Type{c.ctx.Int8Type()}
		switch typ := typ.(type) {
		case *types.Basic:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType)
		case *types.Named, *types.Chan, *types.Slice:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType, c.i8ptrType)
		case *types.Pointer:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType)
		case *types.Array:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType, c.i8ptrType, c.uintptrType)
		case *types.Map:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType, c.i8ptrType, c.i8ptrType)
		case *types.Struct:
			typeFieldTypes = append(typeFieldTypes, c.ctx.Int16Type(), c.i8ptrType, llvm.ArrayType(c.getLLVMRuntimeType("structField"), typ.NumFields()))
		case *types.Interface:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType)
			// TODO: methods
		case *types.Signature:
			typeFieldTypes = append(typeFieldTypes, c.i8ptrType)
			// TODO: signature params and return values
		}
		if hasMethodSet {
			typeFieldTypes = append([]llvm.Type{c.i8ptrType}, typeFieldTypes...)
		}
		global = llvm.AddGlobal(c.mod, c.ctx.StructType(typeFieldTypes, false), globalName)
		metabyte := getTypeKind(typ)
		switch typ := typ.(type) {
		case *types.Basic:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
		case *types.Named:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Underlying()),      // underlying
			}
			metabyte |= 1 << 5 // "named" flag
		case *types.Chan:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Elem()),            // elementType
			}
		case *types.Slice:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Elem()),            // elementType
			}
		case *types.Pointer:
			typeFields = []llvm.Value{c.getTypeCode(typ.Elem())}
		case *types.Array:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)),                   // ptrTo
				c.getTypeCode(typ.Elem()),                              // elementType
				llvm.ConstInt(c.uintptrType, uint64(typ.Len()), false), // length
			}
		case *types.Map:
			typeFields = []llvm.Value{
				c.getTypeCode(types.NewPointer(typ)), // ptrTo
				c.getTypeCode(typ.Elem()),            // elementType
				c.getTypeCode(typ.Key()),             // keyType
			}
		case *types.Struct:
			typeFields = []llvm.Value{
				llvm.ConstInt(c.ctx.Int16Type(), uint64(typ.NumFields()), false), // numFields
				c.getTypeCode(types.NewPointer(typ)),                             // ptrTo
			}
			structFieldType := c.getLLVMRuntimeType("structField")
			var fields []llvm.Value
			for i := 0; i < typ.NumFields(); i++ {
				field := typ.Field(i)
				var flags uint8
				if field.Anonymous() {
					flags |= 1
				}
				if typ.Tag(i) != "" {
					flags |= 2
				}
				if token.IsExported(field.Name()) {
					flags |= 4
				}
				data := string(flags) + field.Name()
				if typ.Tag(i) != "" {
					data += "\x00" + typ.Tag(i)
				}
				dataInitializer := c.ctx.ConstString(data, true)
				dataGlobal := llvm.AddGlobal(c.mod, dataInitializer.Type(), globalName+"."+field.Name())
				dataGlobal.SetInitializer(dataInitializer)
				dataGlobal.SetAlignment(1)
				dataGlobal.SetUnnamedAddr(true)
				dataGlobal.SetLinkage(llvm.InternalLinkage)
				dataGlobal.SetGlobalConstant(true)
				fieldType := c.getTypeCode(field.Type())
				fields = append(fields, llvm.ConstNamedStruct(structFieldType, []llvm.Value{
					fieldType,
					llvm.ConstGEP(dataGlobal, []llvm.Value{
						llvm.ConstInt(c.ctx.Int32Type(), 0, false),
						llvm.ConstInt(c.ctx.Int32Type(), 0, false),
					}),
				}))
			}
			typeFields = append(typeFields, llvm.ConstArray(structFieldType, fields))
		case *types.Interface:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
			// TODO: methods
		case *types.Signature:
			typeFields = []llvm.Value{c.getTypeCode(types.NewPointer(typ))}
			// TODO: params, return values, etc
		}
		// Prepend metadata byte.
		typeFields = append([]llvm.Value{
			llvm.ConstInt(c.ctx.Int8Type(), uint64(metabyte), false),
		}, typeFields...)
		if hasMethodSet {
			typeFields = append([]llvm.Value{
				llvm.ConstBitCast(c.getTypeMethodSet(typ), c.i8ptrType),
			}, typeFields...)
		}
		globalValue := c.ctx.ConstStruct(typeFields, false)
		global.SetInitializer(globalValue)
		global.SetLinkage(llvm.LinkOnceODRLinkage)
		global.SetGlobalConstant(true)
		global.SetAlignment(int(c.targetData.TypeAllocSize(c.i8ptrType)))
	}
	offset := uint64(0)
	if hasMethodSet {
		offset = 1
	}
	return llvm.ConstGEP(global, []llvm.Value{
		llvm.ConstInt(llvm.Int32Type(), 0, false),
		llvm.ConstInt(llvm.Int32Type(), offset, false),
	})
}

func getTypeKind(t types.Type) uint8 {
	switch t := t.Underlying().(type) {
	case *types.Basic:
		return basicTypes[t.Kind()]
	case *types.Chan:
		return 19
	case *types.Interface:
		return 20
	case *types.Pointer:
		return 21
	case *types.Slice:
		return 22
	case *types.Array:
		return 23
	case *types.Signature:
		return 24
	case *types.Map:
		return 25
	case *types.Struct:
		return 26
	default:
		panic("unknown type")
	}
}

// getTypeCodeName returns a name for this type that can be used in the
// interface lowering pass to assign type codes as expected by the reflect
// package. See getTypeCodeNum.
func getTypeCodeName(t types.Type) string {
	switch t := t.(type) {
	case *types.Named:
		return "named:" + t.String()
	case *types.Array:
		return "array:" + strconv.FormatInt(t.Len(), 10) + ":" + getTypeCodeName(t.Elem())
	case *types.Basic:
		var kind string
		switch t.Kind() {
		case types.Bool:
			kind = "bool"
		case types.Int:
			kind = "int"
		case types.Int8:
			kind = "int8"
		case types.Int16:
			kind = "int16"
		case types.Int32:
			kind = "int32"
		case types.Int64:
			kind = "int64"
		case types.Uint:
			kind = "uint"
		case types.Uint8:
			kind = "uint8"
		case types.Uint16:
			kind = "uint16"
		case types.Uint32:
			kind = "uint32"
		case types.Uint64:
			kind = "uint64"
		case types.Uintptr:
			kind = "uintptr"
		case types.Float32:
			kind = "float32"
		case types.Float64:
			kind = "float64"
		case types.Complex64:
			kind = "complex64"
		case types.Complex128:
			kind = "complex128"
		case types.String:
			kind = "string"
		case types.UnsafePointer:
			kind = "unsafeptr"
		default:
			panic("unknown basic type: " + t.Name())
		}
		return "basic:" + kind
	case *types.Chan:
		return "chan:" + getTypeCodeName(t.Elem())
	case *types.Interface:
		methods := make([]string, t.NumMethods())
		for i := 0; i < t.NumMethods(); i++ {
			name := t.Method(i).Name()
			if !token.IsExported(name) {
				name = t.Method(i).Pkg().Path() + "." + name
			}
			methods[i] = name + ":" + getTypeCodeName(t.Method(i).Type())
		}
		return "interface:" + "{" + strings.Join(methods, ",") + "}"
	case *types.Map:
		keyType := getTypeCodeName(t.Key())
		elemType := getTypeCodeName(t.Elem())
		return "map:" + "{" + keyType + "," + elemType + "}"
	case *types.Pointer:
		return "pointer:" + getTypeCodeName(t.Elem())
	case *types.Signature:
		params := make([]string, t.Params().Len())
		for i := 0; i < t.Params().Len(); i++ {
			params[i] = getTypeCodeName(t.Params().At(i).Type())
		}
		results := make([]string, t.Results().Len())
		for i := 0; i < t.Results().Len(); i++ {
			results[i] = getTypeCodeName(t.Results().At(i).Type())
		}
		return "func:" + "{" + strings.Join(params, ",") + "}{" + strings.Join(results, ",") + "}"
	case *types.Slice:
		return "slice:" + getTypeCodeName(t.Elem())
	case *types.Struct:
		elems := make([]string, t.NumFields())
		for i := 0; i < t.NumFields(); i++ {
			embedded := ""
			if t.Field(i).Embedded() {
				embedded = "#"
			}
			elems[i] = embedded + t.Field(i).Name() + ":" + getTypeCodeName(t.Field(i).Type())
			if t.Tag(i) != "" {
				elems[i] += "`" + t.Tag(i) + "`"
			}
		}
		return "struct:" + "{" + strings.Join(elems, ",") + "}"
	default:
		panic("unknown type: " + t.String())
	}
}

// getTypeMethodSet returns a reference (GEP) to a global method set. This
// method set should be unreferenced after the interface lowering pass.
func (c *compilerContext) getTypeMethodSet(typ types.Type) llvm.Value {
	globalName := typ.String() + "$methodset"
	global := c.mod.NamedGlobal(globalName)
	if global.IsNil() {
		ms := c.program.MethodSets.MethodSet(typ)

		// Create method set.
		var signatures, wrappers []llvm.Value
		for i := 0; i < ms.Len(); i++ {
			method := ms.At(i)
			signatureGlobal := c.getMethodSignature(method.Obj().(*types.Func))
			signatures = append(signatures, signatureGlobal)
			fn := c.program.MethodValue(method)
			llvmFn := c.getFunction(fn)
			if llvmFn.IsNil() {
				// compiler error, so panic
				panic("cannot find function: " + c.getFunctionInfo(fn).linkName)
			}
			wrapper := c.getInterfaceInvokeWrapper(fn, llvmFn)
			wrappers = append(wrappers, wrapper)
		}

		// Construct global value.
		globalValue := c.ctx.ConstStruct([]llvm.Value{
			llvm.ConstInt(c.uintptrType, uint64(ms.Len()), false),
			llvm.ConstArray(c.i8ptrType, signatures),
			c.ctx.ConstStruct(wrappers, false),
		}, false)
		global = llvm.AddGlobal(c.mod, globalValue.Type(), globalName)
		global.SetInitializer(globalValue)
		global.SetGlobalConstant(true)
		global.SetUnnamedAddr(true)
		global.SetLinkage(llvm.LinkOnceODRLinkage)
	}
	return global
}

// getInterfaceMethodSet returns a global variable with the method set of the
// given named interface type. This method set is used by the interface lowering
// pass.
func (c *compilerContext) getInterfaceMethodSet(typ types.Type) llvm.Value {
	name := typ.String()
	if _, ok := typ.(*types.Named); !ok {
		// Anonymous interface.
		name = "reflect/types.interface:" + name
	}
	global := c.mod.NamedGlobal(name + "$interface")
	zero := llvm.ConstInt(c.ctx.Int32Type(), 0, false)
	if !global.IsNil() {
		// method set already exist, return it
		return llvm.ConstGEP(global, []llvm.Value{zero, zero})
	}

	// Every method is a *i8 reference indicating the signature of this method.
	methods := make([]llvm.Value, typ.Underlying().(*types.Interface).NumMethods())
	for i := range methods {
		method := typ.Underlying().(*types.Interface).Method(i)
		methods[i] = c.getMethodSignature(method)
	}

	value := llvm.ConstArray(c.i8ptrType, methods)
	global = llvm.AddGlobal(c.mod, value.Type(), name+"$interface")
	global.SetInitializer(value)
	global.SetGlobalConstant(true)
	global.SetLinkage(llvm.LinkOnceODRLinkage)
	return llvm.ConstGEP(global, []llvm.Value{zero, zero})
}

// getMethodSignatureName returns a unique name (that can be used as the name of
// a global) for the given method.
func (c *compilerContext) getMethodSignatureName(method *types.Func) string {
	signature := methodSignature(method)
	var globalName string
	if token.IsExported(method.Name()) {
		globalName = "reflect/methods." + signature
	} else {
		globalName = method.Type().(*types.Signature).Recv().Pkg().Path() + ".$methods." + signature
	}
	return globalName
}

// getMethodSignature returns a global variable which is a reference to an
// external *i8 indicating the indicating the signature of this method. It is
// used during the interface lowering pass.
func (c *compilerContext) getMethodSignature(method *types.Func) llvm.Value {
	globalName := c.getMethodSignatureName(method)
	signatureGlobal := c.mod.NamedGlobal(globalName)
	if signatureGlobal.IsNil() {
		// TODO: put something useful in these globals, such as the method
		// signature. Useful to one day implement reflect.Value.Method(n).
		signatureGlobal = llvm.AddGlobal(c.mod, c.ctx.Int8Type(), globalName)
		signatureGlobal.SetInitializer(llvm.ConstInt(c.ctx.Int8Type(), 0, false))
		signatureGlobal.SetLinkage(llvm.LinkOnceODRLinkage)
		signatureGlobal.SetGlobalConstant(true)
		signatureGlobal.SetAlignment(1)
	}
	return signatureGlobal
}

// createTypeAssert will emit the code for a typeassert, used in if statements
// and in type switches (Go SSA does not have type switches, only if/else
// chains). Note that even though the Go SSA does not contain type switches,
// LLVM will recognize the pattern and make it a real switch in many cases.
//
// Type asserts on concrete types are trivial: just compare type numbers. Type
// asserts on interfaces are more difficult, see the comments in the function.
func (b *builder) createTypeAssert(expr *ssa.TypeAssert) llvm.Value {
	itf := b.getValue(expr.X)
	assertedType := b.getLLVMType(expr.AssertedType)

	actualTypeNum := b.CreateExtractValue(itf, 0, "interface.type")
	commaOk := llvm.Value{}
	if _, ok := expr.AssertedType.Underlying().(*types.Interface); ok {
		// Type assert on interface type.
		// This is a call to an interface type assert function.
		// The interface lowering pass will define this function by filling it
		// with a type switch over all concrete types that implement this
		// interface, and returning whether it's one of the matched types.
		// This is very different from how interface asserts are implemented in
		// the main Go compiler, where the runtime checks whether the type
		// implements each method of the interface. See:
		// https://research.swtch.com/interfaces
		fn := b.getInterfaceImplementsFunc(expr.AssertedType)
		commaOk = b.CreateCall(fn, []llvm.Value{actualTypeNum}, "")

	} else {
		globalName := "reflect/types.typeid:" + getTypeCodeName(expr.AssertedType)
		assertedTypeCodeGlobal := b.mod.NamedGlobal(globalName)
		if assertedTypeCodeGlobal.IsNil() {
			// Create a new typecode global.
			assertedTypeCodeGlobal = llvm.AddGlobal(b.mod, b.ctx.Int8Type(), globalName)
			assertedTypeCodeGlobal.SetGlobalConstant(true)
		}
		// Type assert on concrete type.
		// Call runtime.typeAssert, which will be lowered to a simple icmp or
		// const false in the interface lowering pass.
		commaOk = b.createRuntimeCall("typeAssert", []llvm.Value{actualTypeNum, assertedTypeCodeGlobal}, "typecode")
	}

	// Add 2 new basic blocks (that should get optimized away): one for the
	// 'ok' case and one for all instructions following this type assert.
	// This is necessary because we need to insert the casted value or the
	// nil value based on whether the assert was successful. Casting before
	// this check tells LLVM that it can use this value and may
	// speculatively dereference pointers before the check. This can lead to
	// a miscompilation resulting in a segfault at runtime.
	// Additionally, this is even required by the Go spec: a failed
	// typeassert should return a zero value, not an incorrectly casted
	// value.

	prevBlock := b.GetInsertBlock()
	okBlock := b.ctx.AddBasicBlock(b.llvmFn, "typeassert.ok")
	nextBlock := b.ctx.AddBasicBlock(b.llvmFn, "typeassert.next")
	b.blockExits[b.currentBlock] = nextBlock // adjust outgoing block for phi nodes
	b.CreateCondBr(commaOk, okBlock, nextBlock)

	// Retrieve the value from the interface if the type assert was
	// successful.
	b.SetInsertPointAtEnd(okBlock)
	var valueOk llvm.Value
	if _, ok := expr.AssertedType.Underlying().(*types.Interface); ok {
		// Type assert on interface type. Easy: just return the same
		// interface value.
		valueOk = itf
	} else {
		// Type assert on concrete type. Extract the underlying type from
		// the interface (but only after checking it matches).
		valueOk = b.extractValueFromInterface(itf, assertedType)
	}
	b.CreateBr(nextBlock)

	// Continue after the if statement.
	b.SetInsertPointAtEnd(nextBlock)
	phi := b.CreatePHI(assertedType, "typeassert.value")
	phi.AddIncoming([]llvm.Value{llvm.ConstNull(assertedType), valueOk}, []llvm.BasicBlock{prevBlock, okBlock})

	if expr.CommaOk {
		tuple := b.ctx.ConstStruct([]llvm.Value{llvm.Undef(assertedType), llvm.Undef(b.ctx.Int1Type())}, false) // create empty tuple
		tuple = b.CreateInsertValue(tuple, phi, 0, "")                                                          // insert value
		tuple = b.CreateInsertValue(tuple, commaOk, 1, "")                                                      // insert 'comma ok' boolean
		return tuple
	} else {
		// This is kind of dirty as the branch above becomes mostly useless,
		// but hopefully this gets optimized away.
		b.createRuntimeCall("interfaceTypeAssert", []llvm.Value{commaOk}, "")
		return phi
	}
}

// getMethodsString returns a string to be used in the "tinygo-methods" string
// attribute for interface functions.
func (c *compilerContext) getMethodsString(itf *types.Interface) string {
	methods := make([]string, itf.NumMethods())
	for i := range methods {
		methods[i] = c.getMethodSignatureName(itf.Method(i))
	}
	return strings.Join(methods, "; ")
}

// getInterfaceImplementsfunc returns a declared function that works as a type
// switch. The interface lowering pass will define this function.
func (c *compilerContext) getInterfaceImplementsFunc(assertedType types.Type) llvm.Value {
	fnName := getTypeCodeName(assertedType.Underlying()) + ".$typeassert"
	llvmFn := c.mod.NamedFunction(fnName)
	if llvmFn.IsNil() {
		llvmFnType := llvm.FunctionType(c.ctx.Int1Type(), []llvm.Type{c.i8ptrType}, false)
		llvmFn = llvm.AddFunction(c.mod, fnName, llvmFnType)
		c.addStandardDeclaredAttributes(llvmFn)
		methods := c.getMethodsString(assertedType.Underlying().(*types.Interface))
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-methods", methods))
	}
	return llvmFn
}

// getInvokeFunction returns the thunk to call the given interface method. The
// thunk is declared, not defined: it will be defined by the interface lowering
// pass.
func (c *compilerContext) getInvokeFunction(instr *ssa.CallCommon) llvm.Value {
	fnName := getTypeCodeName(instr.Value.Type().Underlying()) + "." + instr.Method.Name() + "$invoke"
	llvmFn := c.mod.NamedFunction(fnName)
	if llvmFn.IsNil() {
		sig := instr.Method.Type().(*types.Signature)
		var paramTuple []*types.Var
		for i := 0; i < sig.Params().Len(); i++ {
			paramTuple = append(paramTuple, sig.Params().At(i))
		}
		paramTuple = append(paramTuple, types.NewVar(token.NoPos, nil, "$typecode", types.Typ[types.UnsafePointer]))
		llvmFnType := c.getRawFuncType(types.NewSignature(sig.Recv(), types.NewTuple(paramTuple...), sig.Results(), false)).ElementType()
		llvmFn = llvm.AddFunction(c.mod, fnName, llvmFnType)
		c.addStandardDeclaredAttributes(llvmFn)
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-invoke", c.getMethodSignatureName(instr.Method)))
		methods := c.getMethodsString(instr.Value.Type().Underlying().(*types.Interface))
		llvmFn.AddFunctionAttr(c.ctx.CreateStringAttribute("tinygo-methods", methods))
	}
	return llvmFn
}

// getInterfaceInvokeWrapper returns a wrapper for the given method so it can be
// invoked from an interface. The wrapper takes in a pointer to the underlying
// value, dereferences or unpacks it if necessary, and calls the real method.
// If the method to wrap has a pointer receiver, no wrapping is necessary and
// the function is returned directly.
func (c *compilerContext) getInterfaceInvokeWrapper(fn *ssa.Function, llvmFn llvm.Value) llvm.Value {
	wrapperName := llvmFn.Name() + "$invoke"
	wrapper := c.mod.NamedFunction(wrapperName)
	if !wrapper.IsNil() {
		// Wrapper already created. Return it directly.
		return wrapper
	}

	// Get the expanded receiver type.
	receiverType := c.getLLVMType(fn.Signature.Recv().Type())
	var expandedReceiverType []llvm.Type
	for _, info := range c.expandFormalParamType(receiverType, "", nil) {
		expandedReceiverType = append(expandedReceiverType, info.llvmType)
	}

	// Does this method even need any wrapping?
	if len(expandedReceiverType) == 1 && receiverType.TypeKind() == llvm.PointerTypeKind {
		// Nothing to wrap.
		// Casting a function signature to a different signature and calling it
		// with a receiver pointer bitcasted to *i8 (as done in calls on an
		// interface) is hopefully a safe (defined) operation.
		return llvmFn
	}

	// create wrapper function
	fnType := llvmFn.Type().ElementType()
	paramTypes := append([]llvm.Type{c.i8ptrType}, fnType.ParamTypes()[len(expandedReceiverType):]...)
	wrapFnType := llvm.FunctionType(fnType.ReturnType(), paramTypes, false)
	wrapper = llvm.AddFunction(c.mod, wrapperName, wrapFnType)
	c.addStandardAttributes(wrapper)

	wrapper.SetLinkage(llvm.LinkOnceODRLinkage)
	wrapper.SetUnnamedAddr(true)

	// Create a new builder just to create this wrapper.
	b := builder{
		compilerContext: c,
		Builder:         c.ctx.NewBuilder(),
	}
	defer b.Builder.Dispose()

	// add debug info if needed
	if c.Debug {
		pos := c.program.Fset.Position(fn.Pos())
		difunc := c.attachDebugInfoRaw(fn, wrapper, "$invoke", pos.Filename, pos.Line)
		b.SetCurrentDebugLocation(uint(pos.Line), uint(pos.Column), difunc, llvm.Metadata{})
	}

	// set up IR builder
	block := b.ctx.AddBasicBlock(wrapper, "entry")
	b.SetInsertPointAtEnd(block)

	receiverValue := b.emitPointerUnpack(wrapper.Param(0), []llvm.Type{receiverType})[0]
	params := append(b.expandFormalParam(receiverValue), wrapper.Params()[1:]...)
	if llvmFn.Type().ElementType().ReturnType().TypeKind() == llvm.VoidTypeKind {
		b.CreateCall(llvmFn, params, "")
		b.CreateRetVoid()
	} else {
		ret := b.CreateCall(llvmFn, params, "ret")
		b.CreateRet(ret)
	}

	return wrapper
}

// methodSignature creates a readable version of a method signature (including
// the function name, excluding the receiver name). This string is used
// internally to match interfaces and to call the correct method on an
// interface. Examples:
//
//     String() string
//     Read([]byte) (int, error)
func methodSignature(method *types.Func) string {
	return method.Name() + signature(method.Type().(*types.Signature))
}

// Make a readable version of a function (pointer) signature.
// Examples:
//
//     () string
//     (string, int) (int, error)
func signature(sig *types.Signature) string {
	s := ""
	if sig.Params().Len() == 0 {
		s += "()"
	} else {
		s += "("
		for i := 0; i < sig.Params().Len(); i++ {
			if i > 0 {
				s += ", "
			}
			s += sig.Params().At(i).Type().String()
		}
		s += ")"
	}
	if sig.Results().Len() == 0 {
		// keep as-is
	} else if sig.Results().Len() == 1 {
		s += " " + sig.Results().At(0).Type().String()
	} else {
		s += " ("
		for i := 0; i < sig.Results().Len(); i++ {
			if i > 0 {
				s += ", "
			}
			s += sig.Results().At(i).Type().String()
		}
		s += ")"
	}
	return s
}
