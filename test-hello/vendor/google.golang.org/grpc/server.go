/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/trace"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/binarylog"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/tap"
)

const (
	defaultServerMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultServerMaxSendMessageSize    = math.MaxInt32
)

var statusOK = status.New(codes.OK, "")

type methodHandler func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor UnaryServerInterceptor) (interface{}, error)

// MethodDesc represents an RPC service's method specification.
type MethodDesc struct {
	MethodName string
	Handler    methodHandler
}

// ServiceDesc represents an RPC service's specification.
type ServiceDesc struct {
	ServiceName string
	// The pointer to the service interface. Used to check whether the user
	// provided implementation satisfies the interface requirements.
	HandlerType interface{}
	Methods     []MethodDesc
	Streams     []StreamDesc
	Metadata    interface{}
}

// service consists of the information of the server serving this service and
// the methods in this service.
type service struct {
	server interface{} // the server for service methods
	md     map[string]*MethodDesc
	sd     map[string]*StreamDesc
	mdata  interface{}
}

// Server is a gRPC server to serve RPC requests.
type Server struct {
	opts serverOptions //初始化

	mu     sync.Mutex // guards following 监控者 ，上下文
	lis    map[net.Listener]bool //服务端静听端口
	//server transport是所有grpc服务器端传输实现的通用接口
	conns  map[transport.ServerTransport]bool
	serve  bool//表示服务是否开启，在Serve()方法中赋值为true
	drain  bool//在调用GracefulStop（优雅的停止服务）方法被赋值为true
	cv     *sync.Cond          // 当连接关闭以正常停止时发出信号
	m      map[string]*service // 存储的是服务信息，一元rpc和流式rpc的 信息 service name -> service info
	events trace.EventLog //跟踪事件日志

	quit               *grpcsync.Event //同步退出事件 chan 记录退出状态
	done               *grpcsync.Event // 同步完成状态
	channelzRemoveOnce sync.Once
	serveWG            sync.WaitGroup // 控制异步服务done

	channelzID int64 // 客户端唯一标识
	//channelzData用于存储ClientConn、addrConn和Server的channelz相关数据。
	//这些字段不能嵌入到原始结构中(例如ClientConn)，因为要执行原子操作
	czData     *channelzData
}

type serverOptions struct {
	creds                 credentials.TransportCredentials //cred证书
	codec                 baseCodec//序列化和反序列化
	cp                    Compressor//压缩接口
	dc                    Decompressor//解压缩接口
	unaryInt              UnaryServerInterceptor//一元拦截器
	streamInt             StreamServerInterceptor//流拦截器
	chainUnaryInts        []UnaryServerInterceptor//
	chainStreamInts       []StreamServerInterceptor
	inTapHandle           tap.ServerInHandle
	statsHandler          stats.Handler
	maxConcurrentStreams  uint32//http2中最大的并发流个数
	maxReceiveMessageSize int//最大接收消息大小 1024 * 1024 * 32
	maxSendMessageSize    int//最大发送消息大小 1024 * 1024 * 32
	unknownStreamDesc     *StreamDesc //流式信息
	keepaliveParams       keepalive.ServerParameters //长连接的server参数
	keepalivePolicy       keepalive.EnforcementPolicy//长连接的等待时间 默认5分钟，以及是否是流式服务
	initialWindowSize     int32//初始化stream的window大小，下限值式64k 上线式2^ 31
	initialConnWindowSize int32//初始化conn大小，一个conn会有多个stream，等于上面的值* 16，http2的限制是大雨0
	writeBufferSize       int //写缓冲大小
	readBufferSize        int//读缓冲大小
	connectionTimeout     time.Duration//连接超时时间
	maxHeaderListSize     *uint32//最大请求头信息
	headerTableSize       *uint32//请求头信息
}

var defaultServerOptions = serverOptions{
	maxReceiveMessageSize: defaultServerMaxReceiveMessageSize,//默认接收接收消息size 1024 * 1024 * 4
	maxSendMessageSize:    defaultServerMaxSendMessageSize,//默认发送值int32的最大值
	connectionTimeout:     1 * time.Second,//连接超时时间120s
	writeBufferSize:       defaultWriteBufSize,//默认写缓冲区32 * 1024
	readBufferSize:        defaultReadBufSize,//默认读缓冲区 32 * 1024
}

// A ServerOption sets options such as credentials, codec and keepalive parameters, etc.
type ServerOption interface {
	apply(*serverOptions)
}

// EmptyServerOption does not alter the server configuration. It can be embedded
// in another structure to build custom server options.
//
// This API is EXPERIMENTAL.
type EmptyServerOption struct{}

func (EmptyServerOption) apply(*serverOptions) {}

// funcServerOption wraps a function that modifies serverOptions into an
// implementation of the ServerOption interface.
//type func  serverOptions实现了了几个方法
//writeBufferSize ReadBufferSize InitialWindowSize
type funcServerOption struct {
	f func(*serverOptions)
}

func (fdo *funcServerOption) apply(do *serverOptions) {
	fdo.f(do)
}

func newFuncServerOption(f func(*serverOptions)) *funcServerOption {
	return &funcServerOption{
		f: f,
	}
}

// WriteBufferSize determines how much data can be batched before doing a write on the wire.
// The corresponding memory allocation for this buffer will be twice the size to keep syscalls low.
// The default value for this buffer is 32KB.
// Zero will disable the write buffer such that each write will be on underlying connection.
// Note: A Send call may not directly translate to a write.
// WriteBufferSize决定在对网络执行写操作之前可以批处理多少数据。
//这个缓冲区的相应内存分配将是保持最低系统调用的两倍大小。
//这个缓冲区的默认值是32KB。
// Zero将禁用写缓冲区，以便每个写都位于底层连接上。
//注意:发送调用可能不能直接转换为写。
func WriteBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.writeBufferSize = s
	})
}

// ReadBufferSize lets you set the size of read buffer, this determines how much data can be read at most
// for one read syscall.
// The default value for this buffer is 32KB.
// Zero will disable read buffer for a connection so data framer can access the underlying
// conn directly.
// ReadBufferSize允许设置读取缓冲区的大小，这决定了最多可以读取多少数据
//这个缓冲区的默认值是32KB。
// Zero将禁用连接的读缓冲区，以便数据帧程序可以访问底层
func ReadBufferSize(s int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.readBufferSize = s
	})
}

// InitialWindowSize returns a ServerOption that sets window size for stream.
// The lower bound for window size is 64K and any value smaller than that will be ignored.
// InitialWindowSize返回一个服务器选项，用于设置流的窗口大小。
//窗口大小的下界是64K，小于64K的值将被忽略。
func InitialWindowSize(s int32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.initialWindowSize = s
	})
}

// InitialConnWindowSize returns a ServerOption that sets window size for a connection.
// The lower bound for window size is 64K and any value smaller than that will be ignored.
// InitialConnWindowSize返回一个服务器选项，用于设置连接的窗口大小。
//窗口大小的下界是64K，小于64K的值将被忽略。
func InitialConnWindowSize(s int32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.initialConnWindowSize = s
	})
}

// KeepaliveParams returns a ServerOption that sets keepalive and max-age parameters for the server.
func KeepaliveParams(kp keepalive.ServerParameters) ServerOption {
	if kp.Time > 0 && kp.Time < time.Second {
		grpclog.Warning("Adjusting keepalive ping interval to minimum period of 1s")
		kp.Time = time.Second
	}

	return newFuncServerOption(func(o *serverOptions) {
		o.keepaliveParams = kp
	})
}

// KeepaliveEnforcementPolicy returns a ServerOption that sets keepalive enforcement policy for the server.
func KeepaliveEnforcementPolicy(kep keepalive.EnforcementPolicy) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.keepalivePolicy = kep
	})
}

// CustomCodec returns a ServerOption that sets a codec for message marshaling and unmarshaling.
//
// This will override any lookups by content-subtype for Codecs registered with RegisterCodec.
func CustomCodec(codec Codec) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.codec = codec
	})
}

// RPCCompressor returns a ServerOption that sets a compressor for outbound
// messages.  For backward compatibility, all outbound messages will be sent
// using this compressor, regardless of incoming message compression.  By
// default, server messages will be sent using the same compressor with which
// request messages were sent.
//
// Deprecated: use encoding.RegisterCompressor instead.
func RPCCompressor(cp Compressor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.cp = cp
	})
}

// RPCDecompressor returns a ServerOption that sets a decompressor for inbound
// messages.  It has higher priority than decompressors registered via
// encoding.RegisterCompressor.
//
// Deprecated: use encoding.RegisterCompressor instead.
func RPCDecompressor(dc Decompressor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.dc = dc
	})
}

// MaxMsgSize returns a ServerOption to set the max message size in bytes the server can receive.
// If this is not set, gRPC uses the default limit.
//
// Deprecated: use MaxRecvMsgSize instead.
func MaxMsgSize(m int) ServerOption {
	return MaxRecvMsgSize(m)
}

// MaxRecvMsgSize returns a ServerOption to set the max message size in bytes the server can receive.
// If this is not set, gRPC uses the default 4MB.
func MaxRecvMsgSize(m int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxReceiveMessageSize = m
	})
}

// MaxSendMsgSize returns a ServerOption to set the max message size in bytes the server can send.
// If this is not set, gRPC uses the default `math.MaxInt32`.
func MaxSendMsgSize(m int) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxSendMessageSize = m
	})
}

// MaxConcurrentStreams returns a ServerOption that will apply a limit on the number
// of concurrent streams to each ServerTransport.
func MaxConcurrentStreams(n uint32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxConcurrentStreams = n
	})
}

// Creds returns a ServerOption that sets credentials for server connections.
func Creds(c credentials.TransportCredentials) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.creds = c
	})
}

// UnaryInterceptor returns a ServerOption that sets the UnaryServerInterceptor for the
// server. Only one unary interceptor can be installed. The construction of multiple
// interceptors (e.g., chaining) can be implemented at the caller.
func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.unaryInt != nil {
			panic("The unary server interceptor was already set and may not be reset.")
		}
		o.unaryInt = i
	})
}

// ChainUnaryInterceptor returns a ServerOption that specifies the chained interceptor
// for unary RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All unary interceptors added by this method will be chained.
func ChainUnaryInterceptor(interceptors ...UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.chainUnaryInts = append(o.chainUnaryInts, interceptors...)
	})
}

// StreamInterceptor returns a ServerOption that sets the StreamServerInterceptor for the
// server. Only one stream interceptor can be installed.
func StreamInterceptor(i StreamServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.streamInt != nil {
			panic("The stream server interceptor was already set and may not be reset.")
		}
		o.streamInt = i
	})
}

// ChainStreamInterceptor returns a ServerOption that specifies the chained interceptor
// for stream RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All stream interceptors added by this method will be chained.
func ChainStreamInterceptor(interceptors ...StreamServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.chainStreamInts = append(o.chainStreamInts, interceptors...)
	})
}

// InTapHandle returns a ServerOption that sets the tap handle for all the server
// transport to be created. Only one can be installed.
func InTapHandle(h tap.ServerInHandle) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		if o.inTapHandle != nil {
			panic("The tap handle was already set and may not be reset.")
		}
		o.inTapHandle = h
	})
}

// StatsHandler returns a ServerOption that sets the stats handler for the server.
func StatsHandler(h stats.Handler) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.statsHandler = h
	})
}

// UnknownServiceHandler returns a ServerOption that allows for adding a custom
// unknown service handler. The provided method is a bidi-streaming RPC service
// handler that will be invoked instead of returning the "unimplemented" gRPC
// error whenever a request is received for an unregistered service or method.
// The handling function and stream interceptor (if set) have full access to
// the ServerStream, including its Context.
func UnknownServiceHandler(streamHandler StreamHandler) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.unknownStreamDesc = &StreamDesc{
			StreamName: "unknown_service_handler",
			Handler:    streamHandler,
			// We need to assume that the users of the streamHandler will want to use both.
			ClientStreams: true,
			ServerStreams: true,
		}
	})
}

// ConnectionTimeout returns a ServerOption that sets the timeout for
// connection establishment (up to and including HTTP/2 handshaking) for all
// new connections.  If this is not set, the default is 120 seconds.  A zero or
// negative value will result in an immediate timeout.
//
// This API is EXPERIMENTAL.
func ConnectionTimeout(d time.Duration) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.connectionTimeout = d
	})
}

// MaxHeaderListSize returns a ServerOption that sets the max (uncompressed) size
// of header list that the server is prepared to accept.
func MaxHeaderListSize(s uint32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.maxHeaderListSize = &s
	})
}

// HeaderTableSize returns a ServerOption that sets the size of dynamic
// header table for stream.
//
// This API is EXPERIMENTAL.
func HeaderTableSize(s uint32) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.headerTableSize = &s
	})
}

// NewServer creates a gRPC server which has no service registered and has not
// started to accept requests yet.
func NewServer(opt ...ServerOption) *Server {
	//初始化默认值----接收、发送消息，连接超时时间、读写缓冲区大小
	opts := defaultServerOptions
	//对serverOptions 进行初始化赋值为默认值
	for _, o := range opt {
		o.apply(&opts)
	}
	//对server进行地址初始化
	s := &Server{
		lis:    make(map[net.Listener]bool),//分配连接空间
		opts:   opts,//serverOption的默认值
		//server transport是所有grpc服务器端传输实现的通用接口
		conns:  make(map[transport.ServerTransport]bool),
		m:      make(map[string]*service),//一元rpc的方法和对应回调处理方法的集合
		quit:   grpcsync.NewEvent(),
		done:   grpcsync.NewEvent(),
		czData: new(channelzData),//channelzData结构初始化，channelzData用于存储ClientConn、addrConn和Server的channelz相关数据
	}
	//拦截器的初始化
	chainUnaryServerInterceptors(s)
	chainStreamServerInterceptors(s)
	//条件锁，消息通知，保存在通知列表中，用来唤醒一个或者所有等待条件变量而足赛的Go程
	s.cv = sync.NewCond(&s.mu)
	//被调用服务信息的追踪
	if EnableTracing {
		_, file, line, _ := runtime.Caller(1)
		s.events = trace.NewEventLog("grpc.Server", fmt.Sprintf("%s:%d", file, line))
	}
	//返回对应服务端口和listenSocket信息 和方法的编号，院子性递增的
	if channelz.IsOn() {
		s.channelzID = channelz.RegisterServer(&channelzServer{s}, "")
	}
	return s
}

// printf records an event in s's event log, unless s has been stopped.
// REQUIRES s.mu is held.
func (s *Server) printf(format string, a ...interface{}) {
	if s.events != nil {
		s.events.Printf(format, a...)
	}
}

// errorf records an error in s's event log, unless s has been stopped.
// REQUIRES s.mu is held.
func (s *Server) errorf(format string, a ...interface{}) {
	if s.events != nil {
		s.events.Errorf(format, a...)
	}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve.
/*
* 参数1 初始化的sever  参数2 服务的方法列表
*/
func (s *Server) RegisterService(sd *ServiceDesc, ss interface{}) {
	ht := reflect.TypeOf(sd.HandlerType).Elem()
	st := reflect.TypeOf(ss)
	if !st.Implements(ht) {
		grpclog.Fatalf("grpc: Server.RegisterService found the handler of type %v that does not satisfy %v", st, ht)
	}
	s.register(sd, ss)
}

func (s *Server) register(sd *ServiceDesc, ss interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.printf("RegisterService(%q)", sd.ServiceName)
	if s.serve {
		grpclog.Fatalf("grpc: Server.RegisterService after Server.Serve for %q", sd.ServiceName)
	}
	if _, ok := s.m[sd.ServiceName]; ok {
		grpclog.Fatalf("grpc: Server.RegisterService found duplicate service registration for %q", sd.ServiceName)
	}
	srv := &service{
		server: ss,
		md:     make(map[string]*MethodDesc),
		sd:     make(map[string]*StreamDesc),
		mdata:  sd.Metadata,
	}
	for i := range sd.Methods {
		d := &sd.Methods[i]
		srv.md[d.MethodName] = d
	}
	for i := range sd.Streams {
		d := &sd.Streams[i]
		srv.sd[d.StreamName] = d
	}
	s.m[sd.ServiceName] = srv
}

// MethodInfo contains the information of an RPC including its method name and type.
type MethodInfo struct {
	// Name is the method name only, without the service name or package name.
	Name string
	// IsClientStream indicates whether the RPC is a client streaming RPC.
	IsClientStream bool
	// IsServerStream indicates whether the RPC is a server streaming RPC.
	IsServerStream bool
}

// ServiceInfo contains unary RPC method info, streaming RPC method info and metadata for a service.
type ServiceInfo struct {
	Methods []MethodInfo
	// Metadata is the metadata specified in ServiceDesc when registering service.
	Metadata interface{}
}

// GetServiceInfo returns a map from service names to ServiceInfo.
// Service names include the package names, in the form of <package>.<service>.
func (s *Server) GetServiceInfo() map[string]ServiceInfo {
	ret := make(map[string]ServiceInfo)
	for n, srv := range s.m {
		methods := make([]MethodInfo, 0, len(srv.md)+len(srv.sd))
		for m := range srv.md {
			methods = append(methods, MethodInfo{
				Name:           m,
				IsClientStream: false,
				IsServerStream: false,
			})
		}
		for m, d := range srv.sd {
			methods = append(methods, MethodInfo{
				Name:           m,
				IsClientStream: d.ClientStreams,
				IsServerStream: d.ServerStreams,
			})
		}

		ret[n] = ServiceInfo{
			Methods:  methods,
			Metadata: srv.mdata,
		}
	}
	return ret
}

// ErrServerStopped indicates that the operation is now illegal because of
// the server being stopped.
var ErrServerStopped = errors.New("grpc: the server has been stopped")

func (s *Server) useTransportAuthenticator(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if s.opts.creds == nil {
		return rawConn, nil, nil
	}
	return s.opts.creds.ServerHandshake(rawConn)
}

type listenSocket struct {
	net.Listener
	channelzID int64
}

func (l *listenSocket) ChannelzMetric() *channelz.SocketInternalMetric {
	return &channelz.SocketInternalMetric{
		SocketOptions: channelz.GetSocketOption(l.Listener),
		LocalAddr:     l.Listener.Addr(),
	}
}

func (l *listenSocket) Close() error {
	err := l.Listener.Close()
	if channelz.IsOn() {
		channelz.RemoveEntry(l.channelzID)
	}
	return err
}

// Serve accepts incoming connections on the listener lis, creating a new
// ServerTransport and service goroutine for each. The service goroutines
// read gRPC requests and then call the registered handlers to reply to them.
// Serve returns when lis.Accept fails with fatal errors.  lis will be closed when
// this method returns.
// Serve will return a non-nil error unless Stop or GracefulStop is called.
func (s *Server) Serve(lis net.Listener) error {
	s.mu.Lock()
	s.printf("serving")
	s.serve = true
	if s.lis == nil {
		// Serve called after Stop or GracefulStop.
		s.mu.Unlock()
		lis.Close()
		return ErrServerStopped
	}

	s.serveWG.Add(1)
	//优雅的停止服务
	defer func() {
		s.serveWG.Done()
		if s.quit.HasFired() {
			// Stop or GracefulStop called; block until done and return nil.
			<-s.done.Done()
		}
	}()
	//包装连接对象，并声明为true，代表有效
	ls := &listenSocket{Listener: lis}
	s.lis[ls] = true
	//服务的监听端口和socket信息 请求的方法对应的标识是否在注册的map中，
	if channelz.IsOn() {
		ls.channelzID = channelz.RegisterListenSocket(ls, s.channelzID, lis.Addr().String())
	}
	s.mu.Unlock()
	//清理资源
	defer func() {
		s.mu.Lock()
		if s.lis != nil && s.lis[ls] {
			ls.Close()
			delete(s.lis, ls)
		}
		s.mu.Unlock()
	}()

	var tempDelay time.Duration // how long to sleep on accept failure
	//for 进行accept处理
	for {
		rawConn, err := lis.Accept()
		if err != nil {//监听返回错误处理逻辑
			if ne, ok := err.(interface {
				Temporary() bool
			}); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond //5毫秒
				} else {
					tempDelay *= 2
				}
				//最长一秒
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				s.mu.Lock()
				s.printf("Accept error: %v; retrying in %v", err, tempDelay)
				s.mu.Unlock()//一秒后触发关闭此次连接
				timer := time.NewTimer(tempDelay)
				select {
				case <-timer.C:
				case <-s.quit.Done():
					timer.Stop()
					return nil
				}
				continue
			}
			s.mu.Lock()
			s.printf("done serving; Accept = %v", err)
			s.mu.Unlock()
			//记录服务监听失败次数
			if s.quit.HasFired() {
				return nil
			}
			return err
		}
		tempDelay = 0
		// Start a new goroutine to deal with rawConn so we don't stall this Accept
		// loop goroutine.
		//
		// Make sure we account for the goroutine so GracefulStop doesn't nil out
		// s.conns before this conn can be added.
		s.serveWG.Add(1)
		//handleRawConn生成一个goroutine来处理刚刚接受的连接
		go func() {
			/*
				创建了一个 Http2Transport ，然后通过 serveStreams 方法将这个 Http2Transport 层层透传下去。
			 */
			s.handleRawConn(rawConn)
			//处理完进行回收
			s.serveWG.Done()
		}()
	}
}

// handleRawConn forks a goroutine to handle a just-accepted connection that
// has not had any I/O performed on it yet.
//handleRawConn生成一个goroutine来处理刚刚接受的连接
func (s *Server) handleRawConn(rawConn net.Conn) {
	if s.quit.HasFired() {
		rawConn.Close()
		return
	}
	rawConn.SetDeadline(time.Now().Add(s.opts.connectionTimeout))
	conn, authInfo, err := s.useTransportAuthenticator(rawConn)
	if err != nil {
		// ErrConnDispatched means that the connection was dispatched away from
		// gRPC; those connections should be left open.
		if err != credentials.ErrConnDispatched {
			s.mu.Lock()
			s.errorf("ServerHandshake(%q) failed: %v", rawConn.RemoteAddr(), err)
			s.mu.Unlock()
			channelz.Warningf(s.channelzID, "grpc: Server.Serve failed to complete security handshake from %q: %v", rawConn.RemoteAddr(), err)
			rawConn.Close()
		}
		rawConn.SetDeadline(time.Time{})
		return
	}

	// Finish handshaking (HTTP2)
	st := s.newHTTP2Transport(conn, authInfo)
	if st == nil {
		return
	}

	rawConn.SetDeadline(time.Time{})
	if !s.addConn(st) {
		return
	}
	go func() {
		s.serveStreams(st)
		s.removeConn(st)
	}()
}

// newHTTP2Transport sets up a http/2 transport (using the
// gRPC http2 server transport in transport/http2_server.go).
func (s *Server) newHTTP2Transport(c net.Conn, authInfo credentials.AuthInfo) transport.ServerTransport {
	config := &transport.ServerConfig{
		MaxStreams:            s.opts.maxConcurrentStreams,
		AuthInfo:              authInfo,
		InTapHandle:           s.opts.inTapHandle,
		StatsHandler:          s.opts.statsHandler,
		KeepaliveParams:       s.opts.keepaliveParams,
		KeepalivePolicy:       s.opts.keepalivePolicy,
		InitialWindowSize:     s.opts.initialWindowSize,
		InitialConnWindowSize: s.opts.initialConnWindowSize,
		WriteBufferSize:       s.opts.writeBufferSize,
		ReadBufferSize:        s.opts.readBufferSize,
		ChannelzParentID:      s.channelzID,
		MaxHeaderListSize:     s.opts.maxHeaderListSize,
		HeaderTableSize:       s.opts.headerTableSize,
	}
	st, err := transport.NewServerTransport("http2", c, config)
	if err != nil {
		s.mu.Lock()
		s.errorf("NewServerTransport(%q) failed: %v", c.RemoteAddr(), err)
		s.mu.Unlock()
		c.Close()
		channelz.Warning(s.channelzID, "grpc: Server.Serve failed to create ServerTransport: ", err)
		return nil
	}

	return st
}

func (s *Server) serveStreams(st transport.ServerTransport) {
	defer st.Close()
	var wg sync.WaitGroup
	st.HandleStreams(func(stream *transport.Stream) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleStream(st, stream, s.traceInfo(st, stream))
		}()
	}, func(ctx context.Context, method string) context.Context {
		if !EnableTracing {
			return ctx
		}
		tr := trace.New("grpc.Recv."+methodFamily(method), method)
		return trace.NewContext(ctx, tr)
	})
	wg.Wait()
}

var _ http.Handler = (*Server)(nil)

// ServeHTTP implements the Go standard library's http.Handler
// interface by responding to the gRPC request r, by looking up
// the requested gRPC method in the gRPC server s.
//
// The provided HTTP request must have arrived on an HTTP/2
// connection. When using the Go standard library's server,
// practically this means that the Request must also have arrived
// over TLS.
//
// To share one port (such as 443 for https) between gRPC and an
// existing http.Handler, use a root http.Handler such as:
//
//   if r.ProtoMajor == 2 && strings.HasPrefix(
//   	r.Header.Get("Content-Type"), "application/grpc") {
//   	grpcServer.ServeHTTP(w, r)
//   } else {
//   	yourMux.ServeHTTP(w, r)
//   }
//
// Note that ServeHTTP uses Go's HTTP/2 server implementation which is totally
// separate from grpc-go's HTTP/2 server. Performance and features may vary
// between the two paths. ServeHTTP does not support some gRPC features
// available through grpc-go's HTTP/2 server, and it is currently EXPERIMENTAL
// and subject to change.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st, err := transport.NewServerHandlerTransport(w, r, s.opts.statsHandler)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.addConn(st) {
		return
	}
	defer s.removeConn(st)
	s.serveStreams(st)
}

// traceInfo returns a traceInfo and associates it with stream, if tracing is enabled.
// If tracing is not enabled, it returns nil.
func (s *Server) traceInfo(st transport.ServerTransport, stream *transport.Stream) (trInfo *traceInfo) {
	if !EnableTracing {
		return nil
	}
	tr, ok := trace.FromContext(stream.Context())
	if !ok {
		return nil
	}

	trInfo = &traceInfo{
		tr: tr,
		firstLine: firstLine{
			client:     false,
			remoteAddr: st.RemoteAddr(),
		},
	}
	if dl, ok := stream.Context().Deadline(); ok {
		trInfo.firstLine.deadline = time.Until(dl)
	}
	return trInfo
}

func (s *Server) addConn(st transport.ServerTransport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		st.Close()
		return false
	}
	if s.drain {
		// Transport added after we drained our existing conns: drain it
		// immediately.
		st.Drain()
	}
	s.conns[st] = true
	return true
}

func (s *Server) removeConn(st transport.ServerTransport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns != nil {
		delete(s.conns, st)
		s.cv.Broadcast()
	}
}

func (s *Server) channelzMetric() *channelz.ServerInternalMetric {
	return &channelz.ServerInternalMetric{
		CallsStarted:             atomic.LoadInt64(&s.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&s.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&s.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&s.czData.lastCallStartedTime)),
	}
}

func (s *Server) incrCallsStarted() {
	atomic.AddInt64(&s.czData.callsStarted, 1)
	atomic.StoreInt64(&s.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (s *Server) incrCallsSucceeded() {
	atomic.AddInt64(&s.czData.callsSucceeded, 1)
}

func (s *Server) incrCallsFailed() {
	atomic.AddInt64(&s.czData.callsFailed, 1)
}

func (s *Server) sendResponse(t transport.ServerTransport, stream *transport.Stream, msg interface{}, cp Compressor, opts *transport.Options, comp encoding.Compressor) error {
	data, err := encode(s.getCodec(stream.ContentSubtype()), msg)
	if err != nil {
		channelz.Error(s.channelzID, "grpc: server failed to encode response: ", err)
		return err
	}
	compData, err := compress(data, cp, comp)
	if err != nil {
		channelz.Error(s.channelzID, "grpc: server failed to compress response: ", err)
		return err
	}
	hdr, payload := msgHeader(data, compData)
	// TODO(dfawley): should we be checking len(data) instead?
	if len(payload) > s.opts.maxSendMessageSize {
		return status.Errorf(codes.ResourceExhausted, "grpc: trying to send message larger than max (%d vs. %d)", len(payload), s.opts.maxSendMessageSize)
	}
	err = t.Write(stream, hdr, payload, opts)
	if err == nil && s.opts.statsHandler != nil {
		s.opts.statsHandler.HandleRPC(stream.Context(), outPayload(false, msg, data, payload, time.Now()))
	}
	return err
}

// chainUnaryServerInterceptors chains all unary server interceptors into one.
func chainUnaryServerInterceptors(s *Server) {
	// Prepend opts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	interceptors := s.opts.chainUnaryInts
	if s.opts.unaryInt != nil {
		interceptors = append([]UnaryServerInterceptor{s.opts.unaryInt}, s.opts.chainUnaryInts...)
	}

	var chainedInt UnaryServerInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, req interface{}, info *UnaryServerInfo, handler UnaryHandler) (interface{}, error) {
			return interceptors[0](ctx, req, info, getChainUnaryHandler(interceptors, 0, info, handler))
		}
	}

	s.opts.unaryInt = chainedInt
}

// getChainUnaryHandler recursively generate the chained UnaryHandler
func getChainUnaryHandler(interceptors []UnaryServerInterceptor, curr int, info *UnaryServerInfo, finalHandler UnaryHandler) UnaryHandler {
	if curr == len(interceptors)-1 {
		return finalHandler
	}

	return func(ctx context.Context, req interface{}) (interface{}, error) {
		return interceptors[curr+1](ctx, req, info, getChainUnaryHandler(interceptors, curr+1, info, finalHandler))
	}
}

func (s *Server) processUnaryRPC(t transport.ServerTransport, stream *transport.Stream, srv *service, md *MethodDesc, trInfo *traceInfo) (err error) {
	sh := s.opts.statsHandler
	if sh != nil || trInfo != nil || channelz.IsOn() {
		if channelz.IsOn() {
			s.incrCallsStarted()
		}
		var statsBegin *stats.Begin
		if sh != nil {
			beginTime := time.Now()
			statsBegin = &stats.Begin{
				BeginTime: beginTime,
			}
			sh.HandleRPC(stream.Context(), statsBegin)
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(&trInfo.firstLine, false)
		}
		// The deferred error handling for tracing, stats handler and channelz are
		// combined into one function to reduce stack usage -- a defer takes ~56-64
		// bytes on the stack, so overflowing the stack will require a stack
		// re-allocation, which is expensive.
		//
		// To maintain behavior similar to separate deferred statements, statements
		// should be executed in the reverse order. That is, tracing first, stats
		// handler second, and channelz last. Note that panics *within* defers will
		// lead to different behavior, but that's an acceptable compromise; that
		// would be undefined behavior territory anyway.
		defer func() {
			if trInfo != nil {
				if err != nil && err != io.EOF {
					trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
					trInfo.tr.SetError()
				}
				trInfo.tr.Finish()
			}

			if sh != nil {
				end := &stats.End{
					BeginTime: statsBegin.BeginTime,
					EndTime:   time.Now(),
				}
				if err != nil && err != io.EOF {
					end.Error = toRPCErr(err)
				}
				sh.HandleRPC(stream.Context(), end)
			}

			if channelz.IsOn() {
				if err != nil && err != io.EOF {
					s.incrCallsFailed()
				} else {
					s.incrCallsSucceeded()
				}
			}
		}()
	}

	binlog := binarylog.GetMethodLogger(stream.Method())
	if binlog != nil {
		ctx := stream.Context()
		md, _ := metadata.FromIncomingContext(ctx)
		logEntry := &binarylog.ClientHeader{
			Header:     md,
			MethodName: stream.Method(),
			PeerAddr:   nil,
		}
		if deadline, ok := ctx.Deadline(); ok {
			logEntry.Timeout = time.Until(deadline)
			if logEntry.Timeout < 0 {
				logEntry.Timeout = 0
			}
		}
		if a := md[":authority"]; len(a) > 0 {
			logEntry.Authority = a[0]
		}
		if peer, ok := peer.FromContext(ctx); ok {
			logEntry.PeerAddr = peer.Addr
		}
		binlog.Log(logEntry)
	}

	// comp and cp are used for compression.  decomp and dc are used for
	// decompression.  If comp and decomp are both set, they are the same;
	// however they are kept separate to ensure that at most one of the
	// compressor/decompressor variable pairs are set for use later.
	var comp, decomp encoding.Compressor
	var cp Compressor
	var dc Decompressor

	// If dc is set and matches the stream's compression, use it.  Otherwise, try
	// to find a matching registered compressor for decomp.
	if rc := stream.RecvCompress(); s.opts.dc != nil && s.opts.dc.Type() == rc {
		dc = s.opts.dc
	} else if rc != "" && rc != encoding.Identity {
		decomp = encoding.GetCompressor(rc)
		if decomp == nil {
			st := status.Newf(codes.Unimplemented, "grpc: Decompressor is not installed for grpc-encoding %q", rc)
			t.WriteStatus(stream, st)
			return st.Err()
		}
	}

	// If cp is set, use it.  Otherwise, attempt to compress the response using
	// the incoming message compression method.
	//
	// NOTE: this needs to be ahead of all handling, https://github.com/grpc/grpc-go/issues/686.
	if s.opts.cp != nil {
		cp = s.opts.cp
		stream.SetSendCompress(cp.Type())
	} else if rc := stream.RecvCompress(); rc != "" && rc != encoding.Identity {
		// Legacy compressor not specified; attempt to respond with same encoding.
		comp = encoding.GetCompressor(rc)
		if comp != nil {
			stream.SetSendCompress(rc)
		}
	}

	var payInfo *payloadInfo
	if sh != nil || binlog != nil {
		payInfo = &payloadInfo{}
	}
	d, err := recvAndDecompress(&parser{r: stream}, stream, dc, s.opts.maxReceiveMessageSize, payInfo, decomp)
	if err != nil {
		if st, ok := status.FromError(err); ok {
			if e := t.WriteStatus(stream, st); e != nil {
				channelz.Warningf(s.channelzID, "grpc: Server.processUnaryRPC failed to write status %v", e)
			}
		}
		return err
	}
	if channelz.IsOn() {
		t.IncrMsgRecv()
	}
	df := func(v interface{}) error {
		if err := s.getCodec(stream.ContentSubtype()).Unmarshal(d, v); err != nil {
			return status.Errorf(codes.Internal, "grpc: error unmarshalling request: %v", err)
		}
		if sh != nil {
			sh.HandleRPC(stream.Context(), &stats.InPayload{
				RecvTime:   time.Now(),
				Payload:    v,
				WireLength: payInfo.wireLength,
				Data:       d,
				Length:     len(d),
			})
		}
		if binlog != nil {
			binlog.Log(&binarylog.ClientMessage{
				Message: d,
			})
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(&payload{sent: false, msg: v}, true)
		}
		return nil
	}
	ctx := NewContextWithServerTransportStream(stream.Context(), stream)
	reply, appErr := md.Handler(srv.server, ctx, df, s.opts.unaryInt)
	if appErr != nil {
		appStatus, ok := status.FromError(appErr)
		if !ok {
			// Convert appErr if it is not a grpc status error.
			appErr = status.Error(codes.Unknown, appErr.Error())
			appStatus, _ = status.FromError(appErr)
		}
		if trInfo != nil {
			trInfo.tr.LazyLog(stringer(appStatus.Message()), true)
			trInfo.tr.SetError()
		}
		if e := t.WriteStatus(stream, appStatus); e != nil {
			channelz.Warningf(s.channelzID, "grpc: Server.processUnaryRPC failed to write status: %v", e)
		}
		if binlog != nil {
			if h, _ := stream.Header(); h.Len() > 0 {
				// Only log serverHeader if there was header. Otherwise it can
				// be trailer only.
				binlog.Log(&binarylog.ServerHeader{
					Header: h,
				})
			}
			binlog.Log(&binarylog.ServerTrailer{
				Trailer: stream.Trailer(),
				Err:     appErr,
			})
		}
		return appErr
	}
	if trInfo != nil {
		trInfo.tr.LazyLog(stringer("OK"), false)
	}
	opts := &transport.Options{Last: true}

	if err := s.sendResponse(t, stream, reply, cp, opts, comp); err != nil {
		if err == io.EOF {
			// The entire stream is done (for unary RPC only).
			return err
		}
		if sts, ok := status.FromError(err); ok {
			if e := t.WriteStatus(stream, sts); e != nil {
				channelz.Warningf(s.channelzID, "grpc: Server.processUnaryRPC failed to write status: %v", e)
			}
		} else {
			switch st := err.(type) {
			case transport.ConnectionError:
				// Nothing to do here.
			default:
				panic(fmt.Sprintf("grpc: Unexpected error (%T) from sendResponse: %v", st, st))
			}
		}
		if binlog != nil {
			h, _ := stream.Header()
			binlog.Log(&binarylog.ServerHeader{
				Header: h,
			})
			binlog.Log(&binarylog.ServerTrailer{
				Trailer: stream.Trailer(),
				Err:     appErr,
			})
		}
		return err
	}
	if binlog != nil {
		h, _ := stream.Header()
		binlog.Log(&binarylog.ServerHeader{
			Header: h,
		})
		binlog.Log(&binarylog.ServerMessage{
			Message: reply,
		})
	}
	if channelz.IsOn() {
		t.IncrMsgSent()
	}
	if trInfo != nil {
		trInfo.tr.LazyLog(&payload{sent: true, msg: reply}, true)
	}
	// TODO: Should we be logging if writing status failed here, like above?
	// Should the logging be in WriteStatus?  Should we ignore the WriteStatus
	// error or allow the stats handler to see it?
	err = t.WriteStatus(stream, statusOK)
	if binlog != nil {
		binlog.Log(&binarylog.ServerTrailer{
			Trailer: stream.Trailer(),
			Err:     appErr,
		})
	}
	return err
}

// chainStreamServerInterceptors chains all stream server interceptors into one.
func chainStreamServerInterceptors(s *Server) {
	// Prepend opts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	interceptors := s.opts.chainStreamInts
	if s.opts.streamInt != nil {
		interceptors = append([]StreamServerInterceptor{s.opts.streamInt}, s.opts.chainStreamInts...)
	}

	var chainedInt StreamServerInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(srv interface{}, ss ServerStream, info *StreamServerInfo, handler StreamHandler) error {
			return interceptors[0](srv, ss, info, getChainStreamHandler(interceptors, 0, info, handler))
		}
	}

	s.opts.streamInt = chainedInt
}

// getChainStreamHandler recursively generate the chained StreamHandler
func getChainStreamHandler(interceptors []StreamServerInterceptor, curr int, info *StreamServerInfo, finalHandler StreamHandler) StreamHandler {
	if curr == len(interceptors)-1 {
		return finalHandler
	}

	return func(srv interface{}, ss ServerStream) error {
		return interceptors[curr+1](srv, ss, info, getChainStreamHandler(interceptors, curr+1, info, finalHandler))
	}
}

func (s *Server) processStreamingRPC(t transport.ServerTransport, stream *transport.Stream, srv *service, sd *StreamDesc, trInfo *traceInfo) (err error) {
	if channelz.IsOn() {
		s.incrCallsStarted()
	}
	sh := s.opts.statsHandler
	var statsBegin *stats.Begin
	if sh != nil {
		beginTime := time.Now()
		statsBegin = &stats.Begin{
			BeginTime: beginTime,
		}
		sh.HandleRPC(stream.Context(), statsBegin)
	}
	ctx := NewContextWithServerTransportStream(stream.Context(), stream)
	ss := &serverStream{
		ctx:                   ctx,
		t:                     t,
		s:                     stream,
		p:                     &parser{r: stream},
		codec:                 s.getCodec(stream.ContentSubtype()),
		maxReceiveMessageSize: s.opts.maxReceiveMessageSize,
		maxSendMessageSize:    s.opts.maxSendMessageSize,
		trInfo:                trInfo,
		statsHandler:          sh,
	}

	if sh != nil || trInfo != nil || channelz.IsOn() {
		// See comment in processUnaryRPC on defers.
		defer func() {
			if trInfo != nil {
				ss.mu.Lock()
				if err != nil && err != io.EOF {
					ss.trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
					ss.trInfo.tr.SetError()
				}
				ss.trInfo.tr.Finish()
				ss.trInfo.tr = nil
				ss.mu.Unlock()
			}

			if sh != nil {
				end := &stats.End{
					BeginTime: statsBegin.BeginTime,
					EndTime:   time.Now(),
				}
				if err != nil && err != io.EOF {
					end.Error = toRPCErr(err)
				}
				sh.HandleRPC(stream.Context(), end)
			}

			if channelz.IsOn() {
				if err != nil && err != io.EOF {
					s.incrCallsFailed()
				} else {
					s.incrCallsSucceeded()
				}
			}
		}()
	}

	ss.binlog = binarylog.GetMethodLogger(stream.Method())
	if ss.binlog != nil {
		md, _ := metadata.FromIncomingContext(ctx)
		logEntry := &binarylog.ClientHeader{
			Header:     md,
			MethodName: stream.Method(),
			PeerAddr:   nil,
		}
		if deadline, ok := ctx.Deadline(); ok {
			logEntry.Timeout = time.Until(deadline)
			if logEntry.Timeout < 0 {
				logEntry.Timeout = 0
			}
		}
		if a := md[":authority"]; len(a) > 0 {
			logEntry.Authority = a[0]
		}
		if peer, ok := peer.FromContext(ss.Context()); ok {
			logEntry.PeerAddr = peer.Addr
		}
		ss.binlog.Log(logEntry)
	}

	// If dc is set and matches the stream's compression, use it.  Otherwise, try
	// to find a matching registered compressor for decomp.
	if rc := stream.RecvCompress(); s.opts.dc != nil && s.opts.dc.Type() == rc {
		ss.dc = s.opts.dc
	} else if rc != "" && rc != encoding.Identity {
		ss.decomp = encoding.GetCompressor(rc)
		if ss.decomp == nil {
			st := status.Newf(codes.Unimplemented, "grpc: Decompressor is not installed for grpc-encoding %q", rc)
			t.WriteStatus(ss.s, st)
			return st.Err()
		}
	}

	// If cp is set, use it.  Otherwise, attempt to compress the response using
	// the incoming message compression method.
	//
	// NOTE: this needs to be ahead of all handling, https://github.com/grpc/grpc-go/issues/686.
	if s.opts.cp != nil {
		ss.cp = s.opts.cp
		stream.SetSendCompress(s.opts.cp.Type())
	} else if rc := stream.RecvCompress(); rc != "" && rc != encoding.Identity {
		// Legacy compressor not specified; attempt to respond with same encoding.
		ss.comp = encoding.GetCompressor(rc)
		if ss.comp != nil {
			stream.SetSendCompress(rc)
		}
	}

	if trInfo != nil {
		trInfo.tr.LazyLog(&trInfo.firstLine, false)
	}
	var appErr error
	var server interface{}
	if srv != nil {
		server = srv.server
	}
	if s.opts.streamInt == nil {
		appErr = sd.Handler(server, ss)
	} else {
		info := &StreamServerInfo{
			FullMethod:     stream.Method(),
			IsClientStream: sd.ClientStreams,
			IsServerStream: sd.ServerStreams,
		}
		appErr = s.opts.streamInt(server, ss, info, sd.Handler)
	}
	if appErr != nil {
		appStatus, ok := status.FromError(appErr)
		if !ok {
			appStatus = status.New(codes.Unknown, appErr.Error())
			appErr = appStatus.Err()
		}
		if trInfo != nil {
			ss.mu.Lock()
			ss.trInfo.tr.LazyLog(stringer(appStatus.Message()), true)
			ss.trInfo.tr.SetError()
			ss.mu.Unlock()
		}
		t.WriteStatus(ss.s, appStatus)
		if ss.binlog != nil {
			ss.binlog.Log(&binarylog.ServerTrailer{
				Trailer: ss.s.Trailer(),
				Err:     appErr,
			})
		}
		// TODO: Should we log an error from WriteStatus here and below?
		return appErr
	}
	if trInfo != nil {
		ss.mu.Lock()
		ss.trInfo.tr.LazyLog(stringer("OK"), false)
		ss.mu.Unlock()
	}
	err = t.WriteStatus(ss.s, statusOK)
	if ss.binlog != nil {
		ss.binlog.Log(&binarylog.ServerTrailer{
			Trailer: ss.s.Trailer(),
			Err:     appErr,
		})
	}
	return err
}

func (s *Server) handleStream(t transport.ServerTransport, stream *transport.Stream, trInfo *traceInfo) {
	sm := stream.Method()
	if sm != "" && sm[0] == '/' {
		sm = sm[1:]
	}
	pos := strings.LastIndex(sm, "/")
	if pos == -1 {
		if trInfo != nil {
			trInfo.tr.LazyLog(&fmtStringer{"Malformed method name %q", []interface{}{sm}}, true)
			trInfo.tr.SetError()
		}
		errDesc := fmt.Sprintf("malformed method name: %q", stream.Method())
		if err := t.WriteStatus(stream, status.New(codes.ResourceExhausted, errDesc)); err != nil {
			if trInfo != nil {
				trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
				trInfo.tr.SetError()
			}
			channelz.Warningf(s.channelzID, "grpc: Server.handleStream failed to write status: %v", err)
		}
		if trInfo != nil {
			trInfo.tr.Finish()
		}
		return
	}
	service := sm[:pos]
	method := sm[pos+1:]

	srv, knownService := s.m[service]
	if knownService {
		if md, ok := srv.md[method]; ok {
			s.processUnaryRPC(t, stream, srv, md, trInfo)
			return
		}
		if sd, ok := srv.sd[method]; ok {
			s.processStreamingRPC(t, stream, srv, sd, trInfo)
			return
		}
	}
	// Unknown service, or known server unknown method.
	if unknownDesc := s.opts.unknownStreamDesc; unknownDesc != nil {
		s.processStreamingRPC(t, stream, nil, unknownDesc, trInfo)
		return
	}
	var errDesc string
	if !knownService {
		errDesc = fmt.Sprintf("unknown service %v", service)
	} else {
		errDesc = fmt.Sprintf("unknown method %v for service %v", method, service)
	}
	if trInfo != nil {
		trInfo.tr.LazyPrintf("%s", errDesc)
		trInfo.tr.SetError()
	}
	if err := t.WriteStatus(stream, status.New(codes.Unimplemented, errDesc)); err != nil {
		if trInfo != nil {
			trInfo.tr.LazyLog(&fmtStringer{"%v", []interface{}{err}}, true)
			trInfo.tr.SetError()
		}
		channelz.Warningf(s.channelzID, "grpc: Server.handleStream failed to write status: %v", err)
	}
	if trInfo != nil {
		trInfo.tr.Finish()
	}
}

// The key to save ServerTransportStream in the context.
type streamKey struct{}

// NewContextWithServerTransportStream creates a new context from ctx and
// attaches stream to it.
//
// This API is EXPERIMENTAL.
func NewContextWithServerTransportStream(ctx context.Context, stream ServerTransportStream) context.Context {
	return context.WithValue(ctx, streamKey{}, stream)
}

// ServerTransportStream is a minimal interface that a transport stream must
// implement. This can be used to mock an actual transport stream for tests of
// handler code that use, for example, grpc.SetHeader (which requires some
// stream to be in context).
//
// See also NewContextWithServerTransportStream.
//
// This API is EXPERIMENTAL.
type ServerTransportStream interface {
	Method() string
	SetHeader(md metadata.MD) error
	SendHeader(md metadata.MD) error
	SetTrailer(md metadata.MD) error
}

// ServerTransportStreamFromContext returns the ServerTransportStream saved in
// ctx. Returns nil if the given context has no stream associated with it
// (which implies it is not an RPC invocation context).
//
// This API is EXPERIMENTAL.
func ServerTransportStreamFromContext(ctx context.Context) ServerTransportStream {
	s, _ := ctx.Value(streamKey{}).(ServerTransportStream)
	return s
}

// Stop stops the gRPC server. It immediately closes all open
// connections and listeners.
// It cancels all active RPCs on the server side and the corresponding
// pending RPCs on the client side will get notified by connection
// errors.
func (s *Server) Stop() {
	s.quit.Fire()

	defer func() {
		s.serveWG.Wait()
		s.done.Fire()
	}()

	s.channelzRemoveOnce.Do(func() {
		if channelz.IsOn() {
			channelz.RemoveEntry(s.channelzID)
		}
	})

	s.mu.Lock()
	listeners := s.lis
	s.lis = nil
	st := s.conns
	s.conns = nil
	// interrupt GracefulStop if Stop and GracefulStop are called concurrently.
	s.cv.Broadcast()
	s.mu.Unlock()

	for lis := range listeners {
		lis.Close()
	}
	for c := range st {
		c.Close()
	}

	s.mu.Lock()
	if s.events != nil {
		s.events.Finish()
		s.events = nil
	}
	s.mu.Unlock()
}

// GracefulStop stops the gRPC server gracefully. It stops the server from
// accepting new connections and RPCs and blocks until all the pending RPCs are
// finished.
func (s *Server) GracefulStop() {
	s.quit.Fire()
	defer s.done.Fire()

	s.channelzRemoveOnce.Do(func() {
		if channelz.IsOn() {
			channelz.RemoveEntry(s.channelzID)
		}
	})
	s.mu.Lock()
	if s.conns == nil {
		s.mu.Unlock()
		return
	}

	for lis := range s.lis {
		lis.Close()
	}
	s.lis = nil
	if !s.drain {
		for st := range s.conns {
			st.Drain()
		}
		s.drain = true
	}

	// Wait for serving threads to be ready to exit.  Only then can we be sure no
	// new conns will be created.
	s.mu.Unlock()
	s.serveWG.Wait()
	s.mu.Lock()

	for len(s.conns) != 0 {
		s.cv.Wait()
	}
	s.conns = nil
	if s.events != nil {
		s.events.Finish()
		s.events = nil
	}
	s.mu.Unlock()
}

// contentSubtype must be lowercase
// cannot return nil
func (s *Server) getCodec(contentSubtype string) baseCodec {
	if s.opts.codec != nil {
		return s.opts.codec
	}
	if contentSubtype == "" {
		return encoding.GetCodec(proto.Name)
	}
	codec := encoding.GetCodec(contentSubtype)
	if codec == nil {
		return encoding.GetCodec(proto.Name)
	}
	return codec
}

// SetHeader sets the header metadata.
// When called multiple times, all the provided metadata will be merged.
// All the metadata will be sent out when one of the following happens:
//  - grpc.SendHeader() is called;
//  - The first response is sent out;
//  - An RPC status is sent out (error or success).
func SetHeader(ctx context.Context, md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	return stream.SetHeader(md)
}

// SendHeader sends header metadata. It may be called at most once.
// The provided md and headers set by SetHeader() will be sent.
func SendHeader(ctx context.Context, md metadata.MD) error {
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	if err := stream.SendHeader(md); err != nil {
		return toRPCErr(err)
	}
	return nil
}

// SetTrailer sets the trailer metadata that will be sent when an RPC returns.
// When called more than once, all the provided metadata will be merged.
func SetTrailer(ctx context.Context, md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	stream := ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return status.Errorf(codes.Internal, "grpc: failed to fetch the stream from the context %v", ctx)
	}
	return stream.SetTrailer(md)
}

// Method returns the method string for the server context.  The returned
// string is in the format of "/service/method".
func Method(ctx context.Context) (string, bool) {
	s := ServerTransportStreamFromContext(ctx)
	if s == nil {
		return "", false
	}
	return s.Method(), true
}

type channelzServer struct {
	s *Server
}

func (c *channelzServer) ChannelzMetric() *channelz.ServerInternalMetric {
	return c.s.channelzMetric()
}
