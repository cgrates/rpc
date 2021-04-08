// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package birpc

import (
	"context"
	"errors"
	"go/token"
	"log"
	"reflect"
	"strings"
	"sync"

	"github.com/cgrates/rpc/birpc/internal/svc"
)

// Precompute the reflect type for error. Can't use error directly
// because Typeof takes an empty interface value. This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()
var typeOfCtx = reflect.TypeOf((*context.Context)(nil)).Elem()
var typeOfClnt = reflect.TypeOf((*ClientConnector)(nil)).Elem()

// NewService creates a new service
func NewService(rcvr interface{}, name string, useName bool) (s *Service, err error) {
	s = new(Service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if useName {
		sname = name
	}
	if sname == "" {
		return nil, errors.New("rpc.Register: no service name for type " + s.typ.String())
	}
	if !token.IsExported(sname) && !useName {
		return nil, errors.New("rpc.Register: type " + sname + " is not exported")
	}
	s.name = sname

	// Install the methods
	s.method = suitableMethods(s.typ, true)

	if len(s.method) == 0 {
		var str string

		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(reflect.PtrTo(s.typ), false)
		if len(method) != 0 {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			str = "rpc.Register: type " + sname + " has no exported methods of suitable type"
		}
		return nil, errors.New(str)
	}
	return
}

type methodType struct {
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
}

type Service struct {
	name   string                 // name of service
	rcvr   reflect.Value          // receiver of methods for the service
	typ    reflect.Type           // type of the receiver
	method map[string]*methodType // registered methods
}

func (s *Service) call(server *basicServer, sending *sync.Mutex, pending *svc.Pending, wg *sync.WaitGroup, mtype *methodType, req *Request, argv, replyv reflect.Value, codec writeServerCodec, clnt reflect.Value) {
	if wg != nil {
		defer wg.Done()
	}
	// _goRPC_ service calls require internal state.
	if s.name == "_goRPC_" {
		switch v := argv.Interface().(type) {
		case *svc.CancelArgs:
			v.SetPending(pending)
		}
	}
	ctx := pending.Start(req.Seq)
	defer pending.Cancel(req.Seq)
	function := mtype.method.Func
	// Invoke the method, providing a new value for the reply.
	returnValues := function.Call([]reflect.Value{s.rcvr, reflect.ValueOf(ctx), clnt, argv, replyv})
	// The return value for the method is an error.
	errInter := returnValues[0].Interface()
	errmsg := ""
	if errInter != nil {
		errmsg = errInter.(error).Error()
	}
	server.sendResponse(sending, req, replyv.Interface(), codec, errmsg)
	server.freeRequest(req)
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return token.IsExported(t.Name()) || t.PkgPath() == ""
}

// suitableMethods returns suitable Rpc methods of typ, it will report
// error using log if reportErr is true.
func suitableMethods(typ reflect.Type, reportErr bool) map[string]*methodType {
	methods := make(map[string]*methodType)
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name
		// Method must be exported.
		if method.PkgPath != "" {
			continue
		}
		// Method needs four ins: receiver, ctx, client, *args, *reply.
		if mtype.NumIn() != 5 {
			if reportErr {
				log.Printf("rpc.Register: method %q has %d input parameters; needs exactly five\n", mname, mtype.NumIn())
			}
			continue
		}
		// First arg must be context.Context
		if ctxType := mtype.In(1); ctxType != typeOfCtx {
			if reportErr {
				log.Printf("rpc.Register: return type of method %q is %q, must be error\n", mname, ctxType)
			}
			continue
		}
		// First arg must be rpc.ClientConnection
		if ctxType := mtype.In(2); ctxType != typeOfClnt {
			if reportErr {
				log.Printf("rpc.Register: return type of method %q is %q, must be error\n", mname, typeOfClnt)
			}
			continue
		}
		// Second arg need not be a pointer.
		argType := mtype.In(3)
		if !isExportedOrBuiltinType(argType) {
			if reportErr {
				log.Printf("rpc.Register: argument type of method %q is not exported: %q\n", mname, argType)
			}
			continue
		}
		// Third arg must be a pointer.
		replyType := mtype.In(4)
		if replyType.Kind() != reflect.Ptr {
			if reportErr {
				log.Printf("rpc.Register: reply type of method %q is not a pointer: %q\n", mname, replyType)
			}
			continue
		}
		// Reply type must be exported.
		if !isExportedOrBuiltinType(replyType) {
			if reportErr {
				log.Printf("rpc.Register: reply type of method %q is not exported: %q\n", mname, replyType)
			}
			continue
		}
		// Method needs one out.
		if mtype.NumOut() != 1 {
			if reportErr {
				log.Printf("rpc.Register: method %q has %d output parameters; needs exactly one\n", mname, mtype.NumOut())
			}
			continue
		}
		// The return type of the method must be error.
		if returnType := mtype.Out(0); returnType != typeOfError {
			if reportErr {
				log.Printf("rpc.Register: return type of method %q is %q, must be error\n", mname, returnType)
			}
			continue
		}
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

func (s *Service) Call(ctx context.Context, clnt ClientConnector, serviceMethod string, args, rply interface{}) (err error) {
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		return errors.New("rpc: service/method request ill-formed: " + serviceMethod)
	}
	methodName := serviceMethod[dot+1:]

	// Look up the request.
	if serviceName := serviceMethod[:dot]; s.name != serviceName {
		return errors.New("rpc: can't find service " + serviceMethod)
	}
	mtype := s.method[methodName]
	function := mtype.method.Func
	// Invoke the method, providing a new value for the reply.
	returnValues := function.Call([]reflect.Value{s.rcvr, reflect.ValueOf(ctx), reflect.ValueOf(clnt), reflect.ValueOf(args), reflect.ValueOf(rply)})
	// The return value for the method is an error.
	return returnValues[0].Interface().(error)
}

func getArgv(mtype *methodType) (argv reflect.Value, argIsValue bool) {
	if mtype.ArgType.Kind() == reflect.Ptr {
		argv = reflect.New(mtype.ArgType.Elem())
	} else {
		argv = reflect.New(mtype.ArgType)
		argIsValue = true
	}
	return
}

func getReplyv(mtype *methodType) (replyv reflect.Value) {
	replyv = reflect.New(mtype.ReplyType.Elem())

	switch mtype.ReplyType.Elem().Kind() {
	case reflect.Map:
		replyv.Elem().Set(reflect.MakeMap(mtype.ReplyType.Elem()))
	case reflect.Slice:
		replyv.Elem().Set(reflect.MakeSlice(mtype.ReplyType.Elem(), 0, 0))
	}
	return
}