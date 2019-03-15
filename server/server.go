package server

import (
	"context"
	"errors"
	"github.com/megaredfan/rpc-demo/codec"
	"github.com/megaredfan/rpc-demo/protocol"
	"github.com/megaredfan/rpc-demo/transport"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type RPCServer interface {
	Register(rcvr interface{}, metaData map[string]string) error
	Serve(network string, addr string) error
	Services() []ServiceInfo
	Close() error
}

type ServiceInfo struct {
	Name    string   `json:"name"`
	Methods []string `json:"methods"`
}

type rpcServer struct {
	codec      codec.Codec
	serviceMap sync.Map
	tr         transport.ServerTransport
	mutex      sync.Mutex
	shutdown   bool

	Option Option
}

func (s *rpcServer) Services() []ServiceInfo {
	var srvs []ServiceInfo
	s.serviceMap.Range(func(key, value interface{}) bool {
		sname, ok := key.(string)
		if ok {
			srv, ok := value.(*service)
			if ok {
				var methodList []string
				//srv.methodsMu.RLock()
				srv.methods.Range(func(key, value interface{}) bool {
					if m, ok := key.(*methodType); ok {
						methodList = append(methodList, m.method.Name)
					}
					return true
				})
				//srv.methodsMu.RUnlock()
				srvs = append(srvs, ServiceInfo{sname, methodList})
			}
		}
		return true
	})
	return srvs
}

type methodType struct {
	method    reflect.Method
	ArgType   reflect.Type
	ReplyType reflect.Type
}

type service struct {
	name string
	typ  reflect.Type
	rcvr reflect.Value
	//methodsMu sync.RWMutex
	//methods map[string]*methodType
	methods sync.Map
}

func NewRPCServer(option Option) RPCServer {
	s := new(rpcServer)
	s.Option = option
	s.codec = codec.GetCodec(option.SerializeType)
	return s
}

func (s *rpcServer) Register(rcvr interface{}, metaData map[string]string) error {
	typ := reflect.TypeOf(rcvr)
	name := typ.Name()
	srv := new(service)
	srv.name = name
	srv.rcvr = reflect.ValueOf(rcvr)
	srv.typ = typ
	methods := suitableMethods(typ, true)

	if len(methods) == 0 {
		var errorStr string

		// 如果对应的类型没有任何符合规则的方法，扫描对应的指针类型
		// 也是从net.rpc包里抄来的
		method := suitableMethods(reflect.PtrTo(srv.typ), false)
		if len(method) != 0 {
			errorStr = "Register: type " + name + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			errorStr = "Register: type " + name + " has no exported methods of suitable type"
		}
		log.Println(errorStr)
		return errors.New(errorStr)
	}

	for k, v := range methods {
		srv.methods.Store(k, v)
	}
	//srv.methodsMu.Lock()
	//srv.methods = methods
	//srv.methodsMu.Unlock()

	if _, duplicate := s.serviceMap.LoadOrStore(name, srv); duplicate {
		return errors.New("rpc: service already defined: " + name)
	}
	return nil
}

// Precompute the reflect type for error. Can't use error directly
// because Typeof takes an empty interface value. This is annoying.
var typeOfError = reflect.TypeOf((*error)(nil)).Elem()
var typeOfContext = reflect.TypeOf((*context.Context)(nil)).Elem()

//过滤符合规则的方法，从net.rpc包抄的
func suitableMethods(typ reflect.Type, reportErr bool) map[string]*methodType {
	methods := make(map[string]*methodType)
	for m := 0; m < typ.NumMethod(); m++ {
		method := typ.Method(m)
		mtype := method.Type
		mname := method.Name

		// 方法必须是可导出的
		if method.PkgPath != "" {
			continue
		}
		// 需要有四个参数: receiver, Context, args, *reply.
		if mtype.NumIn() != 4 {
			if reportErr {
				log.Println("method", mname, "has wrong number of ins:", mtype.NumIn())
			}
			continue
		}
		// 第一个参数必须是context.Context
		ctxType := mtype.In(1)
		if !ctxType.Implements(typeOfContext) {
			if reportErr {
				log.Println("method", mname, " must use context.Context as the first parameter")
			}
			continue
		}

		// 第二个参数是arg
		argType := mtype.In(2)
		if !isExportedOrBuiltinType(argType) {
			if reportErr {
				log.Println(mname, "parameter type not exported:", argType)
			}
			continue
		}
		// 第三个参数是返回值，必须是指针类型的
		replyType := mtype.In(3)
		if replyType.Kind() != reflect.Ptr {
			if reportErr {
				log.Println("method", mname, "reply type not a pointer:", replyType)
			}
			continue
		}
		// 返回值的类型必须是可导出的
		if !isExportedOrBuiltinType(replyType) {
			if reportErr {
				log.Println("method", mname, "reply type not exported:", replyType)
			}
			continue
		}
		// 必须有一个返回值
		if mtype.NumOut() != 1 {
			if reportErr {
				log.Println("method", mname, "has wrong number of outs:", mtype.NumOut())
			}
			continue
		}
		// 返回值类型必须是error
		if returnType := mtype.Out(0); returnType != typeOfError {
			if reportErr {
				log.Println("method", mname, "returns", returnType.String(), "not error")
			}
			continue
		}
		methods[mname] = &methodType{method: method, ArgType: argType, ReplyType: replyType}
	}
	return methods
}

// Is this type exported or a builtin?
func isExportedOrBuiltinType(t reflect.Type) bool {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// PkgPath will be non-empty even for an exported type,
	// so we need to check the type name as well.
	return isExported(t.Name()) || t.PkgPath() == ""
}

// Is this an exported - upper case - name?
func isExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func (s *rpcServer) Serve(network string, addr string) error {
	s.tr = transport.NewServerTransport(s.Option.TransportType)
	err := s.tr.Listen(network, addr)
	if err != nil {
		log.Printf("server listen on %s@%s error:%s", network, addr, err)
		return err
	}
	for {
		conn, err := s.tr.Accept()
		if err != nil {
			log.Printf("server accept on %s@%s error:%s", network, addr, err)
			return err
		}
		go s.serveTransport(conn)
	}

}

func (s *rpcServer) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.shutdown = true

	err := s.tr.Close()

	s.serviceMap.Range(func(key, value interface{}) bool {
		s.serviceMap.Delete(key)
		return true
	})
	return err
}

type Request struct {
	Seq   uint32
	Reply interface{}
	Data  []byte
}

func (s *rpcServer) serveTransport(tr transport.Transport) {
	for {
		request, err := protocol.DecodeMessage(s.Option.ProtocolType, tr)

		if err != nil {
			if err == io.EOF {
				//log.Printf("client has closed this connection: %s", tr.RemoteAddr().String())
			} else if strings.Contains(err.Error(), "use of closed network connection") {
				//log.Printf("connection %s is closed", tr.RemoteAddr().String())
			} else {
				log.Printf("failed to read request: %v", err)
			}
			return
		}
		response := request.Clone()
		response.MessageType = protocol.MessageTypeResponse

		deadline, ok := response.Deadline()
		ctx := context.Background()

		if ok {
			ctx, _ = context.WithDeadline(ctx, deadline)
		}

		s.handleRequest(ctx, request, response, tr)
	}
}

func (s *rpcServer) handleRequest(ctx context.Context, request *protocol.Message, response *protocol.Message, tr transport.Transport) {
	sname := request.ServiceName
	mname := request.MethodName
	srvInterface, ok := s.serviceMap.Load(sname)
	if !ok {
		s.writeErrorResponse(response, tr, "can not find service")
		return
	}
	srv, ok := srvInterface.(*service)
	if !ok {
		s.writeErrorResponse(response, tr, "not *service type")
		return

	}

	//srv.methodsMu.RLock()
	//mtype, ok := srv.methods[mname]
	//srv.methodsMu.RUnlock()

	mtypInterface, ok := srv.methods.Load(mname)
	mtype, ok := mtypInterface.(*methodType)

	if !ok {
		s.writeErrorResponse(response, tr, "can not find method")
		return
	}
	argv := newValue(mtype.ArgType)
	replyv := newValue(mtype.ReplyType)

	actualCodec := s.codec
	if request.SerializeType != s.Option.SerializeType {
		actualCodec = codec.GetCodec(request.SerializeType)
	}
	err := actualCodec.Decode(request.Data, argv)
	if err != nil {
		s.writeErrorResponse(response, tr, "decode arg error:"+err.Error())
		return
	}

	var returns []reflect.Value
	if mtype.ArgType.Kind() != reflect.Ptr {
		returns = mtype.method.Func.Call([]reflect.Value{srv.rcvr,
			reflect.ValueOf(ctx),
			reflect.ValueOf(argv).Elem(),
			reflect.ValueOf(replyv)})
	} else {
		returns = mtype.method.Func.Call([]reflect.Value{srv.rcvr,
			reflect.ValueOf(ctx),
			reflect.ValueOf(argv),
			reflect.ValueOf(replyv)})
	}
	if len(returns) > 0 && returns[0].Interface() != nil {
		err = returns[0].Interface().(error)
		s.writeErrorResponse(response, tr, err.Error())
		return
	}

	responseData, err := actualCodec.Encode(replyv)
	if err != nil {
		s.writeErrorResponse(response, tr, err.Error())
		return
	}

	response.StatusCode = protocol.StatusOK
	response.Data = responseData

	deadline, ok := ctx.Deadline()
	if ok {
		if time.Now().Before(deadline) {
			_, err = tr.Write(protocol.EncodeMessage(s.Option.ProtocolType, response))
			if err != nil {
				log.Println("write response error:" + err.Error())
			}
		} else {
			log.Println("passed deadline, give up write response")
		}
	} else {
		_, err = tr.Write(protocol.EncodeMessage(s.Option.ProtocolType, response))
	}
}

func newValue(t reflect.Type) interface{} {
	if t.Kind() == reflect.Ptr {
		return reflect.New(t.Elem()).Interface()
	} else {
		return reflect.New(t).Interface()
	}
}

func (s *rpcServer) writeErrorResponse(response *protocol.Message, w io.Writer, err string) {
	response.Error = err
	//log.Println("writing error response:" + response.Error)
	response.StatusCode = protocol.StatusError
	response.Data = response.Data[:0]
	_, _ = w.Write(protocol.EncodeMessage(s.Option.ProtocolType, response))
}
