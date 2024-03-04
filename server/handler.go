package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"unicode"

	"github.com/0xPolygonHermez/zkevm-pool-manager/log"
)

const (
	requiredReturnParamsPerFn = 2
)

type endpointData struct {
	inNum int
	reqt  []reflect.Type
	fv    reflect.Value
	isDyn bool
}

func (f *endpointData) numParams() int {
	return f.inNum - 1
}

type handleRequest struct {
	Request
	wsConn      *concurrentWsConn
	HttpRequest *http.Request
}

// Handler manage services to handle pool manager requests
type Handler struct {
	endpoints   reflect.Value
	endpointMap map[string]*endpointData
}

func newJSONRpcHandler() *Handler {
	handler := &Handler{
		endpointMap: map[string]*endpointData{},
	}
	return handler
}

// Handle is the function that knows which and how a function should be executed when a pool manager request is received
func (h *Handler) Handle(req handleRequest) Response {
	log.Debugf("request method: %s, id:%d, params %v", req.Method, req.ID, string(req.Params))

	fd, err := h.getFnHandler(req.Request)
	if err != nil {
		return NewResponse(req.Request, nil, err)
	}

	inArgsOffset := 0
	inArgs := make([]reflect.Value, fd.inNum)
	inArgs[0] = h.endpoints

	requestHasWebSocketConn := req.wsConn != nil
	funcHasMoreThanOneInputParams := len(fd.reqt) > 1
	firstFuncParamIsWebSocketConn := false
	firstFuncParamIsHttpRequest := false
	if funcHasMoreThanOneInputParams {
		firstFuncParamIsWebSocketConn = fd.reqt[1].AssignableTo(reflect.TypeOf(&concurrentWsConn{}))
		firstFuncParamIsHttpRequest = fd.reqt[1].AssignableTo(reflect.TypeOf(&http.Request{}))
	}
	if requestHasWebSocketConn && firstFuncParamIsWebSocketConn {
		inArgs[1] = reflect.ValueOf(req.wsConn)
		inArgsOffset++
	} else if firstFuncParamIsHttpRequest {
		// If in the future one endponit needs to have both a websocket connection and an http request
		// we will need to modify this code to properly handle it
		inArgs[1] = reflect.ValueOf(req.HttpRequest)
		inArgsOffset++
	}

	// check params passed by request match function params
	var testStruct []interface{}
	if err := json.Unmarshal(req.Params, &testStruct); err == nil && len(testStruct) > fd.numParams() {
		return NewResponse(req.Request, nil, NewServerError(InvalidParamsErrorCode, fmt.Sprintf("too many arguments, want at most %d", fd.numParams())))
	}

	inputs := make([]interface{}, fd.numParams()-inArgsOffset)

	for i := inArgsOffset; i < fd.inNum-1; i++ {
		val := reflect.New(fd.reqt[i+1])
		inputs[i-inArgsOffset] = val.Interface()
		inArgs[i+1] = val.Elem()
	}

	if fd.numParams() > 0 {
		if err := json.Unmarshal(req.Params, &inputs); err != nil {
			return NewResponse(req.Request, nil, NewServerError(InvalidParamsErrorCode, "Invalid Params"))
		}
	}

	output := fd.fv.Call(inArgs)
	if err := getError(output[1]); err != nil {
		log.Debugf("failed call: [%v]%v. Params: %v", err.ErrorCode(), err.Error(), string(req.Params))
		return NewResponse(req.Request, nil, err)
	}

	var data []byte
	res := output[0].Interface()
	if res != nil {
		d, _ := json.Marshal(res)
		data = d
	}

	return NewResponse(req.Request, data, nil)
}

func (h *Handler) registerEndpoints(endpoints interface{}) {
	st := reflect.TypeOf(endpoints)
	if st.Kind() == reflect.Struct {
		panic("endpoints must be a pointer to struct")
	}

	funcMap := make(map[string]*endpointData)
	for i := 0; i < st.NumMethod(); i++ {
		mv := st.Method(i)
		if mv.PkgPath != "" {
			// skip unexported methods
			continue
		}

		name := lowerCaseFirst(mv.Name)
		funcName := "eth_" + name
		fd := &endpointData{
			fv: mv.Func,
		}
		var err error
		if fd.inNum, fd.reqt, err = validateFunc(funcName, fd.fv, true); err != nil {
			panic(fmt.Sprintf("invalid function '%s', error:%v", funcName, err))
		}
		// check if last item is a pointer
		if fd.numParams() != 0 {
			last := fd.reqt[fd.numParams()]
			if last.Kind() == reflect.Ptr {
				fd.isDyn = true
			}
		}
		funcMap[name] = fd
	}

	h.endpoints = reflect.ValueOf(endpoints)
	h.endpointMap = funcMap
}

func (h *Handler) getFnHandler(req Request) (*endpointData, Error) {
	methodNotFoundErrorMessage := fmt.Sprintf("the method %s does not exist/is not available", req.Method)

	_, funcName, found := strings.Cut(req.Method, "_")
	if !found {
		return nil, NewServerError(NotFoundErrorCode, methodNotFoundErrorMessage)
	}

	fd, ok := h.endpointMap[funcName]
	if !ok {
		log.Debugf("method %s not found", req.Method)
		return nil, NewServerError(NotFoundErrorCode, methodNotFoundErrorMessage)
	}
	return fd, nil
}

func validateFunc(funcName string, fv reflect.Value, isMethod bool) (inNum int, reqt []reflect.Type, err error) {
	if funcName == "" {
		err = fmt.Errorf("function name cannot be empty")
		return
	}

	ft := fv.Type()
	if ft.Kind() != reflect.Func {
		err = fmt.Errorf("function '%s' must be a function instead of %s", funcName, ft)
		return
	}

	inNum = ft.NumIn()
	outNum := ft.NumOut()

	if outNum != requiredReturnParamsPerFn {
		err = fmt.Errorf("unexpected number of output arguments in the function '%s': %d. Expected 2", funcName, outNum)
		return
	}
	if !isRPCErrorType(ft.Out(1)) {
		err = fmt.Errorf("unexpected type for the second return value of the function '%s': '%s'. Expected '%s'", funcName, ft.Out(1), rpcErrType)
		return
	}

	reqt = make([]reflect.Type, inNum)
	for i := 0; i < inNum; i++ {
		reqt[i] = ft.In(i)
	}
	return
}

var rpcErrType = reflect.TypeOf((*Error)(nil)).Elem()

func isRPCErrorType(t reflect.Type) bool {
	return t.Implements(rpcErrType)
}

func getError(v reflect.Value) Error {
	if v.IsNil() {
		return nil
	}

	switch vt := v.Interface().(type) {
	case *ServerError:
		return vt
	default:
		return NewServerError(DefaultErrorCode, "runtime error")
	}
}

func lowerCaseFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}